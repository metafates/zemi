package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// TestSplashExtends: user config can wrap the default splash through
// require("zemi.tui").splash (the zemi skill's ui.md example), and a
// replaced tip leaves nothing of a default one that wrapped.
func TestSplashExtends(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 44, 30)
	after := func() string {
		app.renderSync(t)
		lines := strings.Split(ansi.Strip(app.Frame()), "\n")
		for i, l := range lines[:len(lines)-1] {
			if strings.Contains(l, "● Tip") {
				return strings.TrimSpace(lines[i+1])
			}
		}
		t.Fatalf("no tip:\n%s", app.Frame())
		return ""
	}
	if after() == "" {
		t.Fatal("the default tip does not wrap at this width")
	}
	runLua(t, app, `
local default = require("zemi.tui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    local rows = default.splash(ctx)
    for i, row in ipairs(rows) do
      if row[2] and row[2].text == "● Tip  " then
        rows[i] = { row[1], row[2], { text = "hack the planet", hl = "Dim" } }
        if rows[i + 1] and rows[i + 1][2] then rows[i + 1] = { { text = "" } } end
        break
      end
    end
    return rows
  end,
})`)
	if next := after(); next != "" || !strings.Contains(ansi.Strip(app.Frame()), "hack the planet") || !strings.Contains(ansi.Strip(app.Frame()), "+#%#@%@%%#%#:") {
		t.Fatalf("splash not extended, or the default tip's rest %q left:\n%s", next, app.Frame())
	}
}

// TestTUIPluginRequire: plugin code can require zemi.tui and reaches the same
// instance every requirer gets.
func TestTUIPluginRequire(t *testing.T) {
	t.Parallel()
	_, host := bootAgentApp(t, 80, 30)
	src := `
local d = require("zemi.tui")
assert(type(d.splash) == "function", "splash missing")
assert(require("zemi.tui") == d, "view not cached")`
	plugin := filepath.Join(host.Dirs.PluginsRoot(), "local", "p", ".zemi", "init.lua")
	if err := host.RT.ExecuteChunk(t.Context(), src, plugin, lua.OriginPlugin); err != nil {
		t.Fatal(err)
	}
}

// TestSplashFitsShortChat: a short chat drops the recent sessions, then
// the version, then the logo's art, and always keeps the tip. The pending
// row's row is reserved, so each drop happens one row earlier.
func TestSplashFitsShortChat(t *testing.T) {
	for _, c := range []struct {
		h         int
		want, not []string
	}{
		{25, []string{"3m ago", "v1", "+#%#@%@%%#%#:", "● Tip"}, nil},
		{24, []string{"v1", "+#%#@%@%%#%#:", "● Tip"}, []string{"3m ago"}},
		{20, []string{"+#%#@%@%%#%#:", "● Tip"}, []string{"v1"}},
		{18, []string{"zemi", "● Tip"}, []string{"+#%#@%@%%#%#:"}},
	} {
		app, _ := bootAgentAppPinned(t, 80, c.h)
		runLua(t, app, `
local default = require("zemi.tui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    ctx.version = "v1"
    ctx.projects = { { path = "~/a", root = "/a", ago = "1m ago" }, { path = "~/b", root = "/b", ago = "2m ago" }, { path = "~/c", root = "/c", ago = "3m ago" } }
    return default.splash(ctx)
  end,
})`)
		app.renderSync(t)
		app.Frame() // lays out the chat, so the next render knows its height
		app.frameGen++
		app.renderSync(t)
		frame := ansi.Strip(app.Frame())
		for _, w := range c.want {
			if !strings.Contains(frame, w) {
				t.Errorf("%d rows: no %q\n%s", c.h, w, frame)
			}
		}
		for _, n := range c.not {
			if strings.Contains(frame, n) {
				t.Errorf("%d rows: %q\n%s", c.h, n, frame)
			}
		}
	}
}

