// editor.go hands a file to the user's editor (DESIGN §8). Inside tmux the
// editor opens in a popup or a split and the TUI keeps drawing; otherwise
// bubbletea releases the terminal to it (tea.ExecProcess, never
// internal/proc: a child in its own session has no controlling terminal
// and an editor needs one) and takes it back when the editor exits.
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/editor"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
)

// editGrace swallows keys right after a full-screen editor returns: what
// was typed at its closing prompt is still in the terminal's buffer.
const editGrace = 150 * time.Millisecond

var editSeq atomic.Int64

// editDoneMsg is the full-screen editor's exit, delivered by bubbletea once
// the terminal is back.
type editDoneMsg struct {
	err  error
	done chan error
}

// cfgStr reads a string config key, "" when unset.
func (a *App) cfgStr(key string) string {
	v, _ := a.host.Settings.Get(key)
	s, _ := v.(string)
	return s
}

func (a *App) editorEnv() editor.Env {
	return editor.Env{
		Configured: a.cfgStr("editor.command"),
		Visual:     os.Getenv("VISUAL"),
		Editor:     os.Getenv("EDITOR"),
		TMUX:       os.Getenv("TMUX"),
		TmuxPane:   os.Getenv("TMUX_PANE"),
		Window:     a.cfgStr("editor.window"),
	}
}

// EditFile implements hostapi.UIHooks: opens path in the user's editor and
// blocks until it closes. Never call it on the UI goroutine.
func (a *App) EditFile(ctx context.Context, req hostapi.EditRequest) (string, error) {
	env := a.editorEnv()
	mode := env.Mode()
	title := sanitize.String(req.Title)
	chanName := fmt.Sprintf("zemi-edit-%d-%d", os.Getpid(), editSeq.Add(1))
	cmd := env.Cmd(mode, req.Path, a.host.Dirs.ProjectDir, title, chanName, req.Line)
	a.await(func() tea.Cmd { a.editing = title; return nil })
	defer a.await(func() tea.Cmd { a.editing = ""; a.editEnded = a.now(); return nil })

	if mode == editor.Fullscreen {
		done := make(chan error, 1)
		a.await(func() tea.Cmd {
			return tea.ExecProcess(cmd, func(err error) tea.Msg { return editDoneMsg{err: err, done: done} })
		})
		select {
		case err := <-done:
			return mode, err
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done // the terminal is restored only after the process exits
			return mode, ctx.Err()
		}
	}

	// tmux: a short client that blocks until the popup closes or the split
	// signals; the editor itself lives in the tmux server, so the client
	// keeps zemi's real environment (TERM, EDITOR) and its own session.
	c := proc.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	c.Env = os.Environ()
	buf := &proc.Buffer{Max: 4 << 10}
	c.Stdout, c.Stderr = buf, buf
	if err := proc.Run(c); err != nil {
		return mode, fmt.Errorf("tmux %s: %w: %s", cmd.Args[1], err, strings.TrimSpace(buf.String()))
	}
	if mode == editor.Split {
		w := proc.CommandContext(ctx, "tmux", "wait-for", chanName)
		if err := proc.Run(w); err != nil {
			if ctx.Err() != nil {
				_ = proc.Run(proc.Command("tmux", "wait-for", "-S", chanName))
				return mode, ctx.Err()
			}
			return mode, errors.New("tmux wait-for: " + err.Error())
		}
	}
	return mode, nil
}
