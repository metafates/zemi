package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// TestSplashExtends: user config can wrap the default splash through
// require("zemi.tui").splash (the zemi skill's ui.md example).
func TestSplashExtends(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 30)
	runLua(t, app, `
local default = require("zemi.tui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    local rows = default.splash(ctx)
    for i, row in ipairs(rows) do
      if row[#row].text == "@zemi" then
        table.insert(rows, i + 1, { row[1], { text = "hack the planet", hl = "Title" } })
        break
      end
    end
    return rows
  end,
})`)
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "hack the planet") || !strings.Contains(frame, "╰────╯") {
		t.Fatalf("splash not extended:\n%s", frame)
	}
}

// TestTUIPluginRequire: plugin code can require zemi.tui (its
// body must not rerun under the plugin's origin) but cannot write into it.
func TestTUIPluginRequire(t *testing.T) {
	app, host := bootAgentApp(t, 80, 30)
	src := `
local d = require("zemi.tui")
assert(type(d.splash) == "function", "splash missing")
assert(not pcall(function() d.splash = print end), "plugin wrote into the builtin module")
assert(require("zemi.tui") == d, "view not cached")`
	plugin := filepath.Join(host.Dirs.PluginsRoot(), "local", "p", "init.lua")
	if err := host.RT.ExecuteChunk(context.Background(), src, plugin, lua.OriginPlugin); err != nil {
		t.Fatal(err)
	}
	runLua(t, app, `assert(require("zemi.tui").splash ~= print)`)
}

// TestSplashFitsShortChat: a short chat drops the recent sessions, then
// the mascot, then the context row, instead of cutting the block's top rows.
func TestSplashFitsShortChat(t *testing.T) {
	for h, want := range map[int]string{11: "@zemi", 14: "context  ", 15: "╭────╮", 21: "3m ago"} {
		app, _ := bootAgentApp(t, 80, h)
		runLua(t, app, `
local default = require("zemi.tui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    ctx.recent = { { id = "a", ago = "1m ago", title = "a" }, { id = "b", ago = "2m ago", title = "b" }, { id = "c", ago = "3m ago", title = "c" } }
    return default.splash(ctx)
  end,
})`)
		app.renderSync(t)
		app.Frame() // lays out the chat, so the next render knows its height
		app.frameGen++
		app.renderSync(t)
		if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, want) || strings.Contains(frame, "╭────╮") != (h > 14) {
			t.Fatalf("%d rows: want %q\n%s", h, want, frame)
		}
	}
}

// TestSplashWrapsModelHint: the no-model hint names the fix, so it wraps
// under its label instead of losing its end, and picking a model leaves the
// block where it was.
func TestSplashWrapsModelHint(t *testing.T) {
	app, _ := bootAgentApp(t, 120, 30)
	col := -1
	for _, model := range []string{"", "a/b"} {
		runLua(t, app, `
local default = require("zemi.tui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    ctx.model, ctx.model_hint = "`+model+`", "export ANTHROPIC_API_KEY and start zemi again, or add a provider to ~/.config/zemi/init.lua"
    return default.splash(ctx)
  end,
})`)
		app.frameGen++
		app.renderSync(t)
		frame := ansi.Strip(app.Frame())
		if model == "" && (!strings.Contains(frame, "none yet: export ANTHROPIC_API_KEY and start zemi again, or") || !strings.Contains(frame, "         add a provider to ~/.config/zemi/init.lua")) {
			t.Fatalf("hint cut:\n%s", frame)
		}
		before, _, _ := strings.Cut(frame, "@zemi")
		if c := len(before) - strings.LastIndex(before, "\n"); col == -1 {
			col = c
		} else if c != col {
			t.Fatalf("model %q moved @zemi:\n%s", model, frame)
		}
	}
}

