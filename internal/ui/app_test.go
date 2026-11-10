package ui

import (
	"slices"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
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

// screen renders every widget synchronously and returns the frame as plain
// text: what a reader sees, styling dropped.
func (a *App) screen(t *testing.T) string {
	t.Helper()
	for a.Pump() {
	}
	a.renderSync(t)
	return ansi.Strip(a.Frame())
}
