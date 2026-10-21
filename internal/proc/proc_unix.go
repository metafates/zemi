//go:build !windows

package proc

import (
	"context"
	"os/exec"
	"sync"
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

// Shell is the shell that runs the model's command lines: bash, then zsh,
// then dash, then sh. Looked up once, on PATH like every program zemi runs
// by name.
var Shell = sync.OnceValue(func() string {
	for _, name := range []string{"bash", "zsh", "dash", "sh"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return "sh"
})

// ShellCommand is Command running line with Shell.
func ShellCommand(line string) *exec.Cmd { return Command(Shell(), "-c", line) }

// ShellCommandContext is ShellCommand cancelled as a group when ctx is.
func ShellCommandContext(ctx context.Context, line string) *exec.Cmd {
	return CommandContext(ctx, Shell(), "-c", line)
}
