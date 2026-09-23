//go:build !windows

package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBashStops: an interrupt or a timeout ends a command whose children
// keep its output pipe open, instead of waiting on them forever.
// TestBashProgress: the last output line reaches Progress while the command
// runs, and the line is cleared once it exits.
func TestBashProgress(t *testing.T) {
	p, _ := testPipeline(t, true)
	var mu sync.Mutex
	var lines []string
	p.Progress = func(id, line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, id+":"+line)
	}
	resp := call(t, toolNamed(t, "bash", p), `{"command":"echo one; echo two; sleep 1"}`)
	if resp.IsError {
		t.Fatalf("bash: %+v", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) < 2 || lines[0] != "t1:two" || lines[len(lines)-1] != "t1:" {
		t.Fatalf("progress lines = %q", lines)
	}
}

func TestBashStops(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		input  map[string]any
		cancel bool
		want   string
	}{
		{"interrupt", map[string]any{"command": "sleep 30 | cat"}, true, "interrupted"},
		{"timeout", map[string]any{"command": "sleep 30 & wait", "timeout_ms": float64(300)}, false, "timed out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancel {
				time.AfterFunc(200*time.Millisecond, cancel)
			}
			start := time.Now()
			res, err := runBash(ctx, &Pipeline{}, c.input)
			if err != nil || time.Since(start) > 5*time.Second || !strings.Contains(res.Content, c.want) {
				t.Fatalf("after %s: %v %q", time.Since(start), err, res.Content)
			}
		})
	}
}
