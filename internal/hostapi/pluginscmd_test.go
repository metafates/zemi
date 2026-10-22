package hostapi

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/proc"
)

// pluginsUI hands the lists /plugin opens and the toasts it shows to the
// test, from whichever goroutine sends them.
type pluginsUI struct {
	fakeUI
	opened chan Overlay
	notes  chan string
}

func (u *pluginsUI) OpenOverlay(o Overlay) { u.opened <- o }
func (u *pluginsUI) Notify(level slog.Level, title, body string) {
	u.notes <- notifyName(level) + ": " + title + ": " + body
}

// TestPluginsCommand runs /plugin verbs in order over one plugin tree and
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
	src, mkt := filepath.Join(tmp, "src", "x"), filepath.Join(tmp, "src", "mkt")
	for p, text := range map[string]string{
		filepath.Join(dirs.PluginsRoot(), "local", "a", plugin.InitFile):     "-- a todo list\nzemi.command.add('todo', function() end, {})\nzemi.config.add('live', { kind = 'bool', default = true, desc = 'live' })",
		filepath.Join(dirs.PluginsRoot(), "local", "a", plugin.ManifestFile): `{"version": "0.1.0", "description": "a todo list in the statusline"}`,
		filepath.Join(dirs.PluginsRoot(), "local", "b", plugin.InitFile):     "error('broken')",
		filepath.Join(src, plugin.InitFile):                                  "-- the x plugin\n",
		filepath.Join(mkt, plugin.ZemiMarketplaceFile): fmt.Sprintf(`{"name": "test", "plugins": [
			{"name": "x", "source": {"source": "git", "url": "file://%s"}, "description": "the x plugin", "category": "tools"},
			{"name": "two", "source": "./two", "description": "the two plugin"}]}`,
			filepath.Join(tmp, "src", "x.git")),
	} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "x"}, {"tag", "v1.0.0"}, {"clone", "-q", "--bare", ".", "../x.git"}} {
		if out, err := (proc.Git{Dir: src, Foreign: true}).Run(t.Context(), args...); err != nil {
			t.Fatal(err, out)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "m"}} {
		if out, err := (proc.Git{Dir: mkt, Foreign: true}).Run(t.Context(), args...); err != nil {
			t.Fatal(err, out)
		}
	}
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin}}})
	h := NewHost(r, log, config.NewStore(), dirs)
	ui := &pluginsUI{opened: make(chan Overlay, 4), notes: make(chan string, 4)}
	h.SetUIHooks(ui)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(), h.PluginsStep()}); err == nil {
		t.Fatal("plugin b's error must fail the step")
	}

	var list Overlay
	for _, c := range []struct {
		name, args string
		reload     bool   // read the open list's rebuild: the verb opens nothing
		err        string // a usage error, which opens nothing
		want, not  []string
	}{
		{name: "a verb /plugin does not take", args: "frob", err: "| toggle <name> | open <name>"},
		{name: "toggle without a name names its own grammar", args: "toggle", err: "usage: /plugin toggle <name>"},
		{
			name: "the list", args: "",
			want: []string{
				"text:Install a plugin verb:fill", "run:plugin check text:Check for updates verb:run",
				"desc:0.1.0 · a todo list in the statusline", "[provides command /todo · setting plugins.a.live]",
				"[version 0.1.0]", "on:true run:plugin toggle a section:Local text:a",
				"desc:failed", "[status failed]", "broken", "desc:disable key:d run:plugin disable a",
				"danger:true desc:delete key:x run:plugin delete a --yes",
			},
			not: []string{"key:u"},
		},
		{
			name: "toggle flips the switch before any reload", args: "toggle a", reload: true,
			want: []string{"on:false run:plugin toggle a", "[status off]", "desc:enable key:e run:plugin enable a"},
		},
		{name: "delete a local plugin", args: "delete b --yes", want: []string{"info: Plugin deleted: b"}, not: []string{"text:b"}},
		{
			name: "marketplace add", args: "marketplace add " + mkt,
			want: []string{
				"info: Marketplace added: marketplace test added (2 plugins)",
				"section:Marketplaces text:test", "desc:2 plugins",
				"verb:open", "run:plugin marketplace test", "desc:update key:u run:plugin marketplace update test", "danger:true desc:remove key:x run:plugin marketplace remove test",
			},
		},
		{
			name: "the marketplace drill-down", args: "marketplace test",
			want: []string{
				"section:Marketplace text:" + perm.TildePath(mkt), "desc:update key:u run:plugin marketplace update test",
				"danger:true desc:remove key:x run:plugin marketplace remove test",
				"section:tools text:x", "desc:the x plugin", "key:i run:plugin install x@test",
				"section:Other text:two", "desc:the two plugin", "key:i run:plugin install two@test",
				"[category tools]",
			},
			not: []string{"installed as x"},
		},
		{
			name: "install from the marketplace", args: "install x@test",
			want: []string{
				"info: Plugin installed: x v1.0.0", "section:Installed text:x", "desc:v1.0.0 · from test · the x plugin",
				"[version v1.0.0]", "[status starting]", "[marketplace test]",
				"danger:true desc:delete key:x run:plugin delete x --yes",
			},
		},
		{
			name: "the drill-down marks an entry installed", args: "marketplace test",
			want: []string{"desc:installed as x", "desc:update key:u run:plugin update x"},
			not:  []string{"key:i run:plugin install x@test"},
		},
		{
			name: "the drill-down of a marketplace nobody registered", args: "marketplace frob",
			want: []string{"text:Marketplace removed", "desc:no marketplace is named frob"},
		},
		{name: "check with nothing newer", args: "check", want: []string{"info: Plugins up to date: ", "text:x"}},
		{name: "update with nothing newer", args: "update", want: []string{"info: Plugins up to date: ", "text:x"}, not: []string{"Plugin updated"}},
	} {
		if err := h.RunCommand(t.Context(), "plugin", c.args); c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) || len(ui.opened) > 0 {
				t.Fatalf("%s: err %v, %d lists opened", c.name, err, len(ui.opened))
			}
			continue
		} else if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.reload {
			list.Items = list.Reload()
		} else {
			select {
			case list = <-ui.opened:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s: no list opened", c.name)
			}
		}
		got := dumpItems(list.Items)
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
	// the install completer offers the entries the registered marketplaces list
	if got := fmt.Sprint(h.pluginsCommands()[0].GoComplete(h, "", "/plugin install ")); !strings.Contains(got, "x@test") {
		t.Errorf("/plugin install completes %s, want x@test", got)
	}
}

