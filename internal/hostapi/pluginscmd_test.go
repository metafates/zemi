package hostapi

import (
	"context"
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

func (u *pluginsUI) OpenOverlay(o Overlay)            { u.opened <- o }
func (u *pluginsUI) Notify(level, title, body string) { u.notes <- level + ": " + title + ": " + body }

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
				"desc:a todo list", "provides: command /todo", "key:on run:plugins toggle a section:Local text:a",
				"desc:failed", "status: failed\nerror: ", "broken", "hotkey:d run:plugins disable a",
			},
			not: []string{"text:Delete", "hotkey:u"},
		},
		{
			name: "toggle flips the tag before any reload", args: "toggle a", reload: true,
			want: []string{"key:off run:plugins toggle a", "status: off", "hotkey:e run:plugins enable a"},
		},
		{name: "a local plugin is not deleted", args: "delete b", want: []string{"error: Could not run /plugins: b is a local plugin", "text:b"}},
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
