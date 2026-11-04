package hostapi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/perm"
)

func TestShellChanges(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.StateHome = t.TempDir()
	dir := h.Dirs.ProjectDir
	write := func(name, text string) {
		os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644)
	}
	write(".gitignore", "ignored\n")
	write("a.txt", "one\ntwo\n")
	write("staged.txt", "s\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "a.txt", ".gitignore"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}, {"add", "staged.txt"}} {
		if out, err := h.git(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	// the first snapshot removes the stores of gone processes and of an
	// earlier process with this pid, and leaves a live one's alone
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	tmp, _ := h.TempDir()
	stores := map[int]bool{dead.Process.Pid: false, os.Getpid(): false, os.Getppid(): true}
	for pid := range stores {
		os.MkdirAll(filepath.Join(tmp, fmt.Sprintf("snap-%d-0.objects", pid)), 0o700)
	}
	if note, diff, _ := h.shellChanges(dir)(); note != "" || diff != "" {
		t.Fatalf("nothing changed, got %q %q", note, diff)
	}
	for pid, kept := range stores {
		if _, err := os.Stat(filepath.Join(tmp, fmt.Sprintf("snap-%d-0.objects", pid))); (err == nil) != kept {
			t.Errorf("store of pid %d kept: %v, want %v", pid, err == nil, kept)
		}
	}
	done := h.shellChanges(dir)
	write("a.txt", "one\nTWO\n")
	write("new.txt", "hi\n")
	write("blob.bin", "\x00\x01")
	write(".env", "KEY=hunter2\n")
	write("api_secret.txt", "shh\n")
	write("ignored", "x\n")
	write("tool.txt", "by the edit tool\n")
	h.noteWrite(filepath.Join(dir, "tool.txt"))
	note, diff, paths := done()

	if note != "files changed: .env, a.txt, api_secret.txt, blob.bin, new.txt" {
		t.Errorf("note: %q", note)
	}
	root := perm.RealPath(dir)
	if want := []string{filepath.Join(root, ".env"), filepath.Join(root, "a.txt"), filepath.Join(root, "api_secret.txt"), filepath.Join(root, "blob.bin"), filepath.Join(root, "new.txt")}; !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	for _, want := range []string{"A .env\n@@ -0,0 +1 @@\n+KEY=hunter2", "A api_secret.txt\n@@ -0,0 +1 @@\n+shh", "M a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO", "A new.txt\n@@ -0,0 +1 @@\n+hi", "A blob.bin\nbinary, no text to show"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	for _, bad := range []string{"ignored", "tool.txt", "staged.txt", _snapOverlap} {
		if strings.Contains(diff, bad) {
			t.Errorf("diff holds %q:\n%s", bad, diff)
		}
	}
	// the snapshot keeps its objects out of the repository's own store
	h.snapshot(dir)
	if objs, _ := h.git(dir, "cat-file", "--batch-all-objects", "--batch"); strings.Contains(objs, "shh") {
		t.Errorf("the repository's object store holds %q", "shh")
	}
	if now, _ := h.git(dir, "status", "--porcelain"); !strings.Contains(now, "A  staged.txt") || !strings.Contains(now, "?? new.txt") {
		t.Errorf("the user's index moved: %q", now)
	}

	// two calls at once cannot be told apart
	first, second := h.shellChanges(dir), h.shellChanges(dir)
	write("b.txt", "b\n")
	first()
	if _, diff, _ := second(); !strings.HasPrefix(diff, _snapOverlap) {
		t.Errorf("overlap not labeled:\n%s", diff)
	}

	// a gc prunes a blob the warm index still names: the next snapshot starts cold
	for _, args := range [][]string{{"rm", "--cached", "-q", "staged.txt"}, {"prune", "--expire=now"}} {
		if out, err := h.git(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if _, _, tree := h.snapshot(dir); tree == "" {
		t.Error("no snapshot after a prune")
	}
	if _, diff, _ := h.shellChanges(t.TempDir())(); diff != "" {
		t.Errorf("no repository, got %q", diff)
	}
}

// TestSnapshotRacyEntry: a file rewritten at the same size in the second its
// index was written looks clean by stat alone; the first snapshot must still
// hash it, or a shell row misses the change the second snapshot should show.
func TestSnapshotRacyEntry(t *testing.T) {
	h, _ := newTestHostDB(t)
	dir := h.Dirs.ProjectDir
	a := filepath.Join(dir, "a.txt")
	// one second an hour ago stands in for "the same second"; checkStat
	// minimal leaves out the ctime a real race would share too
	then := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.WriteFile(a, []byte("v1"), 0o644)
	os.Chtimes(a, then, then)
	for _, args := range [][]string{{"init", "-q"}, {"config", "core.checkStat", "minimal"}, {"config", "core.trustCtime", "false"}, {"add", "-A"}} {
		if out, err := h.git(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(a, []byte("v2"), 0o644)
	os.Chtimes(a, then, then)
	os.Chtimes(filepath.Join(dir, ".git", "index"), then, then)
	root, env, tree := h.snapshot(dir)
	if got, err := h.gitEnv(root, env, "cat-file", "-p", tree+":a.txt"); got != "v2" {
		t.Fatalf("snapshot holds %q (%v), want v2", got, err)
	}
}
