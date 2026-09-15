package hostapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandAttachments: project files referenced with @ attach; secret-
// shaped paths, missing files and symlinks leading out of the project do
// not.
func TestExpandAttachments(t *testing.T) {
	h, _ := newTestHostDB(t)
	proj := h.Dirs.ProjectDir
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proj, "main.go"), "package main // attached")
	write(filepath.Join(proj, ".env"), "TOKEN=from-dotenv")
	outside := filepath.Join(t.TempDir(), "creds")
	write(outside, "outside-secret")
	if err := os.Symlink(outside, filepath.Join(proj, "notes.md")); err != nil {
		t.Fatal(err)
	}

	got := h.expandAttachments("look at @main.go, @.env, @notes.md and @missing.go")
	if !strings.HasPrefix(got, "look at @main.go, @.env, @notes.md and @missing.go"+attachMarker) {
		t.Fatalf("typed text must lead, then the marker: %q", got)
	}
	if !strings.Contains(got, `<file path="main.go">`) || !strings.Contains(got, "package main // attached") {
		t.Fatalf("main.go not attached: %q", got)
	}
	for _, leak := range []string{"from-dotenv", "outside-secret", "missing.go\">"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q must not attach: %q", leak, got)
		}
	}
	if plain := h.expandAttachments("no references here"); plain != "no references here" {
		t.Fatalf("plain text changed: %q", plain)
	}
}
