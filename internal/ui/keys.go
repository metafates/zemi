package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// handleKey is the key router (DESIGN §6.6 order): Go-reserved triggers,
// trust prompt, Go-owned chrome overlays, active Lua overlay, then the
// composer. There are no modes: the composer is always focused and slash
// commands replace the cmdline.
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
		case a.memReview != nil:
			a.answerMemReview()
		case a.cfgReview != nil:
			a.answerCfgReview()
		case a.question != nil:
			a.answerQuestion("", false)
		case a.host.AgentActive():
			a.host.AgentCancel()
			a.restoreQueue()
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
		if moveSel(&a.modelPick.sel, len(a.modelPick.items), key) {
			return nil
		}
		switch key {
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

	// 2c. Memory review: Go-owned chrome capturing all keys (§8, §10.4).
	if a.memReview != nil {
		if moveSel(&a.memReview.sel, len(a.memReview.items), key) {
			return nil
		}
		switch key {
		case "d", " ":
			if a.memReview.sel < len(a.memReview.items) {
				a.memReview.marked[a.memReview.sel] = !a.memReview.marked[a.memReview.sel]
			}
		case "esc", "enter", "q":
			a.answerMemReview()
		}
		return nil
	}

	// 2c-bis. Config review: Go-owned chrome capturing all keys (§8,
	// §10.4).
	if a.cfgReview != nil {
		if moveSel(&a.cfgReview.sel, len(a.cfgReview.items), key) {
			return nil
		}
		switch key {
		case "y", "a":
			a.cfgDecide(true)
		case "n", "x":
			a.cfgDecide(false)
		case "esc", "q":
			a.answerCfgReview()
		}
		return nil
	}

	// 2d. Question overlay: Go-owned chrome capturing all keys (§8).
	if a.question != nil {
		if moveSel(&a.question.sel, len(a.question.q.Options), key) {
			return nil
		}
		switch key {
		case "enter":
			if a.question.sel < len(a.question.q.Options) {
				a.answerQuestion(a.question.q.Options[a.question.sel].Label, true)
			} else {
				a.answerQuestion("", false)
			}
		case "esc", "q":
			a.answerQuestion("", false)
		}
		return nil
	}

	// 3. Active overlay.
	if len(a.overlays) > 0 {
		return a.overlayKey(key)
	}

	// 4. Insert-mode keymap bindings: exact single-key bindings, so users
	// can attach ctrl/alt combos; printable keys stay with the composer.
	if b := a.host.Keymaps.Lookup("insert", key); b != nil {
		return a.runRHS(b)
	}

	// 5. Composer (Go-owned multiline editor + popups).
	return a.composerKey(key, msg)
}

// composerKey routes keys to the multiline editor; the completion popup and
// the slash palette capture navigation while open. Returns a cmd when the
// file scan should launch.
func (a *App) composerKey(key string, msg tea.KeyPressMsg) tea.Cmd {
	c := &a.composer

	if a.palette.open {
		switch key {
		case "up":
			a.palette.move(-1)
			return nil
		case "down":
			a.palette.move(1)
			return nil
		case "tab":
			a.acceptPalette()
			return nil
		case "esc":
			a.palette.close()
			return nil
		}
		// enter falls through: it submits the typed "/name args" line
		// (running the command), tab is what accepts the selection.
	}

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
		a.updatePalette()
		return a.updateCompletion()
	case "delete":
		c.delete()
		a.updatePalette()
		return a.updateCompletion()
	case "left":
		c.left()
		a.updatePalette()
		return a.updateCompletion()
	case "right":
		c.right()
		a.updatePalette()
		return a.updateCompletion()
	case "home", "ctrl+a":
		c.lineStart()
		a.updatePalette()
		return a.updateCompletion()
	case "end", "ctrl+e":
		c.lineEnd()
		a.updatePalette()
		return a.updateCompletion()
	case "up":
		if c.empty() && a.restoreQueue() {
			return nil
		}
		if strings.Contains(c.text, "\n") {
			c.up()
		} else {
			c.histPrev()
		}
		a.updatePalette()
		return a.updateCompletion()
	case "down":
		if strings.Contains(c.text, "\n") {
			c.down()
		} else {
			c.histNext()
		}
		a.updatePalette()
		return a.updateCompletion()
	case "ctrl+p":
		c.histPrev()
	case "ctrl+n":
		c.histNext()
	case "ctrl+w", "alt+backspace":
		c.delWord()
		a.updatePalette()
		return a.updateCompletion()
	case "ctrl+u":
		c.delToLineStart()
		a.updatePalette()
		return a.updateCompletion()
	case "ctrl+k":
		c.delToLineEnd()
		a.updatePalette()
		return a.updateCompletion()
	case "esc":
		a.completion.close()
		a.palette.close()
	default:
		if t := keyText(msg); t != "" {
			c.insert(t)
			a.updatePalette()
			return a.updateCompletion()
		}
	}
	return nil
}

// moveSel applies up/down (k/j) to a list selection over n items; it
// reports whether the key was a move.
func moveSel(sel *int, n int, key string) bool {
	switch key {
	case "up", "k":
		*sel = max(*sel-1, 0)
	case "down", "j":
		*sel = max(min(*sel+1, n-1), 0)
	default:
		return false
	}
	return true
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
