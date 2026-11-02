package hostapi

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
)

// The highlight groups every theme draws with, and the shipped themes.
// Nothing registers them: a group names a palette role (or a hex or an ANSI
// color) and a theme is one line of colors, so the screen's whole styling is
// these two tables (DESIGN §6.6).
var shippedGroups = map[string]HLSpec{
	// the canvas is the palette's bg, which every group without a bg of its
	// own sits on; a bg here colors Normal text alone
	"Normal": {Fg: "fg", Bg: "bg"},
	"Accent": {Fg: "accent"},
	"Dim":    {Fg: "dim"},
	"Rule":   {Fg: "rule"},
	"Title":  {Fg: "accent", Bold: new(true)},

	"RuleCommand": {Link: "Accent"},
	"Literal":     {Fg: "literal"},
	"Danger":      {Fg: "danger"},
	"Warn":        {Fg: "warn"},
	"Ok":          {Fg: "ok"},
	"Snail":       {Link: "Accent"},
	"UserMsg":     {Link: "Accent"},
	"ToastInfo":   {Link: "Title"},
	"ToastWarn":   {Link: "Warn", Bold: new(true)},
	"ToastErr":    {Link: "Danger", Bold: new(true)},

	"MarkdownHeading": {Link: "Title"},
	// an inline span reads by its color: a filled background hangs off the
	// text as a stray block where it wraps
	"MarkdownCode":  {Link: "Literal"},
	"MarkdownLink":  {Link: "Accent", Underline: UnderlineSingle},
	"MarkdownQuote": {Fg: "dim", Italic: new(true)},

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

	// the shell glyph: what follows goes to the shell, not to the model
	"PromptShell":   {Link: "Warn", Bold: new(true)},
	"PromptCommand": {Link: "Title"},
	"Queued":        {Link: "Dim"},
	"SplashKey":     {Link: "Accent"},

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
}

func init() {
	// the knot's ramp: a shade draws the whorl knot's curve through the
	// theme's rule, accent and literal
	for i := range knotStops {
		shade := float64(i) / float64(knotStops-1)
		shippedGroups[fmt.Sprintf("Knot%d", i)] = HLSpec{Shade: &shade}
	}
}

// knotStops is the number of stops in the knot's shade ramp.
const knotStops = 32

