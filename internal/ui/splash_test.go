package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
    ctx.recent = { { id = "a", ago = "1m ago", title = "a" }, { id = "b", ago = "2m ago", title = "b" }, { id = "c", ago = "3m ago", title = "c" } }
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
	hint := "export ANTHROPIC_API_KEY and start zemi again, or add a provider to ~/.config/zemi/init.lua"
	cases := []struct {
		name, ctx, want, fill string
		moved                 bool // the logo moved: the listed rows changed
	}{
		{"init.lua failed", `init_error = "/cfg/init.lua:12: boom", model = ""`, "● Tip  init.lua:12 failed: /errors", "", false},
		{"no model", `model = ""`, "● Tip  No model yet: export ANTHROPIC_API_KEY and start zemi again, or\nadd a provider to ~/.config/zemi/init.lua", "", false},
		{"untrusted", `trusted = false`, "● Tip  /trust lets zemi run this project's code and read its instructions", "", false},
		{"at home", `trusted = false, trustable = false`, "● Tip  /resume picks up the latest session", "", false},
		{"no instruction file", `instructions = {}`, "● Tip  /init writes an AGENTS.md, so zemi knows this project", "/init", false},
		{"other projects", `projects = { { path = "~/p", root = "/p", ago = "now" } }`, "● Tip  /projects switches to one, or type to stay here", "", false},
		{"a first session", `recent = {}`, `● Tip  !git status runs in your shell; the output rides your next prompt`, "!git status", true},
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
    ctx.instructions, ctx.recent, ctx.projects = { "AGENTS.md" }, { { id = "a", ago = "1m ago", title = "a" } }, nil
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
				app.composer.text = ""
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

// TestFarewellWraps: an exit widget that asks to wrap prints wrapped at the
// terminal's width, as the run view draws the region, not one over-long row.
func TestFarewellWraps(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 40, 20)
	runLua(t, app, `zemi.ui.widget.add({ id = "exit.wide", region = "exit", wrap = true, render = function()
  return { { { text = string.rep("word ", 20) } } }
end })`)
	rows := strings.Split(strings.TrimRight(app.Farewell(), "\n"), "\n")
	if len(rows) < 2 {
		t.Fatalf("a wrapping exit widget printed %d row(s):\n%s", len(rows), strings.Join(rows, "\n"))
	}
	for _, r := range rows {
		if w := ansi.StringWidth(r); w > 40 {
			t.Fatalf("a row is %d cells wide, want 40 at most: %q", w, r)
		}
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
		mark      bool // a named session's title leads with its marker emoji
		want, not []string
	}{
		{"no messages", 80, colorprofile.TrueColor, "t", 0, false, false, false, nil, []string{"zemi"}},
		// a quit during the trust prompt: that boot step never returns
		{"booting", 80, colorprofile.TrueColor, "t", 0, false, true, false, nil, []string{"zemi"}},
		{"plain", 80, colorprofile.NoTTY, "Fix the flaky test", 0.42, true, false, true, []string{"+#%#@%@%%#%#:", "--==-\n\n", "cost     $0.42"}, []string{"\x1b"}},
		{"color", 80, colorprofile.TrueColor, "t", 0, true, false, true, []string{"\x1b[38;"}, []string{"[48;", ";48;2;"}},
		{"narrow", 30, colorprofile.NoTTY, "t", 0, true, false, true, []string{"+#%#@%@%%#%#:", "--==-\n\n"}, []string{"┗━━ ┗━━ ╹╹╹ ╹"}},
		{"untitled", 80, colorprofile.NoTTY, "zemi", 0, true, false, false, []string{"fix the build"}, []string{"more", "cost"}},
		{"control chars", 80, colorprofile.TrueColor, "a\x1b]52;c;x\x07b", 0, true, false, false, []string{"ab"}, []string{"\x1b]52"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, c.width, 30)
			app.colors = c.profile
			db := testStore(t, host)
			if c.send {
				sid := store.ShortID()
				if err := db.CreateSession(sid, "", c.title, ""); err != nil {
					t.Fatal(err)
				}
				if err := db.AddUsage(sid, 0, 0, 0, 0, c.cost); err != nil {
					t.Fatal(err)
				}
				if err := host.ResumeSession(sid); err != nil {
					t.Fatal(err)
				}
				host.AppendMessage("user", "  fix the build\nmore")
				// whole even where the terminal wraps it
				c.want = append(c.want, hostapi.ResumeCommand("", sid))
				if c.mark {
					c.want = append(c.want, hostapi.SessionMark(sid)+" "+c.title)
				}
			}
			if c.booting {
				block, reached := make(chan struct{}), make(chan struct{})
				t.Cleanup(func() { close(block) })
				go func() {
					// Background: the cleanup releases this goroutine, and
					// t.Context is cancelled before cleanups run
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
