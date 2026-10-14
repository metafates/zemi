//go:build unix

package skill

import (
	"syscall"
	"testing"
)

func makeFifo(t *testing.T, path string) {
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
}
