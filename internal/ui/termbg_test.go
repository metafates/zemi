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
	app, _ := bootAgentApp(t, 80, 24)
	if _, err := app.host.Themes.Add(&hostapi.Theme{Name: "bare", HL: map[string]hostapi.HLSpec{"Normal": {Fg: "#d8d8d8"}}}); err != nil {
		t.Fatal(err)
	}
	canvas := rgb(0x141414)
	hint := hostapi.Segment{Text: " ", HL: "Host:Hint"}
	for _, c := range []struct {
		name    string
		answer  bool
		profile colorprofile.Profile
		theme   string
		wantBG  bool // the terminal is painted with the canvas
	}{
		{"no answer", false, colorprofile.TrueColor, "whorl", false},
		{"truecolor", true, colorprofile.TrueColor, "whorl", true},
		{"256 colors", true, colorprofile.ANSI256, "whorl", true},
		{"no color", true, colorprofile.Ascii, "whorl", false},
		{"theme without bg", true, colorprofile.TrueColor, "bare", false},
	} {
		app.termBG = nil
		if c.answer {
			app.Update(tea.BackgroundColorMsg{Color: canvas})
		}
		app.Update(tea.ColorProfileMsg{Profile: c.profile})
		if _, err := app.host.Settings.SetQuiet("ui.theme", c.theme); err != nil {
			t.Fatal(err)
		}
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

// TestRuleAt16Colors: the rule grey and the dark canvas both land on black
// at 16 colors, so a rule, the host frame and the dim behind a popup take
// bright black there, and keep their own grey at 256.
func TestRuleAt16Colors(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	for _, c := range []struct {
		profile colorprofile.Profile
		bright  bool
	}{{colorprofile.ANSI, true}, {colorprofile.ANSI256, false}} {
		app.Update(tea.ColorProfileMsg{Profile: c.profile})
		for _, hl := range []string{"Rule", "Host:Rule", "Host:Dim"} {
			if got := app.hlStyle(hl).Fg == ansi.BrightBlack; got != c.bright {
				t.Errorf("%v %s: bright black %v, want %v", c.profile, hl, got, c.bright)
			}
		}
	}
}
