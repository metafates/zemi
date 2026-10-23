// editor.go hands a file to the user's editor (DESIGN §8): bubbletea's
// tea.ExecProcess releases the terminal and runs the editor fullscreen over
// zemi, then restores the TUI when it exits.

package ui

import (
	"cmp"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
)

// editorScript is the shell text that runs editor on "$1", so an editor with
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

// editorDoneMsg is delivered when the editor's process exits; errc unblocks
// the waiting EditFile caller.
type editorDoneMsg struct {
	errc chan<- error
	err  error
}

// EditFile implements hostapi.UIHooks: opens path in the user's editor
// (editor.command, $VISUAL, $EDITOR, vi) fullscreen and blocks until it
// exits. A cancelled ctx returns and leaves the editor running, so unsaved
// work stays the user's. Never call it on the UI goroutine.
func (a *App) EditFile(ctx context.Context, req hostapi.EditRequest) error {
	editor := cmp.Or(strings.TrimSpace(a.host.Settings.Str(config.KeyEditorCommand)), strings.TrimSpace(os.Getenv("VISUAL")), strings.TrimSpace(os.Getenv("EDITOR")), "vi")
	sh := proc.Shell()
	script := editorScript(editor, req.Line)
	// sh takes the file as argv["1"]; cmd and PowerShell take one command
	// line, so the file is quoted into it in place of "$1".
	var c *exec.Cmd
	switch {
	case sh == "cmd":
		c = exec.Command(sh, "/c", strings.Replace(script, `"$1"`, `"`+req.Path+`"`, 1))
	case sh == "pwsh":
		q := "'" + strings.ReplaceAll(req.Path, "'", "''") + "'"
		c = exec.Command(sh, "-NoProfile", "-NonInteractive", "-Command", strings.Replace(script, `"$1"`, q, 1))
	default:
		c = exec.Command(sh, "-c", script, sh, req.Path)
	}
	// NOTE: not internal/proc: the editor needs the controlling terminal
	// Setsid would drop, and tea.ExecProcess runs and waits for it itself.
	errc := make(chan error, 1)
	a.await(func() tea.Cmd {
		a.editing = sanitize.String(req.Title)
		return tea.ExecProcess(c, func(err error) tea.Msg { return editorDoneMsg{errc: errc, err: err} })
	})
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
