package hostapi

import (
	"image/color"
	"math"
	"testing"

	"github.com/lucasb-eyer/go-colorful"
)

// TestPaletteDerive: a role the spec leaves out comes from ansi, the
// literal lands near the scheme's own orange, and a role that would not read
// on bg, panel or band is moved in lightness to its floor, keeping its hue.
func TestPaletteDerive(t *testing.T) {
	nord := []any{"#3b4252", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#88c0d0", "#e5e9f0", "#596377", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb", "#eceff4"}
	// a plugin theme asking for boxes the color of its text
	white := map[string]any{"bg": "#000000", "fg": "#ffffff", "accent": "#a2adfd", "ok": "#68d7a1", "warn": "#edcf6f", "danger": "#f17070", "panel": "#ffffff", "band": "#ffffff"}
	for _, c := range []struct {
		name string
		spec map[string]any
		role func(p *Palette) color.Color
		near string  // the color whose OKLCH hue it keeps
		min  float64 // or its contrast on bg, panel and band
	}{
		{"nord's literal is nord12", map[string]any{"bg": "#2e3440", "fg": "#d8dee9", "ansi": nord}, func(p *Palette) color.Color { return p.Literal }, "#d08770", 0},
		{"ansi blue is the accent", map[string]any{"bg": "#2e3440", "fg": "#d8dee9", "ansi": nord}, func(p *Palette) color.Color { return p.Accent }, "#81a1c1", 0},
		{"a warn on bg is clamped", map[string]any{"bg": "#fafafc", "fg": "#202127", "accent": "#5458c8", "ok": "#007f52", "warn": "#fafafc", "danger": "#c72d2e"}, func(p *Palette) color.Color { return p.Warn }, "", 3.5},
		{"a dim given dim is clamped", map[string]any{"bg": "#17181e", "fg": "#e7e7ec", "accent": "#a2adfd", "ok": "#68d7a1", "warn": "#edcf6f", "danger": "#f17070", "dim": "#303030"}, func(p *Palette) color.Color { return p.Dim }, "", 4.5},
		{"a white panel and band still show fg", white, func(p *Palette) color.Color { return p.Fg }, "", 7},
		{"a white panel and band still show dim", white, func(p *Palette) color.Color { return p.Dim }, "", 4.5},
	} {
		p, err := parsePalette(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := c.role(&p)
		if c.near != "" {
			want, _ := colorful.Hex(c.near)
			_, _, h1 := col(got).OkLch()
			_, _, h2 := want.OkLch()
			if d := math.Abs(math.Mod(h1-h2+540, 360) - 180); d > 15 {
				t.Errorf("%s: %s is %.1f degrees from %s", c.name, Hex(got), d, c.near)
			}
		}
		for _, on := range []color.Color{p.Bg, p.Panel, p.Band} {
			if r := Contrast(got, on); r < c.min {
				t.Errorf("%s: %s is %.2f:1 on %s", c.name, Hex(got), r, Hex(on))
			}
		}
	}
}
