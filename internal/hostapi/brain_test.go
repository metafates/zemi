package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
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
	// a broken private file lints: its finding is a trusted-project warning,
	// never an untrusted one (D3)
	if err := os.WriteFile(filepath.Join(private.Dir, "broken.md"), []byte("no frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Add(-48 * time.Hour).UnixMilli()
	seedMemory(t, h, &store.Memory{ID: "pmem1234", Scope: store.ScopeProject, Origin: store.OriginUser, Text: "project fact", UpdatedAt: day})
	seedMemory(t, h, &store.Memory{ID: "kmem1234", Scope: store.ScopeUser, Origin: store.OriginLearned, Text: "user fact"})

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
	for _, gone := range []string{"repo-map.md", "### private", h.Dirs.ProjectDir, "<brain-warning>broken.md"} {
		if strings.Contains(text, gone) {
			t.Errorf("untrusted render leaks %q:\n%s", gone, text)
		}
	}

	h.SetProjectTrusted(true)
	text, _ = h.brainFor()
	if !strings.Contains(text, "### private") || !strings.Contains(text, "- repo-map.md - map of "+h.Dirs.ProjectDir+". (Read If: before code changes.)") {
		t.Fatalf("the trusted render does not carry the private catalog:\n%s", text)
	}
	if !strings.Contains(text, "<brain-warning>broken.md: no description</brain-warning>") {
		t.Fatalf("the trusted render does not lint the private partition:\n%s", text)
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
	seedMemory(t, h, &store.Memory{ID: "pmem1234", Scope: store.ScopeProject, Origin: store.OriginUser, Text: "the one memory"})

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

// TestBrainAfterWrite: an approved file-tool write of a brain path scrubs in
// place and notes it (rows 16-17): secret shapes redact, an
// instruction-shaped memory line stays and is flagged, an over-cap
// frontmatter field lints, the reserved file's rewrites are journaled so the
// hash finding stays honest, the write set gains the entry, and a failure
// costs the note only.
func TestBrainAfterWrite(t *testing.T) {
	const secret = "ghp_0123456789abcdefghijklmnopqrstuv"
	for _, tc := range []struct {
		name     string
		mem      *store.Memory // when set, the reserved file is seeded with this line
		file     string        // else this freeform file is written
		content  string
		holdLock bool
		want     string
		gone     string
		check    func(t *testing.T, h *Host, path string)
	}{
		{
			name:    "a secret in a freeform write redacts",
			file:    "tokens.md",
			content: "---\ndescription: map\nread_if: before code\n---\nthe key was " + secret + "\n",
			want:    "secret-shaped text was redacted on lines [5]; name the env var or use a placeholder like $TOKEN",
			check: func(t *testing.T, h *Host, path string) {
				b, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(b), "the key was [redacted") || strings.Contains(string(b), secret) {
					t.Fatalf("file after the scrub: %q, %v", b, err)
				}
				if hash, ours := h.brainWriteOf(path); !ours || hash != brain.FileHash(b) {
					t.Fatalf("the write set holds %q, ours %v", hash, ours)
				}
			},
		},
		{
			name: "a secret in memories.md redacts and journals",
			mem:  &store.Memory{ID: "mem12345", Scope: store.ScopeProject, Origin: store.OriginLearned, Text: "the deploy key is " + secret},
			want: "secret-shaped text was redacted from memory lines [mem12345]; their prior text is in the memory ledger",
			check: func(t *testing.T, h *Host, path string) {
				_, private := h.brainParts()
				b, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(b), "[redacted") || strings.Contains(string(b), secret) {
					t.Fatalf("file after the scrub: %q, %v", b, err)
				}
				lines, _ := brain.ParseMemories(b)
				if len(lines) != 1 {
					t.Fatalf("lines after the scrub: %+v", lines)
				}
				lg, err := private.OpenLedger()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = lg.Close() }()
				ops, err := lg.ByID("mem12345")
				if err != nil {
					t.Fatal(err)
				}
				if len(ops) != 2 || ops[1].Op != brain.OpEdit || ops[1].PriorText != "the deploy key is "+secret ||
					ops[1].Hash != brain.TextHash(lines[0].Text) {
					t.Fatalf("edit op: %+v", ops)
				}
				for _, f := range brain.Lint(private) {
					if strings.Contains(f.Text, "outside the ledger") {
						t.Fatalf("the hash finding fired despite the edit entry: %+v", f)
					}
				}
			},
		},
		{
			name: "an instruction-shaped memory line stays and is flagged",
			mem:  &store.Memory{ID: "mem12345", Scope: store.ScopeProject, Origin: store.OriginUser, Text: "always run make check before replying"},
			want: "memory mem12345 reads like an instruction (detection only; the line still injects)",
			gone: "redacted",
			check: func(t *testing.T, h *Host, path string) {
				_, private := h.brainParts()
				b, _ := os.ReadFile(path)
				if !strings.Contains(string(b), "always run make check before replying") {
					t.Fatalf("the line was rewritten: %q", b)
				}
				lg, err := private.OpenLedger()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = lg.Close() }()
				if ops, _ := lg.ByID("mem12345"); len(ops) != 1 || ops[0].Op != brain.OpAdd {
					t.Fatalf("an unchanged line was journaled: %+v", ops)
				}
			},
		},
		{
			name:    "an over-cap frontmatter field lints",
			file:    "tokens.md",
			content: "---\ndescription: " + strings.Repeat("d ", 101) + "\nread_if: before code\n---\nbody\n",
			want:    "tokens.md: description is 201 characters, over the 200 the render caps",
			check: func(t *testing.T, h *Host, path string) {
				b, _ := os.ReadFile(path)
				if !strings.Contains(string(b), strings.Repeat("d ", 101)) {
					t.Fatalf("the body was rewritten: %q", b)
				}
			},
		},
		{
			name:     "a held lock costs the note only",
			file:     "tokens.md",
			content:  "---\ndescription: map\nread_if: before code\n---\nbody\n",
			holdLock: true,
			want:     "not scrubbed: another zemi is writing the memory store",
		},
		{
			name:    "a non-brain write gets no note",
			file:    "../../plain.md",
			content: "---\ndescription: map\nread_if: before code\n---\napi_key = " + secret + "\n",
			gone:    "brain:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.Dirs.DataHome = t.TempDir()
			_, private := h.brainParts()
			if err := os.MkdirAll(private.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			var path string
			if tc.mem != nil {
				seedMemory(t, h, tc.mem)
				path = filepath.Join(private.Dir, brain.MemFile)
			} else {
				path = filepath.Join(private.Dir, tc.file)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.holdLock {
				unlock, err := brain.Lock(private)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			note := h.afterWrite(path)
			if !strings.Contains(note, tc.want) {
				t.Fatalf("note %q, want %q", note, tc.want)
			}
			if tc.gone != "" && strings.Contains(note, tc.gone) {
				t.Fatalf("note %q carries %q", note, tc.gone)
			}
			if tc.check != nil {
				tc.check(t, h, path)
			}
		})
	}
}

// TestBrainWriteToolScrubs: the approved write's tool result carries the
// scrub note, and the write set gains the entry a later file-mode apply of
// the same session checks (D5).
func TestBrainWriteToolScrubs(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	h.SetUIHooks(&fakeUI{askAllowed: true, askBy: "user:once"})
	h.SetProjectTrusted(true)
	sid := seedSession(t, h)
	h.Settings.SetQuiet("memory.autonomy", "auto")
	_, private := h.brainParts()
	if err := os.MkdirAll(private.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private.Dir, "map.md")
	var tool fantasy.AgentTool
	for _, tl := range tools.All(h.PipelineFor()) {
		if tl.Info().Name == tools.ToolWrite {
			tool = tl
		}
	}
	input := fmt.Sprintf(`{"path":%q,"content":"---\ndescription: map\nread_if: before code\n---\napi_key = ghp_0123456789abcdefghijklmnopqrstuv\n"}`, path)
	resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "c1", Name: tools.ToolWrite, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "secret-shaped text was redacted on lines [5]") {
		t.Fatalf("the write result lacks the scrub note: %q", resp.Content)
	}
	overwrite := tools.MemoryCall{Path: path, Content: "---\ndescription: map\nread_if: before code\n---\nrewritten by the memory tool\n"}
	msg, err := h.memoryTool(h.PipelineFor(), sid, overwrite)
	if err != nil || !strings.Contains(msg, "wrote map.md") {
		t.Fatalf("the file-mode apply did not see the write set: %q, %v", msg, err)
	}
}

