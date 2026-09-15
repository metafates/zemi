// repo.go is the config-repo git plumbing (DESIGN §5, §6.1, §10.4):
// ~/.config/zemi is a git repo, and every committed change to it is a
// plain git commit (the permanent-rule keypress is the review; learned
// patches commit as `learn: <title> (from <session id>)`). The helpers
// shell out to the git binary and degrade to a log entry when git is
// unavailable: the files and the revisions table stay the source of
// truth, the repo history is the audit trail.
package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/store"
)

const gitTimeout = 15 * time.Second

// EnsureConfigRepo makes the config home a git repo (idempotent, §5). A
// missing git binary or a failed init is logged and otherwise ignored:
// the config dir keeps working unversioned.
func (h *Host) EnsureConfigRepo() {
	dir := h.Dirs.ConfigHome
	if dir == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return
	}
	if _, err := exec.LookPath("git"); err != nil {
		h.Log.Warn("config-repo", "git unavailable; config stays unversioned", nil)
		return
	}
	if _, err := h.git(dir, "init", "-q"); err != nil {
		h.Log.Warn("config-repo", "git init failed; config stays unversioned", map[string]any{
			"error": err.Error(),
		})
	}
}

// git runs one git command in dir with a timeout; stderr is folded into
// the error.
func (h *Host) git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := proc.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// zemi commits must work on machines without a configured identity.
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// commitConfigPaths stages exactly paths (repo-relative) and commits
// them with a self-contained identity (machines without git config must
// still get commits). Unchanged trees are a no-op (git exits non-zero
// with "nothing to commit"); that is success here.
func (h *Host) commitConfigPaths(msg string, paths []string) error {
	dir := h.Dirs.ConfigHome
	if dir == "" {
		return fmt.Errorf("config repo: no config home")
	}
	h.EnsureConfigRepo()
	args := []string{"add", "--"}
	args = append(args, paths...)
	if _, err := h.git(dir, args...); err != nil {
		return err
	}
	commit := []string{
		"-c", "user.name=zemi", "-c", "user.email=zemi@localhost",
		"commit", "-q", "-m", msg, "--",
	}
	commit = append(commit, paths...)
	_, err := h.git(dir, commit...)
	if err != nil && strings.Contains(err.Error(), "nothing to commit") {
		return nil
	}
	return err
}

// recordRevision inserts the revision row, commits the config repo, and
// marks the row applied or failed (DB mark lands before the commit,
// §10.4). Errors are returned for the caller to surface; a failed commit
// never rolls back the file write.
func (h *Host) commitConfigRevision(title, changesJSON string, paths []string, appliedBy string) error {
	if h.GlobalDB == nil {
		return nil
	}
	pathsJSON, _ := json.Marshal(paths)
	rev := &store.ConfigRevision{
		ID:          store.NewID(),
		Title:       title,
		ChangesJSON: changesJSON,
		PathsJSON:   string(pathsJSON),
		Status:      "pending",
		AppliedBy:   appliedBy,
	}
	if err := h.GlobalDB.AddRevision(rev); err != nil {
		return err
	}
	if err := h.commitConfigPaths(title, paths); err != nil {
		if serr := h.GlobalDB.SetRevisionStatus(rev.ID, "failed"); serr != nil {
			h.Log.Warn("config-repo", "revision status update failed", map[string]any{
				"id": rev.ID, "error": serr.Error(),
			})
		}
		return err
	}
	// Record what :revision revert will revert (best effort: a missed SHA
	// only costs the revert path, the audit trail is the git history).
	if out, err := h.git(h.Dirs.ConfigHome, "rev-parse", "HEAD"); err == nil {
		if sha := strings.TrimSpace(out); sha != "" {
			if serr := h.GlobalDB.SetRevisionCommit(rev.ID, sha); serr != nil {
				h.Log.Warn("config-repo", "revision commit update failed", map[string]any{
					"id": rev.ID, "error": serr.Error(),
				})
			}
		}
	}
	return h.GlobalDB.SetRevisionStatus(rev.ID, "applied")
}
