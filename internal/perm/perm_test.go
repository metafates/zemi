package perm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTildePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ p, want string }{
		{"", ""},
		{"/elsewhere/f", "/elsewhere/f"},
		{home, "~"},
		{home + "/x/y", "~/x/y"},
		// the backslash form only shortens where it is a separator
		{home + string(filepath.Separator) + "x", "~" + string(filepath.Separator) + "x"},
	} {
		if got := TildePath(c.p); got != c.want {
			t.Errorf("TildePath(%q) = %q, want %q", c.p, got, c.want)
		}
	}
}
