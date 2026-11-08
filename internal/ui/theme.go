package ui

import (
	"image/color"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"

	"github.com/metafates/zemi/internal/hostapi"
)

// themeResolver turns highlight-group names into ultraviolet styles in the
// whorl palette (DESIGN §6.6: groups name palette roles, hex or ANSI colors;
// Go resolves them).
type themeResolver struct {
	sgr map[string]uv.Style // decoded "sgr:" hls; compose runs on the UI goroutine only
}

// _chrome is the whorl _chrome, built once: the palette never changes.
var _chrome = sync.OnceValue(func() map[string]uv.Style {
	return chromeStyles(hostapi.Whorl)
})

// _knot is the whorl _knot's ramp, built once with the chrome.
var _knot = sync.OnceValue(func() [32]color.Color {
	return hostapi.KnotRamp(hostapi.Whorl)
})

// style resolves hl; what sets no fill, an unstyled span (a draft's typed
// text) included, sits on the canvas.
func (t themeResolver) style(hl string) uv.Style {
	var st uv.Style
	if params, ok := strings.CutPrefix(hl, hostapi.SGRPrefix); ok {
		st = t.sgrStyle(params)
	} else if spec, ok := t.group(hl); ok {
		st = drawOver(st, spec, hostapi.Whorl)
	}
	if st.Bg == nil {
		st.Bg = t.canvas()
	}
	return st
}

// canvas is whorl's bg. Every group without a fill of its own sits on it, so
// the app paints one canvas and the terminal's background never shows through
// a gap.
func (t themeResolver) canvas() color.Color {
	return hostapi.Whorl.Bg
}

// group is a group's spec.
func (t themeResolver) group(hl string) (hostapi.HLSpec, bool) {
	if hl == "" {
		return hostapi.HLSpec{}, false
	}
	return hostapi.Group(hl)
}

// chromeStyle is a Host: style in the whorl palette, on the canvas where it
// sets no fill; false for any other name.
func (t *themeResolver) chromeStyle(hl string) (uv.Style, bool) {
	st, ok := _chrome()[hl]
	if ok && st.Bg == nil {
		st.Bg = hostapi.Whorl.Bg
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
		hostapi.ChromeDim:        {Fg: p.Rule}, // the modal backdrop's grey: the rule's, so sunk rows read as a shape, not as dim text
	}
}

// hover draws a hover group ("Hover", "HoverItem") over base, the text's
// own style. With the group undefined it still underlines, so a target
// never goes unmarked.
func (t themeResolver) hover(base uv.Style, group string) uv.Style {
	spec, ok := hostapi.Group(group)
	if !ok {
		spec = hostapi.HLSpec{Underline: hostapi.UnderlineSingle}
	}
	return t.hoverSpec(base, spec)
}

// hoverSpec draws one hover group's spec over base.
func (t themeResolver) hoverSpec(base uv.Style, spec hostapi.HLSpec) uv.Style {
	if spec.Replace {
		base = uv.Style{}
	}
	st := drawOver(base, spec, hostapi.Whorl)
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
func drawOver(st uv.Style, spec hostapi.HLSpec, pal *hostapi.Palette) uv.Style {
	set := func(dst *color.Color, ref string) {
		switch {
		case ref == "NONE":
			*dst = nil
		case ref != "":
			if c := pal.Color(ref); c != nil {
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
	}{{on: spec.Bold, attr: uv.AttrBold}, {on: spec.Italic, attr: uv.AttrItalic}, {on: spec.Reverse, attr: uv.AttrReverse}} {
		switch {
		case a.on == nil:
		case *a.on:
			st.Attrs |= a.attr
		default:
			st.Attrs &^= a.attr
		}
	}
	if u, ok := _underlines[spec.Underline]; ok {
		st.Underline = u
	}
	if spec.Shade != nil {
		st.Fg = _knot()[min(31, max(0, int(math.Round(*spec.Shade*31))))]
	}
	return st
}

//exhaustive:enforce
var _underlines = map[hostapi.Underline]uv.Underline{
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

// sgrStyle decodes inline SGR params (Host.Markdown's output), cached
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

// _xterm is the OkLab color of each of the 240 fixed 256-color entries.
var _xterm = func() (t [256][3]float64) {
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
		e := _xterm[i]
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

// _greys16 are the 16-color greys, dark to light.
var _greys16 = [...]ansi.BasicColor{ansi.Black, ansi.BrightBlack, ansi.White, ansi.BrightWhite}

// grey16 is the 16-color grey for an OKLCH lightness.
func grey16(l float64) ansi.BasicColor {
	switch {
	case l < 0.3:
		return ansi.Black
	case l < 0.72:
		return ansi.BrightBlack
	case l < 0.93:
		return ansi.White
	default:
		return ansi.BrightWhite
	}
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

// toProfile is what a terminal of p shows for a style: the 256- and 16-color
// downgrades, the 16-color one keeping a foreground apart from the background
// it would vanish on.
func toProfile(st uv.Style, p colorprofile.Profile) uv.Style {
	switch p {
	case colorprofile.ANSI256:
		st.Fg, st.Bg = ansi256(st.Fg), ansi256(st.Bg)
	case colorprofile.ANSI:
		fg, bg := ansi16(st.Fg), surface16(st.Bg)
		// a fg on its own color would vanish: dim on a band both bright
		// black, or red on a group's red fill, since a hue and its bright
		// twin read as one
		f, ok1 := fg.(ansi.BasicColor)
		b, ok2 := bg.(ansi.BasicColor)
		if ok1 && ok2 && (f == b || f%8 == b%8 && f%8 != ansi.Black && f%8 != ansi.White) {
			fg = apart(st.Fg, st.Bg, b)
		}
		st.Fg, st.Bg = fg, bg
	default: // truecolor draws as given, and bubbletea drops color where none shows
	}
	return st
}

// apart is the grey next to bg16 on the side fg sits of bg, for a
// foreground that would vanish on it; a hue bg stands in for the grey
// it is near.
func apart(fg, bg color.Color, bg16 ansi.BasicColor) ansi.BasicColor {
	i := slices.Index(_greys16[:], bg16)
	if i < 0 {
		i = 1 + int(bg16/8)
	}
	if luma(fg) > luma(bg) && i < len(_greys16)-1 || i == 0 {
		return _greys16[i+1]
	}
	return _greys16[i-1]
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
	}{{upTo: 40, c: ansi.Red}, {upTo: 115, c: ansi.Yellow}, {upTo: 175, c: ansi.Green}, {upTo: 235, c: ansi.Cyan}, {upTo: 300, c: ansi.Blue}, {upTo: 345, c: ansi.Magenta}, {upTo: 361, c: ansi.Red}} {
		if h < r.upTo {
			if l > 0.65 {
				return r.c + 8
			}
			return r.c
		}
	}
	return c
}
