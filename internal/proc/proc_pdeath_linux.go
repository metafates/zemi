package proc

import "syscall"

// dieWithParent has the kernel kill the child when zemi goes away, the case
// KillAll cannot cover: a SIGKILL of zemi runs no cleanup.
// NOTE: only the direct child dies, so what it left in its own group is
// orphaned, and the kernel ties the signal to the thread that forked, which
// the Go runtime may retire.
func dieWithParent(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