// TestMarketplaceLabel: the busy line names what it acts on, the source for
// add, whose clone decides the name, and the argument for the rest.
func TestMarketplaceLabel(t *testing.T) {
	for _, c := range []struct {
		args string
		want string
	}{
		{"marketplace add owner/repo", "Adding marketplace owner/repo"},
		{"marketplace remove test", "Removing marketplace test"},
		{"marketplace update test", "Updating marketplace test"},
		{"marketplace update", "Updating marketplaces"},
	} {
		if got := marketplaceLabel(strings.Fields(c.args)); got != c.want {
			t.Errorf("%s: %q, want %q", c.args, got, c.want)
		}
	}
}

// TestPluginsOpen: Open in the editor opens .zemi/init.lua, or the directory
// of a plugin that has none.
func TestPluginsOpen(t *testing.T) {
	dirs := config.Dirs{ProjectDir: t.TempDir(), DataHome: t.TempDir()}
	os.MkdirAll(filepath.Join(dirs.PluginsRoot(), "local", "skills"), 0o755)
	os.MkdirAll(filepath.Join(dirs.PluginsRoot(), "local", "lua", plugin.CodeDir), 0o755)
	os.WriteFile(filepath.Join(dirs.PluginsRoot(), "local", "lua", plugin.InitFile), nil, 0o644)
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), dirs)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	edited := make(chan string, 1)
	h.SetUIHooks(&fakeUI{edit: func(path string) { edited <- path }})
	for name, want := range map[string]string{"lua": "lua/" + plugin.InitFile, "skills": "skills"} {
		if err := h.RunCommand(t.Context(), "plugin", "open "+name); err != nil {
			t.Fatal(err)
		}
		if got := <-edited; got != filepath.Join(dirs.PluginsRoot(), "local", want) {
			t.Errorf("%s opened %s", name, got)
		}
	}
}