// TestSplashTip: the splash ends with the first tip that applies, the long
// no-model hint wrapped to a second row under its text, and a tip that wraps
// moves nothing above it, on a short chat too.
func TestSplashTip(t *testing.T) {
	hint := "export ANTHROPIC_API_KEY and start zemi again, or press enter on a provider in /model to paste its key"
	cases := []struct {
		name, ctx, want, fill string
		moved                 bool // the logo moved: the listed rows changed
	}{
		{"init.lua failed", `init_error = "/cfg/init.lua:12: boom", model = ""`, "● Tip  init.lua:12 failed: /errors", "", false},
		{"no model", `model = ""`, "● Tip  No model yet: export ANTHROPIC_API_KEY and start zemi again, or\npress enter on a provider in /model to paste its key", "", false},
		{"untrusted", `trusted = false`, "● Tip  /trust lets zemi read this project's instructions and run code", "", false},
		{"at home", `trusted = false, trustable = false`, "● Tip  !git status runs in your shell; the output rides your next prompt", "!git status", false},
		{"no instruction file", `instructions = {}`, "● Tip  /init writes an AGENTS.md, so zemi knows this project", "/init", false},
		{"other projects", `projects = { { path = "~/p", root = "/p", ago = "now" } }`, "● Tip  /projects switches to one, or type to stay here", "", true},
		{"a first session", `projects = {}`, `● Tip  !git status runs in your shell; the output rides your next prompt`, "!git status", false},
	}
	for _, h := range []int{31, 23} {
		app, _ := bootAgentApp(t, 100, h)
		logo := [2]int{-1, -1}
		for _, c := range cases {
			runLua(t, app, `
local default = require("zemi.tui")
zemi.ui.widget.add({
  id = "splash", region = "chat", priority = 5, on_click = require("zemi.tui.text").run_click,
  render = function(ctx)
    ctx.init_error, ctx.model, ctx.model_hint, ctx.trusted, ctx.trustable = "", "a/b", "`+hint+`", true, true
    ctx.instructions, ctx.projects = { "AGENTS.md" }, nil
    for k, v in pairs({ `+c.ctx+` }) do
      ctx[k] = v
    end
    return default.splash(ctx)
  end,
})`)
			app.frameGen++
			app.renderSync(t)
			var rows []string
			for r := range strings.SplitSeq(ansi.Strip(app.Frame()), "\n") {
				rows = append(rows, strings.TrimSpace(r))
			}
			if frame := strings.Join(rows, "\n"); !strings.Contains(frame, c.want) {
				t.Errorf("%d rows, %s: want %q\n%s", h, c.name, c.want, frame)
			}
			if x, y := findCell(t, app, "+#%#@%@%%#%#:"); logo[0] == -1 {
				logo = [2]int{x, y}
			} else if logo != [2]int{x, y} != c.moved {
				t.Errorf("%d rows, %s: the logo moved from %v to %d,%d", h, c.name, logo, x, y)
			}
			if c.fill != "" {
				x, y := findCell(t, app, c.fill)
				clickAt(app, x, y)
				for app.Pump() {
				}
				if app.composer.text != c.fill {
					t.Errorf("%s: a click on the tip left the composer %q", c.name, app.composer.text)
				}
				app.clearComposer() // a slash fill opened its palette
			}
		}
	}
}

// TestSplashTakesBootWork: the pending row of a boot's MCP starts is drawn in
// the splash's own block, which the chat region still fits at every size, so
// nothing scrolls; where the logo has room, it keeps its place as the work
// comes and goes.
func TestSplashTakesBootWork(t *testing.T) {
	for _, c := range []struct {
		w, h int
		logo bool // a chat with room for the logo and the pending row
	}{
		{80, 24, true},
		{40, 9, false},
	} {
		t.Run(fmt.Sprintf("%dx%d", c.w, c.h), func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, c.w, c.h)
			app.screen(t)
			x, y := -1, -1
			if c.logo {
				x, y = findCell(t, app, "+#%#@%@%%#%#:")
			}
			stop := host.Busy("Starting mcp_filesystem", nil)
			stop2 := host.Busy("Starting mcp_github", nil)
			app.frameGen++
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			i := slices.IndexFunc(app.shownWidgets(), func(w shownWidget) bool { return w.ID == "splash" })
			if i < 0 {
				t.Fatal("no splash is shown")
			}
			w := app.shownWidgets()[i]
			if rows := app.regionLines("chat", w.width, w.height); len(rows) > w.height {
				t.Fatalf("the chat's widgets draw %d rows into %d:\n%s", len(rows), w.height, frame)
			}
			if !c.logo {
				return
			}
			if !strings.Contains(frame, "starting mcp_filesystem, and 1 more") {
				t.Fatalf("the boot work is not drawn collapsed in the chat:\n%s", frame)
			}
			if nx, ny := findCell(t, app, "+#%#@%@%%#%#:"); nx != x || ny != y {
				t.Fatalf("the logo moved from %d,%d to %d,%d", x, y, nx, ny)
			}
			stop()
			stop2()
			app.frameGen++
			app.renderSync(t)
			if nx, ny := findCell(t, app, "+#%#@%@%%#%#:"); nx != x || ny != y {
				t.Fatalf("the logo moved back to %d,%d from %d,%d", nx, ny, x, y)
			}
		})
	}
}

// TestSplashProjects: with another project known the splash lists it where
// recent sessions go, and a click on its path switches there.
func TestSplashProjects(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	root := t.TempDir()
	g, p, err := store.OpenForDirs(host.Dirs.StateHome, root)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	p.Close()
	app.frameGen++
	app.renderSync(t)
	// a long path keeps its tail
	x, y := findCell(t, app, root[len(root)-20:])
	if seg := dropCells(app.lastRows[y], x)[0]; seg.Click != "projects switch "+root || seg.Widget != "splash" {
		t.Fatalf("the path's segment: %+v", seg)
	}
}

// TestSplashWidePath: a project path of wide characters keeps its tail: the
// cut counts cells, not runes, so the name at the path's end is what is left.
func TestSplashWidePath(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	const name = "プロジェクトのディレクトリ"
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	g, p, err := store.OpenForDirs(host.Dirs.StateHome, root)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	p.Close()
	app.frameGen++
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, name) {
		t.Fatalf("the path's name is cut away:\n%s", frame)
	}
}
