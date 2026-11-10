package hostapi

import (
	"cmp"
	"fmt"
	"sync"
)

// The highlight groups every screen draws with, and the one palette they
// draw in. Nothing registers them: a group names a palette role (or a hex or
// an ANSI color) and the palette is one line of colors, so the screen's whole
// styling is these two tables (DESIGN §6.6).
var _shippedGroups = withKnots(map[string]HLSpec{
	// the canvas is the palette's bg, which every group without a bg of its
	// own sits on; a bg here colors Normal text alone
	"Normal": {Fg: "fg", Bg: "bg"},
	"Accent": {Fg: "accent"},
	"Dim":    {Fg: "dim"},
	"Rule":   {Fg: "rule"},
	"Title":  {Fg: "accent", Bold: new(true)},

	"Literal":   {Fg: "literal"},
	"Danger":    {Fg: "danger"},
	"Warn":      {Fg: "warn"},
	"Ok":        {Fg: "ok"},
	"Snail":     {Link: "Accent"},
	"UserMsg":   {Link: "Accent"},
	"ToastInfo": {Link: "Title"},
	"ToastWarn": {Link: "Warn", Bold: new(true)},
	"ToastErr":  {Link: "Danger", Bold: new(true)},

	"MarkdownHeading": {Link: "Title"},
	// an inline span reads by its color: a filled background hangs off the
	// text as a stray block where it wraps
	"MarkdownCode":  {Link: "Literal"},
	"MarkdownLink":  {Link: "Accent", Underline: UnderlineSingle},
	"MarkdownQuote": {Fg: "dim", Italic: new(true)},
	"MarkdownTLDR":  {Link: "MarkdownQuote", Bold: new(true)},

	"AgentReason":   {Link: "Dim"},
	"ThinkingMark":  {Link: "Accent"},
	"ThinkingBar":   {Link: "Rule"},
	"AgentToolName": {Fg: "fg", Bold: new(true)},

	// a diff marks the sign and the line number and tints the row no further
	// than it has to
	"DiffAdd":    {Fg: "fg", Bg: "addbg"},
	"DiffAddNum": {Fg: "ok", Bg: "addbg"},
	"DiffDel":    {Fg: "fg", Bg: "delbg"},
	"DiffDelNum": {Fg: "danger", Bg: "delbg"},
	"DiffCtx":    {Link: "Normal"},
	"DiffCtxNum": {Link: "Dim"},

	"TodoTitle":  {Link: "Title"},
	"TodoMark":   {Link: "Accent"},
	"TodoDone":   {Link: "Ok"},
	"TodoActive": {Fg: "fg", Bold: new(true)},

	// the shell echo: what follows went to a shell, not to the model
	"PromptShell": {Link: "Warn", Bold: new(true)},
	"Queued":      {Link: "Dim"},
	"SplashKey":   {Link: "Accent"},

	"ScrollbarTrack": {Link: "Rule"},
	"ScrollbarThumb": {Link: "Accent"},

	// a scrolled region's pinned header (a prompt over its turn): chrome, so
	// grey over a rule the width of the view, never read as a message
	"Pinned": {Fg: "dim"},

	// hover draws over the text's own style: keys left out keep it. A row
	// header takes the accent and nothing else; body rows opt out entirely
	// (NoHover) and say a click acts with the pointer alone. A list row
	// underlines, so it never takes the selection's or a match's accent.
	"Hover":     {Link: "Accent"},
	"HoverItem": {Underline: UnderlineSingle},
})

// withKnots adds the knot's ramp: a shade draws the whorl knot's curve through
// the rule, accent and literal.
func withKnots(groups map[string]HLSpec) map[string]HLSpec {
	for i := range _knotStops {
		shade := float64(i) / float64(_knotStops-1)
		groups[fmt.Sprintf("Knot%d", i)] = HLSpec{Shade: &shade}
	}
	return groups
}

// _knotStops is the number of stops in the knot's shade ramp.
const _knotStops = 32

// _whorlSpec is zemi's one palette: slate greys, a periwinkle accent and an
// apricot literal, with red, amber and mint for state. warn sits lighter than
// the others: deutan folds amber and apricot into one yellow at the same
// lightness, so the two split by lightness instead.
var _whorlSpec = map[string]any{
	"bg": "#17181e", "fg": "#e7e7ec", "dim": "#898b98", "rule": "#4b4c58",
	"panel": "#202128", "accent": "#a2adfd", "literal": "#fcb67c",
	"ok": "#68d7a1", "warn": "#fde59c", "danger": "#f17070",
}

// Whorl is the palette the screen draws in, derived once at first use: a bad
// literal is a bug in this file, so it panics instead of booting a broken
// screen.
var Whorl = sync.OnceValue(func() *Palette {
	p, err := parsePalette(_whorlSpec)
	if err != nil {
		panic(fmt.Sprintf("the whorl palette: %v", err))
	}
	return &p
})()

// _maxHLLinks bounds a chain of linked groups, so a cycle resolves to nothing.
const _maxHLLinks = 8

// Group resolves a highlight group, following links. The first link's bold,
// italic and underline apply over the group it draws as.
func Group(group string) (HLSpec, bool) {
	var over HLSpec
	for i := range _maxHLLinks {
		s, ok := _shippedGroups[group]
		if !ok || s.Link == "" {
			s.Bold, s.Italic = cmp.Or(over.Bold, s.Bold), cmp.Or(over.Italic, s.Italic)
			s.Underline = cmp.Or(over.Underline, s.Underline)
			return s, ok
		}
		if i == 0 {
			over = s
		}
		group = s.Link
	}
	return HLSpec{}, false
}

// HLSpec is one highlight group definition. Unset fields matter to the hover
// groups, which draw over the text's own style: nil attributes and empty
// colors keep what is under them.
type HLSpec struct {
	Link         string // another group this one draws as; of the other fields only Bold and Italic apply
	Fg, Bg, Sp   string // hex, a palette name, or "NONE" (no color, clearing one under a hover)
	Bold, Italic *bool
	Reverse      *bool
	Underline    Underline
	Shade        *float64 // a stop of the knot's ramp (KnotRamp), 0 to 1; takes the place of fg
	Brighten     float64  // hover groups: mix the drawn fg this far toward white, negative toward black
	Replace      bool     // hover groups: draw as this group alone, not over the text's style
}

// Underline is a highlight group's underline style; "" keeps what is under it
// and UnderlineNone removes one.
type Underline string

const (
	UnderlineNone   Underline = "none"
	UnderlineSingle Underline = "single"
	UnderlineDouble Underline = "double"
	UnderlineCurl   Underline = "curl"
	UnderlineDotted Underline = "dotted"
	UnderlineDashed Underline = "dashed"
)
