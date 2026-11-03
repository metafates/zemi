package hostapi

import (
	"fmt"
	"image/color"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"
)

// Palette is the screen's colors by role, each set: what the spec leaves out
// is derived, Panel and Band stay near Bg, and every text role meets its
// contrast floor on all three, so nothing can hide a prompt's text.
type Palette struct {
	Bg, Fg, Accent, Literal, Ok, Warn, Danger, Dim, Rule, Panel, Band, AddBg, DelBg, Well color.Color
}

// paletteRole pairs a role's name with its field: the one source both the
// Roles list and role() read.
type paletteRole struct {
	name  string
	field func(*Palette) *color.Color
}

var paletteRoles = []paletteRole{
	{"bg", func(p *Palette) *color.Color { return &p.Bg }},
	{"fg", func(p *Palette) *color.Color { return &p.Fg }},
	{"accent", func(p *Palette) *color.Color { return &p.Accent }},
	{"literal", func(p *Palette) *color.Color { return &p.Literal }},
	{"ok", func(p *Palette) *color.Color { return &p.Ok }},
	{"warn", func(p *Palette) *color.Color { return &p.Warn }},
	{"danger", func(p *Palette) *color.Color { return &p.Danger }},
	{"dim", func(p *Palette) *color.Color { return &p.Dim }},
	{"rule", func(p *Palette) *color.Color { return &p.Rule }},
	{"panel", func(p *Palette) *color.Color { return &p.Panel }},
	{"band", func(p *Palette) *color.Color { return &p.Band }},
	{"addbg", func(p *Palette) *color.Color { return &p.AddBg }},
	{"delbg", func(p *Palette) *color.Color { return &p.DelBg }},
	{"well", func(p *Palette) *color.Color { return &p.Well }},
}

// Roles are the names a theme spec and a group's colors give Palette's fields.
var Roles = paletteRoleNames()

func paletteRoleNames() []string {
	names := make([]string, len(paletteRoles))
	for i, r := range paletteRoles {
		names[i] = r.name
	}
	return names
}

func (p *Palette) role(name string) *color.Color {
	for _, r := range paletteRoles {
		if r.name == name {
			return r.field(p)
		}
	}
	return nil
}

// Color resolves a group's color: a role, hex, or an ANSI color name the
// terminal decides. nil when it names nothing.
func (p *Palette) Color(ref string) color.Color {
	if c, ok := ANSIColors[ref]; ok {
		return c
	}
	if strings.HasPrefix(ref, "#") {
		c, err := colorful.Hex(ref)
		if err != nil {
			return nil
		}
		return rgb(c)
	}
	if p == nil {
		return nil
	}
	if r := p.role(ref); r != nil {
		return *r
	}
	return nil
}

// ansiRoles are the terminal colors a role left out comes from: red, green,
// yellow and blue are the slots every scheme sets with care.
var ansiRoles = [...]struct {
	role string
	slot int
}{{"danger", 1}, {"ok", 2}, {"warn", 3}, {"accent", 4}}


// mixes derive the rest, each role mixed from one toward another in OkLab.
// The band tints the panel, since only a box draws it.
// The diff tints are mixed far enough to keep an OKLCH chroma over the
// 256-color quantizer's grey cut (0.03) on whorl.
// NOTE: on a greyish bg they may still round to one grey at 256 colors;
// the diff's colored numbers tell the rows apart.
var mixes = [...]struct {
	role, from, to string
	t              float64
}{
	{"dim", "fg", "bg", 0.4},
	{"rule", "bg", "fg", 0.28},
	// a box lifts far enough off the canvas to read as one above a whole
	// screen of canvas, and no further
	{"panel", "bg", "fg", 0.07},
	{"band", "panel", "accent", 0.16},
	{"addbg", "bg", "ok", 0.28},
	{"delbg", "bg", "danger", 0.28},
	// a command well sinks to the canvas; what it answered lifts a step off
	// the box, far enough to stay a surface of its own at 256 colors
	{"well", "panel", "fg", 0.08},
}

// surfaceSpan is how far in OKLCH lightness panel and band may sit from bg,
// a spec's own included: a box stays a box, and text reads on it.
const surfaceSpan = 0.13

// floors are the WCAG contrast each text role is clamped to on bg, panel,
// band and well, the surfaces text draws on.
var floors = [...]struct {
	role string
	min  float64
}{
	{"fg", 7},
	{"dim", 4.5},
	{"accent", 4.5},
	{"literal", 4.5},
	{"ok", 4.5},
	{"danger", 4.5},
	// NOTE: under 4.5, since a light theme's amber darker than it must be
	// reads as danger to deuteranopes.
	{"warn", 3.5},
}

// parsePalette checks a palette spec and derives what it leaves out: bg and
// fg, the four hues, any other role as an override.
func parsePalette(spec map[string]any) (Palette, error) {
	var p Palette
	for k, v := range spec {
		dst := p.role(k)
		if dst == nil {
			return p, fmt.Errorf("unknown key %q: a palette takes %s", k, strings.Join(Roles, ", "))
		}
		s, _ := v.(string)
		col, err := parseHex(s)
		if err != nil {
			return p, fmt.Errorf("%s = %q is not #rrggbb", k, s)
		}
		*dst = rgb(col)
	}
	if p.Bg == nil || p.Fg == nil || p.Accent == nil || p.Ok == nil || p.Warn == nil || p.Danger == nil {
		return p, fmt.Errorf("bg, fg, accent, ok, warn and danger are required")
	}
	derive(&p)
	return p, nil
}

