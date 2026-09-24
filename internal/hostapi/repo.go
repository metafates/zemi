// repo.go runs git for zemi's own checks.
package hostapi

import (
	"context"

	"github.com/metafates/zemi/internal/proc"
)

// git runs one hardened git command in dir (proc.Git).
// NOTE: a status listing may name a stat-dirty filtered file as modified
// (proc.Git's NOTE). Only the environment prompt's file list shows it;
// lspSync keys on mtime and size, so it reports at most one extra change.
func (h *Host) git(dir string, args ...string) (string, error) {
	return h.gitEnv(dir, nil, args...)
}

func (h *Host) gitEnv(dir string, env []string, args ...string) (string, error) {
	return proc.Git{Dir: dir, Env: env}.Run(context.Background(), args...)
}
