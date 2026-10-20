package hostapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNearestInstructions: a package's own AGENTS.md rides the first read of
// a file under it, once per session. Files already in the prompt do not
// repeat, which is every instruction file from the repo root down, nor does
// a CLAUDE.md beside an AGENTS.md.
func TestNearestInstructions(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	pkg := filepath.Join(h.Dirs.ProjectDir, "sub")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("root rules"), 0o644)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "CLAUDE.md"), []byte("claude rules"), 0o644)
	os.WriteFile(filepath.Join(pkg, "AGENTS.md"), []byte("use tabs here"), 0o644)
	file := filepath.Join(pkg, "x.go")
	os.WriteFile(file, []byte("package sub\n"), 0o644)

	if got := h.nearestInstructions(file, h.instructionFiles(), h.instructionSent); got != "" {
		t.Fatalf("an untrusted project's instructions must not load: %q", got)
	}
	h.SetProjectTrusted(true)
	if got := h.nearestInstructions(file, h.instructionFiles(), h.instructionSent); !strings.Contains(got, "use tabs here") {
		t.Fatalf("first read must carry the package instructions: %q", got)
	}
	if got := h.nearestInstructions(file, h.instructionFiles(), h.instructionSent); got != "" {
		t.Fatalf("they must not repeat in the same session: %q", got)
	}
	h.compactDone(0, false)
	if got := h.nearestInstructions(file, h.instructionFiles(), h.instructionSent); !strings.Contains(got, "use tabs here") {
		t.Fatalf("compaction clears the read that carried them, so they come back: %q", got)
	}
	if got := h.nearestInstructions(filepath.Join(h.Dirs.ProjectDir, "main.go"), h.instructionFiles(), h.instructionSent); got != "" {
		t.Fatalf("the root AGENTS.md is already in the prompt, and CLAUDE.md beside it is never read: %q", got)
	}

	// a sibling directory whose name starts with the repo root's name is
	// outside the repo, however much of the path string it shares
	sibling := h.Dirs.ProjectDir + "2"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sibling) })
	os.WriteFile(filepath.Join(sibling, "AGENTS.md"), []byte("not ours"), 0o644)
	if got := h.nearestInstructions(filepath.Join(sibling, "x.go"), h.instructionFiles(), h.instructionSent); got != "" {
		t.Fatalf("nothing outside the repo may be read: %q", got)
	}

	leak := filepath.Join(h.Dirs.ProjectDir, "leak")
	os.MkdirAll(leak, 0o755)
	if err := os.Symlink(filepath.Join(sibling, "AGENTS.md"), filepath.Join(leak, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if got := h.nearestInstructions(filepath.Join(leak, "x.go"), h.instructionFiles(), h.instructionSent); got != "" {
		t.Fatalf("a symlink must not lead out of the repo: %q", got)
	}
}
