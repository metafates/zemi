package hostapi

import (
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
func TestMarkdownSplitRender(t *testing.T) {
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
	h, _ := newTestHost(t)
	width := func(lines [][]Segment) int {
		w := 0
		for _, l := range lines {
			w = max(w, segsWidth(l))
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
	}{
		{"short line passes through", []Segment{{Text: "hi"}}, 10, "hi"},
		{"word wrap", []Segment{{Text: "hello world foo"}}, 11, "hello |world foo"},
		{"hard split", []Segment{{Text: "abcdefgh"}}, 4, "abcd|efgh"},
		{"segments wrap apart", []Segment{{Text: "one two "}, {Text: "three", HL: "B"}}, 7, "one |two |three"},
		{"gutter hangs", []Segment{{Text: "  ✗ "}, {Text: "network: cannot resolve the gateway host"}}, 22, "  ✗ network: cannot |    resolve the |    gateway host"},
		{"decoration hangs", []Segment{{Text: "▌ ", NoCopy: true}, {Text: "a user message that wraps"}}, 12, "▌ a user |  message |  that wraps"},
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
		if got := copyText(rows); got != plainText(c.line) {
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
