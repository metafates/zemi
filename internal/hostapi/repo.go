// repo.go runs git for zemi's own checks.
package hostapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/proc"
)

const gitTimeout = 15 * time.Second

// git runs one git command in dir with a timeout; stderr is folded into
// the error. A repository's config may name programs even status and
// ls-files run: core.fsmonitor, filter drivers, those of submodules, and
// hooks (an index refresh runs post-index-change), so those are switched
// off, in a trusted project too: a sandboxed command can plant a nested
// repository whose config zemi's own checks would then run unconfined.
// NOTE: with a repo-local filter off, status compares a stat-dirty filtered
// file's raw bytes with the cleaned blob and lists it as modified (git-crypt,
// nbstripout). Only the environment prompt's file list shows it; lspSync keys
// on mtime and size, so it reports at most one extra change.
func (h *Host) git(dir string, args ...string) (string, error) {
	safe := []string{"-c", "core.fsmonitor=false", "-c", "diff.ignoreSubmodules=all", "-c", "core.hooksPath=/dev/null"}
	out, _ := h.runGit(dir, "config", "--show-scope", "--name-only", "--get-regexp", `^filter\..*\.(clean|process)$`)
	for line := range strings.Lines(out) {
		// the user's own drivers (git-lfs) stay on
		if scope, key, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && scope != "global" {
			// a required driver left blank fails status outright (git lfs install --local)
			name := key[:strings.LastIndexByte(key, '.')]
			safe = append(safe, "-c", key+"=", "-c", name+".required=false")
		}
	}
	return h.runGit(dir, append(safe, args...)...)
}

func (h *Host) runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := proc.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// the system gitconfig does not apply; status leaves a repository's
	// index alone
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
