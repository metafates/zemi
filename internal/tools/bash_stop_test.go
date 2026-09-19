//go:build !windows

package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestBashStops: an interrupt or a timeout ends a command whose children
// keep its output pipe open, instead of waiting on them forever.
func TestBashStops(t *testing.T) {
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
