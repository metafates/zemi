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
		got := tipStyled(p, " tip ", want)
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
	if got := tipStyled(colorprofile.NoTTY, " tip ", _tips[0]); got != " tip  /btw  asks a side question without interrupting main agent" {
		t.Fatalf("plain tip: %q", got)
	}
}

// TestBusy: the one-instance refusal dresses like a tip, the pid as its
// keycap, and no pid leaves the keycap off.
func TestBusy(t *testing.T) {
	t.Parallel()
	const text = "another zemi is already open on this project; close it to open here"
	if got := tipStyled(colorprofile.NoTTY, " busy ", tip{key: "57356", text: text}); got != " busy  57356  "+text {
		t.Fatalf("pid refusal: %q", got)
	}
	if got := tipStyled(colorprofile.NoTTY, " busy ", tip{text: text}); got != " busy  "+text {
		t.Fatalf("pidless refusal: %q", got)
	}
	if got := Busy(&colorprofile.Writer{}, 57356); !strings.Contains(got, " 57356 ") || !strings.Contains(got, text) {
		t.Fatalf("busy drew %q", got)
	}
}
