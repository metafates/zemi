package ui

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

func TestEditorScript(t *testing.T) {
	for _, c := range []struct {
		editor string
		line   int
		want   string
	}{
		{"nvim", 12, `nvim +12 "$1"`},
		{"/usr/bin/vim -u NONE", 3, `/usr/bin/vim -u NONE +3 "$1"`},
		{"code --wait", 5, `code --wait "$1"`},
		{"nvim", 0, `nvim "$1"`},
	} {
		if got := editorScript(c.editor, c.line); got != c.want {
			t.Errorf("%s line %d: %q, want %q", c.editor, c.line, got, c.want)
		}
	}
}

// TestEditFileWindow: the editor runs in a focused float on the path, the
// window closes on exit and gives the focus back, and a failed exit is an
// error. A cancel returns at once and the window closes when the editor exits.
func TestEditFileWindow(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.CaptureHooks()
	defer a.host.Wins.CloseAll()
	dir := t.TempDir()
	// the editor stays open until the test has seen its window focused
	fifo := filepath.Join(dir, "seen")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	seen, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer seen.Close()
	for _, c := range []struct {
		code    string
		cancel  bool
		wantErr bool
	}{{"0", false, false}, {"3", false, true}, {"0", true, true}} {
		script := filepath.Join(dir, "ed"+c.code)
		if err := os.WriteFile(script, []byte("#!/bin/sh\nread x <"+fifo+"\necho edited >\"$1\"\nexit "+c.code+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		a.host.Settings.SetQuiet("editor.command", script)
		path := filepath.Join(dir, "f.txt")
		_ = os.Remove(path)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			err := a.EditFile(ctx, hostapi.EditRequest{Path: path, Title: "f.txt"})
			done <- err
		}()
		var err error
		focused := false
	wait:
		for {
			select {
			case err = <-done:
				break wait
			case <-time.After(5 * time.Second):
				t.Fatal("EditFile did not return")
			default:
				if !a.Pump() {
					time.Sleep(5 * time.Millisecond)
				}
				if wn := a.focusedWin(); wn != nil && wn.Owner == "editor" && wn.Title() == "Edit f.txt" {
					if !focused {
						seen.WriteString("\n")
					}
					focused = true
					if c.cancel {
						cancel()
					}
				}
			}
		}
		for deadline := time.Now().Add(3 * time.Second); len(a.host.Wins.List()) > 0 && time.Now().Before(deadline); {
			if !a.Pump() {
				time.Sleep(5 * time.Millisecond)
			}
		}
		if b, _ := os.ReadFile(path); string(b) != "edited\n" || !focused || (err != nil) != c.wantErr {
			t.Errorf("exit %s: file %q, focused %v, err %v", c.code, b, focused, err)
		}
		if a.focusWin != 0 || len(a.host.Wins.List()) != 0 || a.editing != "" {
			t.Errorf("exit %s: focus %d, windows %d, editing %q left", c.code, a.focusWin, len(a.host.Wins.List()), a.editing)
		}
	}
}
