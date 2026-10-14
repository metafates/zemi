//go:build unix

package cli

import (
	"os"
	"syscall"
	"testing"
)

// limitFDs lowers the file-descriptor ceiling to what the process holds
// plus spare, so the run must keep its opens tight; restore lifts it back.
func limitFDs(t *testing.T, spare uint64) func() {
	var was syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &was); err != nil {
		t.Fatal(err)
	}
	// other tests' hosts stay open, the more so under -count
	open, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	lower := was
	lower.Cur = uint64(len(open)) + spare
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lower); err != nil {
		t.Fatal(err)
	}
	return func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &was) }
}
