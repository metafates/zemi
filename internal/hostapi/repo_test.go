// repo_test.go checks what gitBranch reads out of the files git keeps.

package hostapi

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

// newHostAt is a host whose project is dir; gitBranch reads Dirs alone, so no
// boot is needed.
func newHostAt(t *testing.T, dir string) *Host {
	t.Helper()
	return NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: slog.New(slog.DiscardHandler)}),
		slog.New(slog.DiscardHandler), config.NewStore(), config.Dirs{ProjectDir: dir})
}

// repoWithHead writes root/.git/HEAD by hand: gitBranch reads the files, not git.
func repoWithHead(t *testing.T, head string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGitBranch(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	for _, c := range []struct {
		name, head, want string
	}{
		{"branch", "ref: refs/heads/main\n", "main"},
		{"slashed branch", "ref: refs/heads/feature/x\n", "feature/x"},
		{"unborn branch", "ref: refs/heads/main", "main"},
		{"detached", sha + "\n", sha[:7]},
		{"empty head", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := newHostAt(t, repoWithHead(t, c.head)).gitBranch(); got != c.want {
				t.Fatalf("gitBranch = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("no repository", func(t *testing.T) {
		t.Parallel()
		if got := newHostAt(t, t.TempDir()).gitBranch(); got != "" {
			t.Fatalf("gitBranch = %q, want empty", got)
		}
	})

	t.Run("linked worktree", func(t *testing.T) {
		t.Parallel()
		main := repoWithHead(t, "ref: refs/heads/main\n")
		wt := filepath.Join(main, ".git", "worktrees", "wt")
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, "HEAD"), []byte("ref: refs/heads/wtbranch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+wt+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := newHostAt(t, root).gitBranch(); got != "wtbranch" {
			t.Fatalf("gitBranch = %q, want wtbranch", got)
		}
	})

	t.Run("relative gitdir", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		gitdir := filepath.Join(root, ".git", "modules", "sub")
		if err := os.MkdirAll(gitdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte("ref: refs/heads/subbranch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sub := filepath.Join(root, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../.git/modules/sub\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := newHostAt(t, sub).gitBranch(); got != "subbranch" {
			t.Fatalf("gitBranch = %q, want subbranch", got)
		}
	})

	t.Run("gone git dir", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+filepath.Join(root, "nowhere")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := newHostAt(t, root).gitBranch(); got != "" {
			t.Fatalf("gitBranch = %q, want empty", got)
		}
	})

	t.Run("a switch is read again", func(t *testing.T) {
		t.Parallel()
		root := repoWithHead(t, "ref: refs/heads/main\n")
		h := newHostAt(t, root)
		if got := h.gitBranch(); got != "main" {
			t.Fatalf("gitBranch = %q, want main", got)
		}
		head := filepath.Join(root, ".git", "HEAD")
		if err := os.WriteFile(head, []byte("ref: refs/heads/next\n"), 0o644); err != nil { // same size: the mtime keys the cache
			t.Fatal(err)
		}
		later := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(head, later, later); err != nil {
			t.Fatal(err)
		}
		if got := h.gitBranch(); got != "next" {
			t.Fatalf("after a switch gitBranch = %q, want next", got)
		}
	})

	t.Run("reaches the screen state", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHost(t)
		root := repoWithHead(t, "ref: refs/heads/main\n")
		if err := os.CopyFS(h.Dirs.ProjectDir, os.DirFS(root)); err != nil {
			t.Fatal(err)
		}
		if got := h.ScreenState(20, 1, UIState{}).Branch; got != "main" {
			t.Fatalf("the screen state's branch = %q, want main", got)
		}
	})
}
