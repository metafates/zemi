package ui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// A terminal that answers an OSC 11 background query gets its own background
// painted with the theme's canvas color, and the draft keeps them in sync.
func TestTerminalBackgroundSync(t *testing.T) {
	if got := termBytes(rgb(0x141414)); got != "\x1b]11;#141414\x07" {
		t.Fatalf("termBytes = %q", got)
	}
	for _, c := range []struct {
		a, b color.Color
		want bool
	}{
		{rgb(0x141414), rgb(0x141414), true},
		{rgb(0x141414), rgb(0xfafafa), false},
		{color.Color(nil), color.Color(nil), true},
	} {
		if sameColor(c.a, c.b) != c.want {
			t.Fatalf("sameColor(%v, %v) = %v, want %v", c.a, c.b, !c.want, c.want)
		}
	}

	app, _ := bootAgentApp(t, 80, 24)
	// no terminal answer: nothing is painted on the frame
	frame := app.onFrame()
	if osc := collectOSC11(t, frame); len(osc) != 0 {
		t.Fatalf("painted on an unsupported terminal: %v", osc)
	}

	// an answer picks the dark theme and paints its canvas as the background
	app.Update(tea.BackgroundColorMsg{Color: rgb(0x141414)})
	if got := app.syncedTermBG(); !sameColor(got, rgb(0x141414)) {
		t.Fatalf("syncedTermBG = %v, want the theme bg", got)
	}
	osc := collectOSC11(t, app.onFrame())
	if len(osc) != 1 || !strings.Contains(osc[0], "\x1b]11;#141414\x07") {
		t.Fatalf("frame did not paint the theme bg: %v", osc)
	}
	if !sameColor(app.paintedTermBG, rgb(0x141414)) {
		t.Fatalf("paintedTermBG not recorded: %v", app.paintedTermBG)
	}
	// unchanged: a later frame sends nothing new
	if osc := collectOSC11(t, app.onFrame()); len(osc) != 0 {
		t.Fatalf("re-painted an unchanged background: %v", osc)
	}
}

// collectOSC11 runs a frame command and returns the OSC 11 background
// sequences it emits.
func collectOSC11(t *testing.T, cmd tea.Cmd) []string {
	t.Helper()
	var out []string
	add := func(msg tea.Msg) {
		if rm, ok := msg.(tea.RawMsg); ok {
			if s := fmt.Sprint(rm.Msg); strings.Contains(s, "\x1b]11;") {
				out = append(out, s)
			}
		}
	}
	if msg := cmd(); msg != nil {
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				if c != nil {
					add(c())
				}
			}
		} else {
			add(msg)
		}
	}
	return out
}
