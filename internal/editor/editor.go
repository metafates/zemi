// Package editor builds the command that hands a file to the user's editor:
// full screen over the TUI, or inside tmux a popup or a split so the TUI
// keeps drawing. It decides and builds only; running is the caller's.
package editor

import (
	"cmp"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Env is everything the choice depends on, read once by the caller.
type Env struct {
	Configured string // the editor.command setting, wins over the environment
	Visual     string // $VISUAL
	Editor     string // $EDITOR
	TMUX       string // $TMUX, set inside a tmux pane
	TmuxPane   string // $TMUX_PANE
	Window     string // editor.window: auto, popup, split or fullscreen
}

// Modes.
const (
	Fullscreen = "fullscreen"
	Popup      = "popup"
	Split      = "split"
)

// Program is the editor command line: setting, then $VISUAL, $EDITOR, vi.
func (e Env) Program() string {
	return cmp.Or(strings.TrimSpace(e.Configured), strings.TrimSpace(e.Visual), strings.TrimSpace(e.Editor), "vi")
}

// Mode picks where the editor opens. Outside tmux everything is full screen;
// inside, auto means a popup.
func (e Env) Mode() string {
	if e.TMUX == "" || e.Window == Fullscreen {
		return Fullscreen
	}
	if e.Window == Split {
		return Split
	}
	return Popup
}

// lineFlag is how the common editors take a starting line.
func lineFlag(editor string, line int) []string {
	if line <= 0 {
		return nil
	}
	switch filepath.Base(strings.Fields(editor)[0]) {
	case "vi", "vim", "nvim", "nano", "micro", "emacs", "kak", "hx", "helix":
		return []string{"+" + strconv.Itoa(line)}
	}
	return nil
}

// script is the sh -c text that runs the editor on "$1", so an editor with
// flags ("code --wait") splits like a shell would.
func script(editor string, line int) string {
	return editor + " " + strings.Join(append(lineFlag(editor, line), `"$1"`), " ")
}

// Cmd is the command for mode. Full screen needs the terminal: run it
// through tea.ExecProcess, never internal/proc. Popup and split are short
// tmux clients that block until the editor exits; run them as ordinary
// children. chanName is the tmux wait-for channel a split signals on exit.
func (e Env) Cmd(mode, path, dir, title, chanName string, line int) *exec.Cmd {
	sh := script(e.Program(), line)
	switch mode {
	case Popup:
		return exec.Command("tmux", "display-popup", "-E", "-w", "90%", "-h", "90%",
			"-d", dir, "-t", e.TmuxPane, "-T", " "+title+" ", "sh", "-c", sh, "sh", path)
	case Split:
		// the pane runs the editor, then wakes the waiter
		return exec.Command("tmux", "split-window", "-h", "-c", dir, "-t", e.TmuxPane,
			"sh", "-c", sh+"; tmux wait-for -S "+chanName, "sh", path)
	}
	return exec.Command("sh", "-c", sh, "sh", path)
}
