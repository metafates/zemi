package ui

import (
	"slices"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// TestWidgetRows: one widget's lines become the rows the layout draws, a
// wrapping widget wrapped and the rest cut, whichever renderer maps them.
func TestWidgetRows(t *testing.T) {
	line := []hostapi.Segment{{Text: "alpha\tbeta gamma delta"}}
	for _, c := range []struct {
		name string
		wrap bool
		w    int
		want []string
	}{
		{"a cut row keeps the width, tabs expanded", false, 12, []string{"alpha    bet"}},
		{"a wrapped row flows into the next", true, 12, []string{"alpha    ", "beta gamma ", "delta"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, r := range widgetRows([][]hostapi.Segment{line}, c.wrap, c.w) {
				got = append(got, hostapi.SegsText(r))
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("rows = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRelaunch: a relaunch the quit veto stopped leaves no dir for the next
// /quit to restart in, and one past it names its dir and the session to
// resume there.
func TestRelaunch(t *testing.T) {
	for _, c := range []struct {
		name, veto, want, wantResume string
	}{
		{"a vetoed relaunch", `return { decision = "deny", reason = "busy" }`, "", ""},
		{"a relaunch past the veto", `return nil`, "/tmp/other", "s1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, _ := bootAgentApp(t, 80, 20)
			runLua(t, app, `zemi.event.on("app.quit", function() `+c.veto+` end)`)
			app.Relaunch("/tmp/other", "s1")
			if dir, resume := app.RelaunchDir(); dir != c.want || resume != c.wantResume {
				t.Fatalf("RelaunchDir = %q, %q, want %q, %q", dir, resume, c.want, c.wantResume)
			}
		})
	}
}

// TestFadeSparesTheWidgetCache: the modal fade dims copies, never the rows a
// non-scrolled wrap region shares with the widget cache, so the frame after
// the box closes draws the widget as before.
func TestFadeSparesTheWidgetCache(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 20)
	runLua(t, app, `
zemi.ui.layout.set({ root = { kind = "box", dir = "col", stage = true, children = {
  { kind = "region", id = "chat", region = "chat", scroll = "bottom", size = "fill" },
  { kind = "region", id = "notes", region = "notes", size = 2 },
  { kind = "composer" },
} } })
zemi.ui.widget.add({ id = "notes.wide", region = "notes", wrap = true, render = function()
  return { { { text = string.rep("word ", 14) } } }
end })`)
	app.screen(t)
	before := app.Frame()
	app.openOverlay(hostapi.Overlay{
		ID: "pick", Title: "pick", Origin: lua.OriginBuiltin,
		Items: []hostapi.OverlayItem{{Text: "a row"}},
	})
	app.renderSync(t)
	app.Frame()
	c := app.caches["notes.wide"]
	if c == nil {
		t.Fatal("the wrap widget never rendered")
	}
	for _, row := range c.wrapped {
		for _, s := range row {
			if s.Fade != 0 {
				t.Fatalf("the modal's fade reached the widget cache: %+v", s)
			}
		}
	}
	app.closeOverlays("pick")
	if after := app.Frame(); after != before {
		t.Fatalf("the frame kept the fade past the box:\n%s\nwant:\n%s", after, before)
	}
}
