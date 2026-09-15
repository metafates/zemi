package ui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// handleKey is the key router (DESIGN §6.6 order): Go-reserved triggers,
// trust prompt, cmdline, active overlay, mode keymap (with pending
// multi-key state in normal mode), composer, builtin fallbacks.
func (a *App) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	a.frameGen++

	// 1. Go-reserved cancel trigger: never routed to Lua, never rebindable
	// (DESIGN §4, §6.2). During an active turn it aborts the agent.
	if key == "ctrl+c" {
		switch {
		case a.permAsk != nil:
			a.answerPerm(false, "user")
		case a.modelPick != nil:
			a.answerPick("", false)
		case a.host.AgentActive():
			a.host.AgentCancel()
			a.pushToast("warn", "cancel", "cancelling turn")
		default:
			a.pushToast("warn", "cancel", "no active turn")
		}
		return nil
	}

	// 2. The trust prompt is Go-owned chrome and captures all keys (§8).
	if a.trust != nil {
		switch key {
		case "y", "enter":
			a.answerTrust(true)
		case "n", "esc":
			a.answerTrust(false)
		}
		return nil
	}

	// 2b. Permission ask and model picker: Go-owned chrome capturing all
	// keys (§8).
	if a.permAsk != nil {
		switch key {
		case "o":
			a.answerPerm(true, "user:once")
		case "a":
			a.answerPerm(true, "user:always")
		case "n", "esc":
			a.answerPerm(false, "user")
		}
		return nil
	}
	if a.modelPick != nil {
		switch key {
		case "up", "k":
			if a.modelPick.sel > 0 {
				a.modelPick.sel--
			}
		case "down", "j":
			if a.modelPick.sel < len(a.modelPick.items)-1 {
				a.modelPick.sel++
			}
		case "enter":
			if a.modelPick.sel < len(a.modelPick.items) {
				a.answerPick(a.modelPick.items[a.modelPick.sel].ID, true)
			} else {
				a.answerPick("", false)
			}
		case "esc", "q":
			a.answerPick("", false)
		}
		return nil
	}

	// 3. Cmdline.
	if a.cmdline.open {
		return a.cmdlineKey(key, msg)
	}

	// 4. Active overlay.
	if len(a.overlays) > 0 {
		return a.overlayKey(key)
	}

	// 5. Mode keymap. Normal mode arms the pending state for multi-key
	// sequences (statusline shows it, 1s timeout, §6.2).
	if a.mode == "normal" {
		seq := a.pending + key
		if b := a.host.Keymaps.Lookup("normal", seq); b != nil {
			a.pending = ""
			return a.runRHS(b)
		}
		if a.host.Keymaps.IsPrefix("normal", seq) {
			a.pending = seq
			a.pendAt = time.Now()
			return a.pendingTimeoutCmd(seq)
		}
		a.pending = ""
	} else if b := a.host.Keymaps.Lookup("insert", key); b != nil {
		// Insert mode: exact single-key bindings only (no pending state in
		// the middle of typing).
		return a.runRHS(b)
	}

	// 6. Composer (insert mode; Go-owned multiline editor + completion).
	if a.mode == "insert" {
		return a.composerKey(key, msg)
	}

	// 7. Builtin fallbacks after Lua keymaps missed (normal mode).
	switch key {
	case "i":
		a.mode = "insert"
	}
	return nil
}

func (a *App) pendingTimeoutCmd(seq string) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(pendingMaxAge)
		return pendingTimeoutMsg{seq: seq}
	}
}

