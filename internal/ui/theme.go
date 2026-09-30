package ui

import (
	"image/color"
	"math"
	"slices"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"

	"github.com/metafates/zemi/internal/hostapi"
)

// themeResolver turns highlight-group names into ultraviolet styles in the
// active theme's palette (DESIGN §6.6: groups name palette roles, hex or
// ANSI colors; Go resolves them).
type themeResolver struct {
	host *hostapi.Host
	sgr  map[string]uv.Style // decoded "sgr:" hls; compose runs on the UI goroutine only
	// the Host: styles in the palette of chromeOf, built again when the
	// active theme changes
	chromeOf *hostapi.Theme
	chrome   map[string]uv.Style
}

func (t themeResolver) style(hl string) uv.Style {
	if params, ok := strings.CutPrefix(hl, hostapi.SGRPrefix); ok {
		st := t.sgrStyle(params)
		if st.Bg == nil {
			st.Bg = t.canvas() // a raw span without a fill sits on the canvas
		}
		return st
	}
	spec, theme, ok := t.group(hl)
	if !ok {
		// an unstyled span (a draft's typed text) still sits on the canvas
		// with the terminal's own foreground
		return uv.Style{Bg: t.canvas()}
	}
	st := drawOver(uv.Style{}, spec, theme)
	if st.Bg == nil {
		st.Bg = t.canvas()
	}
	return st
}

// canvas is the active theme's bg. Every group without a fill of its own
// sits on it, so the app paints one canvas and the terminal's background
// never shows through a gap.
func (t themeResolver) canvas() color.Color {
	if theme := t.active(); theme != nil {
		return theme.Palette.Bg
	}
	return nil
}

// active resolves the theme in force; nil when no theme is registered.
func (t themeResolver) active() *hostapi.Theme {
	if t.host == nil {
		return nil
	}
	return t.host.ActiveTheme()
}

// group is a group's spec and the theme its roles resolve in.
func (t themeResolver) group(hl string) (hostapi.HLSpec, *hostapi.Theme, bool) {
	if hl == "" || t.host == nil {
		return hostapi.HLSpec{}, nil, false
	}
	spec, ok := t.host.Themes.Group(hl)
	return spec, t.active(), ok
}

// chromeStyle is a Host: style in the active palette, on the canvas where
// it sets no fill; false for any other name.
func (t *themeResolver) chromeStyle(hl string) (uv.Style, bool) {
	theme := t.active()
	if theme == nil {
		return uv.Style{}, false
	}
	if t.chromeOf != theme {
		t.chromeOf, t.chrome = theme, chromeStyles(&theme.Palette)
	}
	st, ok := t.chrome[hl]
	if ok && st.Bg == nil {
		st.Bg = theme.Palette.Bg
	}
	return st, ok
}

// chromeStyles are the styles of the permission prompt and other Go
// chrome, in the palette's roles: no group or hl.set reaches them, so
// nothing can dress up a prompt, and every theme's contrast floors hold.
func chromeStyles(p *hostapi.Palette) map[string]uv.Style {
	return map[string]uv.Style{
		hostapi.ChromeBold:       {Fg: p.Fg, Attrs: uv.AttrBold},
		hostapi.ChromeTitle:      {Fg: p.Accent, Attrs: uv.AttrBold},
		hostapi.ChromeRule:       {Fg: p.Rule},
		hostapi.ChromeCursor:     {Attrs: uv.AttrReverse},
		hostapi.ChromeValue:      {Fg: p.Fg},
		hostapi.ChromeHint:       {Fg: p.Dim},
		hostapi.ChromeHintKey:    {Fg: p.Accent},
		hostapi.ChromeWarn:       {Fg: p.Warn},
		hostapi.ChromeOk:         {Fg: p.Ok},
		hostapi.ChromeDanger:     {Fg: p.Danger},
		hostapi.ChromeDiffAdd:    {Fg: p.Fg, Bg: p.AddBg},
		hostapi.ChromeDiffAddNum: {Fg: p.Ok, Bg: p.AddBg},
		hostapi.ChromeDiffDel:    {Fg: p.Fg, Bg: p.DelBg},
		hostapi.ChromeDiffDelNum: {Fg: p.Danger, Bg: p.DelBg},
		hostapi.ChromeDiffCtx:    {Fg: p.Fg},
		hostapi.ChromeDiffCtxNum: {Fg: p.Dim},
		hostapi.ChromeSelection:  {Attrs: uv.AttrReverse},
		hostapi.ChromeHover:      {Underline: uv.UnderlineSingle},
		hostapi.ChromeDim:        {Fg: p.Rule},
	}
}