// TestPluginsSend: the list offers New plugin first and a local plugin's
// detail Modify, and /plugin modify, headless too, sends the typed line with the plugin's
// prompt behind it and lets auto mode edit that local plugin and nothing
// else, and refuses an installed or a symlinked one; TestCommandSendsTypedLine
// covers /plugin new.
func TestPluginsSend(t *testing.T) {
	for _, c := range []struct {
		args     string
		err      string   // a usage error, which sends nothing
		sent     []string // in the message the agent gets
		link     bool     // the data dir is reached through a symlink
		headless bool     // no UI, as under zemi run
	}{
		{args: "modify mine add a flag\nand a test", sent: []string{`/plugin modify mine add a flag\nand a test`, "Change my zemi plugin mine in "}},
		{args: "modify mine", link: true, sent: []string{"Change my zemi plugin mine in "}},
		{args: "modify theirs fix it", err: "theirs is an installed plugin"},
		{args: "modify linked", err: "linked is a symlink"},
		{args: "modify", err: "usage: /plugin modify <name> [what to change]"},
		{args: "modify mine in zemi run", headless: true, sent: []string{"Change my zemi plugin mine in "}},
	} {
		t.Run(c.args, func(t *testing.T) {
			data := t.TempDir()
			for _, dir := range []string{"local/mine", "installed/theirs"} {
				os.MkdirAll(filepath.Join(data, "plugins", dir), 0o755)
			}
			if c.link {
				link := filepath.Join(t.TempDir(), "data")
				os.Symlink(data, link)
				data = link
			}
			log := slog.New(slog.DiscardHandler)
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
			h := NewHost(r, log, config.NewStore(), config.Dirs{ConfigHome: t.TempDir(), StateHome: t.TempDir(), ProjectDir: t.TempDir(), DataHome: data})
			if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
				t.Fatal(err)
			}
			m := reflectStub(t, h, "ok")
			if !c.headless {
				h.SetUIHooks(&fakeUI{})
			}
			items := dumpItems(h.pluginItems())
			if !strings.Contains(items, "fill:/plugin new  text:New plugin verb:fill") || strings.Count(items, "modify with zemi") != 1 {
				t.Fatalf("items: %s", items)
			}
			if got := fmt.Sprint(h.pluginsCommands()[0].GoComplete(h, "", "/plugin modify ")); got != "[mine]" {
				t.Fatalf("/plugin modify completes %s", got)
			}
			os.Symlink("mine", filepath.Join(data, "plugins", "local", "linked"))
			err := h.pluginsCommand(c.args)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err %v, want %q", err, c.err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if c.sent == nil {
				return
			}
			waitFor(t, func() error {
				if m.streams.Load() == 0 {
					return errors.New("nothing sent")
				}
				return nil
			})
			waitIdle(t, h)
			msgs, err := h.DB.UserMessages(1)
			if err != nil || len(msgs) == 0 {
				t.Fatalf("messages %v, %v", msgs, err)
			}
			for _, s := range c.sent {
				if !strings.Contains(msgs[0].PartsJSON, s) {
					t.Errorf("sent %s, want %q in it", msgs[0].PartsJSON, s)
				}
			}
		})
	}
}
