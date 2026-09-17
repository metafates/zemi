package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/win"
)

// sidebarConfig is a user init.lua that rebuilds the screen: the statusline
// on top, a scrolled sidebar left of the chat, the composer at the bottom
// and a clock floating in the stage's top-right corner.
const sidebarConfig = `
zemi.ui.widget.register({ id = "side", region = "side",
  on_click = function(ev) zemi.state.put("clicked", ev.id) end,
  render = function()
    local rows = {}
    for i = 1, 40 do rows[i] = { { text = "item " .. i, click = "item" .. i } } end
    return rows
  end })
zemi.ui.widget.register({ id = "clock", region = "clock", render = function(ctx)
  return { { { text = " " .. ctx.time .. " ", hl = "Title" } } }
end })
zemi.ui.layout.set({
  root = { kind = "box", dir = "col", children = {
    { kind = "bar", id = "status", left = "statusline.left", right = "statusline.right" },
    { kind = "box", dir = "row", sep = true, children = {
      { kind = "region", id = "side", region = "side", size = 20, min = 16, collapse = true, scroll = "top" },
      { kind = "box", dir = "col", stage = true, min = 20, children = {
        { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
        { kind = "windows", slot = "split" },
      } },
      { kind = "windows", slot = "vsplit" },
    } },
    { kind = "composer" },
  } },
  floats = { { id = "clock", region = "clock", relative = "stage", anchor = "NE" } },
})
zemi.keymap.set("ctrl+j", "zemi.ui.scroll.down side")
zemi.command.add("hello", function() end, { desc = "says hello" })
`

// rowOf is the screen row holding text, -1 when none does.
func rowOf(frame, text string) (y, x int) {
	for y, line := range strings.Split(ansi.Strip(frame), "\r\n") {
		if i := strings.Index(line, text); i >= 0 {
			return y, ansi.StringWidth(line[:i])
		}
	}
	return -1, -1
}

