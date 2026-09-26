package hostapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/proc"
)

// pluginsUI hands the lists /plugins opens and the toasts it shows to the
// test, from whichever goroutine sends them.
type pluginsUI struct {
	fakeUI
	opened chan Overlay
	notes  chan string
}

func (u *pluginsUI) OpenOverlay(o Overlay) { u.opened <- o }
func (u *pluginsUI) Notify(level logging.Level, title, body string) {
	u.notes <- notifyName(level) + ": " + title + ": " + body
}

// TestPluginsCommand runs /plugins verbs in order over one plugin tree and
// reads the list each one leaves, with the toasts it showed.
func TestPluginsCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(tmp, "gitconfig")
	os.WriteFile(global, []byte("[user]\nname = t\nemail = t@t\n"), 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	dirs := config.Dirs{ProjectDir: tmp, DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state")}
	src := filepath.Join(tmp, "src", "x")
	for p, text := range map[string]string{
		filepath.Join(dirs.PluginsRoot(), "local", "a", "init.lua"): "-- a todo list\nzemi.command.add('todo', function() end, {})",
		filepath.Join(dirs.PluginsRoot(), "local", "b", "init.lua"): "error('broken')",
		filepath.Join(src, "init.lua"):                              "-- the x plugin\n",
	} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "x"}, {"tag", "v1.0.0"}, {"clone", "-q", "--bare", ".", "../x.git"}} {
		if out, err := (proc.Git{Dir: src, Foreign: true}).Run(t.Context(), args...); err != nil {
			t.Fatal(err, out)
		}
	}
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin}}})
	h := NewHost(r, log, config.NewStore(), dirs)
	ui := &pluginsUI{opened: make(chan Overlay, 4), notes: make(chan string, 4)}
	h.SetUIHooks(ui)
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(), h.PluginsStep()}); err == nil {
		t.Fatal("plugin b's error must fail the step")
	}

	var list Overlay
	for _, c := range []struct {
		name, args string
		reload     bool   // read the open list's rebuild: the verb opens nothing
		err        string // a usage error, which opens nothing
		want, not  []string
	}{
		{name: "a verb /plugins does not take", args: "frob", err: "| toggle <name> | open <name> (TUI only)"},
		{name: "toggle without a name", args: "toggle", err: "usage: plugins"},
		{
			name: "the list", args: "",
			want: []string{
				"text:Install a plugin verb:install", "run:plugins check text:Check for updates verb:check",
				"desc:a todo list", "provides: command /todo", "on:true run:plugins toggle a section:Local text:a",
				"desc:failed", "status: failed\nerror: ", "broken", "hotkey:d run:plugins disable a",
				"hl:Danger hotkey:x run:plugins delete a text:Delete",
			},
			not: []string{"hotkey:u"},
		},
		{
			name: "toggle flips the switch before any reload", args: "toggle a", reload: true,
			want: []string{"on:false run:plugins toggle a", "status: off", "hotkey:e run:plugins enable a"},
		},
		{name: "delete a local plugin", args: "delete b", want: []string{"info: Plugin deleted: b"}, not: []string{"text:b"}},
		{
			name: "install from a bare repo", args: "install file://" + filepath.Join(tmp, "src", "x.git"),
			want: []string{
				"info: Plugin installed: x v1.0.0", "section:Installed text:x", "desc:v1.0.0 · the x plugin",
				"version: v1.0.0", "status: starting", "hl:Danger hotkey:x run:plugins delete x text:Delete",
			},
		},
		{name: "check with nothing newer", args: "check", want: []string{"info: Plugins up to date: ", "text:x"}},
		{name: "update with nothing newer", args: "update", want: []string{"info: Plugins up to date: ", "text:x"}, not: []string{"Plugin updated"}},
	} {
		if err := h.RunCommand(t.Context(), "plugins", c.args); c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) || len(ui.opened) > 0 {
				t.Fatalf("%s: err %v, %d lists opened", c.name, err, len(ui.opened))
			}
			continue
		} else if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.reload {
			list.Items, _ = list.Reload()
		} else {
			select {
			case list = <-ui.opened:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s: no list opened", c.name)
			}
		}
		got := fmt.Sprint(list.Items)
		for len(ui.notes) > 0 {
			got += "\n" + <-ui.notes
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, got)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q in\n%s", c.name, n, got)
			}
		}
	}
}

// TestPluginsOpen: Open in the editor opens init.lua, or the directory of a
// plugin that has none.
func TestPluginsOpen(t *testing.T) {
	dirs := config.Dirs{ProjectDir: t.TempDir(), DataHome: t.TempDir()}
	for _, name := range []string{"lua", "skills"} {
		os.MkdirAll(filepath.Join(dirs.PluginsRoot(), "local", name), 0o755)
	}
	os.WriteFile(filepath.Join(dirs.PluginsRoot(), "local", "lua", "init.lua"), nil, 0o644)
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), dirs)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	edited := make(chan string, 1)
	h.SetUIHooks(&fakeUI{edit: func(path string) { edited <- path }})
	for name, want := range map[string]string{"lua": "lua/init.lua", "skills": "skills"} {
		if err := h.RunCommand(t.Context(), "plugins", "open "+name); err != nil {
			t.Fatal(err)
		}
		if got := <-edited; got != filepath.Join(dirs.PluginsRoot(), "local", want) {
			t.Errorf("%s opened %s", name, got)
		}
	}
}

// TestPluginsNew: the list offers New plugin first, and /plugins new sends
// the agent to zemi-customize with what the user typed.
func TestPluginsNew(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	if got := fmt.Sprint(h.pluginItems()[0]); !strings.Contains(got, "fill:/plugins new  text:New plugin verb:new") {
		t.Fatalf("first row: %s", got)
	}
	if err := h.pluginsCommand([]string{"new", "a", "clock"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if m.streams.Load() == 0 {
			return errors.New("nothing sent")
		}
		return nil
	})
	waitIdle(t, h)
	msgs, err := h.DB.Messages(h.AgentSessionID())
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages %v, %v", msgs, err)
	}
	if sent := msgs[0].PartsJSON; !strings.Contains(sent, "zemi-customize") || !strings.Contains(sent, "What it should do: a clock") {
		t.Fatalf("sent %s", sent)
	}
}
