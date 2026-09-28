//go:build unix && !darwin && !linux

package perm

import "golang.org/x/sys/unix"

// searchDir opens a directory to walk it; with no search-only open here, a
// walk needs read access to every parent.
const searchDir = unix.O_RDONLY | unix.O_DIRECTORY
