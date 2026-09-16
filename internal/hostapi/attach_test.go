package hostapi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestDropsSweptAfterTTL: idle drop copies age out, one attached again stays.
func TestDropsSweptAfterTTL(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	name, err := h.saveDrop("keep.txt", []byte("keep"))
	if err != nil {
		t.Fatal(err)
	}
	dir := h.dropsDir()
	stale := filepath.Join(dir, "stale.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-dropTTL - time.Hour)
	for _, p := range []string{stale, filepath.Join(dir, name)} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if again, err := h.saveDrop("keep.txt", []byte("keep")); err != nil || again != name {
		t.Fatalf("reuse = %q, %v", again, err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("a drop attached again was swept: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale drop survived: %v", err)
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
	if msg := h.composeUserMessage("what is wrong in @shot.png", false); len(msg.Content) != 1 || !strings.Contains(msg.Content[0].(fantasy.TextPart).Text, `<image path="shot.png">omitted`) {
		t.Fatalf("text-only model: %#v", msg.Content)
	}
	msg := h.composeUserMessage("what is wrong in @shot.png", true)
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

// TestAttachPath: a dropped project file becomes @rel, an image from
// elsewhere a @drop copy that attaches, a directory or secret its path, and
// @drop cannot climb out of the drops directory.
func TestAttachPath(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	out := t.TempDir()
	shot := filepath.Join(out, "Screen Shot 1.png")
	os.WriteFile(shot, []byte("\x89PNGfake"), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600)
	os.WriteFile(filepath.Join(out, ".env"), []byte("TOKEN=x"), 0o600)

	for path, want := range map[string]string{
		filepath.Join(h.Dirs.ProjectDir, "main.go"): "@main.go",
		shot:                       "@drop:Screen-Shot-1.png",
		out:                        out,
		filepath.Join(out, ".env"): filepath.Join(out, ".env"),
	} {
		if got := h.AttachPath(path); got != want {
			t.Errorf("AttachPath(%s) = %q, want %q", path, got, want)
		}
	}
	if again := h.AttachPath(shot); again != "@drop:Screen-Shot-1.png" {
		t.Errorf("same content must keep its name: %q", again)
	}
	msg := h.composeUserMessage("what is this @drop:Screen-Shot-1.png @drop:../attach_test.go", true)
	if len(msg.Content) != 2 {
		t.Fatalf("parts = %d", len(msg.Content))
	}
}

// TestImagesRefusedContinues: a 400 over image input does not end the
// turn: images become notes, the model is remembered as text-only and the
// prompt goes out again.
func TestImagesRefusedContinues(t *testing.T) {
	h, _ := newTestHostDB(t)
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "shot.png"), []byte("\x89PNG\r\n\x1a\nfake"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := reflectStub(t, h, "ok")
	m.toolStep.Store(50_000)
	m.noImages.Store(1)
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	sender(t, h)("what is in @shot.png")
	if n := m.streams.Load(); n != 3 {
		t.Fatalf("model streamed %d times, want 3", n)
	}
	// the step before the refusal keeps its usage and its tool row
	if in := h.AgentStream().SessionIn; in < 50_000 {
		t.Fatalf("session input %d, want the first step's 50000", in)
	}
	toolRow := false
	for _, row := range h.Session().Transcript {
		if row.Kind == "error" {
			t.Fatalf("error row shown: %+v", row)
		}
		toolRow = toolRow || row.Kind == "tool"
	}
	if !toolRow {
		t.Fatal("tool row of the first step lost")
	}
	model := h.currentModelID(nil)
	t.Cleanup(func() { imagesRejected.Delete(model) })
	if h.takesImages(model) {
		t.Fatal("model not remembered as text-only")
	}
	for _, msg := range h.agents.runner.History {
		for _, part := range msg.Content {
			if _, ok := part.(fantasy.FilePart); ok {
				t.Fatal("image part still in history")
			}
			if tp, ok := part.(fantasy.TextPart); ok && strings.Contains(tp.Text, "omitted") {
				return
			}
		}
	}
	t.Fatal("no omission note in history")
}

func TestImagesRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&fantasy.ProviderError{StatusCode: 400, Message: "<path> is not a multimodal model"}, true},
		{&fantasy.ProviderError{StatusCode: 400, Message: "Invalid content type. image_url is only supported by certain models."}, true},
		{&fantasy.ProviderError{StatusCode: 400, Message: "max_tokens too large"}, false},
		{&fantasy.ProviderError{StatusCode: 500, Message: "image service down"}, false},
		{errors.New("image"), false},
		{nil, false},
	} {
		if got := imagesRefused(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}
