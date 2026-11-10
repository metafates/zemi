package lsp_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

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
	goleak.VerifyTestMain(m)
}

// serveBlocking is a single-threaded server: as the message after
// initialized starts arriving, it asks for configuration and writes more
// than a pipe holds, then never reads again, so the document the client is
// writing wedges its sender for good.
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
			_, _ = r.ReadString('\n')
			write(`{"jsonrpc":"2.0","id":"cfg","method":"workspace/configuration","params":{"items":[{}]}}`)
			for range 300 {
				write(`{"jsonrpc":"2.0","method":"window/logMessage","params":{"type":4,"message":"` + strings.Repeat("x", 1000) + `"}}`)
			}
			time.Sleep(time.Hour)
		}
	}
}

// TestServerRequestWhileWriting: a server that stops reading while its
// output drains, asks for configuration while the client is sending it a
// large document, and then never reads again wedges the client's writer.
// Close must not wait on the wedged send's lock: its kill frees the writer
// and ends the server.
func TestServerRequestWhileWriting(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := lsp.Start(ctx, lsp.Config{Command: os.Args[0], Env: append(os.Environ(), "ZEMI_LSP_FAKE=blocking"), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	path := filepath.Join(root, "big.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Touch(ctx, path, "plaintext"); done <- err }()
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not kill the server the writer was wedged on")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Touch outlived the server it was writing to")
		}
	case <-time.After(time.Second):
		t.Fatal("the kill did not free the wedged send")
	}
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
	t.Parallel()
	for _, length := range []string{"999999999999", "12abc"} {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		_, err := lsp.Start(ctx, lsp.Config{Command: "sh", Args: []string{"-c", `printf 'Content-Length: ` + length + `\r\n\r\n'; sleep 30`}, Root: t.TempDir()})
		if err == nil || ctx.Err() != nil {
			t.Fatalf("length %s: start = %v, deadline %v", length, err, ctx.Err())
		}
	}
}

// TestWatchedFiles: the server registers a watcher at start; a file nobody
// opened reaches it only through NotifyChanged, filtered by its globs, and
// a deleted open document is closed on the next touch.
func TestWatchedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := start(t, ctx, root)
	a, b, g := filepath.Join(root, "a.txt"), filepath.Join(root, "..b.txt"), filepath.Join(root, "c.go")
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
	if got := c.Diagnostics(a); len(got) != 0 {
		t.Fatalf("deleted document still open: diags %v", got)
	}
}
