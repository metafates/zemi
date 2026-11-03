package ui

import (
	"image/color"
	"math"
	"testing"

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

// TestWhorl: the one palette keeps each text role over its contrast
// floor on bg, panel, band and well, the literal apart in hue from warn and
// danger, its surfaces, rule and dim apart, danger a hue and the diff
// tints apart at 256 colors, and the Go chrome draws in the colors of the
// groups it stands for.
func TestWhorl(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
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
	pal := hostapi.Whorl
	hue := func(c color.Color) float64 {
		v, _ := colorful.MakeColor(c)
		_, _, h := v.OkLch()
		return h
	}
	for role, floor := range floors {
		for _, on := range []string{"bg", "panel", "band", "well"} {
			if r := hostapi.Contrast(pal.Color(role), pal.Color(on)); r < floor {
				t.Errorf("%s is %.2f:1 on %s, under %.1f", role, r, on, floor)
			}
		}
	}
	for _, other := range []string{"warn", "danger"} {
		d := math.Abs(hue(pal.Literal) - hue(pal.Color(other)))
		if d = min(d, 360-d); d < 15 {
			t.Errorf("literal %s is %.0f degrees from %s", hostapi.Hex(pal.Literal), d, other)
		}
	}
	for _, p := range [][2]string{{"band", "panel"}, {"panel", "bg"}, {"rule", "panel"}, {"fg", "dim"}} {
		if ansi256(pal.Color(p[0])) == ansi256(pal.Color(p[1])) {
			t.Errorf("%s and %s are one color at 256 colors", p[0], p[1])
		}
	}
	if ansi256(pal.AddBg) == ansi256(pal.DelBg) {
		t.Error("an added and a removed row are one color at 256 colors")
	}
	if e := xterm[ansi256(pal.Danger).(ansi.IndexedColor)]; math.Hypot(e[1], e[2]) < 1e-3 {
		t.Error("danger is a grey at 256 colors")
	}
	for _, p := range chrome {
		h, g := app.hlStyle(p[0]), app.hlStyle(p[1])
		if !sameColor(h.Fg, g.Fg) || !sameColor(h.Bg, g.Bg) {
			t.Errorf("%s is %v on %v, %s %v on %v", p[0], h.Fg, h.Bg, p[1], g.Fg, g.Bg)
		}
	}
}

// TestWhorlCVD: whorl keeps the pairs that must stay apart separated under
// deuteranopia and protanopia (Vienot 1999 in linear sRGB) and in greyscale,
// where zemi's marks already carry the state and literal is the one
// text-vs-text neighbor of warn.
func TestWhorlCVD(t *testing.T) {
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
	pairs := map[[2]string]float64{{"warn", "literal"}: 0.04, {"danger", "literal"}: 0.045, {"warn", "danger"}: 0.05}
	color := func(role string) color.Color {
		v := hostapi.Whorl.Color(role)
		if v == nil {
			t.Fatalf("whorl has no %s", role)
		}
		return v
	}
	for pair, floor := range pairs {
		a, b := color(pair[0]), color(pair[1])
		for name, m := range map[string][3][3]float64{"deutan": deutan, "protan": protan} {
			pa, pb := project(a, m), project(b, m)
			d := math.Hypot(math.Hypot(pa[0]-pb[0], pa[1]-pb[1]), pa[2]-pb[2])
			if d < floor {
				t.Errorf("%s and %s are %.3f apart under %s, under %.3f", pair[0], pair[1], d, name, floor)
			}
		}
	}
	if d := math.Abs(grey(color("warn")) - grey(color("literal"))); d < 0.07 {
		t.Errorf("warn and literal are %.3f apart in greyscale, under %.3f", d, 0.07)
	}
}
