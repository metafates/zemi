package ui

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// handleKey is the key router (DESIGN §6.6 order): the Go-reserved ctrl+c,
// the chrome drawn on top (topChrome), a focused layout node's keymaps,
// then keymaps and the composer.
func (a *App) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	a.mouse = mouseSel{}
	top := a.topChrome()

	// A focused node takes every key but ctrl+c: its own keymaps, then the
	// global ones. Unbound keys do nothing, and an unbound esc gives the
	// keys back to the composer.
	if f := a.focusedNode(); top == "" && f != "" && key != "ctrl+c" {
		if !a.keySequence(key, f) && key == "esc" {
			a.host.SetFocus("")
		}
		a.bump()
		return nil
	}

	// Keymaps (zemi.keymap.set) take single keys and bound sequences;
	// printable keys stay with the composer, and an open popup keeps its
	// keys (tab accepts even though tab is bound). The composer is Go-drawn,
	// so typing must not bump the frame generation: that would re-render
	// every Lua widget on every keystroke.
	if top == "" && key != "ctrl+c" && (a.palette.open || a.completion.shown() || !a.keySequence(key, "")) {
		return a.composerKey(key, msg)
	}
	a.bump()

	// Go-reserved cancel trigger: never routed to Lua, never rebindable
	// (DESIGN §4, §6.2). It declines open chrome, interrupts a running
	// turn, and when idle quits on a second press within 2s.
	if key == "ctrl+c" {
		switch {
		case top == "perm":
			a.answerPerm(false, "user")
		case top == "memReview":
			a.answerMemReview()
		case top == "plan":
			a.finishPlan(tools.PlanVerdict{}, false)
		case top == "question":
			a.finishQuestion(false)
		case top == "trust":
			a.answerTrust("later")
		case top == "overlay":
			// the whole menu closes, not only a detail view on top of it
			i := len(a.overlays) - 1
			for i > 0 && a.overlays[i].back {
				i--
			}
			a.cancelOverlay(a.overlays[i].o)
		case a.host.AgentActive() && a.cancelling:
			// the turn did not stop: leave anyway, children die on exit
			a.forceQuit()
		case a.host.AgentActive():
			a.interrupt()
		case a.now().Before(a.quitArmed):
			a.Quit()
		default:
			a.quitArmed = a.now().Add(2 * time.Second)
		}
		return nil
	}

	// Go-owned chrome and Lua overlays capture all keys (§8, §10.4).
	switch top {
	case "trust":
		if a.program != nil && a.now().Sub(a.trust.opened) < permGrace {
			return nil
		}
		// enter is "not now": trusting runs project code, so it takes an explicit y
		switch key {
		case "y":
			a.answerTrust("trust")
		case "n":
			a.answerTrust("untrust")
		case "esc", "enter":
			a.answerTrust("later")
		}
	case "perm":
		pa := a.permAsk
		if a.program != nil && a.now().Sub(pa.opened) < permGrace {
			return nil
		}
		choices := pa.choices()
		if moveSel(&pa.sel, len(choices), key) {
			return nil
		}
		// any single-letter choice key answers directly (y, a, r, t; "o" is
		// "y"), so a new one-letter choice needs no new case here
		switch {
		case key == "enter":
			a.choosePerm(choices[pa.sel])
		case len(key) == 1 && key != "n":
			for _, c := range choices {
				if c.key == key || key == "o" && c.key == "y" {
					a.choosePerm(c)
				}
			}
		case key == "n" || key == "esc":
			a.answerPerm(false, "user")
		case key == "shift+down":
			pa.scroll++
		case key == "shift+up":
			pa.scroll = max(0, pa.scroll-1)
		case key == "pgdown":
			pa.scroll += 10
		case key == "pgup":
			pa.scroll = max(0, pa.scroll-10)
		}
	case "memReview":
		if moveSel(&a.memReview.sel, len(a.memReview.items), key) {
			return nil
		}
		switch key {
		case "a", "y":
			a.markMemory("keep")
		case "d", "x", "backspace", "delete":
			a.markMemory("drop")
		case "s":
			a.markMemory("scope")
		case "e":
			a.editMemory()
		case "u":
			a.undoMemory()
		case "esc", "q":
			a.answerMemReview()
		}
	case "plan":
		a.planKey(key, keyText(msg))
	case "question":
		a.questionKey(key, keyText(msg))
	case "overlay":
		return a.overlayKey(key, keyText(msg))
	}
	return nil
}

