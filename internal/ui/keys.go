package ui

import (
	"strings"
	"time"

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
	// (DESIGN §4, §6.2). It declines open chrome, interrupts a running
	// turn, and when idle quits on a second press within 2s.
	if key == "ctrl+c" {
		switch {
		case a.permAsk != nil:
			a.answerPerm(false, "user")
		case a.modelPick != nil:
			a.answerPick("", false)
		case a.memReview != nil:
			a.answerMemReview(false)
		case a.cfgReview != nil:
			a.answerCfgReview()
		case a.question != nil:
			a.answerQuestion("", false)
		case a.host.AgentActive():
			a.interrupt()
		case a.now().Before(a.quitArmed):
			a.Quit()
		default:
			a.quitArmed = a.now().Add(2 * time.Second)
		}
		return nil
	}

	// 2. The trust prompt is Go-owned chrome and captures all keys (§8).
	if a.trust != nil {
		switch key {
		case "y", "enter":
			a.answerTrust("trust")
		case "n", "esc":
			a.answerTrust("once")
		case "d":
			a.answerTrust("deny")
		}
		return nil
	}

	// 2b. Permission ask and model picker: Go-owned chrome capturing all
	// keys (§8).
	if pa := a.permAsk; pa != nil {
		if a.program != nil && a.now().Sub(pa.opened) < permGrace {
			return nil
		}
		choices := pa.choices()
		pick := func(by string) {
			for _, c := range choices {
				if c.by == by {
					a.answerPerm(c.ok, c.by)
					return
				}
			}
		}
		switch key {
		case "left", "shift+tab", "h":
			pa.sel = (pa.sel + len(choices) - 1) % len(choices)
		case "right", "tab", "l":
			pa.sel = (pa.sel + 1) % len(choices)
		case "enter":
			c := choices[pa.sel]
			a.answerPerm(c.ok, c.by)
		case "y", "o":
			pick("user:once")
		case "a":
			pick("user:always")
		case "n", "esc":
			a.answerPerm(false, "user")
		case "j", "down", "shift+down":
			pa.scroll++
		case "k", "up", "shift+up":
			pa.scroll = max(0, pa.scroll-1)
		case "pgdown":
			pa.scroll += 10
		case "pgup":
			pa.scroll = max(0, pa.scroll-10)
		}
		return nil
	}
	if mp := a.modelPick; mp != nil {
		if key != "j" && key != "k" && moveSel(&mp.selected, len(mp.visible), key) {
			return nil
		}
		if mp.edit(key, keyText(msg), mp.texts) {
			return nil
		}
		switch key {
		case "enter":
			if len(mp.visible) > 0 {
				a.answerPick(mp.items[mp.visible[mp.selected]].ID, true)
			}
		case "esc":
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
		case "a":
			a.markMemory(true)
		case "d", "space":
			a.markMemory(false)
		case "enter":
			a.answerMemReview(true)
		case "esc", "q":
			a.answerMemReview(false)
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
		return a.overlayKey(key, keyText(msg))
	}

	// 4. Keymaps (zemi.keymap.set): single keys, so users attach ctrl/alt
	// combos; printable keys stay with the composer. An open popup keeps
	// its keys (tab accepts even though tab is bound).
	if !a.palette.open && !a.completion.open {
		if cmd, used := a.keySequence(key); used {
			return cmd
		}
	}

	// 5. Composer (Go-owned multiline editor + popups).
	return a.composerKey(key, msg)
}

// composerKey routes keys to the multiline editor; the completion popup and
// the slash palette capture navigation while open. Returns a cmd when the
// file scan should launch.
func (a *App) composerKey(key string, msg tea.KeyPressMsg) tea.Cmd {
	c := &a.composer

	// Popups take only the ctrl moves: plain k/j are typed text.
	popupMove := 0
	if key != "k" && key != "j" {
		popupMove = listMove(key)
	}
	if a.palette.open {
		if popupMove != 0 {
			a.palette.move(popupMove)
			return nil
		}
		switch key {
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
		if popupMove != 0 {
			moveSel(&a.completion.selected, len(a.completion.matches), key)
			return nil
		}
		switch key {
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
		// Open popups consumed esc above, so a bare esc interrupts.
		if a.host.AgentActive() {
			a.interrupt()
		}
	default:
		if t := keyText(msg); t != "" {
			c.insert(t)
			a.updatePalette()
			return a.updateCompletion()
		}
	}
	return nil
}

// interrupt cancels the running turn and hands queued messages back to the
// composer; the working row reads "Cancelling" until the turn ends.
func (a *App) interrupt() {
	a.host.AgentCancel()
	a.restoreQueue()
	a.cancelling = true
}

// moveSel applies up/down to a list selection over n items; it reports
// whether the key was a move.
func moveSel(sel *int, n int, key string) bool {
	switch listMove(key) {
	case -1:
		*sel = max(*sel-1, 0)
	case 1:
		*sel = max(min(*sel+1, n-1), 0)
	default:
		return false
	}
	return true
}

const keySeqTimeout = 1500 * time.Millisecond

// keySequence runs bindings, including multi-key ones ("ctrl+g s"): a key
// that starts or continues a bound sequence is held until the sequence
// completes, esc cancels it, and a key that breaks or outlives it is
// handled on its own.
func (a *App) keySequence(key string) (tea.Cmd, bool) {
	seq := key
	if pending := a.keySeq; len(pending) > 0 {
		a.keySeq = nil
		if key == "esc" {
			return nil, true
		}
		if a.now().Sub(a.keySeqAt) <= keySeqTimeout {
			seq = strings.Join(append(pending, key), " ")
		}
	}
	if b := a.host.Keymaps.Lookup(seq); b != nil {
		return a.runRHS(b), true
	}
	if a.host.Keymaps.IsPrefix(seq) {
		a.keySeq, a.keySeqAt = strings.Fields(seq), a.now()
		return nil, true
	}
	if seq != key {
		return a.keySequence(key)
	}
	return nil, false
}

// listMove maps list navigation keys (arrows, k/j, and the nvim-style
// ctrl+k/j, ctrl+p/n) to -1 or 1; 0 means the key is not a move.
func listMove(key string) int {
	switch key {
	case "up", "k", "ctrl+k", "ctrl+p":
		return -1
	case "down", "j", "ctrl+j", "ctrl+n":
		return 1
	}
	return 0
}

// runRHS executes a keymap right-hand side on the ui class under the
// binding's registration origin (DESIGN §9 attribution).
func (a *App) runRHS(b *hostapi.KeyBinding) tea.Cmd {
	if err := a.host.RunRHS(a.ctx(), b.Origin, b.RHS, ""); err != nil {
		if err == lua.ErrHot || err == lua.ErrReloaded {
			a.log.Debug("ui", "keymap dropped (runtime hot)", map[string]any{"keys": b.Key})
			return nil
		}
		a.host.RecordError("keymap "+b.Key, err.Error())
		a.host.Log.Error("ui", "keymap rhs failed", map[string]any{
			"keys": b.Key, "error": err.Error(),
		})
		a.pushToast("error", "keymap "+b.Key, err.Error())
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
