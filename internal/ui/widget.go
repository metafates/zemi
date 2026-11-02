package ui

import (
	"cmp"
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
	region   region
	priority float64
	wrap     bool
	render   func(*hostapi.ScreenState) ([][]hostapi.Segment, bool)
	onClick  func(a *App, ev map[string]any) bool
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

// renderWidget draws one widget from the frame's state. A panicking render is
// an error, so one broken widget does not take the screen down.
func (a *App) renderWidget(w shownWidget, st *hostapi.ScreenState) (lines [][]hostapi.Segment, more bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v\n%s", r, debug.Stack())
		}
	}()
	lines, more = w.Go.render(st)
	// a click on a row acts as the widget that drew it
	for _, row := range lines {
		for i := range row {
			if row[i].Click != "" {
				row[i].Widget = w.ID
			}
		}
	}
	return lines, more, nil
}

// regionWidget is one widget of a region.
type regionWidget struct {
	ID       string
	Region   region
	Wrap     bool
	Priority float64
	Go       *widget
}

// regionWidgets lists a region's widgets, lowest priority first: a region
// stacks them in that order.
func (a *App) regionWidgets(r region) []regionWidget {
	var out []regionWidget
	for _, w := range a.widgets {
		if w.region == r {
			out = append(out, regionWidget{ID: w.id, Region: w.region, Wrap: w.wrap, Priority: w.priority, Go: w})
		}
	}
	slices.SortStableFunc(out, func(x, y regionWidget) int { return cmp.Compare(x.Priority, y.Priority) })
	return out
}

// shippedWidgets is the screen zemi draws: the splash, the statusline, the
// composer's rule and the chat, one per region.
func shippedWidgets(host *hostapi.Host) []*widget {
	return append([]*widget{chatWidget(host), splashWidget(), workingWidget()}, statuslineWidgets()...)
}
