package ui

import "testing"

func TestEditorScript(t *testing.T) {
	for _, c := range []struct {
		editor string
		line   int
		want   string
	}{
		{"nvim", 12, `nvim +12 "$1"`},
		{"/usr/bin/vim -u NONE", 3, `/usr/bin/vim -u NONE +3 "$1"`},
		{"code --wait", 5, `code --wait "$1"`},
		{"nvim", 0, `nvim "$1"`},
	} {
		if got := editorScript(c.editor, c.line); got != c.want {
			t.Errorf("%s line %d: %q, want %q", c.editor, c.line, got, c.want)
		}
	}
}
