package hostapi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

func TestBashChanges(t *testing.T) {
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
	if note, diff, _ := h.bashChanges(dir)(); note != "" || diff != "" {
		t.Fatalf("nothing changed, got %q %q", note, diff)
	}
	for pid, kept := range stores {
		if _, err := os.Stat(filepath.Join(tmp, fmt.Sprintf("snap-%d-0.objects", pid))); (err == nil) != kept {
			t.Errorf("store of pid %d kept: %v, want %v", pid, err == nil, kept)
		}
	}
	done := h.bashChanges(dir)
	write("a.txt", "one\nTWO\n")
	write("new.txt", "hi\n")
	write("blob.bin", "\x00\x01")
	write(".env", "KEY=hunter2\n")
	write("api_secret.txt", "shh\n")
	write("ignored", "x\n")
	write("tool.txt", "by the edit tool\n")
	h.noteWrite(filepath.Join(dir, "tool.txt"))
	note, diff, paths := done()

	if note != "files changed: a.txt, api_secret.txt, blob.bin, new.txt" {
		t.Errorf("note: %q", note)
	}
	root := perm.RealPath(dir)
	if want := []string{filepath.Join(root, "a.txt"), filepath.Join(root, "api_secret.txt"), filepath.Join(root, "blob.bin"), filepath.Join(root, "new.txt")}; !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	for _, want := range []string{"A api_secret.txt (content hidden", "M a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO", "A new.txt\n@@ -0,0 +1 @@\n+hi", "A blob.bin\nbinary, no text to show"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	for _, bad := range []string{"hunter2", "shh", "ignored", "tool.txt", "staged.txt", snapOverlap} {
		if strings.Contains(diff, bad) {
			t.Errorf("diff holds %q:\n%s", bad, diff)
		}
	}
	// the sandbox hides .env, so no object store may hold it, and the rest of
	// a snapshot stays out of the repository's own
	_, env, _ := h.snapshot(dir)
	for _, c := range []struct {
		env []string
		bad string
	}{{env, "hunter2"}, {nil, "shh"}} {
		if objs, _ := h.gitEnv(dir, c.env, "cat-file", "--batch-all-objects", "--batch"); strings.Contains(objs, c.bad) {
			t.Errorf("an object store holds %q", c.bad)
		}
	}
	if now, _ := h.git(dir, "status", "--porcelain"); !strings.Contains(now, "A  staged.txt") || !strings.Contains(now, "?? new.txt") {
		t.Errorf("the user's index moved: %q", now)
	}

	// a command that outlived its turn checkpoints nothing: a later prompt's
	// rewind must not restore what an earlier command changed
	h.DB.CreateSession("s", "", "t", "")
	for _, id := range []string{"m1", "m2"} {
		h.DB.AddMessage(&store.Message{ID: id, SessionID: "s", Role: "user", PartsJSON: "{}"})
	}
	h.agents.mu.Lock()
	h.agents.sessionID, h.agents.turnMsgID = "s", "m1"
	h.agents.mu.Unlock()
	late, now := h.bashChanges(dir), h.bashChanges(dir)
	write("a.txt", "three\n")
	h.agents.mu.Lock()
	h.agents.turnMsgID = "m2"
	h.agents.mu.Unlock()
	late()
	h.agents.mu.Lock()
	h.agents.turnMsgID = "m1"
	h.agents.mu.Unlock()
	now()
	if paths, _ := h.DB.CheckpointPaths("s"); len(paths["m1"]) != 1 || len(paths["m2"]) != 0 {
		t.Errorf("checkpoints = %v, want a.txt under m1 only", paths)
	}

	// two calls at once cannot be told apart
	first, second := h.bashChanges(dir), h.bashChanges(dir)
	write("b.txt", "b\n")
	first()
	if _, diff, _ := second(); !strings.HasPrefix(diff, snapOverlap) {
		t.Errorf("overlap not labeled:\n%s", diff)
	}
	// a file too large to checkpoint is never read whole and is said so once per turn
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	write("big.log", strings.Repeat("w", maxCheckpointBytes+1))
	for _, fill := range []string{"x", "y"} {
		done := h.bashChanges(dir)
		write("big.log", strings.Repeat(fill, maxCheckpointBytes+1))
		done()
	}
	said := 0
	for _, n := range h.evlog.notes {
		if strings.Contains(n.body, "big.log") {
			said++
		}
	}
	if said != 1 {
		t.Errorf("big.log was reported %d times, want 1", said)
	}
	if log, _ := os.ReadFile(trace); regexp.MustCompile(`cat-file -p \S*big\.log`).Match(log) {
		t.Error("big.log was read whole")
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
	if _, diff, _ := h.bashChanges(t.TempDir())(); diff != "" {
		t.Errorf("no repository, got %q", diff)
	}
}

// TestSnapshotRacyEntry: a file rewritten at the same size in the second its
// index was written looks clean by stat alone; the first snapshot must still
// hash it, or a bash row misses the change and a rewind restores the old text.
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
