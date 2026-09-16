//go:build !unix

package tools

import (
	"os"
	"path/filepath"
)

// openGated opens the path the permission gate checked.
// NOTE: a symlink swapped in after the check is followed; the unix version
// walks the path with O_NOFOLLOW.
func openGated(path string, flag int, mode os.FileMode) (*os.File, error) {
	if flag&os.O_CREATE != 0 {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, flag, mode)
}
