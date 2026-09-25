package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/lua"
)

// TestSplashExtends: user config can wrap the default splash through
// require("zemi.tui").splash (the zemi-customize skill's example).
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
// the mascot, instead of cutting the block's top rows.
func TestSplashFitsShortChat(t *testing.T) {
	for h, want := range map[int]string{13: "@zemi", 14: "╭────╮", 20: "3m ago"} {
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
		if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, want) || strings.Contains(frame, "╭────╮") != (h > 13) {
			t.Fatalf("%d rows: want %q\n%s", h, want, frame)
		}
	}
}

// TestSplashWrapsModelHint: the no-model hint names the fix, so it wraps
// under its label instead of losing its end.
func TestSplashWrapsModelHint(t *testing.T) {
	app, _ := bootAgentApp(t, 120, 30)
	runLua(t, app, `
local default = require("zemi.tui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    ctx.model, ctx.model_hint = "", "export ANTHROPIC_API_KEY or add a provider to ~/.config/zemi/init.lua"
    return default.splash(ctx)
  end,
})`)
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "none yet: export ANTHROPIC_API_KEY or add a provider to") || !strings.Contains(frame, "         ~/.config/zemi/init.lua") {
		t.Fatalf("hint cut:\n%s", frame)
	}
}

// TestSplashInitHint: only a trusted project with no instruction file is
// offered /init, and $HOME is nobody's project.
func TestSplashInitHint(t *testing.T) {
	app, host := bootAgentApp(t, 80, 30)
	proj := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		dir     string
		trusted bool
		file    string
		want    bool
	}{
		{"untrusted", proj, false, "", false},
		{"trusted", proj, true, "", true},
		{"has CLAUDE.md", proj, true, "CLAUDE.md", false},
		{"trusted home", home, true, "", false},
	} {
		host.Dirs.ProjectDir = c.dir
		host.SetProjectTrusted(c.trusted)
		if c.file != "" {
			if err := os.WriteFile(filepath.Join(c.dir, c.file), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		app.frameGen++
		app.renderSync(t)
		if frame := ansi.Strip(app.Frame()); strings.Contains(frame, "no AGENTS.md yet · /init writes one") != c.want {
			t.Errorf("%s: want hint %v\n%s", c.name, c.want, frame)
		}
	}
}