// The shipped themes, by the name ui.theme takes. whorl and whorl-light are
// hand-picked; the rest are ported from the Ghostty scheme each names (those
// are MIT): the terminal's bg, fg and 16 colors, everything else derived.
var themeSpecs = map[string]map[string]any{
	// zemi's own: slate greys, a periwinkle accent and an apricot literal,
	// with red, amber and mint for state. warn sits lighter than the others:
	// deutan folds amber and apricot into one yellow at the same lightness, so
	// the two split by lightness instead.
	"whorl": {
		"bg": "#17181e", "fg": "#e7e7ec", "dim": "#898b98", "rule": "#4b4c58",
		"panel": "#202128", "accent": "#a2adfd", "literal": "#fcb67c",
		"ok": "#68d7a1", "warn": "#fde59c", "danger": "#f17070",
	},
	// picked automatically when the terminal reports a light background. A
	// white canvas leaves the five text roles one narrow lightness band, so
	// the state trio counts on its marks and literal sits darkest to stay
	// apart from warn under deuteranopia and at 256 colors.
	// NOTE: under the 4.5 floor a darker amber reads as danger to
	// deuteranopes, so warn keeps the 3.5 floor and lands at 4.8:1 on bg.
	"whorl-light": {
		"bg": "#f9faff", "fg": "#202127", "dim": "#696b78", "rule": "#c1c3d3",
		"panel": "#eef0f6", "accent": "#4f44e7", "literal": "#6d3800",
		"ok": "#006842", "warn": "#927100", "danger": "#c72d2e",
	},

	// each port on one line, the 16 slots in order
	"catppuccin":       ansiTheme("#1e1e2e", "#cdd6f4", [16]string{"#45475a", "#f38ba8", "#a6e3a1", "#f9e2af", "#89b4fa", "#f5c2e7", "#94e2d5", "#a6adc8", "#585b70", "#f37799", "#89d88b", "#ebd391", "#74a8fc", "#f2aede", "#6bd7ca", "#bac2de"}),
	"catppuccin-light": ansiTheme("#eff1f5", "#4c4f69", [16]string{"#5c5f77", "#d20f39", "#40a02b", "#df8e1d", "#1e66f5", "#ea76cb", "#179299", "#acb0be", "#6c6f85", "#de293e", "#49af3d", "#eea02d", "#456eff", "#fe85d8", "#2d9fa8", "#bcc0cc"}),
	"tokyonight":       ansiTheme("#1a1b26", "#c0caf5", [16]string{"#15161e", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff", "#a9b1d6", "#414868", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff", "#c0caf5"}),
	"tokyonight-light": ansiTheme("#e1e2e7", "#3760bf", [16]string{"#e9e9ed", "#f52a65", "#587539", "#8c6c3e", "#2e7de9", "#9854f1", "#007197", "#6172b0", "#a1a6c5", "#f52a65", "#587539", "#8c6c3e", "#2e7de9", "#9854f1", "#007197", "#3760bf"}),
	"gruvbox":          ansiTheme("#282828", "#ebdbb2", [16]string{"#282828", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#a89984", "#928374", "#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c", "#ebdbb2"}),
	"gruvbox-light":    ansiTheme("#fbf1c7", "#3c3836", [16]string{"#fbf1c7", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#7c6f64", "#928374", "#9d0006", "#79740e", "#b57614", "#076678", "#8f3f71", "#427b58", "#3c3836"}),
	"nord":             ansiTheme("#2e3440", "#d8dee9", [16]string{"#3b4252", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#88c0d0", "#e5e9f0", "#596377", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb", "#eceff4"}),
	"dracula":          ansiTheme("#282a36", "#f8f8f2", [16]string{"#21222c", "#ff5555", "#50fa7b", "#f1fa8c", "#bd93f9", "#ff79c6", "#8be9fd", "#f8f8f2", "#6272a4", "#ff6e6e", "#69ff94", "#ffffa5", "#d6acff", "#ff92df", "#a4ffff", "#ffffff"}),
	"solarized":        ansiTheme("#002b36", "#839496", [16]string{"#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5", "#335e69", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3"}),
	"solarized-light":  ansiTheme("#fdf6e3", "#657b83", [16]string{"#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#bbb5a2", "#002b36", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3"}),
	"rose-pine":        ansiTheme("#191724", "#e0def4", [16]string{"#26233a", "#eb6f92", "#31748f", "#f6c177", "#9ccfd8", "#c4a7e7", "#ebbcba", "#e0def4", "#6e6a86", "#eb6f92", "#31748f", "#f6c177", "#9ccfd8", "#c4a7e7", "#ebbcba", "#e0def4"}),
	"rose-pine-light":  ansiTheme("#faf4ed", "#575279", [16]string{"#f2e9e1", "#b4637a", "#286983", "#ea9d34", "#56949f", "#907aa9", "#d7827e", "#575279", "#9893a5", "#b4637a", "#286983", "#ea9d34", "#56949f", "#907aa9", "#d7827e", "#575279"}),
	"kanagawa":         ansiTheme("#1f1f28", "#dcd7ba", [16]string{"#090618", "#c34043", "#76946a", "#c0a36e", "#7e9cd8", "#957fb8", "#6a9589", "#c8c093", "#727169", "#e82424", "#98bb6c", "#e6c384", "#7fb4ca", "#938aa9", "#7aa89f", "#dcd7ba"}),
	"kanagawa-light":   ansiTheme("#f2ecbc", "#545464", [16]string{"#1f1f28", "#c84053", "#6f894e", "#77713f", "#4d699b", "#b35b79", "#597b75", "#545464", "#8a8980", "#d7474b", "#6e915f", "#836f4a", "#6693bf", "#624c83", "#5e857a", "#43436c"}),
	"one":              ansiTheme("#21252b", "#abb2bf", [16]string{"#21252b", "#e06c75", "#98c379", "#e5c07b", "#61afef", "#c678dd", "#56b6c2", "#abb2bf", "#767676", "#e06c75", "#98c379", "#e5c07b", "#61afef", "#c678dd", "#56b6c2", "#abb2bf"}),
	"one-light":        ansiTheme("#f9f9f9", "#2a2c33", [16]string{"#000000", "#de3e35", "#3f953a", "#d2b67c", "#2f5af3", "#950095", "#3f953a", "#bbbbbb", "#000000", "#de3e35", "#3f953a", "#d2b67c", "#2f5af3", "#a00095", "#3f953a", "#ffffff"}),
	"everforest":       ansiTheme("#1e2326", "#d3c6aa", [16]string{"#7a8478", "#e67e80", "#a7c080", "#dbbc7f", "#7fbbb3", "#d699b6", "#83c092", "#f2efdf", "#a6b0a0", "#f85552", "#8da101", "#dfa000", "#3a94c5", "#df69ba", "#35a77c", "#fffbef"}),
	"github":           ansiTheme("#0d1117", "#e6edf3", [16]string{"#484f58", "#ff7b72", "#3fb950", "#d29922", "#58a6ff", "#bc8cff", "#39c5cf", "#b1bac4", "#6e7681", "#ffa198", "#56d364", "#e3b341", "#79c0ff", "#d2a8ff", "#56d4dd", "#ffffff"}),
	"github-light":     ansiTheme("#ffffff", "#1f2328", [16]string{"#24292f", "#cf222e", "#116329", "#4d2d00", "#0969da", "#8250df", "#1b7c83", "#6e7781", "#57606a", "#a40e26", "#1a7f37", "#633c01", "#218bff", "#a475f9", "#3192aa", "#8c959f"}),
}

// ansiTheme is a ported Ghostty scheme: bg, fg and the 16 terminal colors.
func ansiTheme(bg, fg string, ansi [16]string) map[string]any {
	slots := make([]any, len(ansi))
	for i, c := range ansi {
		slots[i] = c
	}
	return map[string]any{"bg": bg, "fg": fg, "ansi": slots}
}

// shippedThemeMap derives every palette once: a bad literal is a bug in this
// file, so it fails the tests instead of a boot.
var shippedThemeMap = sync.OnceValue(func() map[string]*Theme {
	out := make(map[string]*Theme, len(themeSpecs))
	for name, spec := range themeSpecs {
		p, err := parsePalette(spec)
		if err != nil {
			panic(fmt.Sprintf("shipped theme %q: %v", name, err))
		}
		out[name] = &Theme{Name: name, Palette: p}
	}
	return out
})

// ThemeByName is the shipped theme called name, nil when none is.
func ThemeByName(name string) *Theme { return shippedThemeMap()[name] }

// ThemeList is every shipped theme, sorted by name.
func ThemeList() []*Theme {
	themes := shippedThemeMap()
	names := make([]string, 0, len(themes))
	for name := range themes {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]*Theme, len(names))
	for i, name := range names {
		out[i] = themes[name]
	}
	return out
}

// maxHLLinks bounds a chain of linked groups, so a cycle resolves to nothing.
const maxHLLinks = 8

// Group resolves a highlight group, following links. The first link's bold,
// italic and underline apply over the group it draws as.
func Group(group string) (HLSpec, bool) {
	var over HLSpec
	for i := range maxHLLinks {
		s, ok := shippedGroups[group]
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

// HLSpec is one highlight group definition from a theme. Unset fields matter
// to the hover groups, which draw over the text's own style: nil attributes
// and empty colors keep what is under them.
type HLSpec struct {
	Link         string // another group this one draws as; of the other fields only Bold and Italic apply
	Fg, Bg, Sp   string // hex, a palette name, or "NONE" (no color, clearing one under a hover)
	Bold, Italic *bool
	Reverse      *bool
	Underline    Underline
	Shade        *float64 // a stop of the theme's knot ramp (KnotRamp), 0 to 1; takes the place of fg
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
