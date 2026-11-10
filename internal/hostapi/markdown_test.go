package hostapi

import (
	"cmp"
	"fmt"
	"strings"
	"testing"
)

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
		name   string
		line   []Segment
		w      int
		want   string // rows joined by |, trailing spaces kept
		copied string // what a selection copies when it is not the line
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
		if got := copyText(rows); got != cmp.Or(c.copied, plainText(c.line)) {
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
			// a swatch is the one run drawn in black or white on the color itself
			if strings.Contains(s.Text, "#") && (strings.Contains(s.HL, "38;2;0;0;0;") || strings.Contains(s.HL, "38;2;255;255;255;")) {
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
		{"~~~\nVec<T>\n~~~\nVec<T>", "~~~\nVec<T>\n~~~\nVec\\<T>"},
		{"```go\nx := Vec<T>{}\n```go\nstill <T>\n```\nVec<T>", "```go\nx := Vec<T>{}\n```go\nstill <T>\n```\nVec\\<T>"},
		{"Text:\n\n    ```\n    Vec<T>\n\nprose Vec<T> out", "Text:\n\n    ```\n    Vec<T>\n\nprose Vec\\<T> out"},
		{"```a`b\nVec<T>\n```\nVec<T>", "```a`b\nVec\\<T>\n```\nVec<T>"},
		{"```\nVec<T>\n    ```\nVec<T>", "```\nVec<T>\n    ```\nVec<T>"},
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
