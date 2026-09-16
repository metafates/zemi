package ui

import (
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestWrapSegs: rows break after spaces, overlong words hard-split, styles
// survive the cut, and continuations hang past a short gutter.
func TestWrapSegs(t *testing.T) {
	for _, c := range []struct {
		name string
		line []hostapi.Segment
		w    int
		want string // rows joined by |, trailing spaces kept
	}{
		{"short line passes through", []hostapi.Segment{{Text: "hi"}}, 10, "hi"},
		{"word wrap", []hostapi.Segment{{Text: "hello world foo"}}, 11, "hello |world foo"},
		{"hard split", []hostapi.Segment{{Text: "abcdefgh"}}, 4, "abcd|efgh"},
		{"segments wrap apart", []hostapi.Segment{{Text: "one two "}, {Text: "three", HL: "B"}}, 7, "one |two |three"},
		{"gutter hangs", []hostapi.Segment{{Text: "  ✗ "}, {Text: "network: cannot resolve the gateway host"}}, 22, "  ✗ network: cannot |    resolve the |    gateway host"},
	} {
		rows := wrapSegs(c.line, c.w)
		var got []string
		for _, row := range rows {
			got = append(got, plainSegs(row))
		}
		if strings.Join(got, "|") != c.want {
			t.Errorf("%s: %q", c.name, got)
		}
		if last := rows[len(rows)-1]; last[len(last)-1].HL != c.line[len(c.line)-1].HL {
			t.Errorf("%s: style lost: %+v", c.name, rows)
		}
	}
}