// TestSplashContext: the context row names what a send carries; /init is
// offered only in a trusted project with no instruction file, $HOME is
// nobody's project, and a zero count is left out.
func TestSplashContext(t *testing.T) {
	app, host := bootAgentApp(t, 80, 30)
	proj := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		dir       string
		trusted   bool
		file      string
		memory    bool
		want, not string
	}{
		// the app boots with no project, so /trust has nothing to turn on; the
		// golden frame pins an untrusted project's "instructions off until /trust"
		{"no project", "", false, "", false, "context  no project instructions", "off until"},
		{"no sessions", proj, true, "", false, `try      "explain this project"`, ""},
		{"trusted", proj, true, "", false, "context  /init writes AGENTS.md", ""},
		{"has CLAUDE.md", proj, true, "CLAUDE.md", false, "context  CLAUDE.md", "writes AGENTS.md"},
		{"trusted home", home, true, "", false, "context  no project instructions", ""},
		{"one memory", proj, true, "CLAUDE.md", true, "context  CLAUDE.md · 1 memory", "skill"},
	} {
		host.Dirs.ProjectDir = c.dir
		host.SetProjectTrusted(c.trusted)
		if c.file != "" {
			if err := os.WriteFile(filepath.Join(c.dir, c.file), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if c.memory {
			if err := host.GlobalDB.AddMemory(&store.Memory{Scope: store.ScopeUser, Text: "prefers tabs", ProvenanceSessionID: "seed", Origin: store.OriginUser}); err != nil {
				t.Fatal(err)
			}
		}
		app.frameGen++
		app.renderSync(t)
		frame := ansi.Strip(app.Frame())
		if !strings.Contains(frame, c.want) || c.not != "" && strings.Contains(frame, c.not) {
			t.Errorf("%s: want %q, not %q\n%s", c.name, c.want, c.not, frame)
		}
	}
}

// TestSplashProjects: with another project known the splash lists it where
// recent sessions go, a click on its path switches there, and with none it
// offers what to try.
func TestSplashProjects(t *testing.T) {
	for _, c := range []struct {
		name      string
		other     bool
		want, not []string
	}{
		{"no other project", false, []string{`try      "explain this project"`}, []string{"projects"}},
		{"another project", true, []string{"projects", "now      ", "/projects switches, or type to stay here"}, []string{"try"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, host := bootAgentApp(t, 100, 30)
			root := t.TempDir()
			if c.other {
				g, p, err := store.OpenForDirs(host.Dirs.StateHome, root)
				if err != nil {
					t.Fatal(err)
				}
				g.Close()
				p.Close()
				c.want = append(c.want, root[len(root)-20:]) // a long path keeps its tail
			}
			app.frameGen++
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			for _, w := range c.want {
				if !strings.Contains(frame, w) {
					t.Errorf("no %q in\n%s", w, frame)
				}
			}
			for _, n := range c.not {
				if strings.Contains(frame, n) {
					t.Errorf("%q in\n%s", n, frame)
				}
			}
			if c.other {
				x, y := findCell(t, app, root[len(root)-20:])
				if seg := dropCells(app.lastRows[y], x)[0]; seg.Click != "projects switch "+root || seg.Widget != "splash" {
					t.Fatalf("the path's segment: %+v", seg)
				}
			}
		})
	}
}

// TestFarewell: what zemi prints to the shell on quit, the exit region.
func TestFarewell(t *testing.T) {
	for _, c := range []struct {
		name      string
		width     int
		profile   colorprofile.Profile
		title     string
		cost      float64
		send      bool
		booting   bool
		want, not []string
	}{
		{"no messages", 80, colorprofile.TrueColor, "t", 0, false, false, nil, []string{"zemi"}},
		// a quit during the trust prompt: that boot step never returns
		{"booting", 80, colorprofile.TrueColor, "t", 0, false, true, nil, []string{"zemi"}},
		{"plain", 80, colorprofile.NoTTY, "Fix the flaky test", 0.42, true, false, []string{"@zemi", "Fix the flaky test", "╭────╮", "cost     $0.42"}, []string{"\x1b"}},
		{"color", 80, colorprofile.TrueColor, "t", 0, true, false, []string{"\x1b[38;"}, []string{"[48;", ";48;2;"}},
		{"narrow", 30, colorprofile.NoTTY, "t", 0, true, false, []string{"  @zemi  t\n"}, []string{"╭────╮"}},
		{"untitled", 80, colorprofile.NoTTY, "zemi", 0, true, false, []string{"fix the build"}, []string{"more", "cost"}},
		{"control chars", 80, colorprofile.TrueColor, "a\x1b]52;c;x\x07b", 0, true, false, []string{"  ab\n"}, []string{"\x1b]52"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, host := bootAgentApp(t, c.width, 30)
			app.colors = c.profile
			db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			host.DB = db
			if c.send {
				sid := store.NewID()
				if err := db.CreateSession(sid, "", c.title, ""); err != nil {
					t.Fatal(err)
				}
				if err := db.AddUsage(sid, 0, 0, c.cost); err != nil {
					t.Fatal(err)
				}
				if err := host.ResumeSession(sid); err != nil {
					t.Fatal(err)
				}
				host.AppendMessage("user", "  fix the build\nmore")
				// whole even where the terminal wraps it
				c.want = append(c.want, hostapi.ResumeCommand("", sid))
			}
			if c.booting {
				block, reached := make(chan struct{}), make(chan struct{})
				t.Cleanup(func() { close(block) })
				go func() {
					_ = host.RT.Boot(context.Background(), []lua.BootStep{host.NamespacesStep(), lua.RequireStep("zemi.tui"), {
						Name: "trust", Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
							close(reached)
							<-block
							return nil
						},
					}})
				}()
				<-reached
			}
			done := make(chan string, 1)
			go func() { done <- app.Farewell() }()
			var out string
			select {
			case out = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Farewell held the exit for 3s")
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q:\n%s", w, out)
				}
			}
			for _, n := range c.not {
				if strings.Contains(out, n) {
					t.Errorf("has %q:\n%q", n, out)
				}
			}
		})
	}
}
