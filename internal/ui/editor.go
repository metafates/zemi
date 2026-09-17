// editor.go hands a file to the user's editor (DESIGN §8): it runs in a
// focused float over zemi, so the TUI keeps drawing and keeps the terminal.
package ui

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/win"
)

// cfgStr reads a string config key, "" when unset.
func (a *App) cfgStr(key string) string {
	v, _ := a.host.Settings.Get(key)
	s, _ := v.(string)
	return s
}

// editorScript is the sh -c text that runs editor on "$1", so an editor with
// flags ("code --wait") splits like a shell would, with the common editors'
// starting line flag.
func editorScript(editor string, line int) string {
	args := []string{editor}
	if line > 0 {
		switch filepath.Base(strings.Fields(editor)[0]) {
		case "vi", "vim", "nvim", "nano", "micro", "emacs", "kak", "hx", "helix":
			args = append(args, "+"+strconv.Itoa(line))
		}
	}
	return strings.Join(append(args, `"$1"`), " ")
}

// EditFile implements hostapi.UIHooks: opens path in the user's editor
// (editor.command, $VISUAL, $EDITOR, vi) and blocks until it exits. A
// cancelled ctx returns and leaves the editor running, so unsaved work stays
// the user's. Never call it on the UI goroutine.
func (a *App) EditFile(ctx context.Context, req hostapi.EditRequest) (string, error) {
	editor := cmp.Or(strings.TrimSpace(a.cfgStr("editor.command")), strings.TrimSpace(os.Getenv("VISUAL")), strings.TrimSpace(os.Getenv("EDITOR")), "vi")
	w, err := a.host.OpenWindow(win.Spec{
		Cmd: editorScript(editor, req.Line), Args: []string{req.Path}, Kind: win.KindFloat,
		Title: "Edit " + req.Title, Owner: "editor",
		// the user's own editor gets the user's env: plugins read API keys, and the model cannot read the window
		Env: os.Environ(),
	})
	if err != nil {
		return "window", err
	}
	// NOTE: floatBox caps the size to the frame every frame, so an oversized
	// layout keeps the editor as large as the frame through resizes
	_ = w.SetLayout(func(l *win.Layout) { l.Cols, l.Rows = 1<<14, 1<<14 })
	prev := 0 // read and written on the UI goroutine only
	a.await(func() tea.Cmd {
		prev, a.editing = a.focusWin, sanitize.String(req.Title)
		a.setFocusWin(w.ID)
		return nil
	})
	if err := w.Wait(ctx); err != nil {
		a.await(func() tea.Cmd { a.editing = ""; return nil })
		return "window", err
	}
	a.await(func() tea.Cmd {
		a.editing = ""
		if a.focusWin == w.ID {
			a.setFocusWin(prev)
		}
		a.host.CloseWindow(w.ID)
		return nil
	})
	if code := w.ExitCode(); code != 0 {
		return "window", fmt.Errorf("the editor exited with code %d", code)
	}
	return "window", nil
}