// TestRelayout: a user layout moves every piece and the UI keeps working
// in it: widget clicks and hover, scrolling a region by id and by wheel,
// popups, prompts and windows land over the stage, and a narrow screen
// collapses the sidebar.
func TestRelayout(t *testing.T) {
	app, host := bootAgentApp(t, 100, 24)
	defer host.Wins.CloseAll()
	host.AppendMessage("user", "hello from the middle")
	runLua(t, app, sidebarConfig)
	for range 2 {
		app.renderSync(t)
		app.Frame()
	}
	golden.RequireEqual(t, app.Frame())

	rects := app.LayoutRects()
	if s, c := rects["status"], rects["composer"]; s.Y != 0 || c.Y+c.Height != 24 || rects["side"].Width != 20 || rects["stage"].X != 21 {
		t.Fatalf("rects %+v", rects)
	}
	if clock := rects["clock"]; clock.X+clock.Width != 100 || clock.Y != rects["stage"].Y {
		t.Fatalf("clock float at %+v, stage %+v", clock, rects["stage"])
	}

	y, x := rowOf(app.Frame(), "item 3")
	app.hover(x+1, y)
	if spans, _, ok := app.target(x+1, y); !ok || len(spans) != 1 {
		t.Fatalf("sidebar item is not a target: %v", spans)
	}
	app.Update(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	app.Update(tea.MouseReleaseMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	runLua(t, app, `assert(zemi.state.get("clicked") == "item3", tostring(zemi.state.get("clicked")))`)

	press(t, app, tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "item 40") || strings.Contains(frame, "item 3 ") || !app.scrolls["chat"].follow {
		t.Fatalf("ctrl+j should scroll the sidebar alone:\n%s", ansi.Strip(frame))
	}
	app.Update(tea.MouseWheelMsg{X: 2, Y: 5, Button: tea.MouseWheelUp})
	if side := app.scrolls["side"]; side.top != side.rows-side.viewH-chatWheelStep || !app.scrolls["chat"].follow {
		t.Fatalf("the wheel over the sidebar should scroll it: side top %d", app.scrolls["side"].top)
	}

	app.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	app.Frame()
	if stage := app.stage; app.popRect.Empty() || app.popRect.Min.X != stage.Min.X || app.popRect.Max.Y != stage.Max.Y {
		t.Fatalf("palette at %v, want the bottom of the stage %v", app.popRect, stage)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	runLua(t, app, `zemi.ui.select({ "alpha", "beta" }, { prompt = "Pick" }, function() end)`)
	if y, x := rowOf(app.Frame(), "alpha"); y < 1 || x < app.stage.Min.X {
		t.Fatalf("the select box is at %d,%d, outside the stage %v", x, y, app.stage)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	for _, kind := range []string{win.KindSplit, win.KindVSplit} {
		if _, err := host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	app.Frame()
	split, vsplit := app.winRects[0], app.winRects[1]
	if split.x != app.stage.Min.X || split.y+split.h > app.stage.Max.Y || vsplit.x < app.stage.Max.X || vsplit.titleY != 1 {
		t.Fatalf("split %+v, vsplit %+v, stage %v", split, vsplit, app.stage)
	}

	host.Wins.CloseAll()
	app.Update(tea.WindowSizeMsg{Width: 30, Height: 24})
	app.Frame()
	if _, ok := app.LayoutRects()["side"]; ok {
		t.Fatal("the sidebar should collapse on a narrow screen")
	}
}

// TestRelayoutComposerOnTop: with the composer on the first row and no
// statusline, completion opens below the composer, a float over the
// composer leaves it visible, and the alarm pills still show.
func TestRelayoutComposerOnTop(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	runLua(t, app, `
zemi.command.add("hello", function() end, { desc = "says hello" })
zemi.ui.widget.register({ id = "cover", region = "cover", render = function()
  return { { { text = string.rep("#", 40) } }, { { text = string.rep("#", 40) } } }
end })
zemi.ui.layout.set({
  root = { kind = "box", dir = "col", children = {
    { kind = "composer" },
    { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill", stage = true },
  } },
  floats = { { region = "cover", relative = "screen" } },
})`)
	app.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	app.Frame()
	if c := app.LayoutRects()["composer"]; app.popRect.Empty() || app.popRect.Min.Y != c.Y+c.Height {
		t.Fatalf("palette at %v, want right below the composer %+v", app.popRect, c)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	app.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})

	for _, k := range "hello" {
		app.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
	}
	host.RecordError("test", "boom")
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	lines := strings.Split(frame, "\r\n")
	if !strings.Contains(lines[0], "hello") || !strings.Contains(lines[1], "####") {
		t.Fatalf("the float should cover row 1 and leave the composer:\n%s", frame)
	}
	if !strings.Contains(lines[len(lines)-1], "errors: /errors") {
		t.Fatalf("no statusline, yet the error pill is missing:\n%s", frame)
	}

}

// TestVsplitLeftDrag: a vsplit slot left of the chat is resized by the
// separator on its right.
func TestVsplitLeftDrag(t *testing.T) {
	app, host := bootAgentApp(t, 120, 30)
	defer host.Wins.CloseAll()
	runLua(t, app, `zemi.ui.layout.set({ root = { kind = "box", dir = "row", sep = true, children = {
  { kind = "windows", slot = "vsplit" },
  { kind = "box", dir = "col", stage = true, children = {
    { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
    { kind = "composer" },
  } },
} } })`)
	if _, err := host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindVSplit}); err != nil {
		t.Fatal(err)
	}
	app.Frame()
	r := app.winRects[0]
	if r.x != 0 || r.sepX != r.w {
		t.Fatalf("vsplit %+v should sit left with its separator on the right", r)
	}
	app.Update(tea.MouseClickMsg{X: r.sepX, Y: 10, Button: tea.MouseLeft})
	app.Update(tea.MouseMotionMsg{X: r.sepX + 10, Y: 10, Button: tea.MouseLeft})
	app.Update(tea.MouseReleaseMsg{X: r.sepX + 10, Y: 10, Button: tea.MouseLeft})
	app.Frame()
	if got := app.winRects[0]; got.sepX != r.sepX+10 {
		t.Fatalf("separator at %d after dragging it from %d by 10", got.sepX, r.sepX)
	}
}
