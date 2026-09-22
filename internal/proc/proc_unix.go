//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

func setSession(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	dieWithParent(cmd.SysProcAttr)
}

func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }
