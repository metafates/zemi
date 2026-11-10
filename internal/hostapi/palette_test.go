package hostapi

import (
	"image/color"
	"testing"
)

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
