package ui

import (
	"cmp"
	"context"
	"fmt"
	"runtime/debug"
	"slices"

	"github.com/metafates/zemi/internal/hostapi"
)

// widget is one piece of the screen: the rows of a region at a priority,
// drawn from a frame's state. more asks for another frame, which a widget too
// big for one render uses (the chat fills a long log over a few of them).
type widget struct {
	id       string
	region   string
	priority float64
	wrap     bool
	render   func(*hostapi.ScreenState) ([][]hostapi.Segment, bool)
	onClick  func(ev map[string]any)
}

// widgetByID is the shipped widget with that id, nil for none.
func (a *App) widgetByID(id string) *widget {
	for _, w := range a.widgets {
		if w.id == id {
			return w
		}
	}
	return nil
}

// renderWidget draws one widget: a Go one from the frame's state, a Lua one
// through the interpreter. A panicking render is an error like a raising Lua
// one, so one broken widget does not take the screen down.
func (a *App) renderWidget(w shownWidget, st *hostapi.ScreenState, state hostapi.UIState) (lines [][]hostapi.Segment, more bool, err error) {
	if w.Go == nil {
		return a.host.RenderWidget(context.Background(), w.Lua, w.width, w.height, state)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v\n%s", r, debug.Stack())
		}
	}()
	lines, more = w.Go.render(st)
	// a click on a row acts as that widget, as the Lua conversion stamps it
	for _, row := range lines {
		for i := range row {
			if row[i].Click != "" {
				row[i].Widget = w.ID
			}
		}
	}
	return lines, more, nil
}

// regionWidget is one widget of a region, drawn or not: the fields a region
// reads of either kind.
type regionWidget struct {
	ID, Region string
	Wrap       bool
	Priority   float64
	Lua        *hostapi.Widget // a Lua widget, nil for a Go one
	Go         *widget         // a Go widget, nil for a Lua one
}

// regionWidgets lists a region's widgets, Lua and Go together, lowest
// priority first: a region stacks them in that order.
func (a *App) regionWidgets(region string) []regionWidget {
	var out []regionWidget
	for _, wg := range a.host.Widgets.InRegion(region) {
		out = append(out, regionWidget{ID: wg.ID, Region: wg.Region, Wrap: wg.Wrap, Priority: wg.Priority, Lua: wg})
	}
	for _, w := range a.widgets {
		if w.region == region {
			out = append(out, regionWidget{ID: w.id, Region: w.region, Wrap: w.wrap, Priority: w.priority, Go: w})
		}
	}
	slices.SortStableFunc(out, func(x, y regionWidget) int {
		if c := cmp.Compare(x.Priority, y.Priority); c != 0 {
			return c
		}
		return 0
	})
	return out
}

// shippedWidgets is the screen zemi draws: the splash, the statusline, the
// composer's rule and the chat, one per region.
func shippedWidgets() []*widget {
	return append([]*widget{splashWidget()}, statuslineWidgets()...)
}
