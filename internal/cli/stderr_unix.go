// The TUI owns the terminal: while the altscreen is up, anything the process
// writes to stderr - a panic's stack, a chatty library - would land on the
// screen, and the renderer repaints only its own changes, so it would stay
// there. These files put fd 2 into the launch log instead.

//go:build unix

package cli

import (
	"sync"

	"golang.org/x/sys/unix"
)

// stderrToLog points fd 2 at path (the launch log) and returns restore,
// which puts the tty back. The swap closes fd 2 and dups into the hole it
// left: darwin/arm64 has no Dup2. NOTE: another goroutine's open in the
// nanoseconds between the close and the dup could take fd 2; the redirect
// then gives up and stderr stays where it was.
func stderrToLog(path string) (restore func(), ok bool) {
	logfd, err := unix.Open(path, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT, 0o600)
	if err != nil {
		return func() {}, false
	}
	defer unix.Close(logfd) // the copy on 2 keeps the file open
	ttyfd, err := unix.Dup(2)
	if err != nil {
		return func() {}, false
	}
	// A failed dup in the paths below leaves the tty wherever dup put it:
	// stderr keeps working, only the redirect is lost.
	unix.Close(2)
	if fd, err := unix.Dup(logfd); err != nil || fd != 2 {
		if err == nil {
			unix.Close(fd)
		}
		_, _ = unix.Dup(ttyfd)
		unix.Close(ttyfd)
		return func() {}, false
	}
	return sync.OnceFunc(func() {
		unix.Close(2)
		_, _ = unix.Dup(ttyfd)
		unix.Close(ttyfd)
	}), true
}
