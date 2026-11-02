package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

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
// /quit to restart in, and one past it names its dir.
func TestRelaunch(t *testing.T) {
	for _, c := range []struct {
		name, veto, want string
	}{
		{"a vetoed relaunch", `return { decision = "deny", reason = "busy" }`, ""},
		{"a relaunch past the veto", `return nil`, "/tmp/other"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, _ := bootAgentApp(t, 80, 20)
			runLua(t, app, `zemi.event.on("app.quit", function() `+c.veto+` end)`)
			app.Relaunch("/tmp/other", "")
			if dir, _ := app.RelaunchDir(); dir != c.want {
				t.Fatalf("RelaunchDir = %q, want %q", dir, c.want)
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
	addWidget(app, &widget{id: "notes.wide", region: "chat", priority: 20, wrap: true, render: func(*hostapi.ScreenState) ([][]hostapi.Segment, bool) {
		return [][]hostapi.Segment{{{Text: strings.Repeat("word ", 14)}}}, false
	}})
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

// screen renders every widget synchronously and returns the frame as plain
// text: what a reader sees, styling dropped.
func (a *App) screen(t *testing.T) string {
	t.Helper()
	for a.Pump() {
	}
	a.renderSync(t)
	return ansi.Strip(a.Frame())
}
