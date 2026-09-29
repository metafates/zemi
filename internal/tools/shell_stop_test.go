//go:build !windows

package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestShellProgress: the last output line reaches Progress while the command
// runs, and nothing clears it: the result replaces the row in one change.
func TestShellProgress(t *testing.T) {
	p, _ := testPipeline(t, true)
	ctx, stop := context.WithCancel(t.Context())
	var mu sync.Mutex
	var lines []string
	p.Progress = func(id, line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, id+":"+line)
		stop() // the line came while the command ran
	}
	// one write, so no poll lands between the lines
	input := `{"command":"printf 'one\\ntwo\\n'; sleep 30"}`
	if _, err := toolNamed(t, "shell", p).Run(ctx, fantasy.ToolCall{ID: "t1", Name: "shell", Input: input}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || lines[0] != "t1:two" {
		t.Fatalf("progress lines = %q", lines)
	}
}

// TestShellStops: an interrupt or a timeout ends a command whose children
// keep its output pipe open, instead of waiting on them forever.
func TestShellStops(t *testing.T) {
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
			res, err := runShell(ctx, &Pipeline{}, c.input)
			if err != nil || time.Since(start) > 5*time.Second || !strings.Contains(res.Content, c.want) {
				t.Fatalf("after %s: %v %q", time.Since(start), err, res.Content)
			}
		})
	}
}
