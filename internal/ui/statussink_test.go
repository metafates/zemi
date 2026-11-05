package ui

import (
	"testing"
	"time"

	"github.com/metafates/zemi/internal/anim"
	"github.com/metafates/zemi/internal/hostapi"
)

// TestPillSinksDoNotWaitOnEachOther: two pills of one statusline side that
// leave together (a scroll pill hitting the bottom while a ctrl+c arm lifts)
// must ride their sinks through — each waiting on the other froze both, and
// neither pill ever appeared again.
func TestPillSinksDoNotWaitOnEachOther(t *testing.T) {
	_, host := statusApp(t)
	app := &App{
		host:    host,
		widgets: statuslineWidgets(),
		rises:   map[string]*anim.Curtain{},
		caches:  map[string]*widgetCache{},
		now:     func() time.Time { return time.UnixMilli(500) },
	}
	const scroll, quit = "statusline.scroll", "statusline.quit"
	line := func(id string, gen int64) widgetRender {
		return widgetRender{id: id, lines: [][]hostapi.Segment{{{Text: "pill " + id, HL: hostapi.GroupDim}}}, gen: gen, seq: gen, width: 80, height: 14}
	}
	pill := func(id string) bool {
		c := app.caches[id]
		return c != nil && len(c.lines) > 0
	}

	app.onWidgetRendered(line(scroll, 1))
	app.onWidgetRendered(line(quit, 1))
	app.now = func() time.Time { return time.UnixMilli(1500) }
	app.onWidgetRendered(line(scroll, 2))
	app.onWidgetRendered(line(quit, 2))
	if !pill(scroll) || !pill(quit) {
		t.Fatal("the pills did not settle shown")
	}

	// both leave in one window, then both come back while their sinks still
	// run: each must ride its own sink instead of waiting on the other
	app.onWidgetRendered(widgetRender{gen: 3, seq: 3, width: 80, height: 14})
	app.now = func() time.Time { return time.UnixMilli(1600) }
	app.onWidgetRendered(line(scroll, 3))
	app.onWidgetRendered(line(quit, 3))
	if !pill(scroll) || !pill(quit) {
		t.Fatalf("the returning pills stayed sunk: scroll %v, quit %v", pill(scroll), pill(quit))
	}
}
