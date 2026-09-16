package lsp

import "testing"

func TestSection(t *testing.T) {
	settings := map[string]any{"gopls": map[string]any{"staticcheck": true}}
	for _, c := range []struct {
		name string
		want any
	}{
		{"gopls.staticcheck", true},
		{"gopls.missing", nil},
		{"pyright", nil},
	} {
		if got := section(settings, c.name); got != c.want {
			t.Errorf("section(%q) = %v, want %v", c.name, got, c.want)
		}
	}
	if got := section(settings, ""); got == nil {
		t.Error("the empty section is the whole table")
	}
}
