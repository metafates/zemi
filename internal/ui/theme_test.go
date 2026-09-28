package ui

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// TestANSI16: a 16-color terminal keeps a warning yellow and a hint dimmer
// than text, where the stock table draws amber red and #828282 as fg.
func TestANSI16(t *testing.T) {
	for _, c := range []struct{ in, want color.Color }{
		{rgb(0xf0d680), ansi.BrightYellow},
		{rgb(0xe97170), ansi.BrightRed},
		{rgb(0x82d2a8), ansi.BrightGreen},
		{rgb(0xa97500), ansi.Yellow},
		{rgb(0x828282), ansi.BrightBlack},
		{rgb(0xd8d8d8), ansi.White},
		{ansi.Red, ansi.Red},
		{nil, nil},
	} {
		if got := ansi16(c.in); got != c.want {
			t.Errorf("ansi16(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A light terminal background switches both the fixed chrome and the theme
// to their light variant, a dark one switches back, and the chrome follows
// the light theme picked in a dark terminal.
func TestLightBackground(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	darkTitle, darkFg := app.hlStyle("Host:Title"), app.thm.style("Normal").Fg

	app.Update(tea.BackgroundColorMsg{Color: rgb(0xfafafa)})
	if !app.thm.light {
		t.Fatal("a light background did not switch the chrome")
	}
	lightTitle := app.hlStyle("Host:Title")
	if lightTitle == darkTitle {
		t.Fatal("the fixed chrome kept its dark accent")
	}
	if fg := app.thm.style("Normal").Fg; fg == darkFg {
		t.Fatalf("the theme kept its dark foreground: %v", fg)
	}

	app.Update(tea.BackgroundColorMsg{Color: rgb(0x141414)})
	if app.thm.light {
		t.Fatal("a dark background did not switch back")
	}
	// the light theme picked in a dark terminal paints a light canvas
	if err := host.SetTheme("whorl-light"); err != nil {
		t.Fatal(err)
	}
	if app.hlStyle("Host:Title") != lightTitle {
		t.Fatal("the fixed chrome kept its dark accent on the light theme's canvas")
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

// TestChromeIsWhorl: the host chrome's copy of the palette draws each
// Host: style in the colors of the whorl group it stands for, dark and light.
func TestChromeIsWhorl(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	pairs := [][2]string{
		{"Host:Value", "Normal"},
		{"Host:Hint", "Dim"},
		{"Host:HintKey", "Accent"},
		{"Host:Rule", "Rule"},
		{"Host:Title", "Title"},
		{"Host:Ok", "Ok"},
		{"Host:Warn", "Warn"},
		{"Host:Danger", "Danger"},
		{"Host:DiffAdd", "DiffAdd"},
		{"Host:DiffAddNum", "DiffAddNum"},
		{"Host:DiffDel", "DiffDel"},
		{"Host:DiffDelNum", "DiffDelNum"},
	}
	for _, bg := range []uint32{0x141414, 0xfafafa} {
		app.Update(tea.BackgroundColorMsg{Color: rgb(bg)})
		for _, p := range pairs {
			host, group := app.hlStyle(p[0]), app.hlStyle(p[1])
			if !sameColor(host.Fg, group.Fg) || !sameColor(host.Bg, group.Bg) {
				t.Errorf("bg %06x: %s is %v on %v, %s %v on %v", bg, p[0], host.Fg, host.Bg, p[1], group.Fg, group.Bg)
			}
		}
	}
}
