package ui

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/tools"
)

// handleKey is the key router (DESIGN §6.6 order): the Go-reserved ctrl+c,
// the chrome drawn on top (topChrome), a focused layout node's keymaps,
// then keymaps and the composer.
func (a *App) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	a.mouse = mouseSel{}
	a.dropAnsweredPerm() // a lost Refresh must not leave an inert box holding the keys
	top := a.topChrome()
	// an armed key acts only when pressed again right away on the bare
	// composer: an esc that answers a box in between does not count, and a
	// held key prefix never outlives a key that bypasses keySequence
	elsewhere := top != noModal || a.palette.open || a.completion.shown()
	// a list alias (zemi.keymap.set with list = true) swaps the key before
	// any list, popup or prompt sees it, but for a move a typed line edits with
	if b := a.host.Keymaps.ListAlias(key); elsewhere && b != nil && !(lineKey(b.RHS.Move) && a.typing(top)) {
		key, msg = b.RHS.Move, tea.KeyPressMsg{}
	}
	if elsewhere || key != a.armed.key {
		a.armed = armed{}
	}
	if elsewhere || key == "ctrl+c" {
		a.keySeq = nil
	}

	// A focused node takes every key but ctrl+c: its own keymaps, then the
	// global ones. Unbound keys do nothing, and an unbound esc gives the
	// keys back to the composer.
	if f := a.focusedNode(); top == noModal && f != "" && key != "ctrl+c" {
		if ok, _ := a.keySequence(key, f); !ok && key == "esc" {
			a.host.SetFocus("")
		}
		a.bump()
		return nil
	}

	// Keymaps (zemi.keymap.set) take single keys and bound sequences;
	// printable keys stay with the composer, and an open popup keeps its
	// keys. The composer is Go-drawn, so typing must not bump the frame
	// generation: that would re-render every Lua widget on every keystroke.
	if top == noModal && key != "ctrl+c" {
		if a.palette.open || a.completion.shown() {
			return a.composerKey(key, msg)
		}
		ok, replay := a.keySequence(key, "")
		if !ok {
			return tea.Batch(replay, a.composerKey(key, msg))
		}
		a.bump()
		return replay
	}
	a.bump()

	// Go-reserved cancel trigger: never routed to Lua, never rebindable
	// (DESIGN §4, §6.2). It declines open chrome, interrupts a running
	// turn, clears a draft, and on an empty composer quits when pressed twice.
	if key == "ctrl+c" {
		switch {
		case top == modalPerm && a.permAsk.answer != nil:
			a.answerPerm(false, tools.ByUser)
		case top == modalPlan:
			a.finishPlan(tools.PlanVerdict{}, false)
		case top == modalQuestion:
			a.finishQuestion(false)
		case top == modalTrust:
			a.answerTrust("later")
		case top == modalOverlay:
			a.cancelMenu()
		case a.host.AgentActive() && a.cancelling:
			// the turn did not stop: leave anyway, past the app.quit veto;
			// children die on exit
			go a.leave()
		case a.host.AgentActive():
			a.interrupt()
		case !a.composer.empty():
			// the draft stays one up away, and one ctrl+z
			a.seedHistory()
			a.composer.pushHistory(a.composer.text, a.historySize())
			prev := composerSnap{a.composer.text, a.composer.cursor}
			a.clearComposer()
			a.composer.snapshot(prev, false)
		case a.arm(key):
			a.Quit()
		}
		return nil
	}

	// Go-owned chrome and Lua overlays capture all keys (§8, §10.4).
	switch top {
	case modalTrust:
		if a.fresh(a.trust.opened) {
			return nil
		}
		tp := a.trust
		if scrollKey(&tp.view, key, false) || moveSel(&tp.sel, len(trustAnswers), len(trustAnswers), key) {
			return nil
		}
		switch key {
		case "y":
			a.answerTrust("trust")
		case "n":
			a.answerTrust("untrust")
		case "l", "esc":
			a.answerTrust("later")
		case "enter":
			a.answerTrust(trustAnswers[tp.sel])
		}
	case modalPerm:
		pa := a.permAsk
		if pa.answer == nil || a.fresh(pa.opened) {
			return nil
		}
		choices := pa.choices()
		if pa.view.overflows() && scrollKey(&pa.view, key, false) || moveSel(&pa.sel, len(choices), len(choices), key) {
			return nil
		}
		// a choice's key answers directly ("o" is "y"), so a new choice needs
		// no case here
		switch key {
		case "enter":
			a.choosePerm(choices[pa.sel])
		case "esc":
			a.answerPerm(false, tools.ByUser)
		default:
			for _, c := range choices {
				if c.key == key || key == "o" && c.key == "y" {
					a.choosePerm(c)
				}
			}
		}
	case modalPlan:
		a.planKey(key, keyText(msg))
	case modalQuestion:
		a.questionKey(key, keyText(msg))
	case modalOverlay:
		return a.overlayKey(key, keyText(msg))
	case noModal:
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
		return a.updateCompletion()
	}
	t := keyText(msg)
	defer c.snapshot(composerSnap{c.text, c.cursor}, t != "" && t != " ")

	// Popups take the list moves but for the ones that edit the draft.
	popupMove := key != "k" && key != "j" && !lineKey(key)
	if p := &a.palette; p.open {
		if popupMove && moveSel(&p.selected, p.count(), p.view.viewH, key) {
			p.moved = true
			return nil
		}
		switch key {
		case "tab":
			return a.acceptPalette()
		case "esc":
			a.palette.close()
			return nil
		case "enter":
			// the highlighted argument replaces a typed or picked one, so a
			// bare "/name " runs as typed, and the highlighted command
			// replaces a typed name that is none or that the user moved off
			name, args := hostapi.SplitCommand(strings.TrimPrefix(c.text, "/"))
			switch {
			case p.args != nil && p.selected < len(p.args) && (p.moved || c.cursor > p.argStart):
				c.replace(p.argStart, p.args[p.selected].label)
			case p.selected < len(p.matches) && (p.moved || a.host.Commands.Lookup(name) == nil):
				c.text = strings.TrimSpace("/" + p.matches[p.selected].Name + " " + args)
			}
		}
		// enter falls through: it submits the "/name args" line
	}

	if f := &a.completion; f.shown() {
		if popupMove && moveSel(&f.selected, f.count(), f.view.viewH, key) {
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
	case "shift+enter", "alt+enter", "ctrl+enter", "ctrl+j":
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
	case "ctrl+space": // reopens whatever popup the cursor's word takes, after esc
		return a.updateCompletion()
	case "esc":
		// Open popups consumed esc above, so a bare esc leaves an agent's
		// transcript, or else interrupts on a second press: esc closes every
		// box, and one press too many must not kill the turn.
		if a.host.ViewedAgent() != "" {
			a.armed = armed{}
			if back := a.host.LeaveAgentView(); back != "" {
				a.runCommand(hostapi.SplitCommand(back))
			}
		} else if _, _, busy := a.host.BusyNow(); (a.host.AgentActive() || busy) && a.arm(key) {
			a.host.CancelBusy()
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
	return a.updateCompletion()
}

// interrupt cancels the running turn and hands queued messages back to the
// composer; the working row reads "Cancelling" until the turn ends.
func (a *App) interrupt() {
	a.host.AgentCancel()
	a.restoreQueue()
	a.cancelling = a.host.AgentActive()
}

// moveSel applies a list key to a selection over n items: the list moves
// one item, ctrl+d/u half a page, the page keys page items, home and end
// the ends. It reports whether the key was a move.
func moveSel(sel *int, n, page int, key string) bool {
	switch key {
	case "ctrl+d":
		*sel += max(page/2, 1)
	case "ctrl+u":
		*sel -= max(page/2, 1)
	case "pgup":
		*sel -= page
	case "pgdown":
		*sel += page
	case "home", "ctrl+home":
		*sel = 0
	case "end", "ctrl+end":
		*sel = n - 1
	default:
		if listMove(key) == 0 {
			return false
		}
		*sel += listMove(key)
	}
	*sel = max(min(*sel, n-1), 0)
	return true
}

const keySeqTimeout = 1500 * time.Millisecond

// keySequence runs bindings for focus, including multi-key ones ("ctrl+x
// s"): a key that starts or continues a bound sequence is held until the
// sequence completes, esc cancels it, and when a key breaks or outlives it
// the held keys replay as plain ones before the key is handled on its own.
func (a *App) keySequence(key, focus string) (bool, tea.Cmd) {
	held := a.keySeq
	a.keySeq = nil
	if len(held) > 0 && key == "esc" {
		return true, nil
	}
	var replay tea.Cmd
	seq := key
	if len(held) > 0 && a.now().Sub(a.keySeqAt) <= keySeqTimeout {
		seq = strings.Join(append(held, key), " ")
	} else {
		replay, held = a.replayKeys(held, focus), nil
	}
	if b := a.host.Keymaps.Lookup(focus, seq); b != nil {
		a.runRHS(b)
		return true, replay
	}
	if a.host.Keymaps.IsPrefix(focus, seq) {
		a.keySeq, a.keySeqAt = strings.Fields(seq), a.now()
		return true, replay
	}
	if len(held) == 0 {
		return false, replay
	}
	replay = a.replayKeys(held, focus)
	ok, cmd := a.keySequence(key, focus)
	return ok, tea.Batch(replay, cmd)
}

// replayKeys hands held keys to the composer as plain ones, so a Go key
// under a leader (ctrl+g opens the editor) still works on its own. A
// focused node drops them, like any key it has no binding for.
func (a *App) replayKeys(held []string, focus string) tea.Cmd {
	if focus != "" || a.topChrome() != noModal {
		return nil
	}
	var cmds []tea.Cmd
	for _, k := range held {
		msg := tea.KeyPressMsg{}
		if len([]rune(k)) == 1 {
			msg.Text = k
		}
		cmds = append(cmds, a.composerKey(k, msg))
	}
	return tea.Batch(cmds...)
}

type armed struct {
	key   string
	until time.Time
}

// arm reports whether key is armed, disarming it: the second press acts.
// Otherwise it arms key for 2s.
func (a *App) arm(key string) bool {
	if a.isArmed(key) {
		a.armed = armed{}
		return true
	}
	a.armed = armed{key, a.now().Add(2 * time.Second)}
	return false
}

func (a *App) isArmed(key string) bool {
	return a.armed.key == key && a.now().Before(a.armed.until)
}

// focusedNode is the layout node taking keys: the host's focus while the
// last frame drew it, else "" (the composer).
func (a *App) focusedNode() string {
	if f := a.host.Focus(); f != "" && slices.Contains(a.focusables, f) {
		return f
	}
	return ""
}

// listMove maps list navigation keys (arrows, k/j, ctrl+p/n) to -1 or 1;
// 0 means the key is not a move. ctrl+k and ctrl+j stay the composer's
// unless a list alias takes them.
func listMove(key string) int {
	switch key {
	case "up", "k", "ctrl+p":
		return -1
	case "down", "j", "ctrl+n":
		return 1
	}
	return 0
}

// lineKey reports whether a list move is a key a typed line edits with,
// which the line keeps: home, end and ctrl+u, which deletes to its start.
func lineKey(key string) bool { return key == "home" || key == "end" || key == "ctrl+u" }

// typing reports whether the keys go to a line being typed: the draft, a
// filter, an input overlay or a typed answer.
func (a *App) typing(top modal) bool {
	switch top {
	case noModal:
		return true
	case modalOverlay:
		st := &a.overlays[len(a.overlays)-1]
		return st.o.Input || st.active
	case modalPlan:
		return a.planAsk.typing
	case modalQuestion:
		qp := a.question
		return !qp.reviewing() && qp.states[qp.at].typing
	default:
		return false
	}
}

// runRHS executes a keymap right-hand side on the background worker.
func (a *App) runRHS(b *hostapi.KeyBinding) {
	a.background(func() {
		_ = a.host.RunRHS(context.Background(), b) // it reports its own failure
		// what it changed shows without waiting for another redraw
		a.Refresh()
	})
}

// background runs fn in order on one worker goroutine: a Lua keymap or
// command holds it for a slice (the queue behind a long tool, its ceiling),
// and the key loop, ctrl+c included, must not wait on them. Without a
// program (tests) fn runs inline.
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
							a.log.Error(fmt.Sprintf("keymap or command panicked: %v", r), "origin", "panic", "stack", string(debug.Stack()))
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
		a.pushToast(slog.LevelWarn, "Key dropped", "an earlier key or command is still running")
	}
}

