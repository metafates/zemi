package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
)

// TestTip: a tip draws its chip, keycap and text in the whorl chrome, and
// drawing at random reaches every tip.
func TestTip(t *testing.T) {
	t.Parallel()
	p := colorprofile.TrueColor
	for _, want := range _tips {
		got := tipStyled(p, want)
		if !strings.Contains(got, "\x1b[1;38;2;23;24;30;48;2;162;173;253m tip \x1b[m") ||
			!strings.Contains(got, "\x1b[1;38;2;162;173;253;48;2;32;33;40m "+strings.TrimRight(want.key, " ")+" \x1b[m") ||
			!strings.HasSuffix(got, " "+want.text) {
			t.Fatalf("%q drew as %q", want.key, got)
		}
	}
	seen := map[string]bool{}
	for range 500 {
		seen[Tip(&colorprofile.Writer{})] = true
	}
	if len(seen) != len(_tips) {
		t.Fatalf("%d tips, %d drawn in 500 tries", len(_tips), len(seen))
	}
}

// TestTipPlain: off a terminal a tip reads as plain words, bold alone.
func TestTipPlain(t *testing.T) {
	t.Parallel()
	if got := tipStyled(colorprofile.NoTTY, _tips[0]); got != " tip  /btw  asks a side question the conversation does not keep" {
		t.Fatalf("plain tip: %q", got)
	}
}
