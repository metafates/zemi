package ui

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
)

// The terminal's own background is the theme's canvas through
// tea.View.BackgroundColor (so bubbletea resets it on every exit path),
// only on a terminal that answered OSC 11 and shows the color, and the host
// chrome sits on the same canvas, whatever the theme sets it to.
func TestTerminalBackground(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	canvas := hostapi.Whorl.Bg
	hint := hostapi.Segment{Text: " ", HL: "Host:Hint"}
	for _, c := range []struct {
		name    string
		answer  bool
		profile colorprofile.Profile
		wantBG  bool // the terminal is painted with the canvas
	}{
		{"no answer", false, colorprofile.TrueColor, false},
		{"truecolor", true, colorprofile.TrueColor, true},
		{"256 colors", true, colorprofile.ANSI256, true},
		{"no color", true, colorprofile.Ascii, false},
	} {
		app.termBG = nil
		if c.answer {
			app.Update(tea.BackgroundColorMsg{Color: rgb(0x141414)})
		}
		app.Update(tea.ColorProfileMsg{Profile: c.profile})
		got := app.View().BackgroundColor
		if c.wantBG != sameColor(got, canvas) || (!c.wantBG && got != nil) {
			t.Fatalf("%s: terminal bg = %v, painted %v", c.name, got, c.wantBG)
		}
		themeBG := app.thm.style("Normal").Bg
		if chromeBG := app.hlStyle("Host:Hint").Bg; !sameColor(chromeBG, themeBG) {
			t.Fatalf("%s: chrome bg %v, theme canvas %v", c.name, chromeBG, themeBG)
		}
		if app.painted(hint) {
			t.Fatalf("%s: a chrome spacer counts as painted", c.name)
		}
	}
}

// TestSurfacesAt16Colors: at 16 colors whorl's canvas, panel and band are
// greys by lightness (the stock table paints a dark tint like dracula's blue),
// and a fg that would land on its own color steps to the grey past it: a
// near-black group on the black canvas, dim on the band. A diff's number
// keeps its red on the grey row. At 256 each keeps a color of its own.
func TestSurfacesAt16Colors(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	for _, c := range []struct {
		profile colorprofile.Profile
		hl      string
		fill    hostapi.Fill
		fg, bg  color.Color
	}{
		{colorprofile.ANSI, "Rule", hostapi.FillAuto, ansi.BrightBlack, ansi.Black},
		{colorprofile.ANSI, "Host:Dim", hostapi.FillAuto, ansi.BrightBlack, ansi.Black},
		{colorprofile.ANSI, "Dim", hostapi.FillBand, ansi.White, ansi.BrightBlack},
		{colorprofile.ANSI, "Host:DiffDelNum", hostapi.FillAuto, ansi.BrightRed, ansi.BrightBlack},
		{colorprofile.ANSI256, "Rule", hostapi.FillAuto, ansi.IndexedColor(239), ansi.IndexedColor(234)},
		{colorprofile.ANSI256, "Host:Value", hostapi.FillPanel, ansi.IndexedColor(254), ansi.IndexedColor(235)},
	} {
		app.Update(tea.ColorProfileMsg{Profile: c.profile})
		if st := app.segStyle(c.hl, "", c.fill, 0, false, nil); st.Fg != c.fg || st.Bg != c.bg {
			t.Errorf("%v %s: %v on %v, want %v on %v", c.profile, c.hl, st.Fg, st.Bg, c.fg, c.bg)
		}
	}
}