// composerKey routes keys to the multiline editor; the completion popup and
// the slash palette capture navigation while open. Returns a cmd when the
// file scan should launch.
func (a *App) composerKey(key string, msg tea.KeyPressMsg) tea.Cmd {
	c := &a.composer
	if key == "ctrl+z" {
		c.undoLast()
		a.updatePalette()
		return a.updateCompletion()
	}
	t := keyText(msg)
	defer c.snapshot(composerSnap{c.text, c.cursor}, t != "" && t != " ")

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
		case "enter":
			// a typed name that is not a command runs the highlighted one
			name, args := splitCommand(strings.TrimPrefix(c.text, "/"))
			if p := &a.palette; p.args == nil && p.selected < len(p.matches) && name != "reload" && a.host.Commands.Lookup(name) == nil {
				c.text = strings.TrimSpace("/" + p.matches[p.selected].Name + " " + args)
			}
		}
		// enter falls through: it submits the "/name args" line
	}

	if a.completion.shown() {
		if popupMove != 0 {
			moveSel(&a.completion.selected, a.completion.count(), key)
			return nil
		}
		switch {
		case key == "esc":
			a.completion.close()
			return nil
		case key != "tab" && key != "enter":
		case a.completion.count() > 0:
			a.acceptCompletion()
			return nil
		default: // no matches: the key is the composer's
			a.completion.close()
		}
	}

	switch key {
	case "enter":
		a.submitComposer()
		return nil
	case "shift+enter", "alt+enter", "ctrl+enter":
		if !a.multiline() {
			return nil
		}
		c.insert("\n")
		return a.updateCompletion()
	case "up":
		if c.empty() && a.restoreQueue() {
			return nil
		}
		if !c.up() {
			a.seedHistory()
			c.histPrev()
		}
	case "down":
		if !c.down() {
			c.histNext()
		}
	case "ctrl+p":
		a.seedHistory()
		c.histPrev()
		return nil
	case "ctrl+n":
		c.histNext()
		return nil
	case "ctrl+v":
		// terminals paste text themselves; this also reaches images
		return a.pasteClipboard()
	case "ctrl+g":
		return a.openEditor()
	case "esc":
		// Open popups consumed esc above, so a bare esc leaves an agent's
		// transcript, or else interrupts.
		if a.host.ViewedAgent() != "" {
			_ = a.host.ViewAgent("")
		} else if a.host.AgentActive() {
			a.interrupt()
		}
		return nil
	default:
		if !c.editKey(key) {
			if t == "" {
				return nil
			}
			c.insert(t)
		}
	}
	a.updatePalette()
	return a.updateCompletion()
}

