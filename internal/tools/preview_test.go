package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChangePreview(t *testing.T) {
	for _, c := range []struct {
		name string
		in   map[string]any
		want string
	}{
		{"write", map[string]any{"content": "a\nb\n"}, "+ a\n+ b"},
		{"edit", map[string]any{"old_string": "x", "new_string": "y"}, "- x\n+ y"},
	} {
		if got := ChangePreview(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestChangeDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := ChangeDiff("edit", map[string]any{"path": path, "old_string": "two", "new_string": "TWO"})
	for _, want := range []string{"@@ -1,3 +1,3 @@", " one", "-two", "+TWO", " three"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff misses %q:\n%s", want, d)
		}
	}
	if d := ChangeDiff("edit", map[string]any{"path": path, "old_string": "nope", "new_string": "x"}); d != "- nope\n+ x" {
		t.Fatalf("fallback = %q", d)
	}
	if d := ChangeDiff("write", map[string]any{"path": os.DevNull, "content": "x"}); d != "+ x" {
		t.Fatalf("device fallback = %q", d)
	}
}
