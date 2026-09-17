package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// todoConfig is a todo panel written in Lua and driven by keys alone: a
// focusable region with keymaps scoped to it.
const todoConfig = `
local items = { { text = "write tests" }, { text = "fix focus" }, { text = "ship" } }
local sel = 1
zemi.ui.widget.register({ id = "todo", region = "todo", render = function(ctx)
  local rows = { { { text = ctx.focus == "todo" and "TODO*" or "TODO", hl = "Title" } } }
  for i, it in ipairs(items) do
    local mark = (i == sel and ctx.focus == "todo") and "> " or "  "
    rows[#rows + 1] = { { text = mark .. (it.done and "[x] " or "[ ] ") .. it.text } }
  end
  return rows
end })
zemi.ui.layout.set({ root = { kind = "box", dir = "row", sep = true, children = {
  { kind = "region", id = "todo", region = "todo", size = 24, focusable = true },
  { kind = "box", dir = "col", stage = true, children = {
    { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
    { kind = "composer" },
  } },
} } })
local function move(d) return function() sel = math.max(1, math.min(#items, sel + d)) end end
zemi.keymap.set("j", move(1), { focus = "todo" })
zemi.keymap.set("k", move(-1), { focus = "todo" })
zemi.keymap.set("space", function() items[sel].done = not items[sel].done end, { focus = "todo" })
zemi.keymap.set("g g", function() sel = 1 end, { focus = "todo" })
zemi.keymap.set("ctrl+t", "zemi.ui.focus.set todo")
`

func (a *App) screen(t *testing.T) string {
	t.Helper()
	for a.Pump() {
	}
	a.renderSync(t)
	a.Frame()
	return ansi.Strip(a.Frame())
}

// TestFocusTodoPanel: a key focuses the panel, its scoped keys move and
// toggle rows while typed letters stay out of the composer, esc gives the
// keys back, ctrl+c and prompts keep priority, and clicks move the focus.
func TestFocusTodoPanel(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	runLua(t, app, todoConfig)
	app.screen(t)

	press(t, app, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if host.Focus() != "todo" || !strings.Contains(app.screen(t), "TODO*") {
		t.Fatalf("ctrl+t did not focus the panel: %q\n%s", host.Focus(), app.screen(t))
	}
	pressKeys(app, "jj k", "space", "jgg")
	frame := app.screen(t)
	if !strings.Contains(frame, "> [ ] write tests") || !strings.Contains(frame, "  [x] fix focus") {
		t.Fatalf("keys did not drive the panel:\n%s", frame)
	}
	if app.composer.text != "" {
		t.Fatalf("keys leaked into the composer: %q", app.composer.text)
	}
	runLua(t, app, `assert(zemi.ui.focus.get() == "todo")
		assert(zemi.keymap.get("j", { focus = "todo" }).focus == "todo")
		assert(zemi.keymap.get("j") == nil)`)

	app.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !app.now().Before(app.quitArmed) {
		t.Fatal("ctrl+c must stay Go-reserved while a node has the keys")
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	pressKeys(app, "hi")
	if host.Focus() != "" || app.composer.text != "hi" || strings.Contains(app.screen(t), "TODO*") {
		t.Fatalf("esc should give the keys back: focus %q, composer %q", host.Focus(), app.composer.text)
	}

	// a click on the panel focuses it, one on the composer takes the keys back
	y, x := rowOf(app.screen(t), "write tests")
	app.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	app.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if host.Focus() != "todo" {
		t.Fatalf("a click on the panel did not focus it: %q", host.Focus())
	}
	runLua(t, app, `zemi.ui.select({ "a", "b" }, {}, function() end)`)
	pressKeys(app, "j")
	if app.overlays[0].selected != 1 {
		t.Fatal("an overlay must take keys before the focused node")
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	c := app.LayoutRects()["composer"]
	app.Update(tea.MouseClickMsg{X: c.X + 3, Y: c.Y, Button: tea.MouseLeft})
	app.Update(tea.MouseReleaseMsg{X: c.X + 3, Y: c.Y, Button: tea.MouseLeft})
	if host.Focus() != "" {
		t.Fatalf("a click on the composer should focus it: %q", host.Focus())
	}

	runLua(t, app, `zemi.ui.focus.next()`)
	if host.Focus() != "todo" {
		t.Fatalf("focus.next from the composer: %q", host.Focus())
	}
	runLua(t, app, `zemi.ui.focus.next()
		local ok, err = pcall(zemi.ui.focus.set, "chat")
		assert(not ok and tostring(err):find("no focusable node"), tostring(err))`)
	if host.Focus() != "" {
		t.Fatalf("focus.next should wrap to the composer: %q", host.Focus())
	}

	runLua(t, app, `zemi.ui.focus.set("todo")`)
	runLua(t, app, `zemi.ui.layout.set({ root = { kind = "box", dir = "col", children = { { kind = "composer" } } } })`)
	if host.Focus() != "" {
		t.Fatalf("a layout without the node should drop its focus: %q", host.Focus())
	}
}
