package hostapi

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/sanitize"
)

// SGRPrefix marks a segment hl carrying raw SGR parameters instead of a
// group name, so glamour's styling survives the round trip through Lua.
// The UI decodes it through an SGR parser: only colors and attributes come
// out, never other escapes.
const SGRPrefix = "sgr:"

// NOTE: rendered lines use a two-generation cache: entries used in the
// last generation survive a rotation, so a transcript re-rendered every
// frame keeps its working set. A transcript with more than mdLinesMax
// messages re-runs glamour for the overflow on every render; a per-message
// cache keyed by block id is the upgrade.
const (
	mdLinesMax     = 4096
	mdBytesMax     = 8 << 20
	mdRenderersMax = 16
)

// markdownCache memoizes glamour renderers per (style, width) and rendered
// lines per (style, width, text), so re-rendering the transcript every
// frame never re-runs glamour for settled messages. The mutex matters: a
// wedged interpreter from an old generation may still be inside Markdown
// while the fresh one renders.
type markdownCache struct {
	mu        sync.Mutex
	renderers map[string]*glamour.TermRenderer
	lines     map[string][][]Segment
	prev      map[string][][]Segment
	bytes     int // roughly what lines holds: a streaming reply keyed by its
	// whole text would otherwise retain every version of it
}

func (c *markdownCache) put(k string, v [][]Segment) {
	if c.lines == nil || len(c.lines) >= mdLinesMax || c.bytes >= mdBytesMax {
		c.prev, c.lines, c.bytes = c.lines, map[string][][]Segment{}, 0
	}
	c.lines[k] = v
	c.bytes += len(k)
	for _, line := range v {
		for _, s := range line {
			c.bytes += len(s.Text) + len(s.HL) + 32
		}
	}
}

func (c *markdownCache) get(k string) ([][]Segment, bool) {
	if v, ok := c.lines[k]; ok {
		return v, true
	}
	v, ok := c.prev[k]
	if ok {
		c.put(k, v)
	}
	return v, ok
}

// Markdown renders text as styled segment lines at most width cells wide
// (zemi.ui.markdown, DESIGN §8). Input is sanitized before glamour sees it.
func (h *Host) Markdown(text string, width int) [][]Segment {
	width = max(width, 1)
	c := &h.md
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg, styleKey := h.markdownStyle() // registers a chroma style: under the lock renders read them
	// A streaming reply is re-rendered on every delta: render its settled
	// prefix from the cache and only the tail anew. The two halves meet at
	// a blank line, which is where glamour would put one anyway.
	if cut := settledPrefix(text); cut > 0 {
		lines := append([][]Segment{}, c.render(&cfg, styleKey, text[:cut], width)...)
		lines = append(lines, []Segment{})
		return append(lines, c.render(&cfg, styleKey, text[cut:], width)...)
	}
	return c.render(&cfg, styleKey, text, width)
}

// settledPrefix is the length of the part of a reply that will not change
// as more deltas arrive: up to the last blank line outside a code fence.
// 0 renders the whole text at once, which is cheap enough for short ones.
func settledPrefix(s string) int {
	if len(s) < 2000 {
		return 0
	}
	fence, cut, at := false, 0, 0
	for at < len(s) {
		end := len(s)
		if i := strings.IndexByte(s[at:], '\n'); i >= 0 {
			end = at + i + 1
		}
		trimmed := strings.TrimSpace(s[at:end])
		if strings.HasPrefix(trimmed, "```") {
			fence = !fence
		}
		at = end
		if !fence && trimmed == "" {
			cut = at
		}
	}
	if cut >= len(s) {
		return 0 // nothing left to render as the tail
	}
	return cut
}

