package hostapi

import (
	"image/color"
	"testing"

	"github.com/lucasb-eyer/go-colorful"
)

// TestPaletteDerive: a role the spec leaves out comes from ansi, the
// literal lands near the scheme's own orange, and a role that would not read
// on bg is moved in lightness to its floor, keeping its hue.
func TestPaletteDerive(t *testing.T) {
	nord := []any{"#3b4252", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#88c0d0", "#e5e9f0", "#596377", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb", "#eceff4"}
	for _, c := range []struct {
		name string
		spec map[string]any
		role func(p *Palette) color.Color
		near string  // the color it should land near, by CIEDE2000
		min  float64 // or its contrast on bg
	}{
		{"nord's literal is nord12", map[string]any{"bg": "#2e3440", "fg": "#d8dee9", "ansi": nord}, func(p *Palette) color.Color { return p.Literal }, "#d08770", 0},
		{"ansi blue is the accent", map[string]any{"bg": "#2e3440", "fg": "#d8dee9", "ansi": nord}, func(p *Palette) color.Color { return p.Accent }, "#81a1c1", 0},
		{"a warn on bg is clamped", map[string]any{"bg": "#fafafc", "fg": "#202127", "accent": "#5458c8", "ok": "#007f52", "warn": "#fafafc", "danger": "#c72d2e"}, func(p *Palette) color.Color { return p.Warn }, "", 3.5},
		{"a dim given dim is clamped", map[string]any{"bg": "#17181e", "fg": "#e7e7ec", "accent": "#a2adfd", "ok": "#68d7a1", "warn": "#edcf6f", "danger": "#f17070", "dim": "#303030"}, func(p *Palette) color.Color { return p.Dim }, "", 4.5},
	} {
		p, err := parsePalette(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := c.role(&p)
		if c.near != "" {
			want, _ := colorful.Hex(c.near)
			if d := col(got).DistanceCIEDE2000(want); d > 0.08 {
				t.Errorf("%s: %s is %.3f from %s", c.name, Hex(got), d, c.near)
			}
		}
		if r := Contrast(got, p.Bg); r < c.min {
			t.Errorf("%s: %s is %.2f:1 on bg", c.name, Hex(got), r)
		}
	}
}
