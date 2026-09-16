package lsp_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/lsp"
	"github.com/metafates/zemi/internal/lsp/lsptest"
)

func TestMain(m *testing.M) {
	if os.Getenv("ZEMI_LSP_FAKE") == "1" {
		lsptest.Serve(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// start launches the fake server against root.
func start(t *testing.T, ctx context.Context, root string) *lsp.Client {
	t.Helper()
	c, err := lsp.Start(ctx, lsp.Config{Command: os.Args[0], Env: append(os.Environ(), "ZEMI_LSP_FAKE=1"), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestTouchNoPublishReturnsEarly: a file the server says nothing about
// ends the wait on the floor timer, not on the caller's whole timeout.
func TestTouchNoPublishReturnsEarly(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := start(t, ctx, root)
	path := filepath.Join(root, "a.quiet")
	if err := os.WriteFile(path, []byte("nothing to say"), 0o644); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	diags, err := c.Touch(ctx, path, "plaintext")
	if err != nil || len(diags) != 0 {
		t.Fatalf("diags %+v, err %v", diags, err)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("Touch waited %s on a server that publishes nothing", elapsed)
	}
}

// TestTouchThroughSymlink: a server that answers with the canonicalized
// path still keys the file the caller asked about.
func TestTouchThroughSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := start(t, ctx, root)
	path := filepath.Join(root, "link", "a.txt")
	if err := os.WriteFile(path, []byte("ok\nBROKEN here"), 0o644); err != nil {
		t.Fatal(err)
	}
	diags, err := c.Touch(ctx, path, "plaintext")
	if err != nil || len(diags) != 1 {
		t.Fatalf("diags %+v, err %v", diags, err)
	}
}

// TestTouchConcurrentVersions: concurrent syncs of one file must reach the
// server with increasing versions; the fake reports any regression.
func TestTouchConcurrentVersions(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := start(t, ctx, root)
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, _ = c.Touch(ctx, path, "plaintext")
		})
	}
	wg.Wait()
	for _, d := range c.Diagnostics(path) {
		if d.Message == "version regression" {
			t.Fatal("document versions reached the server out of order")
		}
	}
}

// TestTouch: opening and changing a file brings its fresh diagnostics.
func TestTouch(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := lsp.Start(ctx, lsp.Config{Command: os.Args[0], Env: append(os.Environ(), "ZEMI_LSP_FAKE=1"), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	path := filepath.Join(root, "a.txt")
	for _, step := range []struct {
		text string
		want int
	}{
		{"ok\nBROKEN here\nBROKEN too", 2},
		{"all fixed", 0},
	} {
		if err := os.WriteFile(path, []byte(step.text), 0o644); err != nil {
			t.Fatal(err)
		}
		diags, err := c.Touch(ctx, path, "plaintext")
		if err != nil || len(diags) != step.want {
			t.Fatalf("%q: diags %+v, err %v", step.text, diags, err)
		}
		if step.want > 0 && (diags[0].Line != 2 || diags[0].Message != "broken" || lsp.SeverityName(diags[0].Severity) != "error") {
			t.Fatalf("diagnostic = %+v", diags[0])
		}
	}
}