// render returns the lines of one chunk wrapped to width, from the cache
// when it holds them. The cache mutex must be held.
func (c *markdownCache) render(cfg *gansi.StyleConfig, styleKey, text string, width int) [][]Segment {
	lk := fmt.Sprintf("%s|%d|%s", styleKey, width, text)
	if lines, ok := c.get(lk); ok {
		return lines
	}
	// a tab occupies no cell once drawn into the buffer, so code would lose
	// its indentation; a marker may only ever come from markDecorations
	text = escapeHTML(sanitize.String(strings.NewReplacer("\t", "    ", markOpen, "", markClose, "").Replace(text)))
	var lines [][]Segment
	for i, chunk := range splitTables(text) {
		var rows [][]Segment
		if chunk.table {
			// A table is laid out, not wrapped like prose: at width 0 glamour
			// sizes it to its content, and only a table too wide for the room
			// is laid out to the width, whose columns it stretches to fill.
			rows = c.glamour(*cfg, styleKey, chunk.text, 0)
			for _, row := range rows {
				if SegsWidth(row) > width {
					rows = c.glamour(*cfg, styleKey, chunk.text, width)
					break
				}
			}
			// glamour draws the grid unstyled
			for _, row := range rows {
				for i, s := range row {
					if !strings.Contains(s.HL, "38;") && strings.ContainsAny(s.Text, "│┼─") && strings.Trim(s.Text, " │┼─") == "" {
						row[i].HL = "Rule"
					}
				}
			}
		} else {
			for _, row := range c.glamour(*cfg, styleKey, chunk.text, 0) {
				wrapped := WrapSegs(row, width)
				// a quote keeps its bar on every row it wraps to, but not the
				// bullet or number of a list inside it
				if len(row) > 0 && row[0].NoCopy && strings.Contains(row[0].Text, "│") {
					bar := row[0].Text[:min(strings.LastIndex(row[0].Text, "│")+len("│ "), len(row[0].Text))]
					w := SegsWidth(row[:1])
					for _, r := range wrapped[1:] {
						if r[0].NoCopy && SegsWidth(r[:1]) == w {
							r[0] = row[0]
							r[0].Text = bar + strings.Repeat(" ", w-ansi.StringWidth(bar))
						}
					}
				}
				rows = append(rows, wrapped...)
			}
		}
		for len(rows) > 0 && len(rows[0]) == 0 {
			rows = rows[1:]
		}
		if i > 0 && len(rows) > 0 && len(lines) > 0 {
			lines = append(lines, []Segment{})
		}
		lines = append(lines, rows...)
	}
	// glamour puts two blank rows between some blocks (a nested list and
	// the list after it): every block gets one
	out := lines[:0]
	for _, l := range lines {
		if len(l) == 0 && len(out) > 0 && !slices.ContainsFunc(out[len(out)-1], func(s Segment) bool { return strings.TrimSpace(s.Text) != "" }) {
			continue
		}
		out = append(out, l)
	}
	c.put(lk, out)
	return out
}

// glamour renders text with the renderer for (key, width); width 0 does not
// wrap, so zemi wraps the prose itself and marks where (Segment.Soft).
func (c *markdownCache) glamour(cfg gansi.StyleConfig, key, text string, width int) [][]Segment {
	rk := fmt.Sprintf("%s|%d", key, width)
	r := c.renderers[rk]
	if r == nil {
		// a line break the author typed is kept: a reply copies back as it was written
		opts := []glamour.TermRendererOption{glamour.WithWordWrap(width), glamour.WithPreservedNewLines(), glamour.WithChromaFormatter(chromaMarked)}
		if width == 0 {
			markDecorations(&cfg)
		}
		var err error
		r, err = glamour.NewTermRenderer(append(opts, glamour.WithStyles(cfg))...)
		if err != nil {
			return plainLines(text)
		}
		if c.renderers == nil || len(c.renderers) >= mdRenderersMax {
			c.renderers = map[string]*glamour.TermRenderer{}
		}
		c.renderers[rk] = r
	}
	out, err := r.Render(text)
	if err != nil {
		return plainLines(text)
	}
	return styledLines(out)
}

