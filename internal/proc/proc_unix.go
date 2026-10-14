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

// Shell is the shell that runs the model's command lines: dash where it is
// installed (macOS, Debian, Ubuntu), else sh. macOS's sh is bash 3.2, which
// ends a $( ) at a ) in a heredoc body inside it and so garbles commit
// messages. Looked up once, on PATH like every program zemi runs by name.
var Shell = sync.OnceValue(func() string {
	for _, name := range []string{"dash", "sh"} {
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
