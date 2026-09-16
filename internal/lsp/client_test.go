package lsp_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/lsp"
	"github.com/metafates/zemi/internal/lsp/lsptest"
)

func TestMain(m *testing.M) {
	switch os.Getenv("ZEMI_LSP_FAKE") {
	case "1":
		lsptest.Serve(os.Stdin, os.Stdout)
		os.Exit(0)
	case "blocking":
		serveBlocking(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// serveBlocking is a single-threaded server: as the message after
// initialized starts arriving, it asks for configuration and writes more
// than a pipe holds before it reads on.
func serveBlocking(in io.Reader, out io.Writer) {
	r := bufio.NewReader(in)
	write := func(s string) { _ = lsp.WriteMessage(out, []byte(s)) }
	for {
		body, err := lsp.ReadMessage(r)
		if err != nil {
			return
		}
		switch {
		case bytes.Contains(body, []byte(`"initialize"`)):
			write(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`)
		case bytes.Contains(body, []byte(`"initialized"`)):
			header, _ := r.ReadString('\n')
			write(`{"jsonrpc":"2.0","id":"cfg","method":"workspace/configuration","params":{"items":[{}]}}`)
			for range 300 {
				write(`{"jsonrpc":"2.0","method":"window/logMessage","params":{"type":4,"message":"` + strings.Repeat("x", 1000) + `"}}`)
			}
			r = bufio.NewReader(io.MultiReader(strings.NewReader(header), r))
		}
	}
}

// TestServerRequestWhileWriting: a server that stops reading until its
// output drains, and asks for configuration while the client is sending it
// a large document, must not deadlock the client's reader and writer.
func TestServerRequestWhileWriting(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := lsp.Start(ctx, lsp.Config{Command: os.Args[0], Env: append(os.Environ(), "ZEMI_LSP_FAKE=blocking"), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "big.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Open(path, "plaintext") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("client and server deadlocked") // no Close: it would wait on the same lock
	}
	c.Close()
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

// TestBadFrameEndsServer: a frame the client refuses ends the connection
// at once, though the server itself keeps running.
func TestBadFrameEndsServer(t *testing.T) {
	for _, length := range []string{"999999999999", "12abc"} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		began := time.Now()
		_, err := lsp.Start(ctx, lsp.Config{Command: "sh", Args: []string{"-c", `printf 'Content-Length: ` + length + `\r\n\r\n'; sleep 30`}, Root: t.TempDir()})
		if err == nil || time.Since(began) > 5*time.Second {
			t.Fatalf("length %s: start = %v after %s", length, err, time.Since(began))
		}
	}
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
	if !errors.Is(err, lsp.ErrNoFresh) || len(diags) != 0 {
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

// TestOthers: errors in other files the server published since this one's
// sync are reported for it, and go away once they are fixed.
func TestOthers(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := start(t, ctx, root)
	a, b := filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")
	os.WriteFile(a, []byte("ok"), 0o644)
	os.WriteFile(b, []byte("BROKEN"), 0o644)
	for _, p := range []string{a, b} {
		if _, err := c.Touch(ctx, p, "plaintext"); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Others(a); len(got) != 1 || len(slices.Collect(maps.Values(got))[0]) != 1 {
		t.Fatalf("others after b broke: %+v", got)
	}
	os.WriteFile(b, []byte("fixed"), 0o644)
	if _, err := c.Touch(ctx, b, "plaintext"); err != nil {
		t.Fatal(err)
	}
	if got := c.Others(a); len(got) != 0 {
		t.Fatalf("others after b was fixed: %+v", got)
	}
}

// TestTouch: opening and changing a file brings its fresh diagnostics.
func TestTouch(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := start(t, ctx, root)
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

// TestOffsetPos: positions count UTF-16 units, so a character outside the
// BMP is two of them and the round trip holds.
func TestOffsetPos(t *testing.T) {
	text := "a😀b\ncd"
	for _, c := range []struct {
		off  int
		line int
		char int
	}{{0, 0, 0}, {1, 0, 1}, {5, 0, 3}, {6, 0, 4}, {7, 1, 0}, {8, 1, 1}} {
		pos := lsp.Position{Line: c.line, Character: c.char}
		if got := lsp.Offset(text, pos); got != c.off {
			t.Errorf("Offset(%+v) = %d, want %d", pos, got, c.off)
		}
		if got := lsp.Pos(text, c.off); got != pos {
			t.Errorf("Pos(%d) = %+v, want %+v", c.off, got, pos)
		}
	}
	if got := lsp.Offset(text, lsp.Position{Line: 9, Character: 0}); got != len(text) {
		t.Errorf("past the end = %d", got)
	}
}

// TestRefreshStaleDocuments: a file changed on disk after the server saw
// it (a rename's edits to other files, a formatter) is sent again before
// the next query, so the server never answers from a stale copy.
func TestRefreshStaleDocuments(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := start(t, ctx, root)
	a, b := filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")
	os.WriteFile(a, []byte("ok"), 0o644)
	os.WriteFile(b, []byte("ok"), 0o644)
	for _, p := range []string{a, b} {
		if _, err := c.Touch(ctx, p, "plaintext"); err != nil {
			t.Fatal(err)
		}
	}
	// b changes behind the server's back
	os.WriteFile(b, []byte("BROKEN"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(b, later, later)
	if _, err := c.Touch(ctx, a, "plaintext"); err != nil {
		t.Fatal(err)
	}
	if got := c.Others(a); len(got) != 1 {
		t.Fatalf("b was not re-sent after changing on disk: %+v", got)
	}
}

// TestWatchedFiles: the server registers a watcher at start; a file nobody
// opened reaches it only through NotifyChanged, filtered by its globs, and
// a deleted open document is closed on the next touch.
func TestWatchedFiles(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := start(t, ctx, root)
	a, b, g := filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt"), filepath.Join(root, "c.go")
	os.WriteFile(a, []byte("func Foo()\nFoo()"), 0o644)
	if _, err := c.Touch(ctx, a, "plaintext"); err != nil {
		t.Fatal(err)
	}
	refs := func() int {
		var locs []lsp.Location
		params := map[string]any{"textDocument": map[string]any{"uri": lsp.DocumentURI(a)}, "position": lsp.Position{Line: 1, Character: 1}, "context": map[string]any{"includeDeclaration": true}}
		if err := c.Call(ctx, "textDocument/references", params, &locs); err != nil {
			t.Fatal(err)
		}
		return len(locs)
	}
	os.WriteFile(b, []byte("Foo()"), 0o644)
	os.WriteFile(g, []byte("Foo()"), 0o644)
	if n := refs(); n != 2 {
		t.Fatalf("before any notification: %d references", n)
	}
	c.NotifyChanged([]lsp.FileChange{{Path: b, Type: 1}, {Path: g, Type: 1}})
	if n := refs(); n != 3 {
		t.Fatalf("after Created for b.txt (c.go is outside the watcher's glob): %d references", n)
	}
	os.Remove(b)
	c.NotifyChanged([]lsp.FileChange{{Path: b, Type: 3}})
	if n := refs(); n != 2 {
		t.Fatalf("after Deleted: %d references", n)
	}
	// the open document itself disappears: closed on the next touch
	os.WriteFile(b, []byte("ok"), 0o644)
	os.Remove(a)
	if _, err := c.Touch(ctx, b, "plaintext"); err != nil {
		t.Fatal(err)
	}
	if got := c.Diagnostics(a); len(got) != 0 || c.Version(a) != 0 {
		t.Fatalf("deleted document still open: diags %v version %d", got, c.Version(a))
	}
}
