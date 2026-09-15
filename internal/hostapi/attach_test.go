package hostapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
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

	got, _ := h.expandAttachments("look at @main.go, @.env, @notes.md and @missing.go")
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
	if plain, _ := h.expandAttachments("no references here"); plain != "no references here" {
		t.Fatalf("plain text changed: %q", plain)
	}
}

// TestImageAttachment: an @image becomes a file part after the text, with a
// note naming it behind the marker.
func TestImageAttachment(t *testing.T) {
	h, _ := newTestHostDB(t)
	png := []byte("\x89PNG\r\n\x1a\nfake")
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "shot.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	msg := h.composeUserMessage("what is wrong in @shot.png")
	if len(msg.Content) != 2 {
		t.Fatalf("parts = %d", len(msg.Content))
	}
	text, ok := msg.Content[0].(fantasy.TextPart)
	if !ok || !strings.HasPrefix(text.Text, "what is wrong in @shot.png"+attachMarker) || !strings.Contains(text.Text, `<image path="shot.png">`) {
		t.Fatalf("text part = %#v", msg.Content[0])
	}
	img, ok := msg.Content[1].(fantasy.FilePart)
	if !ok || img.MediaType != "image/png" || string(img.Data) != string(png) {
		t.Fatalf("file part = %#v", msg.Content[1])
	}
}
