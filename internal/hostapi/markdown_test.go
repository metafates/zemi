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
		local want = { "Title", "", "some bold text", "that wraps" }
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
	whole := join(h.md.render(cfg, key, text, 40))
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
