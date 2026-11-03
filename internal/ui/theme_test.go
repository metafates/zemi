package ui

import (
	"image/color"
	"math"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"

	"github.com/metafates/zemi/internal/hostapi"
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
// floor on bg, panel, band and well, the literal apart in hue from warn and
// danger, its surfaces, rule and dim apart, danger a hue and whorl's diff
// tints apart at 256 colors, and the Go chrome draws in the colors of the
// groups it stands for.
func TestThemes(t *testing.T) {
	t.Parallel()
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
	themes := hostapi.ThemeList()
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
			for _, on := range []string{"bg", "panel", "band", "well"} {
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
		for _, p := range [][2]string{{"band", "panel"}, {"panel", "bg"}, {"rule", "panel"}, {"fg", "dim"}} {
			if ansi256(th.Color(p[0])) == ansi256(th.Color(p[1])) {
				t.Errorf("%s: %s and %s are one color at 256 colors", th.Name, p[0], p[1])
			}
		}
		if strings.HasPrefix(th.Name, "whorl") && ansi256(th.Palette.AddBg) == ansi256(th.Palette.DelBg) {
			t.Errorf("%s: an added and a removed row are one color at 256 colors", th.Name)
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

// TestThemeCVD: whorl and whorl-light keep the pairs that must stay apart
// separated under deuteranopia and protanopia (Vienot 1999 in linear sRGB)
// and in greyscale, where zemi's marks already carry the state and literal
// is the one text-vs-text neighbor of warn.
func TestThemeCVD(t *testing.T) {
	t.Parallel()
	deutan := [3][3]float64{{0.625, 0.375, 0}, {0.7, 0.3, 0}, {0, 0.3, 0.7}}
	protan := [3][3]float64{{0.567, 0.433, 0}, {0.558, 0.442, 0}, {0, 0.242, 0.758}}
	project := func(c color.Color, m [3][3]float64) [3]float64 {
		v, ok := colorful.MakeColor(c)
		if !ok {
			t.Fatal("not a color")
		}
		r, g, b := v.LinearRgb()
		l, a, bb := colorful.LinearRgb(m[0][0]*r+m[0][1]*g+m[0][2]*b, m[1][0]*r+m[1][1]*g+m[1][2]*b, m[2][0]*r+m[2][1]*g+m[2][2]*b).OkLab()
		return [3]float64{l, a, bb}
	}
	grey := func(c color.Color) float64 {
		v, ok := colorful.MakeColor(c)
		if !ok {
			t.Fatal("not a color")
		}
		l, _, _ := v.OkLch()
		return l
	}
	for _, c := range []struct {
		theme     string
		pairs     map[[2]string]float64
		greyFloor float64
	}{
		{theme: "whorl", greyFloor: 0.07, pairs: map[[2]string]float64{{"warn", "literal"}: 0.04, {"danger", "literal"}: 0.045, {"warn", "danger"}: 0.05}},
		{theme: "whorl-light", greyFloor: 0.05, pairs: map[[2]string]float64{{"warn", "literal"}: 0.035, {"danger", "literal"}: 0.045, {"warn", "danger"}: 0.04}},
	} {
		th := hostapi.ThemeByName(c.theme)
		if th == nil {
			t.Fatalf("no %s", c.theme)
		}
		color := func(role string) color.Color {
			v := th.Color(role)
			if v == nil {
				t.Fatalf("%s has no %s", c.theme, role)
			}
			return v
		}
		for pair, floor := range c.pairs {
			a, b := color(pair[0]), color(pair[1])
			for name, m := range map[string][3][3]float64{"deutan": deutan, "protan": protan} {
				pa, pb := project(a, m), project(b, m)
				d := math.Hypot(math.Hypot(pa[0]-pb[0], pa[1]-pb[1]), pa[2]-pb[2])
				if d < floor {
					t.Errorf("%s: %s and %s are %.3f apart under %s, under %.3f", c.theme, pair[0], pair[1], d, name, floor)
				}
			}
		}
		if d := math.Abs(grey(color("warn")) - grey(color("literal"))); d < c.greyFloor {
			t.Errorf("%s: warn and literal are %.3f apart in greyscale, under %.3f", c.theme, d, c.greyFloor)
		}
	}
}

// TestActiveTheme: a light terminal draws the theme's light twin, chrome
// included, a dark one the theme itself, a theme with no twin draws as it is,
// and a name no theme answers to draws whorl and is named in /errors.
func TestActiveTheme(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	title := func() color.Color { return app.hlStyle("Host:Title").Fg }
	whorl, light := hostapi.ThemeByName("whorl"), hostapi.ThemeByName("whorl-light")
	for _, c := range []struct {
		name  string
		bg    uint32
		theme string
		want  *hostapi.Theme
	}{
		{"light terminal", 0xfafafa, "whorl", light},
		{"dark terminal", 0x141414, "whorl", whorl},
		{"light theme on a dark terminal", 0x141414, "whorl-light", light},
		{"a theme with no light twin", 0xfafafa, "nord", hostapi.ThemeByName("nord")},
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
