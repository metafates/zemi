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

// TestRealPath walks the component resolution the file tools and the pad
// rule judge by: a symlink is followed before a later .. climbs, a missing
// leaf joins its real parent, and a loop or an empty path names nothing.
func TestRealPath(t *testing.T) {
	t.Parallel()
	root, out := t.TempDir(), t.TempDir()
	// wants are spelled resolved: the temp dirs may sit behind a symlinked parent (darwin's /var)
	rin, rout := RealPath(root), RealPath(out)
	sep := string(filepath.Separator)
	if err := os.Mkdir(filepath.Join(out, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "..inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, p, want string }{
		{".. after a symlink climbs from its target", root + sep + "link" + sep + ".." + sep + "f", filepath.Join(rout, "f")},
		{"a missing leaf joins its real parent", filepath.Join(root, "new", "f"), filepath.Join(rin, "new", "f")},
		{"a symlink loop names nothing", filepath.Join(root, "loop", "x"), ""},
		{"an empty path names nothing", "", ""},
	} {
		if got := RealPath(c.p); got != c.want {
			t.Errorf("%s: RealPath(%q) = %q, want %q", c.name, c.p, got, c.want)
		}
	}
	inside := RealPath(filepath.Join(root, "..inside", "f"))
	if rel, ok := Under(rin, inside); !ok || rel != filepath.Join("..inside", "f") {
		t.Errorf("a ..-named entry inside the root: Under = %q, %v", rel, ok)
	}
	if InPad(root, root+sep+"link"+sep+".."+sep+"f") {
		t.Error("a symlink out of the pad reads as inside it")
	}
	if !InPad(root, filepath.Join(root, "..inside", "f")) {
		t.Error("a ..-named entry inside the pad reads as outside it")
	}
}
