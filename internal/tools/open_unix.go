//go:build unix

package tools

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var errSwapped = errors.New("a symlink replaced part of the path after the permission check; nothing was opened")

// openGated opens path, the absolute symlink-free path the permission gate
// checked, without following a symlink in any component: a directory a
// sandboxed command swaps for a link after the check fails the open instead
// of leading outside. With os.O_CREATE missing parent directories are made.
func openGated(path string, flag int, mode os.FileMode) (*os.File, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, &os.PathError{Op: "open", Path: path, Err: errors.New("not an absolute path")}
	}
	dir, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { unix.Close(dir) }()
	// ELOOP or ENOTDIR, depending on the kernel, when c is a symlink
	at := func(c string, flag int, mode uint32) (int, error) {
		fd, err := unix.Openat(dir, c, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
		var st unix.Stat_t
		if err != nil && unix.Fstatat(dir, c, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			err = errSwapped
		}
		return fd, err
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for _, c := range parts[:len(parts)-1] {
		fd, err := at(c, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if errors.Is(err, unix.ENOENT) && flag&os.O_CREATE != 0 {
			if err = unix.Mkdirat(dir, c, 0o755); err == nil || errors.Is(err, unix.EEXIST) {
				fd, err = at(c, unix.O_RDONLY|unix.O_DIRECTORY, 0)
			}
		}
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		unix.Close(dir)
		dir = fd
	}
	fd, err := at(parts[len(parts)-1], flag, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