func parseHex(s string) (colorful.Color, error) {
	if !isHexColor(s) {
		return colorful.Color{}, fmt.Errorf("not a hex color")
	}
	return colorful.Hex(s)
}

// derive fills the roles p leaves out, keeps the surfaces near bg and
// clamps every text role to its floor on each surface.
func derive(p *Palette) {
	if p.Literal == nil {
		// the palette's own orange: halfway between its red and its yellow
		l1, c1, h1 := col(p.Danger).OkLch()
		l2, c2, h2 := col(p.Warn).OkLch()
		d := math.Mod(h2-h1+540, 360) - 180
		p.Literal = inGamut((l1+l2)/2, (c1+c2)/2, math.Mod(h1+d/2+360, 360))
	}
	for _, m := range mixes {
		dst := p.role(m.role)
		if *dst != nil {
			continue
		}
		*dst = Mix(*p.role(m.from), *p.role(m.to), m.t)
	}
	lbg, _, _ := col(p.Bg).OkLch()
	for _, s := range []*color.Color{&p.Panel, &p.Band} {
		l, c, h := col(*s).OkLch()
		*s = inGamut(min(max(l, lbg-surfaceSpan), lbg+surfaceSpan), c, h)
	}
	on := []colorful.Color{col(p.Bg), col(p.Panel), col(p.Band), col(p.Well)}
	for _, f := range floors {
		dst := p.role(f.role)
		*dst = clampContrast(col(*dst), on, f.min)
	}
}

// Mix blends a toward b by t in OkLab, where equal steps look equal.
func Mix(a, b color.Color, t float64) color.Color {
	return rgb(col(a).BlendOkLab(col(b), t).Clamped())
}

// Contrast is the WCAG contrast ratio of two colors, 1 to 21.
func Contrast(a, b color.Color) float64 {
	la, lb := luminance(col(a)), luminance(col(b))
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

func luminance(c colorful.Color) float64 {
	r, g, b := c.LinearRgb()
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// clampContrast moves c along OKLCH lightness away from the surfaces until
// it has floor contrast on each, keeping its hue and cutting chroma only to
// stay in gamut; as far as it goes when no lightness reaches the floor.
func clampContrast(c colorful.Color, on []colorful.Color, floor float64) color.Color {
	least := func(c color.Color) float64 {
		r := math.Inf(1)
		for _, s := range on {
			r = min(r, Contrast(c, s))
		}
		return r
	}
	if least(c) >= floor {
		return rgb(c)
	}
	l, ch, h := c.OkLch()
	end := 1.0
	if least(colorful.Color{}) > least(colorful.Color{R: 1, G: 1, B: 1}) {
		end = 0
	}
	at := func(l float64) color.Color { return inGamut(l, ch, h) }
	if least(at(end)) < floor {
		return at(end)
	}
	lo, hi := l, end // lo fails the floor, hi meets it
	for range 24 {
		if mid := (lo + hi) / 2; least(at(mid)) >= floor {
			hi = mid
		} else {
			lo = mid
		}
	}
	return at(hi)
}

// inGamut is the OKLCH color with the most chroma up to c that sRGB shows.
func inGamut(l, c, h float64) color.Color {
	if v := colorful.OkLch(l, c, h); v.IsValid() {
		return rgb(v)
	}
	lo, hi := 0.0, c
	for range 16 {
		if mid := (lo + hi) / 2; colorful.OkLch(l, mid, h).IsValid() {
			lo = mid
		} else {
			hi = mid
		}
	}
	return rgb(colorful.OkLch(l, lo, h).Clamped())
}

// KnotRamp is the 32-stop ramp of the knot piece (docs/whorl-knot.js's
// audited curve) in the palette's colors: lightness linear from rule to
// literal, chroma rising to a plateau at the accent's with a dip between the
// two hues, hue swinging from the accent's to the literal's the long way.
func KnotRamp(p *Palette) (out [32]color.Color) {
	lR, cR, _ := col(p.Rule).OkLch()
	_, cA, hA := col(p.Accent).OkLch()
	lL, cL, hL := col(p.Literal).OkLch()
	cs := []rampKnot{{0, cR}, {0.10, 1.03 * cR}, {0.22, 1.31 * cR}, {0.46, cA}, {0.66, cA}, {0.865, 0.60 * cR}, {0.895, 0.60 * cR}, {1, cL}}
	hs := []rampKnot{{0, hA}, {0.855, hA}, {0.872, hL + 260}, {0.895, hL + 360}, {1, hL + 360}}
	out[0], out[31] = p.Rule, p.Literal // the anchors are the roles themselves
	for i := 1; i < 31; i++ {
		t := float64(i) / 31
		out[i] = inGamut(lR+t*(lL-lR), mixKnots(cs, t), mixKnots(hs, t))
	}
	return out
}

type rampKnot struct{ at, v float64 }

func mixKnots(knots []rampKnot, t float64) float64 {
	for i := 1; i < len(knots); i++ {
		if t <= knots[i].at {
			a, b := knots[i-1], knots[i]
			return a.v + (b.v-a.v)*(t-a.at)/(b.at-a.at)
		}
	}
	return knots[len(knots)-1].v
}

func col(c color.Color) colorful.Color {
	v, _ := colorful.MakeColor(c)
	return v
}

func rgb(c colorful.Color) color.Color {
	r, g, b := c.RGB255()
	return ansi.RGBColor{R: r, G: g, B: b}
}

// Hex writes a color as #rrggbb, "" for nil.
func Hex(c color.Color) string {
	if c == nil {
		return ""
	}
	return col(c).Hex()
}
