package hostapi

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestMarkdown(t *testing.T) {
	_, r := newTestHost(t)
	runUser(t, r, `
		local function join(line)
			local s = ""
			for _, seg in ipairs(line) do s = s .. seg.text end
			return s
		end
		local lines = zemi.ui.markdown("# Title\n\nsome **bold** text that wraps\n", 16)
		local got = {}
		for i, l in ipairs(lines) do got[i] = join(l) end
		-- the wrapped row keeps its space and is soft: a copy rejoins it
		local want = { "Title", "", "some bold text ", "that wraps" }
		assert(lines[3][#lines[3]].soft_wrap, "wrapped row not marked soft")
		assert(#got == #want, "lines: " .. table.concat(got, "|"))
		for i = 1, #want do
			assert(got[i] == want[i], "line " .. i .. ": " .. got[i])
		end
		assert(lines[1][1].hl:find("^sgr:") ~= nil, "heading hl=" .. tostring(lines[1][1].hl))
		local bold
		for _, seg in ipairs(lines[3]) do
			if seg.text == "bold" then bold = seg end
		end
		assert(bold and bold.hl ~= lines[3][1].hl, "bold run not split out")

		local esc = zemi.ui.markdown("a\27[31mb", 20)
		assert(join(esc[1]) == "ab", "escape leaked: " .. join(esc[1]))
	`)
}

// TestMarkdownSplitRender: a long reply renders its settled prefix from the
// cache and only the tail anew, with the same text as one whole render.
// TestMarkdownLinks: a markdown link and a bare URL both carry the URL a
// click opens, and a scheme zemi does not open never does.
func TestMarkdownLinks(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	links := func(text string) map[string]string {
		got := map[string]string{}
		for _, row := range h.Markdown(text, 80) {
			for _, s := range row {
				if s.Link != "" {
					got[s.Text] = s.Link
				}
			}
		}
		return got
	}
	if got := links("see [the docs](https://example.com/docs?a=1) and https://example.com/bare."); got["the docs"] != "https://example.com/docs?a=1" || got["https://example.com/bare"] != "https://example.com/bare" {
		t.Fatalf("links: %q", got)
	}
	// the trailing period is punctuation, not the URL
	for _, text := range []string{"[x](javascript:alert(1))", "[x](file:///etc/passwd)", "mailto a person: [x](mailto:a@b.c)"} {
		got := links(text)
		if text == "mailto a person: [x](mailto:a@b.c)" {
			if got["x"] != "mailto:a@b.c" {
				t.Fatalf("mailto link: %q", got)
			}
			continue
		}
		if len(got) != 0 {
			t.Fatalf("%s kept a link: %q", text, got)
		}
	}
	if out := linkOf("https://example.com/\x1b]8;;evil"); out != "" {
		t.Fatalf("a control byte survived: %q", out)
	}
}

func TestMarkdownSplitRender(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	var b strings.Builder
	for range 40 {
		b.WriteString("some **bold** paragraph text that wraps a little\n\n")
	}
	b.WriteString("```go\nfunc f() {\n\treturn\n}\n```\n\nthe tail\n")
	text := b.String()
	if cut := settledPrefix(text); cut == 0 || cut >= len(text) {
		t.Fatalf("settledPrefix = %d of %d", cut, len(text))
	}
	join := func(lines [][]Segment) string {
		var out []string
		for _, line := range lines {
			var row strings.Builder
			for _, s := range line {
				row.WriteString(s.Text)
			}
			if strings.TrimSpace(row.String()) != "" {
				out = append(out, row.String())
			}
		}
		return strings.Join(out, "\n")
	}
	cfg, key := h.markdownStyle()
	whole := join(h.md.render(&cfg, key, text, 40))
	if got := join(h.Markdown(text, 40)); got != whole {
		t.Fatalf("split render differs:\n%s\n---\n%s", got, whole)
	}
}

