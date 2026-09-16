package ui

import (
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestWrapHangs: wrapped rows continue under the text past a short gutter.
func TestWrapHangs(t *testing.T) {
	for _, c := range []struct {
		name string
		line []hostapi.Segment
		want []string
	}{
		{"gutter", []hostapi.Segment{{Text: "  ✗ "}, {Text: "network: cannot resolve the gateway host"}}, []string{"  ✗ network: cannot", "    resolve the", "    gateway host"}},
		{"no gutter", []hostapi.Segment{{Text: "plain words that wrap around"}}, []string{"plain words that wrap", "around"}},
	} {
		var got []string
		for _, row := range wrapSegs(c.line, 22) {
			got = append(got, strings.TrimRight(plainSegs(row), " "))
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Fatalf("%s: %q", c.name, got)
		}
	}
}
