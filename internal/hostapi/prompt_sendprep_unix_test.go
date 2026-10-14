//go:build unix

package hostapi

import (
	"syscall"
	"testing"
	"time"
)

// TestSendPrepCheap: everything a send does before the provider request is
// in-memory work once the process is warm: git status on a send costs
// seconds in a large repository and lands on time to first token, and a
// subprocess or a disk walk on this path blows this budget by orders of
// magnitude. The
// budget is CPU time, the process's and its children's, which a loaded
// machine does not stretch like the clock; the test runs alone (no
// t.Parallel), so the process's CPU is the builds'.
func TestSendPrepCheap(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	h.buildSystem(nil)
	cpu := func() time.Duration {
		var self, kids syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &self)
		syscall.Getrusage(syscall.RUSAGE_CHILDREN, &kids)
		return time.Duration(self.Utime.Nano() + self.Stime.Nano() + kids.Utime.Nano() + kids.Stime.Nano())
	}
	start := cpu()
	for range 5 {
		toolset, _ := h.profileTools(nil)
		_, _, parts := h.buildSystemParts(nil, toolset)
		promptSizes(parts, toolset)
	}
	if d := cpu() - start; d > 200*time.Millisecond {
		t.Fatalf("5 prompt builds took %s of CPU; something expensive is back on the send path", d)
	}
}
