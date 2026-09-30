package hostapi

import (
	"fmt"
	"image/color"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"

	"github.com/metafates/zemi/internal/lua"
)

// Theme is a palette registered from Lua; the groups that draw with it are
// defined once, apart from any theme (DESIGN §6.6).
type Theme struct {
	Name    string
	Palette Palette
	Origin  lua.Origin
	Path    string
	Gen     Gen
}

// Palette is a theme's colors by role, each set: what the spec leaves out
// is derived, Panel and Band stay near Bg, and every text role meets its
// contrast floor on all three, so no theme can hide a prompt's text.
type Palette struct {
	Bg, Fg, Accent, Literal, Ok, Warn, Danger, Dim, Rule, Panel, Band, AddBg, DelBg color.Color
}

// Roles are the names a theme spec and a group's colors give Palette's fields.
var Roles = []string{"bg", "fg", "accent", "literal", "ok", "warn", "danger", "dim", "rule", "panel", "band", "addbg", "delbg"}

func (p *Palette) role(name string) *color.Color {
	switch name {
	case "bg":
		return &p.Bg
	case "fg":
		return &p.Fg
	case "accent":
		return &p.Accent
	case "literal":
		return &p.Literal
	case "ok":
		return &p.Ok
	case "warn":
		return &p.Warn
	case "danger":
		return &p.Danger
	case "dim":
		return &p.Dim
	case "rule":
		return &p.Rule
	case "panel":
		return &p.Panel
	case "band":
		return &p.Band
	case "addbg":
		return &p.AddBg
	case "delbg":
		return &p.DelBg
	}
	return nil
}

// Color resolves a group's color: a role, hex, or an ANSI color name the
// terminal decides. nil when it names nothing.
func (t *Theme) Color(ref string) color.Color {
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
	if t == nil {
		return nil
	}
	if p := t.Palette.role(ref); p != nil {
		return *p
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
// NOTE: the two faint diff tints round to one grey at 256 colors on most
// themes; the diff's colored numbers still tell the rows apart. Tints
// strong enough to survive the rounding look loud in true color.
var mixes = [...]struct {
	role, from, to string
	t              float64
}{
	{"dim", "fg", "bg", 0.4},
	{"rule", "bg", "fg", 0.28},
	{"panel", "bg", "fg", 0.05},
	{"band", "panel", "accent", 0.16},
	{"addbg", "bg", "ok", 0.16},
	{"delbg", "bg", "danger", 0.16},
}

// surfaceSpan is how far in OKLCH lightness panel and band may sit from bg,
// a spec's own included: a box stays a box, and text reads on it.
const surfaceSpan = 0.13

// floors are the WCAG contrast each text role is clamped to on bg, panel
// and band, the surfaces text draws on.
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

// parsePalette checks a theme spec and derives what it leaves out: bg and
// fg, the four hues or ansi (16 colors, black to bright white), any other
// role as an override.
func parsePalette(spec map[string]any) (Palette, error) {
	var p Palette
	var slots []colorful.Color
	for _, k := range slices.Sorted(maps.Keys(spec)) {
		v := spec[k]
		if k == "ansi" {
			list, ok := v.([]any)
			if !ok || len(list) != 16 {
				return p, fmt.Errorf("ansi must list the 16 terminal colors, black to bright white")
			}
			for i, c := range list {
				s, _ := c.(string)
				col, err := parseHex(s)
				if err != nil {
					return p, fmt.Errorf("ansi[%d] = %q is not #rrggbb", i+1, s)
				}
				slots = append(slots, col)
			}
			continue
		}
		dst := p.role(k)
		if dst == nil {
			return p, fmt.Errorf("unknown key %q: a theme takes %s and ansi", k, strings.Join(Roles, ", "))
		}
		s, _ := v.(string)
		col, err := parseHex(s)
		if err != nil {
			return p, fmt.Errorf("%s = %q is not #rrggbb", k, s)
		}
		*dst = rgb(col)
	}
	if p.Bg == nil || p.Fg == nil {
		return p, fmt.Errorf("bg and fg are required")
	}
	for _, a := range ansiRoles {
		if dst := p.role(a.role); *dst == nil {
			if slots == nil {
				return p, fmt.Errorf("%s is required without ansi", a.role)
			}
			*dst = rgb(slots[a.slot])
		}
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
		// each scheme's own orange: halfway between its red and its yellow
		l1, c1, h1 := col(p.Danger).OkLch()
		l2, c2, h2 := col(p.Warn).OkLch()
		d := math.Mod(h2-h1+540, 360) - 180
		p.Literal = inGamut((l1+l2)/2, (c1+c2)/2, math.Mod(h1+d/2+360, 360))
	}
	for _, m := range mixes {
		if dst := p.role(m.role); *dst == nil {
			*dst = Mix(*p.role(m.from), *p.role(m.to), m.t)
		}
	}
	lbg, _, _ := col(p.Bg).OkLch()
	for _, s := range []*color.Color{&p.Panel, &p.Band} {
		l, c, h := col(*s).OkLch()
		*s = inGamut(min(max(l, lbg-surfaceSpan), lbg+surfaceSpan), c, h)
	}
	on := []colorful.Color{col(p.Bg), col(p.Panel), col(p.Band)}
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
