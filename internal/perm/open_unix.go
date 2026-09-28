//go:build unix

package perm

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var errSwapped = errors.New("a symlink replaced part of the path after the permission check; nothing was opened")

// Open opens the path that was checked without following a symlink in any
// component: a directory a sandboxed command swaps for a link after the
// check fails the open instead of leading outside. With os.O_CREATE missing
// parent directories are made.
func (p Path) Open(flag int, mode os.FileMode) (*os.File, error) {
	parts, err := p.parts()
	if err != nil {
		return nil, err
	}
	dir, err := p.walk(parts[:len(parts)-1], flag&os.O_CREATE != 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	fd, err := at(dir, parts[len(parts)-1], flag, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p.s, Err: err}
	}
	return os.NewFile(uintptr(fd), p.s), nil
}

// MkdirAll makes the directory and its missing parents, walked like Open.
func (p Path) MkdirAll() error {
	parts, err := p.parts()
	if err != nil {
		return err
	}
	dir, err := p.walk(parts, true)
	if err == nil {
		unix.Close(dir)
	}
	return err
}

// Remove deletes one file, symlink or empty directory, its parent walked
// like Open.
func (p Path) Remove() error {
	parts, err := p.parts()
	if err != nil {
		return err
	}
	dir, err := p.walk(parts[:len(parts)-1], false)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	last := parts[len(parts)-1]
	// os.Remove's order: unlink, then rmdir, keeping the error that explains
	if err = unix.Unlinkat(dir, last, 0); err == nil {
		return nil
	}
	e := unix.Unlinkat(dir, last, unix.AT_REMOVEDIR)
	if e == nil {
		return nil
	}
	if !errors.Is(e, unix.ENOTDIR) {
		err = e
	}
	return &os.PathError{Op: "remove", Path: p.s, Err: err}
}

func (p Path) parts() ([]string, error) {
	if !strings.HasPrefix(p.s, "/") {
		return nil, &os.PathError{Op: "open", Path: p.s, Err: errors.New("not a resolved path")}
	}
	return strings.Split(strings.Trim(p.s, "/"), "/"), nil
}

// walk opens the directories parts names below /, one at a time, search
// only, so a parent zemi may not list (hidden by the sandbox it runs in)
// still leads to what it may open; with create a missing one is made. The
// caller closes the fd.
func (p Path) walk(parts []string, create bool) (int, error) {
	dir, err := unix.Open("/", searchDir|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: p.s, Err: err}
	}
	for _, c := range parts {
		fd, err := at(dir, c, searchDir, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(dir, c, 0o755); err == nil || errors.Is(err, unix.EEXIST) {
				fd, err = at(dir, c, searchDir, 0)
			}
		}
		unix.Close(dir)
		if err != nil {
			return -1, &os.PathError{Op: "open", Path: p.s, Err: err}
		}
		dir = fd
	}
	return dir, nil
}

// at opens c in dir; ELOOP or ENOTDIR, depending on the kernel, when c is a
// symlink, reported as errSwapped.
func at(dir int, c string, flag int, mode uint32) (int, error) {
	fd, err := unix.Openat(dir, c, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	var st unix.Stat_t
	if err != nil && unix.Fstatat(dir, c, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
		err = errSwapped
	}
	return fd, err
}