// A table is the header row, its delimiter row and every row up to a blank
// line, outside code fences.
// NOTE: a table inside a list or quote renders as prose and wraps like it.
var tableDelim = regexp.MustCompile(`^ {0,3}\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)

type mdChunk struct {
	text  string
	table bool
}

// splitTables cuts text into prose and table chunks.
func splitTables(text string) []mdChunk {
	lines := strings.SplitAfter(text, "\n")
	var out []mdChunk
	fence, from := false, 0
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			fence = !fence
		}
		if fence || i+1 >= len(lines) || !strings.Contains(lines[i], "|") ||
			!strings.Contains(lines[i+1], "|") || !tableDelim.MatchString(strings.TrimRight(lines[i+1], "\n")) {
			continue
		}
		end := i + 2
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
			end++
		}
		if i > from {
			out = append(out, mdChunk{text: strings.Join(lines[from:i], "")})
		}
		out = append(out, mdChunk{text: strings.Join(lines[i:end], ""), table: true})
		from, i = end, end-1
	}
	if from < len(lines) {
		out = append(out, mdChunk{text: strings.Join(lines[from:], "")})
	}
	return out
}

// Decorations glamour draws (bullets, list numbers, task marks, the quote
// bar) are bracketed by private-use runes that styledLines strips, marking
// the text between them NoCopy and Dim. A close with no open ends a run
// from the line start or the previous close: glamour writes a list number
// and a code block's margin before any hook can open one.
const (
	markOpen     = "\ue000"
	markClose    = "\ue001"
	chromaMarked = "zemi-marked"
)

func markDecorations(cfg *gansi.StyleConfig) {
	nc := func(s string) string { return markOpen + s + markClose }
	cfg.BlockQuote.IndentToken = new(nc("│ "))
	cfg.List.IndentToken = new(nc(" "))
	cfg.Item.BlockPrefix = nc(cfg.Item.BlockPrefix)
	cfg.Enumeration.BlockPrefix += markClose
	cfg.Task.Ticked, cfg.Task.Unticked = nc(cfg.Task.Ticked), nc(cfg.Task.Unticked)
}

// glamour writes a code block's margin itself, so the formatter opens every
// code line with a close marker: what stands before it on the row is margin.
func init() {
	tty := formatters.Get("terminal16m") // the theme's own colors: the UI downsamples
	formatters.Register(chromaMarked, chroma.FormatterFunc(func(w io.Writer, style *chroma.Style, it chroma.Iterator) error {
		return tty.Format(&lineMarker{w: w, start: true}, style, it)
	}))
}

// lineMarker writes markClose at the start of every line, a blank one too.
type lineMarker struct {
	w     io.Writer
	start bool
}

func (m *lineMarker) Write(p []byte) (int, error) {
	var b bytes.Buffer
	for _, c := range p {
		if m.start {
			b.WriteString(markClose)
		}
		m.start = c == '\n'
		b.WriteByte(c)
	}
	if _, err := m.w.Write(b.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}

// markdownStyle builds glamour's style from the active theme's groups and
// returns a key that changes whenever those groups do. Nothing is left to
// glamour's stock palette: a block the theme does not color draws in Normal.
func (h *Host) markdownStyle() (gansi.StyleConfig, string) {
	margin, zero := uint(2), uint(0)
	cfg := gansi.StyleConfig{
		Document:      gansi.StyleBlock{Margin: &zero},
		BlockQuote:    gansi.StyleBlock{Indent: new(uint(1)), IndentToken: new("│ ")},
		List:          gansi.StyleList{LevelIndent: 2},
		Heading:       gansi.StyleBlock{StylePrimitive: gansi.StylePrimitive{BlockSuffix: "\n", Bold: new(true)}},
		Strikethrough: gansi.StylePrimitive{CrossedOut: new(true)},
		Emph:          gansi.StylePrimitive{Italic: new(true)},
		Strong:        gansi.StylePrimitive{Bold: new(true)},
		// the rows around it are glamour's, the line is ours
		HorizontalRule: gansi.StylePrimitive{Format: "\n────────\n"},
		Item:           gansi.StylePrimitive{BlockPrefix: "• "},
		Enumeration:    gansi.StylePrimitive{BlockPrefix: ". "},
		Task:           gansi.StyleTask{Ticked: "✓ ", Unticked: "○ "},
		ImageText:      gansi.StylePrimitive{Format: "Image: {{.text}}"},
		CodeBlock:      gansi.StyleCodeBlock{StyleBlock: gansi.StyleBlock{Margin: &margin}},
	}
	theme := h.Themes.Get(h.ActiveThemeName())
	var key strings.Builder
	if theme != nil {
		key.WriteString(theme.Name)
	}
	apply := func(p *gansi.StylePrimitive, group string) {
		spec, ok := h.Themes.Group(theme, group)
		if !ok {
			return
		}
		fg, bg := theme.color(spec.Fg), theme.color(spec.Bg)
		bold, italic := spec.Bold != nil && *spec.Bold, spec.Italic != nil && *spec.Italic
		underline := spec.Underline != "" && spec.Underline != UnderlineNone
		fmt.Fprintf(&key, ";%s=%s/%s/%t%t%t", group, fg, bg, bold, italic, underline)
		if fg != "" {
			p.Color = &fg
		}
		if bg != "" {
			p.BackgroundColor = &bg
		}
		if bold {
			p.Bold = new(true)
		}
		if italic {
			p.Italic = new(true)
		}
		if underline {
			p.Underline = new(true)
		}
	}
	apply(&cfg.Document.StylePrimitive, "Normal")
	apply(&cfg.Heading.StylePrimitive, "MarkdownHeading")
	apply(&cfg.Code.StylePrimitive, "MarkdownCode")
	apply(&cfg.Link, "MarkdownLink")
	apply(&cfg.LinkText, "MarkdownLink")
	apply(&cfg.Image, "MarkdownLink")
	apply(&cfg.ImageText, "Dim")
	apply(&cfg.BlockQuote.StylePrimitive, "MarkdownQuote")
	apply(&cfg.HorizontalRule, "Rule")
	apply(&cfg.Item, "Dim")
	apply(&cfg.Enumeration, "Dim")

	// Code takes the groups through a chroma style of its own: glamour
	// registers its Chroma config once per process, so a theme switch would
	// keep the first theme's colors.
	var code [len(codeGroups)]gansi.StylePrimitive
	for i, cg := range codeGroups {
		apply(&code[i], cg.group)
	}
	cfg.CodeBlock.Theme = "zemi:" + key.String()
	// NOTE: every distinct theme key stays registered for the process; a
	// session that sets thousands of highlight overrides would grow it.
	if _, ok := styles.Registry[cfg.CodeBlock.Theme]; !ok {
		entries := chroma.StyleEntries{}
		for i, cg := range codeGroups {
			p := code[i]
			var e []string
			// chroma takes hex only: a group in the terminal's own colors
			// falls back to the text color
			if p.Color != nil && strings.HasPrefix(*p.Color, "#") {
				e = append(e, *p.Color)
			}
			// the canvas already paints Normal's background
			if p.BackgroundColor != nil && strings.HasPrefix(*p.BackgroundColor, "#") && cg.tok != chroma.Text {
				e = append(e, "bg:"+*p.BackgroundColor)
			}
			if cg.tok == chroma.Comment {
				e = append(e, "italic")
			}
			entries[cg.tok] = strings.Join(e, " ")
		}
		s, err := chroma.NewStyle(cfg.CodeBlock.Theme, entries)
		if err != nil {
			cfg.CodeBlock.Theme = ""
		} else {
			styles.Register(s)
		}
	}
	return cfg, key.String()
}

// codeGroups is how code draws: the chroma style markdownStyle registers
// for code blocks and the groups Highlight names both come from it.
var codeGroups = [...]struct {
	tok   chroma.TokenType
	group string
}{
	{chroma.Text, "Normal"},
	{chroma.Keyword, "Accent"},
	{chroma.Literal, "Literal"},
	{chroma.Comment, "Dim"},
	{chroma.Punctuation, "Dim"},
	{chroma.Operator, "Dim"},
	{chroma.GenericInserted, "DiffAdd"},
	{chroma.GenericDeleted, "DiffDel"},
}

// Highlight draws code in lang as one row per line in codeGroups' groups,
// each wrapped to width when it is over 0 (zemi.ui.highlight). A lang chroma
// has no lexer for draws plain; rows are cached with the markdown ones.
func (h *Host) Highlight(text, lang string, width int) [][]Segment {
	c := &h.md
	c.mu.Lock()
	defer c.mu.Unlock()
	k := fmt.Sprintf("\x00%s|%d|%s", lang, width, text)
	if rows, ok := c.get(k); ok {
		return rows
	}
	rows := highlight(text, lang)
	if width > 0 {
		var wrapped [][]Segment
		for _, r := range rows {
			wrapped = append(wrapped, WrapSegs(r, width)...)
		}
		rows = wrapped
	}
	c.put(k, rows)
	return rows
}

func highlight(text, lang string) [][]Segment {
	lexer := lexers.Get(lang)
	if lexer == nil {
		return plainLines(text)
	}
	text = sanitize.String(strings.ReplaceAll(text, "\t", "    "))
	it, err := lexer.Tokenise(nil, text)
	if err != nil {
		return plainLines(text)
	}
	rows := make([][]Segment, 1, strings.Count(text, "\n")+2)
	for tok := it(); tok != chroma.EOF; tok = it() {
		group := "Normal"
	find:
		for _, t := range []chroma.TokenType{tok.Type, tok.Type.SubCategory(), tok.Type.Category()} {
			for _, cg := range codeGroups {
				if cg.tok == t {
					group = cg.group
					break find
				}
			}
		}
		for i, part := range strings.Split(tok.Value, "\n") {
			if i > 0 {
				rows = append(rows, nil)
			}
			row := rows[len(rows)-1]
			if n := len(row); n > 0 && row[n-1].HL == group {
				row[n-1].Text += part
			} else if part != "" {
				row = append(row, Segment{Text: part, HL: group})
			}
			rows[len(rows)-1] = row
		}
	}
	// a lexer may end the text in a line break it did not have
	return rows[:min(len(rows), strings.Count(text, "\n")+1)]
}

// color resolves a color reference for glamour: raw hex, a palette name, or
// an ANSI color name as its number, which is how lipgloss takes one. "" when
// nothing resolves.
func (t *Theme) color(ref string) string {
	if strings.HasPrefix(ref, "#") {
		return ref
	}
	if c, ok := ANSIColors[ref]; ok {
		return strconv.Itoa(int(c))
	}
	if t == nil || !strings.HasPrefix(t.Palette[ref], "#") {
		return ""
	}
	return t.Palette[ref]
}

// styledLines parses glamour's ANSI output into segments: runs of equal
// style merge, decoration markers become NoCopy runs, and the unstyled
// padding glamour adds to every line and the trailing blank lines are
// dropped.
func styledLines(s string) [][]Segment {
	var out [][]Segment
	for _, l := range ansiLines(s) {
		end := len(l)
		for end > 0 && (l[end-1].Content == " " || l[end-1].Content == "") && l[end-1].Style.Bg == nil {
			end--
		}
		var cells []Segment
		open, closed := false, 0
		for _, c := range l[:end] {
			switch c.Content {
			case markOpen:
				open = true
				continue
			case markClose:
				for i := closed; !open && i < len(cells); i++ {
					cells[i].NoCopy, cells[i].HL = true, "Dim"
				}
				open, closed = false, len(cells)
				continue
			}
			hl := ""
			switch {
			case open:
				hl = "Dim"
			case !c.Style.IsZero():
				hl = SGRPrefix + strings.TrimSuffix(strings.TrimPrefix(c.Style.String(), "\x1b["), "m")
			}
			cells = append(cells, Segment{Text: c.Content, HL: hl, NoCopy: open})
		}
		row := []Segment{}
		for _, c := range cells {
			if n := len(row); n > 0 && row[n-1].HL == c.HL && row[n-1].NoCopy == c.NoCopy {
				row[n-1].Text += c.Text
				continue
			}
			row = append(row, c)
		}
		out = append(out, swatchColors(row))
	}
	// trailing rows with nothing to read: blank ones, and the margin
	// glamour leaves under a code block
	for len(out) > 0 && !slices.ContainsFunc(out[len(out)-1], func(s Segment) bool {
		return !s.NoCopy && strings.TrimSpace(s.Text) != ""
	}) {
		out = out[:len(out)-1]
	}
	return out
}

// A hex color, #rgb or #rrggbb. The short form needs a letter, or "#123"
// would paint every issue number.
var hexColor = regexp.MustCompile(`#(?:[0-9a-fA-F]{6}|[0-9a-fA-F]{3})\b`)

// swatchColors draws every hex color in a row on that color, in whichever
// of black and white reads on it.
func swatchColors(row []Segment) []Segment {
	var out []Segment
	for _, seg := range row {
		at := 0
		for _, m := range hexColor.FindAllStringIndex(seg.Text, -1) {
			hex := seg.Text[m[0]+1 : m[1]]
			prev, _ := utf8.DecodeLastRuneInString(seg.Text[:m[0]])
			if strings.ContainsRune("/&", prev) || // a URL fragment, an HTML entity
				unicode.IsLetter(prev) || unicode.IsDigit(prev) ||
				len(hex) == 3 && strings.Trim(hex, "0123456789") == "" {
				continue
			}
			if len(hex) == 3 {
				hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
			}
			n, _ := strconv.ParseUint(hex, 16, 32)
			r, g, b := n>>16, n>>8&0xff, n&0xff
			fg := "38;2;0;0;0"
			if 299*r+587*g+114*b < 128000 { // perceived brightness under half
				fg = "38;2;255;255;255"
			}
			// a space beside the color joins the swatch as its padding
			if m[0] > at && seg.Text[m[0]-1] == ' ' {
				m[0]--
			}
			if m[1] < len(seg.Text) && seg.Text[m[1]] == ' ' {
				m[1]++
			}
			pre, sw := seg, seg
			pre.Text, sw.Text = seg.Text[at:m[0]], seg.Text[m[0]:m[1]]
			sw.HL = fmt.Sprintf("%s%s;48;2;%d;%d;%d", SGRPrefix, fg, r, g, b)
			if pre.Text != "" {
				out = append(out, pre)
			}
			out = append(out, sw)
			at = m[1]
		}
		if at == 0 {
			out = append(out, seg)
		} else if at < len(seg.Text) {
			seg.Text = seg.Text[at:]
			out = append(out, seg)
		}
	}
	return out
}

// wrapText wraps plain text into rows of at most w cells, one logical line
// per line break, sanitized like markdown (zemi.ui.wrap).
func wrapText(text string, w int) [][]Segment {
	var rows [][]Segment
	for line := range strings.SplitSeq(text, "\n") {
		rows = append(rows, WrapSegs([]Segment{{Text: sanitize.String(line)}}, w)...)
	}
	return rows
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)`)

// DiffRows renders a unified diff as rows at most width cells wide
// (zemi.ui.diff): one number column (the old line on a removed row, the new
// one otherwise) and the sign in DiffAddNum, DiffDelNum or DiffCtxNum, the
// text wrapped in DiffAdd, DiffDel or DiffCtx with the tint out to the edge,
// so a diff line's rows are exactly width wide, "⋯" between hunks, and any
// other line (a file a command changed) in DiffCtxNum. Only the text copies.
func DiffRows(d string, width int) [][]Segment {
	type line struct {
		kind byte // '+', '-', ' ', '~' a hunk gap, 'f' a file
		num  int
		text string
	}
	var lines []line
	o, n, hunks := 0, 0, 0
	for l := range strings.SplitSeq(sanitize.String(d), "\n") {
		switch {
		case l == "", strings.HasPrefix(l, `\`):
		case strings.HasPrefix(l, "@@"):
			// a bare "@@" (a fragment's preview) numbers nothing: line 0 stays 0
			o, n = 0, 0
			if m := hunkHeader.FindStringSubmatch(l); m != nil {
				o, _ = strconv.Atoi(m[1])
				n, _ = strconv.Atoi(m[2])
			}
			if hunks > 0 {
				lines = append(lines, line{kind: '~'})
			}
			hunks++
		// after a hunk "---" is a removed "-- " line
		case hunks == 0 && (strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ")):
		case l[0] == '+':
			lines = append(lines, line{'+', n, l[1:]})
			n += min(n, 1)
		case l[0] == '-':
			lines = append(lines, line{'-', o, l[1:]})
			o += min(o, 1)
		case l[0] == ' ':
			lines = append(lines, line{' ', n, l[1:]})
			o, n = o+min(o, 1), n+min(n, 1)
		default:
			// each file's hunks count from one
			lines, hunks = append(lines, line{kind: 'f', text: l}), 0
		}
	}
	nw := len(strconv.Itoa(max(o, n)))
	var rows [][]Segment
	for _, l := range lines {
		switch l.kind {
		case '~':
			rows = append(rows, []Segment{{Text: strings.Repeat(" ", nw+2) + "⋯", HL: "DiffCtxNum", NoCopy: true}})
			continue
		case 'f':
			rows = append(rows, WrapSegs([]Segment{{Text: " " + l.text, HL: "DiffCtxNum"}}, width)...)
			continue
		}
		hl := map[byte]string{'+': "DiffAdd", '-': "DiffDel", ' ': "DiffCtx"}[l.kind]
		num := strings.Repeat(" ", nw)
		if l.num > 0 {
			num = fmt.Sprintf("%*d", nw, l.num)
		}
		gutter := " " + num + " " + string(l.kind) + " "
		room := max(width-len(gutter), 1)
		// wrapped, never cut: the user approves every character
		for i, r := range WrapSegs([]Segment{{Text: l.text, HL: hl}}, room) {
			if i == 1 {
				gutter = strings.Repeat(" ", len(gutter))
			}
			for j := range r {
				r[j].HL = hl // a hanging indent takes the tint too
			}
			row := append([]Segment{{Text: gutter, HL: hl + "Num", NoCopy: true}}, r...)
			rows = append(rows, append(row, Segment{Text: strings.Repeat(" ", max(room-SegsWidth(r), 0)), HL: hl, NoCopy: true}))
		}
	}
	return rows
}

// escapeHTML backslash-escapes a '<' that would open raw HTML outside code:
// glamour drops inline HTML, so "Vec<String>" in prose would lose "<String>".
// An autolink (<https://...>) stays a link. An indented code block is a line
// 4 columns past the list item it is in, after a blank line or another one.
// NOTE: tabs are not expanded, so a tab-indented code block is escaped.
func escapeHTML(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	var b strings.Builder
	fence, list, code, blank := "", 0, false, true
	for line := range strings.SplitAfterSeq(s, "\n") {
		body := strings.TrimLeft(line, " ")
		ind := len(line) - len(body)
		f := fenceRun(body)
		wasBlank := blank
		blank = strings.TrimSpace(line) == ""
		switch {
		case fence == "" && f != "":
			fence = f
		case fence != "" && strings.HasPrefix(f, fence):
			fence = ""
		case fence != "" || blank:
		case ind >= list+4 && (wasBlank || code):
			code = true
		default:
			code = false
			if m := listItem.FindString(line); m != "" {
				list = len(m)
			} else if ind == 0 {
				list = 0
			}
			line = escapeLine(line)
		}
		b.WriteString(line)
	}
	return b.String()
}

// listItem is a list marker line up to its content column.
var listItem = regexp.MustCompile(`^ *([-*+]|\d{1,9}[.)]) +`)

func escapeLine(line string) string {
	if !strings.Contains(line, "<") {
		return line
	}
	var b strings.Builder
	tick := 0 // the backtick run that opened a code span on this line
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '`' {
			n := len(line[i:]) - len(strings.TrimLeft(line[i:], "`"))
			if tick == 0 {
				tick = n
			} else if n == tick {
				tick = 0
			}
			b.WriteString(line[i : i+n])
			i += n - 1
			continue
		}
		if c == '<' && tick == 0 && i+1 < len(line) && tagStart(line[i+1]) &&
			(i == 0 || line[i-1] != '\\') && !autolink.MatchString(line[i:]) {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

var autolink = regexp.MustCompile(`^<[a-zA-Z][a-zA-Z0-9+.-]*:[^\s<>]*>`)

func tagStart(c byte) bool {
	return c == '/' || c == '!' || c == '?' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// fenceRun is the ``` or ~~~ run a code fence line starts with, or "".
func fenceRun(t string) string {
	for _, c := range []string{"`", "~"} {
		if run := t[:len(t)-len(strings.TrimLeft(t, c))]; len(run) >= 3 {
			return run
		}
	}
	return ""
}

// WrapSegs word-wraps one logical line into rows of at most w cells,
// keeping each piece's fields; words longer than w are hard-split. Every
// row but the last ends in a Soft segment, and continuation rows hang past
// the line's leading decoration (NoCopy) or a short gutter segment ("  - ")
// behind a NoCopy indent, so a copy gives back the line.
func WrapSegs(line []Segment, w int) [][]Segment {
	w = max(w, 1)
	// a tab measures and draws as no cell, so the words around it would
	// merge: expand before anything measures the line
	if slices.ContainsFunc(line, func(s Segment) bool { return strings.Contains(s.Text, "\t") }) {
		line = slices.Clone(line)
		for i, s := range line {
			line[i].Text = strings.ReplaceAll(s.Text, "\t", "    ")
		}
	}
	if SegsWidth(line) <= w {
		return [][]Segment{line}
	}
	indent := 0
	for _, s := range line {
		if !s.NoCopy {
			break
		}
		indent += ansi.StringWidth(s.Text)
	}
	// an unmarked gutter ("  - ") hangs too, but only when it is
	// punctuation: a word of the text itself must not become the indent
	if g := line[0].Text; indent == 0 && strings.HasSuffix(g, " ") && ansi.StringWidth(g) <= 8 &&
		strings.IndexFunc(g, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0 {
		indent = ansi.StringWidth(g)
	}
	if indent >= w/2 {
		indent = 0
	}
	var rows [][]Segment
	var row []Segment
	width := 0
	flush := func() {
		if n := len(row); n > 0 {
			row[n-1].Soft = true
		}
		rows = append(rows, row)
		row, width = nil, 0
		if indent > 0 {
			row, width = []Segment{{Text: strings.Repeat(" ", indent), NoCopy: true}}, indent
		}
	}
	// place appends a piece, merged into the row's last one when only
	// the text differs
	place := func(seg Segment) {
		if n := len(row); n > 0 {
			last, next := row[n-1], seg
			last.Text, next.Text = "", ""
			if last == next {
				row[n-1].Text += seg.Text
				return
			}
		}
		row = append(row, seg)
	}
	// add places text with seg's fields, splitting it over rows
	add := func(text string, seg Segment) {
		piece := func(t string) Segment { seg.Text = t; return seg }
		tw := ansi.StringWidth(text)
		for {
			if width >= w {
				flush()
			}
			if tw <= w-width {
				place(piece(text))
				width += tw
				return
			}
			cut := ansi.Truncate(text, w-width, "")
			if cut == "" {
				if width <= indent { // a wide rune that fits nowhere: place it anyway
					place(piece(text))
					width += tw
					return
				}
				flush()
				continue
			}
			whole := piece(cut)
			whole.Soft = false
			place(whole)
			flush()
			// the rest is split in one pass: taking a row off the front at a
			// time re-measured everything after it per row, so a single long
			// line cost seconds a frame
			rest := strings.Split(ansi.Hardwrap(strings.TrimPrefix(text, cut), max(w-indent, 1), true), "\n")
			for _, r := range rest[:len(rest)-1] {
				hard := piece(r)
				hard.Soft = false
				place(hard)
				flush()
			}
			text = rest[len(rest)-1]
			tw = ansi.StringWidth(text)
		}
	}
	for _, seg := range line {
		words := strings.SplitAfter(seg.Text, " ")
		soft := seg.Soft
		for i, word := range words {
			if word == "" {
				continue
			}
			if width+ansi.StringWidth(word) > w && width > indent {
				flush()
			}
			seg.Soft = soft && i == len(words)-1
			add(word, seg)
		}
	}
	if width > indent || len(rows) == 0 {
		rows = append(rows, row)
	} else if last := rows[len(rows)-1]; len(last) > 0 {
		last[len(last)-1].Soft = false
	}
	return rows
}

// SegsWidth is the printed width of a segment run.
func SegsWidth(segs []Segment) int {
	n := 0
	for _, s := range segs {
		n += ansi.StringWidth(s.Text)
	}
	return n
}

// SGRStyle decodes the params of an "sgr:" hl. They go through the SGR
// parser, so hostile params can only ever yield a style.
func SGRStyle(params string) uv.Style {
	return ansiLines("\x1b[" + params + "m ")[0][0].Style
}

// ansiLines parses styled text into cell lines by drawing it into a buffer
// sized to fit. StyledString.Lines is not used: with no bounds it stops
// after the first cell.
func ansiLines(s string) []uv.Line {
	rows := strings.Split(s, "\n")
	w := 1
	for _, r := range rows {
		w = max(w, ansi.StringWidth(r))
	}
	buf := uv.NewScreenBuffer(w, len(rows))
	buf.Method = ansi.GraphemeWidth // match ansi.StringWidth used for layout
	uv.NewStyledString(s).Draw(buf, buf.Bounds())
	out := make([]uv.Line, len(rows))
	for y := range rows {
		out[y] = buf.Line(y)
	}
	return out
}

func plainLines(text string) [][]Segment {
	var out [][]Segment
	for l := range strings.SplitSeq(sanitize.String(strings.ReplaceAll(text, "\t", "    ")), "\n") {
		out = append(out, []Segment{{Text: l}})
	}
	return out
}
