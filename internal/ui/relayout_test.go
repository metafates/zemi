package ui

import (
	"log/slog"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/hostapi"
)

// sidebarConfig is a user init.lua that rebuilds the screen: the statusline
// on top, a scrolled sidebar left of the chat, the composer at the bottom
// and a clock floating in the stage's top-right corner.
const sidebarConfig = `
zemi.ui.widget.add({ id = "side", region = "side",
  on_click = function(ev) zemi.state.set("clicked", ev.id) end,
  render = function()
    local rows = {}
    for i = 1, 40 do rows[i] = { { text = "item " .. i, click = "item" .. i } } end
    return rows
  end })
zemi.ui.widget.add({ id = "clock", region = "clock", render = function(ctx)
  return { { { text = " " .. ctx.time .. " ", hl = "Title" } } }
end })
zemi.ui.layout.set({
  root = { kind = "box", dir = "col", children = {
    { kind = "bar", id = "status", left = "statusline.left", right = "statusline.right" },
    { kind = "box", dir = "row", sep = true, children = {
      { kind = "region", id = "side", region = "side", size = 20, min = 16, collapse = true, scroll = "top" },
      { kind = "box", dir = "col", stage = true, min = 20, children = {
        { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
      } },
    } },
    { kind = "composer" },
  } },
  floats = { { id = "clock", region = "clock", relative = "stage", anchor = "NE" } },
})
zemi.keymap.set("ctrl+j", "zemi.ui.scroll.down side")
zemi.command.add("hello", function() end, { desc = "says hello" })
`

// TestUnframedComposer: beside a separator an unframed composer keeps its
// cells, its placeholder cut to them, and a popup over it keeps its bottom
// border, since there is no frame to stand on.
func TestUnframedComposer(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	runLua(t, app, `zemi.ui.layout.set({ root = { kind = "box", dir = "row", sep = true, children = {
  { kind = "box", dir = "col", stage = true, size = 40, children = {
    { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
    { kind = "composer", placeholder = "Ask anything at all,\nzemi answers what it can" },
  } },
  { kind = "region", id = "side", region = "side" },
} } })`)
	app.renderSync(t)
	app.Frame()
	c := app.LayoutRects()["composer"]
	for _, key := range []string{"", "/"} {
		if key != "" {
			app.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
		}
		app.renderSync(t)
		rows := strings.Split(ansi.Strip(app.Frame()), "\r\n")
		if len(rows) != 20 || ansi.Cut(rows[c.Y], c.X+c.Width, c.X+c.Width+1) != "│" {
			t.Fatalf("after %q the separator left column %d:\n%s", key, c.X+c.Width, strings.Join(rows, "\n"))
		}
		if key == "/" && (app.popRect.Max.Y > c.Y || !strings.HasPrefix(rows[app.popRect.Max.Y-1], "└")) {
			t.Fatalf("the palette at %v lost its bottom border over the composer at %+v:\n%s", app.popRect, c, strings.Join(rows, "\n"))
		}
	}
}

