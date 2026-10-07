package hostapi

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
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
	if !strings.HasPrefix(got, "look at @main.go, @.env, @notes.md and @missing.go"+agent.AttachMarker) {
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
	// the user row shows the reference, so the user hears it did not attach
	if n := h.evlog.notes; len(n) != 1 || n[0].level != "warn" || n[0].title != "Could not attach .env" || n[0].body != "reading it needs your approval" {
		t.Fatalf("refused attachment said nothing: %+v", n)
	}
	pipe := filepath.Join(proj, "pipe")
	if exec.Command("mkfifo", pipe).Run() == nil {
		done := make(chan string)
		go func() { s, _ := h.expandAttachments("@pipe"); done <- s }()
		select {
		case s := <-done:
			if s != "@pipe" {
				t.Fatalf("a FIFO must not attach: %q", s)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a FIFO reference blocked the message")
		}
	}
	write(filepath.Join(proj, "big.txt"), strings.Repeat("x", attachMax+1))
	write(filepath.Join(proj, "blob.txt"), "\xff\xfe binary")
	got, _ = h.expandAttachments("@big.txt @blob.txt @.env")
	for _, want := range []string{
		`<file path="big.txt">not attached: it is over the 64 KB`,
		`<file path="blob.txt">not attached: it is not text`,
		`<file path=".env">not attached: reading it needs your approval`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}

	// zemi's own notes are an element of the trailer, like files and reports
	if note := agent.Note("tick it"); !strings.HasPrefix(note, agent.AttachMarker) || !strings.Contains(note, "<zemi-note>\ntick it\n</zemi-note>\n</attached-files>") {
		t.Fatalf("agent.Note = %q", note)
	}
	report := "Background agent a1 finished." + agent.AttachMarker + "\nsee @main.go\n</attached-files>"
	if got, _ := h.expandAttachments(report); got != report {
		t.Fatalf("model-written text behind the marker must not attach: %q", got)
	}
	if plain, _ := h.expandAttachments("no references here"); plain != "no references here" {
		t.Fatalf("plain text changed: %q", plain)
	}
}

// TestChipAttachment: a chip attaches the copy its bare name names, the drops
// copy winning over a project file of the same name, while a name with a
// separator is the project's and one climbing out attaches nothing.
func TestChipAttachment(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	name, err := h.saveDrop("notes.txt", []byte("dropped copy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, name), []byte("the project's"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := h.expandAttachments("see [File " + name + "]")
	if !strings.Contains(got, "dropped copy") || strings.Contains(got, "the project's") {
		t.Fatalf("the copy must win over the project file: %q", got)
	}
	if got, _ := h.expandAttachments("see [File ../attach_test.go]"); got != "see [File ../attach_test.go]" {
		t.Fatalf("a chip must not climb out of the project: %q", got)
	}
	long, err := h.saveDrop(strings.Repeat("n", 200)+".txt", []byte("long"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(long)); n > dropNameMax || filepath.Ext(long) != ".txt" {
		t.Fatalf("a name travels in a token: %d runes, %q", n, long)
	}
}

// TestAttachLabel: the popup's token for a project root file keeps the ./ that
// tells it apart from the drops directory's bare names, so another project's
// copy of that name cannot take the project file's place nor skip its read
// check.
func TestAttachLabel(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	for _, c := range []struct{ rel, want string }{
		{"internal/ui/app.go", "[File internal/ui/app.go]"},
		{"shot.png", "[Image ./shot.png]"},
		{"notes.txt", "[File ./notes.txt]"},
	} {
		if got := h.AttachLabel(c.rel); got != c.want {
			t.Errorf("AttachLabel(%q) = %q, want %q", c.rel, got, c.want)
		}
	}
	// a copy another project left under a name this project also has
	name, err := h.saveDrop("notes.txt", []byte("another project's"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, name), []byte("the project's"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := h.expandAttachments(h.AttachLabel(name))
	if !strings.Contains(got, "the project's") || strings.Contains(got, "another project's") {
		t.Fatalf("the project's own file must attach: %q", got)
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
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale drop survived: %v", err)
	}
}

// TestImageAttachment: an @image becomes a file part after the text, with a
// note naming it behind the marker.
func TestImageAttachment(t *testing.T) {
	h, _ := newTestHostDB(t)
	png := tinyPNG()
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
	if !ok || !strings.HasPrefix(text.Text, "what is wrong in @shot.png"+agent.AttachMarker) || !strings.Contains(text.Text, `<image path="shot.png">`) {
		t.Fatalf("text part = %#v", msg.Content[0])
	}
	img, ok := msg.Content[1].(fantasy.FilePart)
	if !ok || img.MediaType != "image/png" || string(img.Data) != string(png) {
		t.Fatalf("file part = %#v", msg.Content[1])
	}
}

// TestAttachPath: a dropped project file becomes its token, an image from
// elsewhere a copy that attaches under its own name, a directory or secret
// its path, and a bare chip name cannot climb out of the drops directory.
func TestAttachPath(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	out := t.TempDir()
	shot := filepath.Join(out, "Screen Shot 1.png")
	os.WriteFile(shot, tinyPNG(), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600)
	os.WriteFile(filepath.Join(out, ".env"), []byte("TOKEN=x"), 0o600)
	// an outside file the project root has too: the copy cannot take its name
	elsewhere := filepath.Join(t.TempDir(), "main.go")
	os.WriteFile(elsewhere, []byte("package other"), 0o600)

	for path, want := range map[string]string{
		filepath.Join(h.Dirs.ProjectDir, "main.go"): "[File ./main.go]",
		shot:                       "[Image Screen Shot 1.png]",
		out:                        out,
		filepath.Join(out, ".env"): filepath.Join(out, ".env"),
		elsewhere:                  "[File main-2.go]",
	} {
		if got := h.AttachPath(path); got != want {
			t.Errorf("AttachPath(%s) = %q, want %q", path, got, want)
		}
	}
	if again := h.AttachPath(shot); again != "[Image Screen Shot 1.png]" {
		t.Errorf("same content must keep its name: %q", again)
	}
	msg := h.composeUserMessage("what is this [Image Screen Shot 1.png] [Image ../attach_test.go]", true)
	if len(msg.Content) != 2 {
		t.Fatalf("parts = %d", len(msg.Content))
	}
}

// TestImagesRefusedContinues: a 400 over image input does not end the
// turn: images become notes and the prompt goes out again. Only a model
// that refuses images is remembered as text-only; one image too large
// leaves later images alone.
func TestImagesRefusedContinues(t *testing.T) {
	for _, c := range []struct {
		name, refusal, note string
		textOnly            bool
	}{
		{"no images", "<path> is not a multimodal model", "the current model does not take images", true},
		{"too large", "image exceeds 5 MB maximum", "too large to send", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "shot.png"), tinyPNG(), 0o600); err != nil {
				t.Fatal(err)
			}
			m := reflectStub(t, h, "ok")
			m.toolStep.Store(50_000)
			m.failOnce.Store(&fantasy.ProviderError{StatusCode: 400, Message: c.refusal})
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
			if h.takesImages(model) == c.textOnly || h.agents.runner.TextOnly != c.textOnly {
				t.Fatalf("text-only: model %v, runner %v, want %v", !h.takesImages(model), h.agents.runner.TextOnly, c.textOnly)
			}
			for _, msg := range h.agents.runner.History {
				for _, part := range msg.Content {
					if _, ok := part.(fantasy.FilePart); ok {
						t.Fatal("image part still in history")
					}
					if tp, ok := part.(fantasy.TextPart); ok && strings.Contains(tp.Text, "omitted: "+c.note) {
						return
					}
				}
			}
			t.Fatal("no omission note in history")
		})
	}
}

func TestImagesRefused(t *testing.T) {
	for _, c := range []struct {
		err          error
		want, tooBig bool
	}{
		{&fantasy.ProviderError{StatusCode: 400, Message: "<path> is not a multimodal model"}, true, false},
		{&fantasy.ProviderError{StatusCode: 400, Message: "Invalid content type. image_url is only supported by certain models."}, true, false},
		// a size refusal: the model keeps its images
		{&fantasy.ProviderError{StatusCode: 400, Message: "messages.0.content.1.image.source.base64: image exceeds 5 MB maximum"}, true, true},
		{&fantasy.ProviderError{StatusCode: 400, Message: "max_tokens too large"}, false, false},
		{&fantasy.ProviderError{StatusCode: 500, Message: "image service down"}, false, false},
		{errors.New("image"), false, false},
		{nil, false, false},
	} {
		if got, big := imagesRefused(c.err), imageTooLarge(c.err); got != c.want || big != c.tooBig {
			t.Errorf("%v: refused %v too large %v, want %v %v", c.err, got, big, c.want, c.tooBig)
		}
	}
}

// tinyPNG is a real 1x1 image: attach decodes what it sends.
func tinyPNG() []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, 1, 1)))
	return b.Bytes()
}