// TestMarkdownKeepsTabIndent: code indented with tabs keeps its indentation.
func TestMarkdownKeepsTabIndent(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	found := false
	for _, line := range h.Markdown("```go\nfunc f() {\n\treturn\n}\n```", 40) {
		var b strings.Builder
		for _, s := range line {
			b.WriteString(s.Text)
		}
		found = found || strings.Contains(b.String(), "    return")
	}
	if !found {
		t.Fatal("tab-indented code lost its indentation")
	}
}

// copyText joins lines the way a selection does: decoration left out, a
// soft-wrapped row joined to the next.
func copyText(lines [][]Segment) string {
	var b strings.Builder
	joined := true
	for _, line := range lines {
		var row strings.Builder
		soft := false
		for _, s := range line {
			if !s.NoCopy {
				row.WriteString(s.Text)
			}
			if soft = s.Soft; soft {
				break
			}
		}
		text := row.String()
		if !soft {
			text = strings.TrimRight(text, " ")
		}
		if !joined {
			b.WriteByte('\n')
		}
		b.WriteString(text)
		joined = soft
	}
	return b.String()
}

// TestMarkdownCopiesBackToSource: what a selection over rendered markdown
// copies is the text the model wrote, without glamour's decoration.
func TestMarkdownCopiesBackToSource(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	for _, c := range []struct{ name, text, want string }{
		{"paragraph", "a paragraph long enough to wrap over three rows of a narrow chat pane", "a paragraph long enough to wrap over three rows of a narrow chat pane"},
		{"code block", "```go\nfunc f(a string) { return a + \"a long line that has to wrap somewhere\" }\n```", "func f(a string) { return a + \"a long line that has to wrap somewhere\" }"},
		{"inline code", "call `go test ./...` now", "call go test ./... now"},
		{"list", "- first item, long enough that it wraps over two rows\n- second", "first item, long enough that it wraps over two rows\nsecond"},
		{"quote", "> quoted line", "quoted line"},
	} {
		if got := copyText(h.Markdown(c.text, 30)); got != c.want {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

// TestMarkdownTableWidth: a table is only as wide as its content, and one
// whose content does not fit is laid out to the width it was given.
func TestMarkdownTableWidth(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	width := func(lines [][]Segment) int {
		w := 0
		for _, l := range lines {
			w = max(w, SegsWidth(l))
		}
		return w
	}
	if got := width(h.Markdown("| a | b |\n|---|---|\n| 1 | 2 |\n", 60)); got > 10 {
		t.Errorf("a two-column table takes %d cells of 60", got)
	}
	wide := "| " + strings.Repeat("long header ", 6) + "| b |\n|---|---|\n| 1 | 2 |\n"
	if got := width(h.Markdown(wide, 40)); got > 40 {
		t.Errorf("a table too wide for the room takes %d cells", got)
	}
}

// TestWrapSegs: rows break after spaces, overlong words hard-split, styles
// survive the cut, and continuations hang past a gutter.
func TestWrapSegs(t *testing.T) {
	for _, c := range []struct {
		name string
		line []Segment
		w    int
		want string // rows joined by |, trailing spaces kept
		copy string // what a selection copies when it is not the line
	}{
		{"short line passes through", []Segment{{Text: "hi"}}, 10, "hi", ""},
		{"word wrap", []Segment{{Text: "hello world foo"}}, 11, "hello |world foo", ""},
		{"hard split", []Segment{{Text: "abcdefgh"}}, 4, "abcd|efgh", ""},
		{"hard split over many rows", []Segment{{Text: "  ✗ "}, {Text: "abcdefghijklmnopqrstuvwx"}}, 12, "  ✗ abcdefgh|    ijklmnop|    qrstuvwx", ""},
		{"segments wrap apart", []Segment{{Text: "one two "}, {Text: "three", HL: "B"}}, 7, "one |two |three", ""},
		{"gutter hangs", []Segment{{Text: "  ✗ "}, {Text: "network: cannot resolve the gateway host"}}, 22, "  ✗ network: cannot |    resolve the |    gateway host", ""},
		{"decoration hangs", []Segment{{Text: "▌ ", NoCopy: true}, {Text: "a user message that wraps"}}, 12, "▌ a user |  message |  that wraps", ""},
		{"a bar goes down, its gutter too, its mark not", []Segment{{Text: " ", NoCopy: true}, {Text: "│ ", NoCopy: true}, {Text: "✗ ", NoCopy: true}, {Text: "a failed call that wraps"}}, 15, " │ ✗ a failed | │   call that | │   wraps", ""},
		{"a space run at a break is kept", []Segment{{Text: "aaaa bbbb cccc      dddd"}}, 15, "aaaa bbbb cccc |     dddd", ""},
	} {
		rows := WrapSegs(c.line, c.w)
		var got []string
		for _, row := range rows {
			var b strings.Builder
			for _, s := range row {
				b.WriteString(s.Text)
			}
			got = append(got, b.String())
		}
		if strings.Join(got, "|") != c.want {
			t.Errorf("%s: %q", c.name, got)
		}
		if last := rows[len(rows)-1]; last[len(last)-1].HL != c.line[len(c.line)-1].HL {
			t.Errorf("%s: style lost: %+v", c.name, rows)
		}
		if got := copyText(rows); got != cmp.Or(c.copy, plainText(c.line)) {
			t.Errorf("%s: copies %q", c.name, got)
		}
	}
}

func plainText(line []Segment) string {
	var b strings.Builder
	for _, s := range line {
		if !s.NoCopy {
			b.WriteString(s.Text)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// TestMarkdownCacheBounded: the rendered-line cache rotates on bytes as
// well as on entries, so a text window keyed by its whole growing body
// cannot retain every version of it.
func TestMarkdownCacheBounded(t *testing.T) {
	c := &markdownCache{}
	line := []Segment{{Text: strings.Repeat("x", 4096)}}
	for i := range 4000 {
		c.put(mdKey{style: strconv.Itoa(i)}, [][]Segment{line})
	}
	if c.bytes > mdBytesMax+8192 || len(c.lines) == 4000 {
		t.Fatalf("cache holds %d entries, %d bytes", len(c.lines), c.bytes)
	}
}

// TestWrapSegsExpandsTabs: a tab measures as no cell and the renderer drops
// it, so the words around it would merge in every prompt and toast.
func TestWrapSegsExpandsTabs(t *testing.T) {
	rows := WrapSegs([]Segment{{Text: "run\tthe\tlisting"}}, 40)
	if len(rows) != 1 || rows[0][0].Text != "run    the    listing" {
		t.Fatalf("wrapped %+v", rows)
	}
}

func TestMarkdownHexColors(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	for _, c := range []struct{ name, text, want string }{
		{"dark text on a light color", "use #FFF000 here", "#FFF000 38;2;0;0;0;48;2;255;240;0"},
		{"a code span", "`#03f`", "#03f 38;2;255;255;255;48;2;0;51;255"},
		{"issue number", "see #123", ""},
		{"after a word", "café#fff", ""},
		{"url fragment", "https://x.dev/#fff000", ""},
		{"longer word", "#fff0001", ""},
	} {
		text, hl := "", ""
		for _, s := range h.Markdown(c.text, 40)[0] {
			if strings.Contains(s.HL, "48;2;") && (hl == "" || s.HL == hl) {
				text, hl = text+s.Text, s.HL
			}
		}
		if got := strings.TrimSuffix(text+" "+strings.TrimPrefix(hl, SGRPrefix), " "); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEscapeHTML(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a Vec<String> and </div>", "a Vec\\<String> and \\</div>"},
		{"1 < 2, <https://x.dev> and \\<b>", "1 < 2, <https://x.dev> and \\<b>"},
		{"`Vec<T>` or ``a`<T>``", "`Vec<T>` or ``a`<T>``"},
		{"```go\nx := []Vec<T>{}\n```\nVec<T>", "```go\nx := []Vec<T>{}\n```\nVec\\<T>"},
		{"Text:\n\n    let v: Vec<T>;\n    w<T>\nVec<T>", "Text:\n\n    let v: Vec<T>;\n    w<T>\nVec\\<T>"},
		{"- a\n\n      Vec<T>\n\n  more <T>\n    no <T>", "- a\n\n      Vec<T>\n\n  more \\<T>\n    no \\<T>"},
	} {
		if got := escapeHTML(c.in); got != c.want {
			t.Errorf("escapeHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDiffRows: one number column, the sign on the row's tint, the tint
// out to the edge on every wrapped row (a wide rune counts two cells), a
// gap between hunks and a file a command changed as its own row.
func TestDiffRows(t *testing.T) {
	cjk := strings.Repeat("漢", 100)
	for _, c := range []struct {
		name, diff string
		width      int
		want       []string
	}{
		{
			"numbers", "--- a.go\n+++ a.go\n@@ -9,3 +9,3 @@\n one\n-two\n+TWO\n three\n", 40,
			[]string{"  9   one", " 10 - two", " 10 + TWO", " 11   three"},
		},
		{"wrapped", "@@ -1 +1 @@\n+alpha beta gamma delta", 20, []string{" 1 + alpha beta", "     gamma delta"}},
		{
			"files and hunks", "M a.go\n@@ -1 +1 @@\n-a\n+b\nM b.go\n@@ -3 +3 @@\n x\n@@ -9 +9 @@\n--- y", 20,
			[]string{" M a.go", "  1 - a", "  1 + b", " M b.go", "  3   x", "    ⋯", "  9 - -- y"},
		},
		{"cjk", "@@ -1 +1 @@\n+" + cjk, 40, []string{
			" 1 + " + cjk[:3*17], "     " + cjk[3*17:3*34], "     " + cjk[3*34:3*51],
			"     " + cjk[3*51:3*68], "     " + cjk[3*68:3*85], "     " + cjk[3*85:],
		}},
	} {
		var got []string
		copied := ""
		for _, row := range DiffRows(c.diff, c.width) {
			var drawn strings.Builder
			for _, s := range row {
				drawn.WriteString(s.Text)
			}
			got = append(got, strings.TrimRight(drawn.String(), " "))
			if len(row) < 3 {
				continue
			}
			if SegsWidth(row) != c.width || !strings.HasSuffix(row[0].HL, "Num") || row[len(row)-1].HL+"Num" != row[0].HL {
				t.Errorf("%s: row %q is %d cells in %v, want %d tinted", c.name, drawn.String(), SegsWidth(row), row, c.width)
			}
			copied += copyText([][]Segment{row})
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: rows = %q", c.name, got)
		}
		if c.name == "cjk" && copied != cjk {
			t.Errorf("cjk: copies as %q", copied)
		}
	}
}

func TestHighlight(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	for _, c := range []struct {
		name, text, lang string
		width            int
		want             string // rows joined by /, each segment as [text|hl]
	}{
		{
			"keywords and comments", "for f in a; do cat $f; done # why", "bash", 0,
			"[for|Accent][ f in a|Normal][;|Dim][ |Normal][do|Accent][ cat $f|Normal][;|Dim][ |Normal][done|Accent][ |Normal][# why|Dim]",
		},
		{"literals", "echo \"hi\" 42", "bash", 0, "[echo |Normal][\"hi\"|Literal][ |Normal][42|Literal]"},
		{"a row per line", "cat <<EOF\nhi\nEOF", "bash", 0, "[cat |Normal][<<EOF|Literal]/[hi|Literal]/[EOF|Literal]"},
		{"no lexer", "x\t<y>\x1b[31m", "nope", 0, "[x    <y>|]"},
		{"wraps", "echo alpha beta gamma", "bash", 12, "[echo alpha |Normal]/[beta gamma|Normal]"},
	} {
		rows := h.Highlight(c.text, c.lang, c.width)
		var got []string
		for _, row := range rows {
			var b strings.Builder
			for _, s := range row {
				fmt.Fprintf(&b, "[%s|%s]", s.Text, s.HL)
			}
			got = append(got, b.String())
		}
		if g := strings.Join(got, "/"); g != c.want {
			t.Errorf("%s: %s", c.name, g)
		}
		if c.lang == "bash" && copyText(rows) != c.text {
			t.Errorf("%s: copies %q", c.name, copyText(rows))
		}
	}
}

// TestMarkdownTheme: markdown draws only the theme's colors, in the dark
// and the light variant, blocks sit one blank row apart and a quote keeps
// its bar on every row it wraps to.
func TestMarkdownTheme(t *testing.T) {
	h := newTestHostUI(t)
	text := "intro\n\n```go\nfunc f() int {\n\t// why\n\treturn 42 + len(\"s\")\n}\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n" +
		"- one\n  - two\n1. three\n\n> a quote long enough to wrap over more than one row of the pane\n"
	for _, c := range []struct {
		light bool
		theme string
	}{{false, "whorl"}, {true, "whorl-light"}} {
		h.SetLightTerminal(c.light)
		theme := h.ActiveTheme()
		if theme == nil || theme.Name != c.theme {
			t.Fatalf("light=%v: theme %v, want %s", c.light, theme, c.theme)
		}
		palette := map[string]bool{}
		for _, role := range Roles {
			r, g, b, _ := theme.Color(role).RGBA()
			palette[fmt.Sprintf("%d;%d;%d", r>>8, g>>8, b>>8)] = true
		}
		var blank, quote int
		prevBlank := false
		for _, row := range h.Markdown(text, 30) {
			isBlank := strings.TrimSpace(plainText(row)) == ""
			if isBlank && prevBlank {
				t.Errorf("%s: two blank rows", c.theme)
			}
			if isBlank {
				blank++
			}
			prevBlank = isBlank
			if strings.Contains(plainText(row), "quote") || strings.Contains(plainText(row), "pane") {
				if !strings.HasPrefix(row[0].Text, "│") {
					t.Errorf("%s: quote row without its bar: %q", c.theme, plainText(row))
				}
				quote++
			}
			for _, s := range row {
				p := strings.Split(strings.TrimPrefix(s.HL, SGRPrefix), ";")
				for i := 0; i+1 < len(p); i++ {
					if p[i] != "38" && p[i] != "48" {
						continue
					}
					if p[i+1] != "2" || i+4 >= len(p) || !palette[strings.Join(p[i+2:i+5], ";")] {
						t.Errorf("%s: %q draws %s, off the palette", c.theme, s.Text, s.HL)
						break
					}
					i += 4
				}
			}
		}
		// intro, code, table, list, the ordered list, quote
		if blank != 5 || quote < 2 {
			t.Errorf("%s: %d blank rows, %d quote rows", c.theme, blank, quote)
		}
	}
	// a list in a quote marks its first row only and a task mark is a
	// decoration
	for _, c := range []struct{ text, mark string }{
		{"> - a list item in a quote that wraps over rows", "│ • "},
		{"> 1. a list item in a quote that wraps over rows", "│ 1. "},
		{"- [x] done task", "✓ "},
	} {
		marks := 0
		for _, row := range h.Markdown(c.text, 20) {
			if row[0].Text == c.mark && row[0].NoCopy && row[0].HL == "Dim" {
				marks++
			}
		}
		if marks != 1 {
			t.Errorf("%q draws %q %d times", c.text, c.mark, marks)
		}
	}
	if rows := h.Markdown("```diff\n+new\n old\n```", 20); rows[0][1].HL == rows[1][1].HL {
		t.Errorf("a diff fence draws + like context: %v", rows)
	}
}
