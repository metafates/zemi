//go:build !windows

package proc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ShellCommandContext runs a line through the platform shell and stops it as
// a group when its context ends.
func TestShellCommandContext(t *testing.T) {
	cmd := ShellCommandContext(t.Context(), "echo ok")
	out := &Buffer{Max: 1 << 10}
	cmd.Stdout = out
	if err := Run(cmd); err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(out.String()); s != "ok" {
		t.Fatalf("out: %q", s)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd = ShellCommandContext(ctx, "sleep 30")
	cmd.WaitDelay = 100 * time.Millisecond
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	cancel()
	start := time.Now()
	if err := Wait(cmd); err == nil {
		t.Fatal("a cancelled shell command was not stopped")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("returned after %s", took)
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
