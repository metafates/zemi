package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/store"
)

// writeBrainFile lays one freeform brain file down.
func writeBrainFile(t *testing.T, dir, name, desc, readIf string, paths []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("---\ndescription: " + desc + "\nread_if: " + readIf + "\n")
	if len(paths) > 0 {
		b.WriteString("paths:\n")
		for _, p := range paths {
			b.WriteString("  - " + p + "\n")
		}
	}
	b.WriteString("---\nbody\n")
	if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// pinBrain runs a build point by hand: it renders and pins what the session
// would carry, the way ensureSession, ResumeSession and compactDone store it
// (a trust flip does it inside SetProjectTrusted).
func pinBrain(t *testing.T, h *Host) {
	t.Helper()
	text, globs := h.brainFor()
	h.agents.mu.Lock()
	h.agents.brain, h.agents.brainGlobs = text, globs
	h.agents.mu.Unlock()
}

// TestBrainKeyAndReplay: the render is pinned at the build points only, its
// hash moves the context key at a build, and the stored role-"context" row
// replays while the key matches.
func TestBrainKeyAndReplay(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	if err := h.ensureSession(nil, "hello"); err != nil {
		t.Fatal(err)
	}
	toolset, _ := h.profileTools(nil)
	h.pinPrompt(nil, toolset)
	h.agents.mu.Lock()
	pinned, key := h.agents.brain, h.agents.ctx.Key
	h.agents.mu.Unlock()
	if !strings.Contains(pinned, "# Brain") {
		t.Fatalf("no brain section in the context message:\n%s", pinned)
	}

	// a memory landing mid-session changes nothing until a build point
	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	h.agents.mu.Lock()
	still := h.agents.brain
	h.agents.mu.Unlock()
	if still != pinned {
		t.Fatal("the brain re-rendered outside a build point")
	}

	h.compactDone(0, false)
	h.pinPrompt(nil, toolset)
	h.agents.mu.Lock()
	rebuilt, newKey := h.agents.brain, h.agents.ctx.Key
	h.agents.mu.Unlock()
	if !strings.Contains(rebuilt, "deploy by hand") {
		t.Fatalf("the build after the memory does not carry it:\n%s", rebuilt)
	}
	if newKey == key {
		t.Fatal("the context key did not move with the render")
	}
	rows, err := h.DB.Messages(h.AgentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	if stored := contextIn(rows); stored.Key != newKey || !strings.Contains(stored.Text, "deploy by hand") {
		t.Fatal("the rebuilt render was not stored as the session's context row")
	}

	// a resume replays the stored row while its key matches
	h.agents.mu.Lock()
	pinKey, brainText := h.agents.pin.key, h.agents.brain
	h.agents.dropPin()
	h.agents.mu.Unlock()
	row := contextRow{Key: h.contextKey(pinKey, brainText), Text: "STORED"}
	h.agents.mu.Lock()
	h.agents.ctx = row
	h.agents.mu.Unlock()
	if _, _, ctx, _ := h.pinPrompt(nil, toolset); ctx != "STORED" {
		t.Fatalf("the stored row did not replay: %q", ctx)
	}
}

// TestBrainRenderTrust: the untrusted render carries the loop, the user
// catalog and the memory lines, and no byte of the private partition; trust
// adds the private catalog.
func TestBrainRenderTrust(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	if err := os.Mkdir(filepath.Join(h.Dirs.ProjectDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	user, private := h.brainParts()
	for _, dir := range []string{user.Dir, private.Dir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeBrainFile(t, user.Dir, "style.md", "writing style", "writing any reply", nil)
	writeBrainFile(t, private.Dir, "repo-map.md", "map of "+h.Dirs.ProjectDir, "before code changes", nil)
	day := time.Now().Add(-48 * time.Hour).UnixMilli()
	if err := h.DB.AddMemory(&store.Memory{ID: "pmem1234", Scope: store.ScopeProject, Origin: store.OriginUser, Text: "project fact", UpdatedAt: day}); err != nil {
		t.Fatal(err)
	}
	if err := h.GlobalDB.AddMemory(&store.Memory{ID: "kmem1234", Scope: store.ScopeUser, Origin: store.OriginLearned, Text: "user fact"}); err != nil {
		t.Fatal(err)
	}

	text, _ := h.brainFor()
	for _, want := range []string{
		"# Brain",
		"Consult the brain before you",
		"Private brain for this repository: " + private.Dir,
		"User brain, for every project: " + user.Dir,
		"## Catalog", "### user", "- style.md - writing style. (Read If: writing any reply.)",
		"## this project", "- [pmem1234 user 2d] project fact",
		"## every project", "- [kmem1234 learned] user fact",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("untrusted render lacks %q:\n%s", want, text)
		}
	}
	for _, gone := range []string{"repo-map.md", "### private", h.Dirs.ProjectDir} {
		if strings.Contains(text, gone) {
			t.Errorf("untrusted render leaks %q:\n%s", gone, text)
		}
	}

	h.SetProjectTrusted(true)
	text, _ = h.brainFor()
	if !strings.Contains(text, "### private") || !strings.Contains(text, "- repo-map.md - map of "+h.Dirs.ProjectDir+". (Read If: before code changes.)") {
		t.Fatalf("the trusted render does not carry the private catalog:\n%s", text)
	}
}

// TestBrainRebuildsOnTrustFlip: a trust flip with a session live rebuilds the
// pinned render at once, so a revoke drops the private catalog before the
// next send re-pins (D3) and a grant brings it back without a compaction.
func TestBrainRebuildsOnTrustFlip(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	if err := os.Mkdir(filepath.Join(h.Dirs.ProjectDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, private := h.brainParts()
	if err := os.MkdirAll(private.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeBrainFile(t, private.Dir, "repo-map.md", "map of "+h.Dirs.ProjectDir, "before code changes", nil)
	h.SetProjectTrusted(true)
	if err := h.ensureSession(nil, "hello"); err != nil {
		t.Fatal(err)
	}
	h.agents.mu.Lock()
	pinned := h.agents.brain
	h.agents.mu.Unlock()
	if !strings.Contains(pinned, "repo-map.md") {
		t.Fatalf("the trusted render lacks the private catalog:\n%s", pinned)
	}

	h.SetProjectTrusted(false)
	h.agents.mu.Lock()
	held := h.agents.brain
	h.agents.mu.Unlock()
	if strings.Contains(held, "repo-map.md") || strings.Contains(held, h.Dirs.ProjectDir) {
		t.Fatalf("the revoked render still carries the private catalog:\n%s", held)
	}

	h.SetProjectTrusted(true)
	h.agents.mu.Lock()
	back := h.agents.brain
	h.agents.mu.Unlock()
	if !strings.Contains(back, "repo-map.md") {
		t.Fatalf("the granted render lacks the private catalog:\n%s", back)
	}
}

// TestBrainCatalogCaps: a pathological catalog cannot push out the loop, the
// warnings or the memory lines; the cut markers name paths; the instruction
// sections' budget is untouched by the render.
func TestBrainCatalogCaps(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	if err := os.Mkdir(filepath.Join(h.Dirs.ProjectDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("run make check"), 0o644)
	h.SetProjectTrusted(true)
	user, _ := h.brainParts()
	if err := os.MkdirAll(user.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fat := "---\ndescription: " + strings.Repeat("d", 200) + "\nread_if: " + strings.Repeat("r", 200) + "\n---\nbody"
	for i := range brain.MaxCatalogFiles + 6 {
		if err := os.WriteFile(filepath.Join(user.Dir, fmt.Sprintf("map%02d.md", i)), []byte(fat), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(user.Dir, "broken.md"), []byte("no frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.AddMemory(&store.Memory{ID: "pmem1234", Scope: store.ScopeProject, Origin: store.OriginUser, Text: "the one memory"}); err != nil {
		t.Fatal(err)
	}

	text, _ := h.brainFor()
	for _, want := range []string{
		"- [pmem1234 user] the one memory",
		"<brain-warning>broken.md: no description</brain-warning>",
		"more in " + user.Dir,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the render lacks %q:\n%s", want, text)
		}
	}
	if len(text) > brain.Backstop {
		t.Errorf("the render is %d bytes, over the %d backstop", len(text), brain.Backstop)
	}

	toolset, _ := h.profileTools(nil)
	_, ctxMsg, _ := h.buildSystemParts(nil, toolset)
	if !strings.Contains(ctxMsg, "## Instructions: AGENTS.md\nrun make check") || strings.Contains(ctxMsg, "cut here") {
		t.Errorf("the brain render disturbed the instruction sections:\n%s", ctxMsg)
	}
}

// TestBrainPointers: the first read of a mapped path earns one line and the
// first write or edit another, each once per conversation; private globs arm
// in trusted projects only, user globs in every project; three pointers a
// result at most, the rest one aggregate line.
func TestBrainPointers(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	if err := os.Mkdir(filepath.Join(h.Dirs.ProjectDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	user, private := h.brainParts()
	for _, dir := range []string{user.Dir, private.Dir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeBrainFile(t, user.Dir, "style.md", "writing style", "writing any reply", []string{"docs/*.md"})
	writeBrainFile(t, private.Dir, "repo-map.md", "repo map", "before any code change", []string{"sub/*.go"})

	h.SetProjectTrusted(true)
	pinBrain(t, h)
	read := filepath.Join(h.Dirs.ProjectDir, "sub", "x.go")
	if got := h.mainInstructions(read); !strings.Contains(got, "repo-map.md in the brain maps this path (Read If: before any code change)") {
		t.Fatalf("no pointer on the first read: %q", got)
	}
	if got := h.mainInstructions(read); got != "" {
		t.Fatalf("the read pointer repeated: %q", got)
	}
	if got := h.afterWrite(read); !strings.Contains(got, "you edited a file mapped by repo-map.md (Read If: before any code change); check the map still holds") {
		t.Fatalf("no pointer on the write: %q", got)
	}
	if got := h.afterWrite(read); got != "" {
		t.Fatalf("the write pointer repeated: %q", got)
	}

	h.SetProjectTrusted(false)
	pinBrain(t, h)
	if got := h.mainInstructions(read); got != "" {
		t.Fatalf("a private pointer fired untrusted: %q", got)
	}
	doc := filepath.Join(h.Dirs.ProjectDir, "docs", "a.md")
	if err := os.MkdirAll(filepath.Dir(doc), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := h.mainInstructions(doc); !strings.Contains(got, "style.md in the brain maps this path (Read If: writing any reply)") {
		t.Fatalf("the user glob did not fire untrusted: %q", got)
	}

	h.SetProjectTrusted(true)
	for i := range 5 {
		writeBrainFile(t, private.Dir, fmt.Sprintf("map%d.md", i), "a map", "a reason", []string{"cap/*"})
	}
	pinBrain(t, h)
	got := h.mainInstructions(filepath.Join(h.Dirs.ProjectDir, "cap", "y.go"))
	if strings.Count(got, "in the brain maps this path") != 3 || !strings.Contains(got, "2 more brain files map this path") {
		t.Fatalf("the cap and aggregate are wrong: %q", got)
	}
	if got = h.mainInstructions(filepath.Join(h.Dirs.ProjectDir, "cap", "z.go")); strings.Count(got, "in the brain maps this path") != 2 || strings.Contains(got, "more brain files") {
		t.Fatalf("the collapsed pointers did not wait for their own result: %q", got)
	}
}
