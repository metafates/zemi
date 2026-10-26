package hostapi

import "testing"

func TestDidYouMean(t *testing.T) {
	names := []string{"sessions", "help", "model"}
	for _, c := range []struct{ got, want string }{
		{"sesions", "sessions"},
		{"mdl", "model"},
		{"Help", "help"},
		{"unrelated", ""},
	} {
		if got := DidYouMean(c.got, names); got != c.want {
			t.Errorf("%q: got %q, want %q", c.got, got, c.want)
		}
	}
}
