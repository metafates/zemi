// repo.go runs git for zemi's own checks.

package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/proc"
)

// NOTE: only the environment prompt's file list shows proc.Git's false
// modified file; lspSync keys on mtime and size, so it sends at most one extra change.
func (h *Host) git(dir string, args ...string) (string, error) {
	return h.gitEnv(dir, nil, args...)
}

func (h *Host) gitEnv(dir string, env []string, args ...string) (string, error) {
	return proc.Git{Dir: dir, Env: env}.Run(context.Background(), args...)
}

// headPath is the HEAD file of the checkout zemi runs in: the repository's,
// or, when .git names another git dir (a linked worktree, a submodule), that
// one's. "" when there is none to read.
func (h *Host) headPath() string {
	root := h.Dirs.RepoRoot()
	if root == "" {
		return ""
	}
	dotgit := filepath.Join(root, ".git")
	fi, err := os.Stat(dotgit)
	if err != nil {
		return ""
	}
	if !fi.Mode().IsRegular() {
		return filepath.Join(dotgit, "HEAD")
	}
	text, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(text)), "gitdir:")
	if !ok {
		return ""
	}
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	return filepath.Join(dir, "HEAD")
}

// headKey identifies a HEAD read by the stat a branch switch rewrites, the
// same keying lspSync holds its HEAD listing to.
type headKey struct {
	path string
	mod  time.Time
	size int64
}

// gitBranch is the branch of the checkout zemi runs in, or the short sha
// when HEAD detaches (a rebase included). Cached on HEAD's stat, so a render
// reads the file only when git moved the branch; "" when there is none.
func (h *Host) gitBranch() string {
	head := h.headPath()
	if head == "" {
		return ""
	}
	fi, err := os.Stat(head)
	if err != nil {
		return ""
	}
	key := headKey{head, fi.ModTime(), fi.Size()}
	h.branchMu.Lock()
	defer h.branchMu.Unlock()
	if h.branchKey == key {
		return h.branch
	}
	text, err := os.ReadFile(head)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(text))
	name, ok := strings.CutPrefix(line, "ref: refs/heads/")
	if !ok {
		name = line[:min(7, len(line))] // a detached HEAD names its commit
	}
	h.branchKey, h.branch = key, name
	return name
}
