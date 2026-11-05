package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestSplashIsTheWhorl: the splash is the knot centered with nothing else,
// falling back to the one "zemi" row where the art does not fit, and the
// region's last row stays free for the pending work.
func TestSplashIsTheWhorl(t *testing.T) {
	t.Parallel()
	st := &hostapi.ScreenState{Width: 80, Height: 24, Version: "v1", Model: "a/b", Trusted: true, Trustable: true}
	rows := rowTexts(splashRows(st))
	frame := strings.Join(rows, "\n")
	if !strings.Contains(frame, "+#%#@%@%%#%#:") || strings.Contains(frame, "zemi") || strings.Contains(frame, "v1") {
		t.Fatalf("the splash is not the knot alone:\n%s", frame)
	}
	if got := len(rows); got != 24 {
		t.Fatalf("the splash fills %d rows, the region has 24", got)
	}

	st.Height = 4
	if rows := rowTexts(splashRows(st)); !strings.Contains(strings.Join(rows, "\n"), "zemi") || len(rows) != 4 {
		t.Fatalf("a short region keeps the pending row and the fallback: %q", rows)
	}
}

// TestSplashRises: the knot rises out of the surface over _riseDuration
// behind the boot's delay on its first draw and stays settled after.
func TestSplashRises(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	app.renderShown(t, app.frameGen)

	mid := func() float64 {
		for _, row := range app.caches["splash"].lines {
			for _, s := range row {
				if strings.TrimSpace(s.Text) != "" {
					return s.Fade
				}
			}
		}
		return 0
	}
	if mid() <= 0 {
		t.Fatalf("the knot did not rise: fade %v at the stamp", mid())
	}
	if !app.fadesRising() || app.tickInterval() != _riseTick {
		t.Fatalf("the splash's rise must keep the tick awake")
	}

	app.now = func() time.Time { return time.UnixMilli(500 + (_barDelay + _riseDuration).Milliseconds()) }
	app.renderShown(t, app.frameGen)
	if fade := mid(); fade != 0 {
		t.Fatalf("the knot never settled: fade %v after the ramp", fade)
	}
	app.now = func() time.Time { return time.UnixMilli(2000) }
	if app.fadesRising() || app.tickInterval() != _idleInterval {
		t.Fatalf("the splash's rise must end")
	}
}

// rowTexts is each row's text, right-trimmed.
func rowTexts(rows [][]hostapi.Segment) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimRight(hostapi.SegsText(r), " ")
	}
	return out
}
