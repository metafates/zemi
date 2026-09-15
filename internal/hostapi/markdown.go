package hostapi

import (
	"fmt"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
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
}

func (c *markdownCache) put(k string, v [][]Segment) {
	if c.lines == nil || len(c.lines) >= mdLinesMax {
		c.prev, c.lines = c.lines, map[string][][]Segment{}
	}
	c.lines[k] = v
}

// Markdown renders text as styled segment lines at most width cells wide
// (zemi.ui.markdown, DESIGN §8). Input is sanitized before glamour sees it.
func (h *Host) Markdown(text string, width int) [][]Segment {
	width = max(width, 1)
	cfg, styleKey := h.markdownStyle()
	c := &h.md
	c.mu.Lock()
	defer c.mu.Unlock()

	lk := fmt.Sprintf("%s|%d|%s", styleKey, width, text)
	if lines, ok := c.lines[lk]; ok {
		return lines
	}
	if lines, ok := c.prev[lk]; ok {
		c.put(lk, lines)
		return lines
	}
	rk := fmt.Sprintf("%s|%d", styleKey, width)
	r := c.renderers[rk]
	if r == nil {
		var err error
		r, err = glamour.NewTermRenderer(glamour.WithStyles(cfg), glamour.WithWordWrap(width))
		if err != nil {
			return plainLines(text)
		}
		if c.renderers == nil || len(c.renderers) >= mdRenderersMax {
			c.renderers = map[string]*glamour.TermRenderer{}
		}
		c.renderers[rk] = r
	}
	// a tab occupies no cell once drawn into the buffer, so code would lose
	// its indentation
	out, err := r.Render(sanitize.String(strings.ReplaceAll(text, "\t", "    ")))
	if err != nil {
		return plainLines(text)
	}
	lines := styledLines(out)
	c.put(lk, lines)
	return lines
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
		spec, ok := theme.HL[group]
		if !ok {
			return
		}
		fg, bg := theme.hex(spec.Fg), theme.hex(spec.Bg)
		fmt.Fprintf(&key, ";%s=%s/%s/%t%t%t", group, fg, bg, spec.Bold, spec.Italic, spec.Underline)
		if fg != "" {
			p.Color = &fg
		}
		if bg != "" {
			p.BackgroundColor = &bg
		}
		yes := true
		if spec.Bold {
			p.Bold = &yes
		}
		if spec.Italic {
			p.Italic = &yes
		}
		if spec.Underline {
			p.Underline = &yes
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

// hex resolves a color reference (raw hex or palette name) to hex, or "".
func (t *Theme) hex(ref string) string {
	if strings.HasPrefix(ref, "#") {
		return ref
	}
	if v := t.Palette[ref]; strings.HasPrefix(v, "#") {
		return v
	}
	return ""
}

// styledLines parses glamour's ANSI output into segments: runs of equal
// style merge, and the unstyled padding glamour adds to every line and the
// trailing blank lines are dropped.
func styledLines(s string) [][]Segment {
	var out [][]Segment
	for _, l := range ansiLines(s) {
		end := len(l)
		for end > 0 && (l[end-1].Content == " " || l[end-1].Content == "") && l[end-1].Style.Bg == nil {
			end--
		}
		row := []Segment{}
		for _, c := range l[:end] {
			hl := ""
			if !c.Style.IsZero() {
				hl = SGRPrefix + strings.TrimSuffix(strings.TrimPrefix(c.Style.String(), "\x1b["), "m")
			}
			if n := len(row); n > 0 && row[n-1].HL == hl {
				row[n-1].Text += c.Content
				continue
			}
			row = append(row, Segment{Text: c.Content, HL: hl})
		}
		out = append(out, row)
	}
	for len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	return out
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
	for _, l := range strings.Split(sanitize.String(strings.ReplaceAll(text, "\t", "    ")), "\n") {
		out = append(out, []Segment{{Text: l}})
	}
	return out
}
