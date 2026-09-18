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
	"charm.land/glamour/v2/styles"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
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
	bytes     int // roughly what lines holds: a window panel keyed by its
	// whole body would otherwise retain every version of it
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

// Markdown renders text as styled segment lines at most width cells wide
// (zemi.ui.markdown, DESIGN §8). Input is sanitized before glamour sees it.
func (h *Host) Markdown(text string, width int) [][]Segment {
	width = max(width, 1)
	cfg, styleKey := h.markdownStyle()
	c := &h.md
	c.mu.Lock()
	defer c.mu.Unlock()
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
	if lines, ok := c.lines[lk]; ok {
		return lines
	}
	if lines, ok := c.prev[lk]; ok {
		c.put(lk, lines)
		return lines
	}
	// a tab occupies no cell once drawn into the buffer, so code would lose
	// its indentation; a marker may only ever come from markDecorations
	text = sanitize.String(strings.NewReplacer("\t", "    ", markOpen, "", markClose, "").Replace(text))
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
		} else {
			for _, row := range c.glamour(*cfg, styleKey, chunk.text, 0) {
				rows = append(rows, WrapSegs(row, width)...)
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
	c.put(lk, lines)
	return lines
}

// glamour renders text with the renderer for (key, width); width 0 does not
// wrap, so zemi wraps the prose itself and marks where (Segment.Soft).
func (c *markdownCache) glamour(cfg gansi.StyleConfig, key, text string, width int) [][]Segment {
	rk := fmt.Sprintf("%s|%d", key, width)
	r := c.renderers[rk]
	if r == nil {
		// a line break the author typed is kept: a reply copies back as it was written
		opts := []glamour.TermRendererOption{glamour.WithWordWrap(width), glamour.WithPreservedNewLines()}
		if width == 0 {
			markDecorations(&cfg)
			opts = append(opts, glamour.WithChromaFormatter(chromaMarked))
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

// Decorations glamour draws are bracketed by private-use runes that
// styledLines strips, marking the text between them NoCopy. A close with no
// open ends a run that began at the line start or the previous close: a
// list number and code block margins are written before any hook can open.
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
	cfg.Code.Prefix, cfg.Code.Suffix = nc(cfg.Code.Prefix), nc(cfg.Code.Suffix)
}

// glamour writes a code block's margin itself, so the formatter opens every
// code line with a close marker: what stands before it on the row is margin.
func init() {
	tty := formatters.Get("terminal256")
	formatters.Register(chromaMarked, chroma.FormatterFunc(func(w io.Writer, style *chroma.Style, it chroma.Iterator) error {
		return tty.Format(&lineMarker{w: w, start: true}, style, it)
	}))
}

// lineMarker writes markClose before the first byte of every line.
type lineMarker struct {
	w     io.Writer
	start bool
}

func (m *lineMarker) Write(p []byte) (int, error) {
	var b bytes.Buffer
	for _, c := range p {
		if m.start && c != '\n' {
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
// returns a key that changes whenever those groups do.
func (h *Host) markdownStyle() (gansi.StyleConfig, string) {
	cfg := styles.DarkStyleConfig
	zero := uint(0)
	cfg.Document.Margin = &zero
	cfg.Document.BlockPrefix, cfg.Document.BlockSuffix = "", ""
	// Headings read as bold accent text from the Heading style; the "#"
	// markers and the H1 badge are noise.
	for _, hb := range []*gansi.StyleBlock{&cfg.H1, &cfg.H2, &cfg.H3, &cfg.H4, &cfg.H5, &cfg.H6} {
		*hb = gansi.StyleBlock{}
	}

	theme := h.Themes.Get(h.ActiveThemeName())
	if theme == nil {
		return cfg, ""
	}
	var key strings.Builder
	key.WriteString(theme.Name)
	apply := func(p *gansi.StylePrimitive, group string) {
		spec, ok := h.Themes.Group(theme, group)
		if !ok {
			return
		}
		fg, bg := theme.color(spec.Fg), theme.color(spec.Bg)
		bold, italic := spec.Bold != nil && *spec.Bold, spec.Italic != nil && *spec.Italic
		underline := spec.Underline != "" && spec.Underline != "none"
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
	apply(&cfg.BlockQuote.StylePrimitive, "MarkdownQuote")
	apply(&cfg.HorizontalRule, "Comment")
	return cfg, key.String()
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
	if v := t.Palette[ref]; strings.HasPrefix(v, "#") {
		return v
	}
	return ""
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
					cells[i].NoCopy = true
				}
				open, closed = false, len(cells)
				continue
			}
			hl := ""
			if !c.Style.IsZero() {
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
	// a code span's padding cell beside a swatch takes its color
	pad := func(s *Segment, hl string) {
		if s.NoCopy && s.Text == "\u00a0" {
			s.HL = hl
		}
	}
	for i, seg := range row {
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
			pre, sw := seg, seg
			pre.Text, sw.Text = seg.Text[at:m[0]], seg.Text[m[0]:m[1]]
			sw.HL = fmt.Sprintf("%s%s;48;2;%d;%d;%d", SGRPrefix, fg, r, g, b)
			if pre.Text != "" {
				out = append(out, pre)
			} else if len(out) > 0 {
				pad(&out[len(out)-1], sw.HL)
			}
			out = append(out, sw)
			if m[1] == len(seg.Text) && i+1 < len(row) {
				pad(&row[i+1], sw.HL)
			}
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

// WrapSegs word-wraps one logical line into rows of at most w cells,
// keeping each piece's fields; words longer than w are hard-split. Every
// row but the last ends in a Soft segment, and continuation rows hang past
// the line's leading decoration (NoCopy) or a short gutter segment ("  ✗ ",
// "    └ ") behind a NoCopy indent, so a copy gives back the line.
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
	// an unmarked gutter ("  ✗ ", "    └ ") hangs too, but only when it is
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