// hover draws a hover group ("Hover", "HoverItem") over base, the text's
// own style. A theme without the group still underlines, so a target
// never goes unmarked.
func (t themeResolver) hover(base uv.Style, group string) uv.Style {
	spec, theme, ok := t.group(group)
	if !ok {
		spec = hostapi.HLSpec{Underline: hostapi.UnderlineSingle}
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
			to := color.Color(color.White)
			if k < 0 {
				to, k = color.Black, -k
			}
			st.Fg = hostapi.Mix(fg, to, k)
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
			if c := theme.Color(ref); c != nil {
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

//exhaustive:enforce
var underlines = map[hostapi.Underline]uv.Underline{
	hostapi.UnderlineNone: uv.UnderlineNone, hostapi.UnderlineSingle: uv.UnderlineSingle, hostapi.UnderlineDouble: uv.UnderlineDouble,
	hostapi.UnderlineCurl: uv.UnderlineCurly, hostapi.UnderlineDotted: uv.UnderlineDotted, hostapi.UnderlineDashed: uv.UnderlineDashed,
}

func luma(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	return 0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)
}

// sameColor reports whether two colors are the same, or both nil.
func sameColor(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == b
	}
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return ar == br && ag == bg && ab == bb
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

// xterm is the OkLab color of each of the 240 fixed 256-color entries.
var xterm = func() (t [256][3]float64) {
	for i := 16; i < 256; i++ {
		c, _ := colorful.MakeColor(ansi.IndexedColor(i))
		t[i][0], t[i][1], t[i][2] = c.OkLab()
	}
	return t
}()

// ansi256 is what a 256-color terminal shows: a near grey the neutral entry
// nearest in lightness, anything else the entry nearest in OkLab. The stock
// table picks by HSLuv, which turns a near grey lavender, navy or teal.
func ansi256(c color.Color) color.Color {
	switch c.(type) {
	case nil, ansi.BasicColor, ansi.IndexedColor:
		return c
	}
	col, ok := colorful.MakeColor(c)
	if !ok {
		return c
	}
	l, a, b := col.OkLab()
	grey := math.Hypot(a, b) < 0.03
	best, dist := 16, math.Inf(1)
	for i := 16; i < 256; i++ {
		e := xterm[i]
		d := (l - e[0]) * (l - e[0])
		switch {
		case !grey:
			d += (a-e[1])*(a-e[1]) + (b-e[2])*(b-e[2])
		case math.Hypot(e[1], e[2]) > 1e-3:
			continue
		}
		if d < dist {
			best, dist = i, d
		}
	}
	return ansi.IndexedColor(best)
}

// greys16 are the 16-color greys, dark to light.
var greys16 = [...]ansi.BasicColor{ansi.Black, ansi.BrightBlack, ansi.White, ansi.BrightWhite}

// grey16 is the 16-color grey for an OKLCH lightness.
func grey16(l float64) ansi.BasicColor {
	switch {
	case l < 0.3:
		return ansi.Black
	case l < 0.72:
		return ansi.BrightBlack
	case l < 0.93:
		return ansi.White
	}
	return ansi.BrightWhite
}

// surface16 is what a 16-color terminal shows for a background: a grey by
// its lightness, since the stock table paints a dark tint blue or cyan.
func surface16(c color.Color) color.Color {
	if _, basic := c.(ansi.BasicColor); basic || c == nil {
		return c
	}
	col, ok := colorful.MakeColor(c)
	if !ok {
		return c
	}
	l, _, _ := col.OkLch()
	return grey16(l)
}

// apart is the grey next to bg16 on the side fg sits of bg, for a
// foreground that would vanish on it; a hue bg stands in for the grey
// it is near.
func apart(fg, bg color.Color, bg16 ansi.BasicColor) ansi.BasicColor {
	i := slices.Index(greys16[:], bg16)
	if i < 0 {
		i = 1 + int(bg16/8)
	}
	if luma(fg) > luma(bg) && i < len(greys16)-1 || i == 0 {
		return greys16[i+1]
	}
	return greys16[i-1]
}

// ansi16 is what a 16-color terminal shows for a foreground: a grey by its
// lightness, a hue by the ANSI hue its OKLCH hue falls in, bright when light.
// The stock 256-to-16 table draws amber and apricot red and splits greys
// at random (#7a7a7a is bright black, #828282 white).
func ansi16(c color.Color) color.Color {
	if _, basic := c.(ansi.BasicColor); basic || c == nil {
		return c
	}
	col, ok := colorful.MakeColor(c)
	if !ok {
		return c
	}
	l, ch, h := col.OkLch()
	if ch < 0.04 {
		return grey16(l)
	}
	for _, r := range [...]struct {
		upTo float64
		c    ansi.BasicColor
	}{{40, ansi.Red}, {115, ansi.Yellow}, {175, ansi.Green}, {235, ansi.Cyan}, {300, ansi.Blue}, {345, ansi.Magenta}, {361, ansi.Red}} {
		if h < r.upTo {
			if l > 0.65 {
				return r.c + 8
			}
			return r.c
		}
	}
	return c
}