// composerKey routes insert-mode keys to the multiline editor; the
// completion popup captures navigation while open. Returns a cmd when the
// file scan should launch.
func (a *App) composerKey(key string, msg tea.KeyPressMsg) tea.Cmd {
	c := &a.composer

	if a.completion.open {
		switch key {
		case "up":
			if a.completion.selected > 0 {
				a.completion.selected--
			}
			return nil
		case "down":
			if a.completion.selected < len(a.completion.matches)-1 {
				a.completion.selected++
			}
			return nil
		case "tab", "enter":
			a.acceptCompletion()
			return nil
		case "esc":
			a.completion.close()
			return nil
		}
	}

	switch key {
	case "enter":
		a.submitComposer()
	case "shift+enter", "alt+enter":
		if a.multiline() {
			c.insert("\n")
			return a.updateCompletion()
		}
	case "backspace":
		c.backspace()
		return a.updateCompletion()
	case "delete":
		c.delete()
		return a.updateCompletion()
	case "left":
		c.left()
		return a.updateCompletion()
	case "right":
		c.right()
		return a.updateCompletion()
	case "home", "ctrl+a":
		c.lineStart()
		return a.updateCompletion()
	case "end", "ctrl+e":
		c.lineEnd()
		return a.updateCompletion()
	case "up":
		if strings.Contains(c.text, "\n") {
			c.up()
		} else {
			c.histPrev()
		}
		return a.updateCompletion()
	case "down":
		if strings.Contains(c.text, "\n") {
			c.down()
		} else {
			c.histNext()
		}
		return a.updateCompletion()
	case "ctrl+p":
		c.histPrev()
	case "ctrl+n":
		c.histNext()
	case "ctrl+w", "alt+backspace":
		c.delWord()
		return a.updateCompletion()
	case "ctrl+u":
		c.delToLineStart()
		return a.updateCompletion()
	case "ctrl+k":
		c.delToLineEnd()
		return a.updateCompletion()
	case "esc":
		a.mode = "normal"
	default:
		if t := keyText(msg); t != "" {
			c.insert(t)
			return a.updateCompletion()
		}
	}
	return nil
}

// runRHS executes a keymap right-hand side on the ui class under the
// binding's registration origin (DESIGN §9 attribution).
func (a *App) runRHS(b *hostapi.KeyBinding) tea.Cmd {
	if err := a.host.RunRHS(a.ctx(), b.Origin, b.RHS, ""); err != nil {
		if err == lua.ErrHot || err == lua.ErrReloaded {
			a.log.Debug("ui", "keymap dropped (runtime hot)", map[string]any{"keys": b.Keys})
			return nil
		}
		a.errFlag = true
		a.host.Log.Error("ui", "keymap rhs failed", map[string]any{
			"keys": b.Keys, "error": err.Error(),
		})
		a.pushToast("error", "keymap "+b.Keys, err.Error())
	}
	return nil
}

// submitComposer runs the composer.submit vetoable event, then appends the
// message to the transcript (M0 data flow; DESIGN §6.2, §8).
func (a *App) submitComposer() {
	text := strings.TrimSpace(a.composer.text)
	if text == "" {
		return
	}
	res := a.host.EmitVetoable(a.ctx(), "composer.submit", map[string]any{"text": text}, lua.VetoDeadline)
	if res.Denied {
		a.pushToast("warn", "composer", "blocked: "+res.Reason)
		return
	}
	a.composer.pushHistory(text, a.historySize())
	a.composer.text = ""
	a.composer.cursor = 0
	a.completion.close()
	a.host.AppendMessage("user", text)
	// M1 data flow (DESIGN §6.3): the message drives the agent; during an
	// active turn it is enqueued as steering.
	if err := a.host.AgentSend(text); err != nil {
		a.pushToast("error", "agent", err.Error())
	}
}

// keyText extracts the literal text a keypress should insert (printable
// runes, optionally shift-modified; nothing for control combos).
func keyText(msg tea.KeyPressMsg) string {
	if msg.Text == "" {
		if msg.String() == "space" {
			return " "
		}
		return ""
	}
	if msg.Mod != 0 && msg.Mod != tea.ModShift {
		return ""
	}
	for _, r := range msg.Text {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return msg.Text
}
