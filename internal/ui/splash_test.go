package ui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/lua"
)

// TestSplashExtends: user config can wrap the default splash through
// require("zemi.defaultui").splash (the zemi-customize skill's example).
func TestSplashExtends(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 30)
	runLua(t, app, `
local default = require("zemi.defaultui")
zemi.ui.widget.register({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    local rows = default.splash(ctx)
    for i, row in ipairs(rows) do
      if row[2] and row[2].hl == "Logo6" then
        table.insert(rows, i + 1, { row[1], { text = "hack the planet", hl = "Title" } })
        break
      end
    end
    return rows
  end,
})`)
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "hack the planet") || !strings.Contains(frame, "╚══════╝") {
		t.Fatalf("splash not extended:\n%s", frame)
	}
}

// TestDefaultUIPluginRequire: plugin code can require zemi.defaultui (its
// body must not rerun under the plugin's origin) but cannot write into it.
func TestDefaultUIPluginRequire(t *testing.T) {
	app, host := bootAgentApp(t, 80, 30)
	src := `
local d = require("zemi.defaultui")
assert(type(d.splash) == "function", "splash missing")
assert(not pcall(function() d.splash = print end), "plugin wrote into the builtin module")
assert(require("zemi.defaultui") == d, "view not cached")`
	plugin := filepath.Join(host.Dirs.DataHome, "site", "pack", "p", "init.lua")
	if err := host.RT.ExecuteChunk(context.Background(), src, plugin, lua.OriginPlugin); err != nil {
		t.Fatal(err)
	}
	runLua(t, app, `assert(require("zemi.defaultui").splash ~= print)`)
}

// TestSplashFitsShortChat: a short chat drops the recent sessions, then
// the logo, instead of cutting the logo's top rows.
func TestSplashFitsShortChat(t *testing.T) {
	for h, want := range map[int]string{16: "zemi", 17: "███████╗███████╗███╗", 23: "3m ago"} {
		app, _ := bootAgentApp(t, 80, h)
		runLua(t, app, `
local default = require("zemi.defaultui")
zemi.ui.widget.register({
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
		if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, want) || strings.Contains(frame, "╚══") != (want != "zemi") {
			t.Fatalf("%d rows: want %q\n%s", h, want, frame)
		}
	}
}