// forceQuit leaves without the app.quit veto.
func (a *App) forceQuit() {
	go a.leave()
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

// keySequence runs bindings for focus, including multi-key ones ("ctrl+g
// s"): a key that starts or continues a bound sequence is held until the
// sequence completes, esc cancels it, and a key that breaks or outlives it
// is handled on its own.
func (a *App) keySequence(key, focus string) bool {
	seq := key
	if pending := a.keySeq; len(pending) > 0 {
		a.keySeq = nil
		if key == "esc" {
			return true
		}
		if a.now().Sub(a.keySeqAt) <= keySeqTimeout {
			seq = strings.Join(append(pending, key), " ")
		}
	}
	if b := a.host.Keymaps.Lookup(focus, seq); b != nil {
		a.runRHS(b)
		return true
	}
	if a.host.Keymaps.IsPrefix(focus, seq) {
		a.keySeq, a.keySeqAt = strings.Fields(seq), a.now()
		return true
	}
	return seq != key && a.keySequence(key, focus)
}

// focusedNode is the layout node taking keys: the host's focus while the
// last frame drew it, else "" (the composer).
func (a *App) focusedNode() string {
	if f := a.host.Focus(); f != "" && slices.Contains(a.focusables, f) {
		return f
	}
	return ""
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

// scrollDelta is how many rows a scroll key moves a text viewport: the page
// keys move a page, the list keys (j/k, arrows) a line. A viewport that also
// lists rows uses only the page keys, since the list keys select (overlay.go).
func scrollDelta(key string) int {
	switch key {
	case "pgup":
		return -10
	case "pgdown":
		return 10
	}
	return listMove(key)
}

// runRHS executes a keymap right-hand side on the ui class under the
// binding's registration origin (DESIGN §9 attribution), on the background
// worker.
func (a *App) runRHS(b *hostapi.KeyBinding) {
	a.background(func() {
		err := a.host.RunRHS(context.Background(), b)
		a.Refresh() // what it changed shows without waiting for another redraw
		switch {
		case err == nil:
		case errors.Is(err, lua.ErrHot), errors.Is(err, lua.ErrReloaded):
			a.log.Debug("ui", "keymap dropped (runtime hot)", map[string]any{"keys": b.Key})
		default:
			a.host.RecordError("keymap "+b.Key, err.Error())
			a.host.Log.Error("ui", "keymap rhs failed", map[string]any{"keys": b.Key, "error": err.Error()})
			a.Notify("error", "Keymap "+b.Key+" failed", err.Error())
		}
	})
}

// background runs fn in order on one worker goroutine: Lua keymaps and
// commands can block in a Go call (a job wait, say) for as long as the
// runtime's ceiling and grace, and the key loop, ctrl+c included, must not
// wait on them. Without a program (tests) fn runs inline.
func (a *App) background(fn func()) {
	if a.program == nil {
		fn()
		return
	}
	a.workOnce.Do(func() {
		a.work = make(chan func(), 64)
		go func() {
			for f := range a.work {
				func() {
					defer func() {
						if r := recover(); r != nil {
							a.log.Error("panic", fmt.Sprintf("keymap or command panicked: %v", r), map[string]any{"stack": string(debug.Stack())})
						}
					}()
					f()
				}()
			}
		}()
	})
	select {
	case a.work <- fn:
	default:
		a.pushToast("warn", "Busy", "still running an earlier key or command")
	}
}

// runCommand runs a slash command: Go commands inline, Lua ones on the
// background worker.
func (a *App) runCommand(name, args string) {
	if c := a.host.Commands.Lookup(name); c != nil && c.GoFn == nil {
		a.background(func() {
			if err := a.host.RunCommand(context.Background(), name, args); err != nil {
				a.Notify("error", "/"+name+" failed", err.Error())
			}
		})
		return
	}
	if err := a.host.RunCommand(context.Background(), name, args); err != nil {
		a.pushToast("error", "/"+name+" failed", err.Error())
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

// modeBase is the behavior of the active permission mode (plan, auto or
// yolo), which picks its colors.
func (a *App) modeBase() string {
	cur := a.host.PermMode()
	for _, m := range a.host.PermModeList() {
		if m.Name == cur {
			return m.Base
		}
	}
	return "auto"
}

// topChrome names the prompt drawn on top, which takes keys, the wheel,
// pastes and clicks. It mirrors the splice order in compose: perm over
// memReview, question, trust, then Lua overlays.
func (a *App) topChrome() string {
	switch {
	case a.permAsk != nil:
		return "perm"
	case a.memReview != nil:
		return "memReview"
	case a.planAsk != nil:
		return "plan"
	case a.question != nil:
		return "question"
	case a.trust != nil:
		return "trust"
	case len(a.overlays) > 0:
		return "overlay"
	}
	return ""
}

// wheelChrome scrolls the topmost prompt or list instead of the chat;
// it reports whether one took the wheel.
func (a *App) wheelChrome(step int) bool {
	move := func(sel *int, n int) { *sel = max(0, min(*sel+step, n-1)) }
	switch a.topChrome() {
	case "perm":
		a.permAsk.scroll = max(0, a.permAsk.scroll+step)
	case "plan":
		if !a.planAsk.typing { // the wheel scrolls the plan, not a text field
			a.planAsk.scroll = max(0, a.planAsk.scroll+step)
		}
	case "trust":
	case "memReview":
		move(&a.memReview.sel, len(a.memReview.items))
	case "question":
		move(a.question.sel())
	case "overlay":
		st := &a.overlays[len(a.overlays)-1]
		switch {
		case st.o.Input:
			return false
		case st.o.Body != "": // the body scrolls, the arrow keys move between actions
			st.scroll = max(0, st.scroll+step)
		default:
			move(&st.selected, len(st.visible))
		}
	default:
		return false
	}
	a.bump()
	return true
}

// pasteChrome sends a paste to the topmost filter or input instead of the
// composer hidden under it; prompts without text input swallow it.
func (a *App) pasteChrome(text string) bool {
	line := strings.Join(strings.Fields(text), " ")
	switch a.topChrome() {
	case "":
		return false
	case "question":
		qp := a.question
		if qp.reviewing() {
			break
		}
		// like a typed key, a paste on the last row starts the typed answer
		if st := &qp.states[qp.at]; st.typing || st.sel == len(qp.qs[qp.at].Options) {
			st.typing = true
			st.input.insert(line)
		}
	case "plan":
		p := a.planAsk
		if !p.typing {
			p.typing, p.input = true, composerState{}
		}
		p.input.insert(line)
	case "overlay":
		st := &a.overlays[len(a.overlays)-1]
		if st.o.Input {
			st.input.insert(line)
		} else {
			st.edit("", line, st.texts)
		}
	}
	a.bump()
	return true
}
