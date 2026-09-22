package ui

import (
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

// The host chrome sits on the theme's canvas, whatever the theme sets it
// to, so a chrome spacer is never painted content.
func TestChromeCanvas(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	if _, err := app.host.Themes.Add(&hostapi.Theme{Name: "bare", HL: map[string]hostapi.HLSpec{"Normal": {Fg: "#d8d8d8"}}}); err != nil {
		t.Fatal(err)
	}
	hint := hostapi.Segment{Text: " ", HL: "Host:Hint"}
	for _, theme := range []string{"default", "bare"} {
		if err := app.host.Settings.SetHost("ui.theme", theme); err != nil {
			t.Fatal(err)
		}
		themeBG := app.thm.style("Normal").Bg
		if chromeBG := app.hlStyle("Host:Hint").Bg; !sameColor(chromeBG, themeBG) {
			t.Fatalf("%s: chrome bg %v, theme canvas %v", theme, chromeBG, themeBG)
		}
		if app.painted(hint) {
			t.Fatalf("%s: a chrome spacer counts as painted", theme)
		}
	}
}
