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
		{"write", map[string]any{"path": "f", "content": "a\nb\n"}, "--- f\n+++ f\n@@\n+a\n+b"},
		{"multiedit with a deletion", map[string]any{"path": "f", "edits": []any{
			map[string]any{"old_string": "1", "new_string": "2"},
			map[string]any{"old_string": "3", "new_string": ""},
		}}, "--- f\n+++ f\n@@\n-1\n+2\n@@\n-3"},
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
	if d := ChangeDiff("edit", map[string]any{"path": path, "old_string": "nope", "new_string": "x"}); d != "--- "+path+"\n+++ "+path+"\n@@\n-nope\n+x" {
		t.Fatalf("fallback = %q", d)
	}
	if d := ChangeDiff("write", map[string]any{"path": os.DevNull, "content": "x"}); d != "--- "+os.DevNull+"\n+++ "+os.DevNull+"\n@@\n+x" {
		t.Fatalf("device fallback = %q", d)
	}
}
