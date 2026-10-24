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
	"github.com/metafates/zemi/internal/tools"
)

// TestPlusSkillAttachment: a +name token in the message attaches the skill
// behind the marker, body as written - the model binds the request from the
// typed message; a repeated mention attaches once and a name nothing offers
// stays as typed, byte for byte.
func TestPlusSkillAttachment(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	dir := h.userSkillsDir()
	writeSkill(t, dir, "lint", "lint the tree", "")
	write := func(name, front, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("---\n"+front+"---\n\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("humanize", "name: humanize\ndescription: make it human\n", "Rewrite $ARGUMENTS in a human voice.")
	write("big", "name: big\ndescription: huge\n", strings.Repeat("step\n", tools.SkillMaxOut/4))
	h.syncSkills()
	if err := h.setSkillEnabled("lint", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "notes.md"), []byte("project notes"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, text, want string }{
		{"body as written", "+humanize tighten the prose", "Rewrite $ARGUMENTS in a human voice."},
		{"a disabled skill attaches", "+lint go", "Body."},
		{"a big skill is cut", "+big", "Read the cut lines with the read tool"},
	} {
		got, _ := h.expandAttachments(c.text, true)
		typed, tail := agent.CutAttached(got)
		if typed != c.text || !strings.Contains(tail, "<skill>") || !strings.Contains(tail, c.want) {
			t.Errorf("%s: typed %q, tail:\n%s", c.name, typed, tail)
		}
	}
	if n := strings.Count(mustAttached(t, h, "+humanize a then +lint the tree"), "<skill>"); n != 2 {
		t.Errorf("two skills attached %d elements", n)
	}
	// a repeated mention attaches once, like a repeated @file, and both
	// tokens survive in the typed text
	if n := strings.Count(mustAttached(t, h, "+humanize a +humanize b"), "<skill>"); n != 1 {
		t.Errorf("a repeated skill attached %d elements", n)
	}
	if typed, _, _ := strings.Cut(mustExpand(t, h, "+humanize a +humanize b"), agent.AttachMarker); typed != "+humanize a +humanize b" {
		t.Errorf("the repeated token was rewritten: %q", typed)
	}
	// the typed message carries the request: the body's $ARGUMENTS stays
	// literal and the file beside the mention attaches once
	got := mustAttached(t, h, "+humanize this @notes.md")
	if n := strings.Count(got, "<file path="); n != 1 || !strings.Contains(got, "Rewrite $ARGUMENTS") || !strings.Contains(got, "project notes") {
		t.Errorf("mention with a file attached %d files: %q", n, got)
	}
	// a token the regex takes but no skill answers is prose, whole
	for _, text := range []string{"diff:\n+fmt.Println(x)", "+5 tasks", "C++", "a+b", "unknown +nosuch here"} {
		if got, _ := h.expandAttachments(text, true); got != text {
			t.Errorf("%q changed to %q", text, got)
		}
	}
}

// mustAttached is expandAttachments' trailer for text.
func mustAttached(t *testing.T, h *Host, text string) string {
	t.Helper()
	_, tail := agent.CutAttached(mustExpand(t, h, text))
	return tail
}

// mustExpand is expandAttachments' text for text.
func mustExpand(t *testing.T, h *Host, text string) string {
	t.Helper()
	got, _ := h.expandAttachments(text, true)
	return got
}

// TestExpandAttachments: project files referenced with @ attach; files
// outside the project, missing ones and symlinks leading out of it do not.
func TestExpandAttachments(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	proj := h.Dirs.ProjectDir
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proj, "main.go"), "package main // attached")
	outside := filepath.Join(t.TempDir(), "creds")
	write(outside, "outside-secret")
	if err := os.Symlink(outside, filepath.Join(proj, "notes.md")); err != nil {
		t.Fatal(err)
	}

	got, _ := h.expandAttachments("look at @main.go, @notes.md and @missing.go", true)
	if !strings.HasPrefix(got, "look at @main.go, @notes.md and @missing.go"+agent.AttachMarker) {
		t.Fatalf("typed text must lead, then the marker: %q", got)
	}
	if !strings.Contains(got, `<file path="main.go">`) || !strings.Contains(got, "package main // attached") {
		t.Fatalf("main.go not attached: %q", got)
	}
	for _, leak := range []string{"outside-secret", "missing.go\">"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q must not attach: %q", leak, got)
		}
	}
	pipe := filepath.Join(proj, "pipe")
	if exec.Command("mkfifo", pipe).Run() == nil {
		done := make(chan string)
		go func() { s, _ := h.expandAttachments("@pipe", true); done <- s }()
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
	got, _ = h.expandAttachments("@big.txt @blob.txt", true)
	for _, want := range []string{
		`<file path="big.txt">not attached: it is over the 64 KB`,
		`<file path="blob.txt">not attached: it is not text`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}

	// the same file named twice attaches once: a repeated @word, a chip
	// beside its @, and a symlink alias resolving to the same abs
	if err := os.Symlink(filepath.Join(proj, "main.go"), filepath.Join(proj, "alias.md")); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"see @main.go and @main.go again",
		"see @main.go and [File main.go]",
		"see @main.go and @alias.md",
	} {
		if n := strings.Count(mustAttached(t, h, text), "<file path="); n != 1 {
			t.Errorf("%q attached %d file elements", text, n)
		}
	}

	// zemi's own notes are an element of the trailer, like files and reports
	if note := agent.Note("tick it"); !strings.HasPrefix(note, agent.AttachMarker) || !strings.Contains(note, "<zemi-note>\ntick it\n</zemi-note>\n</attached-files>") {
		t.Fatalf("agent.Note = %q", note)
	}
	report := "Background agent a1 finished." + agent.AttachMarker + "\nsee @main.go\n</attached-files>"
	if got, _ := h.expandAttachments(report, true); got != report {
		t.Fatalf("model-written text behind the marker must not attach: %q", got)
	}
	if plain, _ := h.expandAttachments("no references here", true); plain != "no references here" {
		t.Fatalf("plain text changed: %q", plain)
	}
}

