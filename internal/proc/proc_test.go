//go:build !windows

package proc

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gone polls for a pid to disappear (orphans are reaped by init).
func gone(pid int) bool {
	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestStoppable(t *testing.T) {
	for _, c := range []struct {
		name, script string
		cancel       bool
	}{
		{"cancel kills a pipeline", "sleep 30 | cat", true},
		{"cancel kills a backgrounded child", "sleep 30 & echo $! > %s; wait", true},
		{"an orphan holding stdout does not block", "sleep 30 & echo $! > %s", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			script := strings.ReplaceAll(c.script, "%s", pidFile)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := CommandContext(ctx, "sh", "-c", script)
			var out Buffer
			out.Max = 1 << 10
			cmd.Stdout = &out
			if c.cancel {
				time.AfterFunc(200*time.Millisecond, cancel)
			}
			start := time.Now()
			_ = Run(cmd)
			if took := time.Since(start); took > 5*time.Second {
				t.Fatalf("returned after %s", took)
			}
			if b, err := os.ReadFile(pidFile); err == nil {
				pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
				if pid > 0 && !gone(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("child %d still running", pid)
				}
			}
		})
	}
}

func TestBuffer(t *testing.T) {
	b := &Buffer{Max: 10}
	b.Write([]byte("abc"))
	if b.String() != "abc" {
		t.Fatalf("short = %q", b.String())
	}
	b.Write([]byte("defghijklmnopqrstuvwxyz"))
	if got := b.String(); !strings.HasPrefix(got, "abcde\n") || !strings.HasSuffix(got, "\nvwxyz") || !strings.Contains(got, "16 bytes") {
		t.Fatalf("long = %q", got)
	}
}
