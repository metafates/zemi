//go:build !linux && !windows

package proc

import "syscall"

// NOTE: macOS and the BSDs have no parent-death signal, so a SIGKILL of
// zemi, which runs no KillAll, leaves its children running until they
// finish on their own. Every other exit still goes through KillAll.
func dieWithParent(*syscall.SysProcAttr) {}
