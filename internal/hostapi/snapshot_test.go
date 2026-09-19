package hostapi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/perm"
)

func TestBashChanges(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	dir := h.Dirs.ProjectDir
	write := func(name, text string) {
		os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644)
	}
	write(".gitignore", "ignored\n")
	write("a.txt", "one\ntwo\n")
	write("staged.txt", "s\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "a.txt", ".gitignore"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}, {"add", "staged.txt"}} {
		if out, err := h.runGit(dir, nil, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if note, diff, _ := h.bashChanges(dir)(); note != "" || diff != "" {
		t.Fatalf("nothing changed, got %q %q", note, diff)
	}
	done := h.bashChanges(dir)
	write("a.txt", "one\nTWO\n")
	write("new.txt", "hi\n")
	write(".env", "KEY=hunter2\n")
	write("ignored", "x\n")
	write("tool.txt", "by the edit tool\n")
	h.noteWrite(filepath.Join(dir, "tool.txt"))
	note, diff, paths := done()

	if note != "files changed: .env, a.txt, new.txt" {
		t.Errorf("note: %q", note)
	}
	root := perm.RealPath(dir)
	if want := []string{filepath.Join(root, ".env"), filepath.Join(root, "a.txt"), filepath.Join(root, "new.txt")}; !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	for _, want := range []string{"A .env (content hidden", "M a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO", "A new.txt\n@@ -0,0 +1 @@\n+hi"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	for _, bad := range []string{"hunter2", "ignored", "tool.txt", "staged.txt", snapOverlap} {
		if strings.Contains(diff, bad) {
			t.Errorf("diff holds %q:\n%s", bad, diff)
		}
	}
	if now, _ := h.git(dir, "status", "--porcelain"); !strings.Contains(now, "A  staged.txt") || !strings.Contains(now, "?? new.txt") {
		t.Errorf("the user's index moved: %q", now)
	}

	// two calls at once cannot be told apart
	first, second := h.bashChanges(dir), h.bashChanges(dir)
	write("b.txt", "b\n")
	first()
	if _, diff, _ := second(); !strings.HasPrefix(diff, snapOverlap) {
		t.Errorf("overlap not labeled:\n%s", diff)
	}
	if _, diff, _ := h.bashChanges(t.TempDir())(); diff != "" {
		t.Errorf("no repository, got %q", diff)
	}
}
