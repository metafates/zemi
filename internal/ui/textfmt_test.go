package ui

import "testing"

func TestStripMarkdown(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{"**hello world** this is a `*` asterisk", "hello world this is a * asterisk"},
		{"see [the docs](https://x.y) now", "see the docs now"},
		{"# Heading\n- first item\n* second item", "Heading\nfirst item\nsecond item"},
		{"```go\nfmt.Println(1)\n```", "fmt.Println(1)"},
		{"2 * 3 * 4 = 6", "2 * 3 * 4 = 6"},
		{"***both*** _and_ plain", "both _and_ plain"},
	} {
		if got := stripMarkdown(c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
