package ui

import (
	"image/color"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
)

// themeResolver turns highlight-group names into ultraviolet styles using
// the active theme's palette (DESIGN §6.6: palettes map names to colors, hl
// groups reference palette names or raw hex; Go resolves them).
type themeResolver struct {
	host  *hostapi.Host
	sgr   map[string]uv.Style // decoded "sgr:" hls; compose runs on the UI goroutine only
	light bool                // the terminal reported a light background
}

func (t themeResolver) style(hl string) uv.Style {
	if params, ok := strings.CutPrefix(hl, hostapi.SGRPrefix); ok {
		return t.sgrStyle(params)
	}
	spec, theme, ok := t.group(hl)
	if !ok {
		return uv.Style{}
	}
	return drawOver(uv.Style{}, spec, theme)
}

// group is the active theme's spec for a group. On a light terminal a
// theme's "-light" variant is used when it registered one.
func (t themeResolver) group(hl string) (hostapi.HLSpec, *hostapi.Theme, bool) {
	if hl == "" || t.host == nil {
		return hostapi.HLSpec{}, nil, false
	}
	name := t.host.ActiveThemeName()
	theme := t.host.Themes.Get(name)
	if t.light {
		if light := t.host.Themes.Get(name + "-light"); light != nil {
			theme = light
		}
	}
	if theme == nil {
		return hostapi.HLSpec{}, nil, false
	}
	spec, ok := t.host.Themes.Group(theme, hl)
	return spec, theme, ok
}

// hover draws a hover group ("Hover", "HoverItem") over base, the text's
// own style. A theme without the group still underlines, so a target
// never goes unmarked.
func (t themeResolver) hover(base uv.Style, group string) uv.Style {
	spec, theme, ok := t.group(group)
	if !ok {
		spec = hostapi.HLSpec{Underline: "single"}
	}
	if spec.Replace {
		base = uv.Style{}
	}
	st := drawOver(base, spec, theme)
	if spec.Brighten != 0 {
		fg := st.Fg
		if fg == nil { // the terminal's own fg: brighten Normal instead
			fg = t.style("Normal").Fg
		}
		if k := spec.Brighten; fg != nil {
			if st.Bg != nil && luma(st.Bg) > luma(fg) { // dark text on a light fill: brighter reads as darker
				k = -k
			}
			st.Fg = brighten(fg, k)
		}
	}
	return st
}

// drawOver sets the keys spec sets on base and keeps the rest.
func drawOver(st uv.Style, spec hostapi.HLSpec, theme *hostapi.Theme) uv.Style {
	set := func(dst *color.Color, ref string) {
		switch {
		case ref == "NONE":
			*dst = nil
		case ref != "":
			if c := colorValue(ref, theme); c != nil {
				*dst = c
			}
		}
	}
	set(&st.Fg, spec.Fg)
	set(&st.Bg, spec.Bg)
	set(&st.UnderlineColor, spec.Sp)
	for _, a := range []struct {
		on   *bool
		attr uint8
	}{{spec.Bold, uv.AttrBold}, {spec.Italic, uv.AttrItalic}, {spec.Reverse, uv.AttrReverse}} {
		switch {
		case a.on == nil:
		case *a.on:
			st.Attrs |= a.attr
		default:
			st.Attrs &^= a.attr
		}
	}
	if u, ok := underlines[spec.Underline]; ok {
		st.Underline = u
	}
	return st
}

var underlines = map[string]uv.Underline{
	"none": uv.UnderlineNone, "single": uv.UnderlineSingle, "double": uv.UnderlineDouble,
	"curl": uv.UnderlineCurly, "dotted": uv.UnderlineDotted, "dashed": uv.UnderlineDashed,
}

func luma(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	return 0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)
}

// brighten mixes c by k toward white, or toward black when k is negative.
func brighten(c color.Color, k float64) color.Color {
	r, g, b, _ := c.RGBA()
	mix := func(v uint32) uint8 {
		x := float64(v >> 8)
		if k > 0 {
			return uint8(x + (255-x)*k + 0.5)
		}
		return uint8(x*(1+k) + 0.5)
	}
	return ansi.RGBColor{R: mix(r), G: mix(g), B: mix(b)}
}

// sgrStyle decodes inline SGR params (zemi.ui.markdown output), cached
// because compose runs every frame.
func (t themeResolver) sgrStyle(params string) uv.Style {
	if st, ok := t.sgr[params]; ok {
		return st
	}
	st := hostapi.SGRStyle(params)
	if t.sgr != nil {
		if len(t.sgr) >= 1024 {
			clear(t.sgr)
		}
		t.sgr[params] = st
	}
	return st
}

// colorValue resolves a theme color reference: raw hex, a palette name, or
// an ANSI color name the user's terminal decides.
func colorValue(ref string, theme *hostapi.Theme) color.Color {
	if !strings.HasPrefix(ref, "#") {
		if c, ok := hostapi.ANSIColors[ref]; ok {
			return c
		}
		ref = theme.Palette[ref]
	}
	if !strings.HasPrefix(ref, "#") {
		return nil
	}
	col, _ := hexColor(ref)
	return col
}
