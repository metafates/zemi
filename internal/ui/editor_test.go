package ui

import (
	"os"
	"path/filepath"
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
// error.
func TestEditFileWindow(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.CaptureHooks()
	defer a.host.Wins.CloseAll()
	dir := t.TempDir()
	for _, c := range []struct {
		code    string
		wantErr bool
	}{{"0", false}, {"3", true}} {
		script := filepath.Join(dir, "ed"+c.code)
		if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 0.2\necho edited >\"$1\"\nexit "+c.code+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		a.host.Settings.SetQuiet("editor.command", script)
		path := filepath.Join(dir, "f.txt")
		done := make(chan error, 1)
		go func() {
			_, err := a.EditFile(t.Context(), hostapi.EditRequest{Path: path, Title: "f.txt"})
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
					focused = true
				}
			}
		}
		for a.Pump() {
		}
		if b, _ := os.ReadFile(path); string(b) != "edited\n" || !focused || (err != nil) != c.wantErr {
			t.Errorf("exit %s: file %q, focused %v, err %v", c.code, b, focused, err)
		}
		if a.focusWin != 0 || len(a.host.Wins.List()) != 0 || a.editing != "" {
			t.Errorf("exit %s: focus %d, windows %d, editing %q left", c.code, a.focusWin, len(a.host.Wins.List()), a.editing)
		}
	}
}
