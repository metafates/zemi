package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// A light terminal background switches both the fixed chrome and the theme
// to their light variant, and a dark one switches back.
func TestLightBackground(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	darkTitle, darkFg := app.hlStyle("Host:Title"), app.thm.style("Normal").Fg

	app.Update(tea.BackgroundColorMsg{Color: rgb(0xfafafa)})
	if !app.thm.light {
		t.Fatal("a light background did not switch the chrome")
	}
	if app.hlStyle("Host:Title") == darkTitle {
		t.Fatal("the fixed chrome kept its dark accent")
	}
	if fg := app.thm.style("Normal").Fg; fg == darkFg {
		t.Fatalf("the theme kept its dark foreground: %v", fg)
	}

	app.Update(tea.BackgroundColorMsg{Color: rgb(0x141414)})
	if app.thm.light {
		t.Fatal("a dark background did not switch back")
	}
}