// runCommand runs a slash command: Go commands inline, Lua ones on the
// background worker.
func (a *App) runCommand(name, args string) {
	if c := a.host.Commands.Lookup(name); c != nil && c.GoFn == nil {
		a.background(func() {
			if err := a.host.RunCommand(context.Background(), name, args); err != nil {
				a.host.CommandFailed(name, err)
			}
		})
		return
	}
	if err := a.host.RunCommand(context.Background(), name, args); err != nil {
		a.host.CommandFailed(name, err)
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

// modal is a prompt drawn over the composer, which takes keys, the wheel,
// pastes and clicks while it is on top.
type modal int

const (
	noModal modal = iota
	modalPerm
	modalPlan
	modalQuestion
	modalTrust
	modalOverlay
)

// topChrome is the prompt drawn on top. It mirrors the splice order in
// compose: perm over question, trust, then Lua overlays.
func (a *App) topChrome() modal {
	switch {
	case a.permAsk != nil:
		return modalPerm
	case a.planAsk != nil:
		return modalPlan
	case a.question != nil:
		return modalQuestion
	case a.trust != nil:
		return modalTrust
	case len(a.overlays) > 0:
		return modalOverlay
	}
	return noModal
}

// wheel scrolls the box on top: the modal chrome, else an open composer
// popup, else the scrolled region under the pointer. In a box, text that
// overflows takes the wheel unless the pointer is on a list row; a list
// moves one item.
func (a *App) wheel(x, y, step int) {
	text, sel, n, ok := a.boxScroll()
	onRow := false // over a list row or a choice, which carry a Hit
	if y >= 0 && y < len(a.lastRows) {
		rest := dropCells(a.lastRows[y], x)
		onRow = len(rest) > 0 && rest[0].Hit > 0
	}
	switch {
	case !ok:
		a.scrollRows(a.scrollAt(x, y), step*chatWheelStep)
		return
	case text != nil && text.overflows() && !(sel != nil && onRow):
		text.scroll(step * chatWheelStep)
	case sel != nil:
		*sel = max(0, min(*sel+step, n-1))
	}
	a.bump()
}

// boxScroll is what scrolls in the box on top, the modal chrome else an
// open composer popup: its text, and its list's selection of n items. ok is
// false with no box on top.
func (a *App) boxScroll() (text *viewport, sel *int, n int, ok bool) {
	switch a.topChrome() {
	case modalPerm:
		text, sel, n = &a.permAsk.view, &a.permAsk.sel, len(a.permAsk.choices())
	case modalPlan:
		text = &a.planAsk.view
	case modalQuestion:
		if qp := a.question; !qp.reviewing() {
			text = &qp.states[qp.at].ask
		}
		sel, n = a.question.sel()
	case modalOverlay:
		if st := &a.overlays[len(a.overlays)-1]; !st.o.Input {
			text, sel, n = &st.body, &st.selected, len(st.visible)
		}
	case modalTrust:
		text = &a.trust.view
	default:
		switch {
		case a.popRect.Empty():
			return nil, nil, 0, false
		case a.palette.open:
			sel, n = &a.palette.selected, a.palette.count()
		default:
			sel, n = &a.completion.selected, a.completion.count()
		}
	}
	return text, sel, n, true
}

// pasteChrome sends a paste to the topmost filter or input instead of the
// composer hidden under it; prompts without text input swallow it.
func (a *App) pasteChrome(text string) bool {
	line := strings.Join(strings.Fields(text), " ")
	switch a.topChrome() {
	case noModal:
		return false
	case modalQuestion:
		qp := a.question
		if qp.reviewing() {
			break
		}
		// like a typed key, a paste on the last row starts the typed answer
		if st := &qp.states[qp.at]; st.typing || st.sel == len(qp.qs[qp.at].Options) {
			st.typing = true
			st.input.insert(line)
		}
	case modalPlan:
		p := a.planAsk
		if !p.typing {
			p.typing, p.input = true, composerState{}
		}
		p.input.insert(line)
	case modalOverlay:
		st := &a.overlays[len(a.overlays)-1]
		if st.o.Input {
			st.input.insert(line)
		} else if st.filters() {
			st.edit("", line)
		}
	case modalPerm, modalTrust: // no text input: swallowed
	}
	a.bump()
	return true
}
