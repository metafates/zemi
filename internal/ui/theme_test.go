package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
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

// TestDiffTintsApart: an added and a removed row keep different tints on a
// 256-color terminal, in the theme and in the fixed chrome, dark and light.
func TestDiffTintsApart(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	for _, bg := range []uint32{0x141414, 0xfafafa} {
		app.Update(tea.BackgroundColorMsg{Color: rgb(bg)})
		for _, pair := range [][2]string{{"DiffAdd", "DiffDel"}, {"Host:DiffAdd", "Host:DiffDel"}} {
			add, del := app.hlStyle(pair[0]).Bg, app.hlStyle(pair[1]).Bg
			if add == nil || colorprofile.ANSI256.Convert(add) == colorprofile.ANSI256.Convert(del) {
				t.Errorf("bg %06x: %s and %s are one cell at 256 colors", bg, pair[0], pair[1])
			}
		}
	}
}
