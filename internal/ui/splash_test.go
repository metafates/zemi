package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

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
    ctx.recent = { { ago = "1m ago", title = "a" }, { ago = "2m ago", title = "b" }, { ago = "3m ago", title = "c" } }
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
    ctx.model, ctx.model_hint = "`+model+`", "export ANTHROPIC_API_KEY or add a provider to ~/.config/zemi/init.lua"
    return default.splash(ctx)
  end,
})`)
		app.frameGen++
		app.renderSync(t)
		frame := ansi.Strip(app.Frame())
		if model == "" && (!strings.Contains(frame, "none yet: export ANTHROPIC_API_KEY or add a provider to") || !strings.Contains(frame, "         ~/.config/zemi/init.lua")) {
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
		{"untrusted", proj, false, "", false, "context  no project instructions", ""},
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
