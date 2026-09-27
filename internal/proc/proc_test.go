//go:build !windows

package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
			t.Parallel()
			pidFile := filepath.Join(t.TempDir(), "pid")
			script := strings.ReplaceAll(c.script, "%s", pidFile)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := CommandContext(ctx, "sh", "-c", script)
			if cmd.WaitDelay == 0 {
				t.Fatal("no WaitDelay: an orphan holding a pipe blocks Wait forever")
			}
			cmd.WaitDelay = 100 * time.Millisecond // the orphan case waits all of it
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
	for _, c := range "0123456" {
		b.Write([]byte(string(c)))
	}
	if got, n := b.Snapshot(); !strings.HasSuffix(got, "\n23456") || !strings.Contains(got, "23 bytes") || n != 33 {
		t.Fatalf("after small writes = %q, %d", got, n)
	}
}

// TestRefuse: runners that start and end in parallel never lift a refusal
// one still holds, and a release that runs twice counts once.
func TestRefuse(t *testing.T) {
	release := Refuse()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			r := Refuse()
			r()
			r()
		})
	}
	wg.Wait()
	if err := Run(Command("true")); !errors.Is(err, ErrRefused) {
		t.Fatalf("start while refused: %v", err)
	}
	if _, err := (Git{Dir: t.TempDir()}).Run(t.Context(), "version"); !errors.Is(err, ErrRefused) {
		t.Fatalf("git while refused: %v", err)
	}
	release()
	if err := Run(Command("true")); err != nil {
		t.Fatalf("start once released: %v", err)
	}
}
