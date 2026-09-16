package proc

import (
	"os"
	"os/exec"
)

// NOTE: Windows kills only the process itself; children of a shell survive
// until a job object is used.
func setSession(*exec.Cmd) {}

func hangupGroup(pid int) { killGroup(pid) }

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