// TestChipAttachment: a chip attaches the copy its bare name names, the drops
// copy winning over a project file of the same name, while a name with a
// separator is the project's and one climbing out attaches nothing.
func TestChipAttachment(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	name, err := h.saveDrop("notes.txt", []byte("dropped copy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, name), []byte("the project's"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := h.expandAttachments("see [File "+name+"]", true)
	if !strings.Contains(got, "dropped copy") || strings.Contains(got, "the project's") {
		t.Fatalf("the copy must win over the project file: %q", got)
	}
	if got, _ := h.expandAttachments("see [File ../attach_test.go]", true); got != "see [File ../attach_test.go]" {
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

// TestAttachLabel: the popup inserts an @reference, which no drops copy can
// answer, so another project's file of the same name cannot take the project
// file's place nor skip its read check. A path an @ word would not read back
// whole, or that no token carries, keeps the form that does.
func TestAttachLabel(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	for _, c := range []struct{ rel, want string }{
		{"internal/ui/app.go", "@internal/ui/app.go"},
		{"shot.png", "@shot.png"},
		{"notes.txt", "@notes.txt"},
		{"a]b.go", "@a]b.go"},
		{"todo!", "[File ./todo!]"},
		{"docs/My Notes.md", "[File docs/My Notes.md]"},
	} {
		if got := h.AttachLabel(c.rel); got != c.want {
			t.Errorf("AttachLabel(%q) = %q, want %q", c.rel, got, c.want)
		}
	}
	tricky := filepath.Join(h.Dirs.ProjectDir, "My ]Notes.md")
	if got, want := h.AttachLabel("My ]Notes.md"), tools.ShellQuote(tricky); got != want {
		t.Errorf("AttachLabel of a path no token carries = %q, want %q", got, want)
	}
	// a name ending in punctuation the @ word trims must not resolve to the
	// sibling without it
	for name, text := range map[string]string{"x": "PLAIN", "x!": "BANG"} {
		if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := h.expandAttachments(h.AttachLabel("x!"), true)
	if !strings.Contains(got, "BANG") || strings.Contains(got, "PLAIN") {
		t.Fatalf("a trimmed name attached another file: %q", got)
	}
	// a copy another project left under a name this project also has
	name, err := h.saveDrop("notes.txt", []byte("another project's"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, name), []byte("the project's"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ = h.expandAttachments(h.AttachLabel(name), true)
	if !strings.Contains(got, "the project's") || strings.Contains(got, "another project's") {
		t.Fatalf("the project's own file must attach: %q", got)
	}
}

// TestDropsSweptAfterTTL: idle drop copies age out, one attached again stays.
func TestDropsSweptAfterTTL(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestAttachPath: a dropped project file becomes its @reference where an @
// word reads it back whole, an image from elsewhere a copy that attaches under
// its own name, a directory its path, and a bare chip name cannot
// climb out of the drops directory.
func TestAttachPath(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	out := t.TempDir()
	shot := filepath.Join(out, "Screen Shot 1.png")
	os.WriteFile(shot, tinyPNG(), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "My Notes.md"), []byte("notes"), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "todo!"), []byte("notes"), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "a]b.go"), []byte("package a"), 0o600)
	os.WriteFile(filepath.Join(out, ".env"), []byte("TOKEN=x"), 0o600)
	// an outside file the project root has too: the copy cannot take its name
	elsewhere := filepath.Join(t.TempDir(), "main.go")
	os.WriteFile(elsewhere, []byte("package other"), 0o600)

	for path, want := range map[string]string{
		filepath.Join(h.Dirs.ProjectDir, "main.go"):     "@main.go",
		filepath.Join(h.Dirs.ProjectDir, "a]b.go"):      "@a]b.go",
		filepath.Join(h.Dirs.ProjectDir, "My Notes.md"): "[File ./My Notes.md]",
		filepath.Join(h.Dirs.ProjectDir, "todo!"):       "[File ./todo!]",
		shot:                       "[Image Screen Shot 1.png]",
		out:                        out,
		filepath.Join(out, ".env"): "[File env]",
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
			model := h.roleModel("chat")
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
