// repo.go runs git for zemi's own checks.

package hostapi

import (
	"context"

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