// TestRelayout: a user layout moves every piece and the UI keeps working
// in it: widget clicks and hover, scrolling a region by id and by wheel,
// prompts and windows land over the stage, popups too, in line with the
// composer, and a narrow screen collapses the sidebar.
func TestRelayout(t *testing.T) {
	app, host := bootAgentApp(t, 100, 24)
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

	x, y := findCell(t, app, "item 3")
	app.hover(x+1, y)
	if spans, _, ok := app.target(x+1, y); !ok || len(spans) != 1 {
		t.Fatalf("sidebar item is not a target: %v", spans)
	}
	clickAt(app, x+1, y)
	runLua(t, app, `assert(zemi.state.get("clicked") == "item3", tostring(zemi.state.get("clicked")))`)

	press(t, app, tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "item 40") || strings.Contains(frame, "item 3 ") || !app.scrolls["chat"].follow {
		t.Fatalf("ctrl+j should scroll the sidebar alone:\n%s", ansi.Strip(frame))
	}
	app.Update(tea.MouseWheelMsg{X: 2, Y: 5, Button: tea.MouseWheelUp})
	if side := app.scrolls["side"]; side.top != side.rows-side.viewH-1 || !app.scrolls["chat"].follow {
		t.Fatalf("the wheel over the sidebar should scroll it: side top %d", app.scrolls["side"].top)
	}

	app.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	app.Frame()
	if stage, c := app.stage, rects["composer"]; app.popRect.Empty() || app.popRect.Min.X != c.X || app.popRect.Max.Y != stage.Max.Y {
		t.Fatalf("palette at %v, want the bottom of the stage %v in line with the composer %+v", app.popRect, stage, c)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	runLua(t, app, `zemi.ui.select({ "alpha", "beta" }, { prompt = "Pick" }, function() end)`)
	if x, y := findCell(t, app, "alpha"); y < 1 || x < app.stage.Min.X {
		t.Fatalf("the select box is at %d,%d, outside the stage %v", x, y, app.stage)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	app.Update(tea.WindowSizeMsg{Width: 30, Height: 24})
	app.Frame()
	if _, ok := app.LayoutRects()["side"]; ok {
		t.Fatal("the sidebar should collapse on a narrow screen")
	}
}

// TestThinkingPane: the shipped layout centers its column, the stage inside
// it, and in pane mode draws the thinking pane beside the chat from 100
// columns, its separator meeting the composer's top rule; hidden or
// collapsed, the pane takes no separator, and ctx.regions says whether it draws.
func TestThinkingPane(t *testing.T) {
	app, _ := bootAgentApp(t, 140, 24)
	runLua(t, app, `
zemi.ui.widget.add({ id = "probe", region = "header", render = function(ctx)
  return { { { text = ctx.regions.thinking and "pane on" or "pane off" } } }
end })`)
	pane := `local l = zemi.ui.layout.get()
l.root.children[2].max = 181
l.root.children[2].children[2].children[2].max = 157
l.root.children[2].children[2].children[2].children[2].hidden = false
zemi.ui.layout.set(l)`
	for _, c := range []struct {
		name         string
		pane         bool
		width        int
		chatX, paneX int // the left edges of the chat and the drawn pane
		drawn        bool
	}{
		{"inline centers the column", false, 140, 20, 0, false},
		{"the pane draws beside the chat", true, 140, 0, 101, true},
		{"and grows to its max", true, 200, 22, 123, true},
		{"it collapses under 100 columns", true, 99, 0, 0, false},
		{"and draws at 100", true, 100, 0, 64, true},
	} {
		if c.pane {
			runLua(t, app, pane)
		}
		app.Update(tea.WindowSizeMsg{Width: c.width, Height: 24})
		app.renderSync(t)
		frame := ansi.Strip(app.Frame())
		rows := strings.Split(frame, "\r\n")
		rects := app.LayoutRects()
		th, drawn := rects["thinking"]
		if drawn != c.drawn || rects["chat"].X != c.chatX || drawn && th.X != c.paneX || strings.Contains(frame, "pane on") != c.drawn || strings.Contains(frame, "thinking shows here") != c.drawn {
			t.Fatalf("%s: rects %+v:\n%s", c.name, rects, frame)
		}
		top := rows[rects["composer"].Y]
		if strings.Contains(top, "┴") != c.drawn || drawn && ansi.Cut(top, th.X-1, th.X) != "┴" || drawn && ansi.Cut(rows[th.Y], th.X-1, th.X) != "│" {
			t.Fatalf("%s: the separator does not meet the composer rule at column %d:\n%s", c.name, th.X-1, frame)
		}
	}
}

// TestFloatsOverTheColumn: the shipped stage is the row of chat and pane,
// so a modal centers on the column, pane or not, and the toast tray hugs the
// column's right edge, three border cells over it and the rest in the
// margin, or stands flush right inside the screen where no margin fits it.
func TestFloatsOverTheColumn(t *testing.T) {
	for _, c := range []struct {
		name  string
		width int
		pane  bool
		modal bool
		want  func(stage hostapi.Rect, w int) int // the modal's center column, else the tray's corner
	}{
		{"a modal centers on the column with the pane", 140, true, true, func(hostapi.Rect, int) int { return 70 }},
		{"the tray stands inside a narrow screen", 100, false, false, func(_ hostapi.Rect, w int) int { return w - 49 }},
		{"the tray hugs the column", 220, false, false, func(s hostapi.Rect, _ int) int { return s.X + s.Width - 3 }},
		{"the tray hugs the column with the pane", 260, true, false, func(s hostapi.Rect, _ int) int { return s.X + s.Width - 3 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, host := bootAgentApp(t, c.width, 24)
			if c.pane {
				if err := host.ToggleReasoning(); err != nil {
					t.Fatal(err)
				}
				waitPrompt(t, app, func() bool { return !host.Layout().Root.Children[1].Children[1].Children[1].Children[1].Hidden })
			}
			if c.modal {
				runLua(t, app, `zemi.ui.select({ "alpha", "beta" }, { prompt = "Pick" }, function() end)`)
			} else {
				app.pushToast(slog.LevelInfo, "saved", "")
			}
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			rects := app.LayoutRects()
			stage, th := rects["stage"], rects["thinking"]
			if c.pane && (stage.X > rects["chat"].X || stage.X+stage.Width < th.X+th.Width) {
				t.Fatalf("the stage %+v does not span the chat %+v and the pane %+v", stage, rects["chat"], th)
			}
			got := app.chromeRect.Min.X + app.chromeRect.Dx()/2
			if !c.modal {
				row := strings.Split(frame, "\r\n")[stage.Y]
				got = ansi.StringWidth(row[:strings.LastIndex(row, "┌")])
			}
			if want := c.want(stage, c.width); got < want-1 || got > want+1 || !c.modal && got != want {
				t.Fatalf("at column %d, want %d (stage %+v):\n%s", got, want, stage, frame)
			}
		})
	}
}

// TestRelayoutComposerOnTop: with the composer on the first row and no
// statusline, completion opens below the composer, a float over the
// composer leaves it visible, and the alarm pills still show.
func TestRelayoutComposerOnTop(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	runLua(t, app, `
zemi.command.add("hello", function() end, { desc = "says hello" })
zemi.ui.widget.add({ id = "cover", region = "cover", render = function()
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
	if !strings.Contains(lines[len(lines)-1], "errors /errors") {
		t.Fatalf("no statusline, yet the error pill is missing:\n%s", frame)
	}
}
