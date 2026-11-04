package hostapi

import (
	"image/color"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/lucasb-eyer/go-colorful"
)

// TestPaletteRoles: Roles names Palette's fields in their own order and
// role() answers every one of them, so a field added here is named once and
// nothing but the spec's own words reaches a theme.
func TestPaletteRoles(t *testing.T) {
	typ := reflect.TypeFor[Palette]()
	var p Palette
	for i, name := range Roles {
		if i >= typ.NumField() || !strings.EqualFold(typ.Field(i).Name, name) {
			t.Fatalf("Roles[%d] = %q, Palette field %d is not its own", i, name, i)
		}
		if p.role(name) == nil {
			t.Errorf("role(%q) is nil", name)
		}
	}
	if n := typ.NumField(); n != len(Roles) {
		t.Fatalf("Palette has %d fields, Roles names %d", n, len(Roles))
	}
}

// TestPaletteDerive: the literal lands near the palette's own orange, and a
// role that would not read on bg, panel or band is moved in lightness to its
// floor, keeping its hue.
func TestPaletteDerive(t *testing.T) {
	// a palette asking for boxes the color of its text
	white := map[string]any{"bg": "#000000", "fg": "#ffffff", "accent": "#a2adfd", "ok": "#68d7a1", "warn": "#edcf6f", "danger": "#f17070", "panel": "#ffffff", "band": "#ffffff"}
	for _, c := range []struct {
		name string
		spec map[string]any
		role func(p *Palette) color.Color
		low  float64 // its contrast on bg, panel and band
	}{
		{"a warn on bg is clamped", map[string]any{"bg": "#fafafc", "fg": "#202127", "accent": "#5458c8", "ok": "#007f52", "warn": "#fafafc", "danger": "#c72d2e"}, func(p *Palette) color.Color { return p.Warn }, 3.5},
		{"a dim given dim is clamped", map[string]any{"bg": "#17181e", "fg": "#e7e7ec", "accent": "#a2adfd", "ok": "#68d7a1", "warn": "#edcf6f", "danger": "#f17070", "dim": "#303030"}, func(p *Palette) color.Color { return p.Dim }, 4.5},
		{"a white panel and band still show fg", white, func(p *Palette) color.Color { return p.Fg }, 7},
		{"a white panel and band still show dim", white, func(p *Palette) color.Color { return p.Dim }, 4.5},
	} {
		p, err := parsePalette(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := c.role(&p)
		for _, on := range []color.Color{p.Bg, p.Panel, p.Band} {
			if r := Contrast(got, on); r < c.low {
				t.Errorf("%s: %s is %.2f:1 on %s", c.name, Hex(got), r, Hex(on))
			}
		}
	}
}

// TestKnotRamp: the whorl palette reproduces the audited 32 stops shipped in
// docs/whorl-knot.js, to the rounding of a channel, so the TUI's knot draws
// in the landing's colors.
func TestKnotRamp(t *testing.T) {
	p, err := parsePalette(map[string]any{
		"bg": "#17181e", "fg": "#e7e7ec", "dim": "#898b98", "rule": "#4b4c58", "panel": "#202128",
		"accent": "#a2adfd", "literal": "#f9b379", "ok": "#68d7a1", "warn": "#edcf6f", "danger": "#f17070",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{
		"#4b4c58", "#4e505c", "#515360", "#555763", "#585a68", "#5b5e6c", "#5f6271", "#626577",
		"#646881", "#666b8c", "#686e97", "#6a71a2", "#6c74ad", "#6e76b7", "#7079c2", "#747dc8",
		"#7781cd", "#7b84d1", "#7f88d5", "#838cda", "#8690de", "#8b95dd", "#919bd8", "#98a0d2",
		"#9ea5cc", "#a5aac6", "#abafc0", "#b7b1b9", "#c2b5ac", "#d5b59c", "#e7b48c", "#f9b379",
	} {
		got := col(KnotRamp(&p)[i])
		wantC, err := colorful.Hex(want)
		if err != nil {
			t.Fatal(err)
		}
		gr, gg, gb := got.RGB255()
		wr, wg, wb := wantC.RGB255()
		d := func(a, b uint8) float64 { return math.Abs(float64(int(a) - int(b))) }
		if d(gr, wr) > 1 || d(gg, wg) > 1 || d(gb, wb) > 1 {
			t.Errorf("stop %d is %s, want %s", i, Hex(got), want)
		}
	}
}
