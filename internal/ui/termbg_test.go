package ui

import (
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
	app, host := bootAgentApp(t, 80, 24)
	canvas := host.Themes.Get("whorl").Palette.Bg
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

// TestRuleAt16Colors: at 16 colors a rule, the host frame and the dim
// behind a popup take bright black, never the dark canvas's black, and so
// do a near-black group on that canvas and a diff's number, never red on its
// red row; at 256 each keeps its own.
func TestRuleAt16Colors(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	app.host.Themes.SetHL("NearBlack", hostapi.HLSpec{Fg: "#202020"})
	for _, c := range []struct {
		profile colorprofile.Profile
		bright  bool
	}{{colorprofile.ANSI, true}, {colorprofile.ANSI256, false}} {
		app.Update(tea.ColorProfileMsg{Profile: c.profile})
		for _, hl := range []string{"Rule", "Host:Rule", "Host:Dim", "NearBlack", "DiffDelNum", "Host:DiffDelNum"} {
			if got := app.segStyle(hl, "", hostapi.FillAuto).Fg == ansi.BrightBlack; got != c.bright {
				t.Errorf("%v %s: bright black %v, want %v", c.profile, hl, got, c.bright)
			}
		}
	}
}
