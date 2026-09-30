package ui

import (
	"image/color"
	"math"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

func rgb(v uint32) ansi.RGBColor {
	return ansi.RGBColor{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)}
}

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

// TestThemes: every shipped theme keeps each text role over its contrast
// floor on bg, panel and band, the literal apart in hue from warn and
// danger, its surfaces, rule and dim apart and danger a hue at 256 colors,
// and the Go chrome draws in the colors of the groups it stands for.
func TestThemes(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	floors := map[string]float64{"fg": 7, "dim": 4.5, "accent": 4.5, "literal": 4.5, "ok": 4.5, "danger": 4.5, "warn": 3.5}
	chrome := [][2]string{
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
	themes := host.Themes.List()
	if len(themes) < 21 {
		t.Fatalf("%d themes", len(themes))
	}
	hue := func(c color.Color) float64 {
		v, _ := colorful.MakeColor(c)
		_, _, h := v.OkLch()
		return h
	}
	for _, th := range themes {
		if _, err := host.Settings.SetQuiet("ui.theme", th.Name); err != nil {
			t.Fatal(err)
		}
		if host.ActiveTheme() != th {
			t.Fatalf("%s: %s draws", th.Name, host.ActiveTheme().Name)
		}
		for role, floor := range floors {
			for _, on := range []string{"bg", "panel", "band"} {
				if r := hostapi.Contrast(th.Color(role), th.Color(on)); r < floor {
					t.Errorf("%s: %s is %.2f:1 on %s, under %.1f", th.Name, role, r, on, floor)
				}
			}
		}
		for _, other := range []string{"warn", "danger"} {
			d := math.Abs(hue(th.Palette.Literal) - hue(th.Color(other)))
			if d = min(d, 360-d); d < 15 {
				t.Errorf("%s: literal %s is %.0f degrees from %s", th.Name, hostapi.Hex(th.Palette.Literal), d, other)
			}
		}
		for _, p := range [][2]string{{"band", "panel"}, {"rule", "panel"}, {"fg", "dim"}} {
			if ansi256(th.Color(p[0])) == ansi256(th.Color(p[1])) {
				t.Errorf("%s: %s and %s are one color at 256 colors", th.Name, p[0], p[1])
			}
		}
		if e := xterm[ansi256(th.Palette.Danger).(ansi.IndexedColor)]; math.Hypot(e[1], e[2]) < 1e-3 {
			t.Errorf("%s: danger is a grey at 256 colors", th.Name)
		}
		for _, p := range chrome {
			h, g := app.hlStyle(p[0]), app.hlStyle(p[1])
			if !sameColor(h.Fg, g.Fg) || !sameColor(h.Bg, g.Bg) {
				t.Errorf("%s: %s is %v on %v, %s %v on %v", th.Name, p[0], h.Fg, h.Bg, p[1], g.Fg, g.Bg)
			}
		}
	}
}

// TestActiveTheme: a light terminal draws the theme's light twin, chrome
// included, unless less trusted code added the twin, a dark one the theme
// itself, and a name no theme answers to draws whorl and is named in /errors.
func TestActiveTheme(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	title := func() color.Color { return app.hlStyle("Host:Title").Fg }
	whorl, light, nord := host.Themes.Get("whorl"), host.Themes.Get("whorl-light"), host.Themes.Get("nord")
	if _, err := host.Themes.Add(&hostapi.Theme{Name: "nord-light", Palette: light.Palette, Origin: lua.OriginProject}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		bg    uint32
		theme string
		want  *hostapi.Theme
	}{
		{"light terminal", 0xfafafa, "whorl", light},
		{"dark terminal", 0x141414, "whorl", whorl},
		{"light theme on a dark terminal", 0x141414, "whorl-light", light},
		{"a project's twin of a shipped theme", 0xfafafa, "nord", nord},
		{"unknown name", 0x141414, "gone", whorl},
	} {
		app.Update(tea.BackgroundColorMsg{Color: rgb(c.bg)})
		if _, err := host.Settings.SetQuiet("ui.theme", c.theme); err != nil {
			t.Fatal(err)
		}
		if got := host.ActiveTheme(); got != c.want || !sameColor(title(), c.want.Palette.Accent) {
			t.Errorf("%s: %s draws, title %v", c.name, got.Name, title())
		}
	}
	host.CheckTheme()
	if !host.Recorded(`ui.theme "gone" names no theme, so whorl draws; /theme lists them`) {
		t.Error("a stale ui.theme is not in /errors")
	}
}
