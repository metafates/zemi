package tools

import "testing"

func TestChangePreview(t *testing.T) {
	for _, c := range []struct {
		name string
		in   map[string]any
		want string
	}{
		{"write", map[string]any{"content": "a\nb\n"}, "+ a\n+ b"},
		{"edit", map[string]any{"old_string": "x", "new_string": "y"}, "- x\n+ y"},
		{"multiedit with a deletion", map[string]any{"edits": []any{
			map[string]any{"old_string": "1", "new_string": "2"},
			map[string]any{"old_string": "3", "new_string": ""},
		}}, "- 1\n+ 2\n- 3"},
	} {
		if got := changePreview(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
