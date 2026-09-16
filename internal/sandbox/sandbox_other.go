//go:build !darwin && !linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

const (
	deniedErrnos  = `operation not permitted`
	deniedLookups = `nodename nor servname`
)

func probe() (Backend, error) {
	return Backend{}, fmt.Errorf("no sandbox on %s", runtime.GOOS)
}

func wrap(*exec.Cmd, Policy) error { return errors.New("sandbox unavailable") }

// Helper is the Linux re-exec entry.
func Helper([]string) int {
	fmt.Fprintln(os.Stderr, "zemi sandbox: the helper is linux only")
	return 2
}
