//go:build !unix

package perm

import (
	"os"
	"path/filepath"
)

// Open opens the path that was checked.
// NOTE: a symlink swapped in after the check is followed; the unix version
// walks the path with O_NOFOLLOW.
func (p Path) Open(flag int, mode os.FileMode) (*os.File, error) {
	if flag&os.O_CREATE != 0 {
		if err := os.MkdirAll(filepath.Dir(p.s), 0o755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(p.s, flag, mode)
}

// MkdirAll makes the directory and its missing parents.
func (p Path) MkdirAll() error { return os.MkdirAll(p.s, 0o755) }

// Remove deletes one file, symlink or empty directory.
func (p Path) Remove() error { return os.Remove(p.s) }