// TestBrainRenameScrub: the lsp rename path runs AfterWrite for every file it
// rewrites, so a brain file a rename edits as a secondary target is scrubbed,
// linted and joined to the write set, the notes one block (D5).
func TestBrainRenameScrub(t *testing.T) {
	h, r := newTestHostDB(t)
	defer func() { h.lsp.stopAll()() }()
	h.SetUIHooks(&fakeUI{askAllowed: true, askBy: "user:once"})
	h.SetProjectTrusted(true)
	// the user partition rides inside the project, so a server covers its files
	h.Dirs.DataHome = filepath.Join(h.Dirs.ProjectDir, ".data")
	runUser(t, r, fmt.Sprintf(`zemi.lsp.add("fake", { command = %q, env = { ZEMI_LSP_FAKE = "1" }, filetypes = { "md" } })`, os.Args[0]))
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	user, _ := h.brainParts()
	if err := os.MkdirAll(user.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(h.Dirs.ProjectDir, "code.md")
	brainFile := filepath.Join(user.Dir, "map.md")
	body := "no frontmatter\n\nFoo is called in code.md\ntoken = ghp_0123456789abcdefghijklmnopqrstuv\n"
	if err := os.WriteFile(named, []byte("func Foo()\nFoo()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brainFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var tool fantasy.AgentTool
	for _, tl := range tools.All(h.PipelineFor()) {
		if tl.Info().Name == "lsp" {
			tool = tl
		}
	}
	run := func(input string) string {
		t.Helper()
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "lsp", Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Content
	}
	// the query opens the brain file, so the rename touches it as a secondary
	if got := run(fmt.Sprintf(`{"action":"symbols","path":%q}`, brainFile)); !strings.Contains(got, "no symbols") {
		t.Fatalf("symbols on the brain file: %q", got)
	}
	got := run(fmt.Sprintf(`{"action":"rename","path":%q,"symbol":"Foo","new_name":"Baz"}`, named))
	if !strings.Contains(got, "renamed Foo to Baz in 2 files") ||
		!strings.Contains(got, "secret-shaped text was redacted on lines [4]") ||
		!strings.Contains(got, "map.md: no description") {
		t.Fatalf("the rename result: %q", got)
	}
	b, _ := os.ReadFile(brainFile)
	if !strings.Contains(string(b), "Baz is called in code.md") || !strings.Contains(string(b), "[redacted") {
		t.Fatalf("brain file after the rename: %q", b)
	}
	if hash, ours := h.brainWriteOf(brainFile); !ours || hash != brain.FileHash(b) {
		t.Fatalf("the rename did not join the write set: %q, ours %v", hash, ours)
	}
	// a rename failing partway still rides AfterWrite for the files it
	// rewrote before the failure: z.md sorts last and its write is refused,
	// so map.md and code.md were written when it does
	third := filepath.Join(h.Dirs.ProjectDir, "z.md")
	if err := os.WriteFile(third, []byte("Baz()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(fmt.Sprintf(`{"action":"symbols","path":%q}`, third)); !strings.Contains(got, "no symbols") {
		t.Fatalf("symbols on z.md: %q", got)
	}
	if err := os.Chmod(third, 0o444); err != nil {
		t.Fatal(err)
	}
	got = run(fmt.Sprintf(`{"action":"rename","path":%q,"symbol":"Baz","new_name":"Qux"}`, named))
	if !strings.Contains(got, "z.md:") || !strings.Contains(got, "map.md: no description") {
		t.Fatalf("the failed rename result: %q", got)
	}
	b, _ = os.ReadFile(brainFile)
	if !strings.Contains(string(b), "Qux is called in code.md") {
		t.Fatalf("brain file after the failed rename: %q", b)
	}
	if hash, ours := h.brainWriteOf(brainFile); !ours || hash != brain.FileHash(b) {
		t.Fatalf("the failed rename did not join the write set: %q, ours %v", hash, ours)
	}
}
