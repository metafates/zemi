package ui

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/tools"
)

// goKeys are the bindings this router owns: they are not Lua keymaps, so
// /help would list none of them.
var goKeys = [][2]string{
	{"enter", "send; steers a running turn"},
	{"shift+enter, alt+enter, ctrl+enter", "newline"},
	{"esc", "back; twice to interrupt"},
	{"ctrl+c", "clear the draft, interrupt, quit"},
	{"ctrl+g", "edit the draft in your editor"},
	{"ctrl+v", "paste, images included"},
	{"up, ctrl+p", "earlier prompts or queued ones"},
	{"down, ctrl+n", "later prompts"},
	{"ctrl+z", "undo in the composer"},
	{"ctrl+a/e", "line start, line end"},
	{"alt+left, alt+right", "move by word"},
	{"ctrl+d/u", "scroll the chat half a view"},
	{"ctrl+w", "delete the word before the cursor"},
	{"delete", "forward delete"},
	{"/", "command palette"},
	{"@", "attach a project file"},
	{"+", "attach a skill"},
	{"ctrl+space", "show completions; tab, enter accept"},
	{"!cmd", "run a shell command with your next message"},
}

// listKeys are the keys every list and box takes.
var listKeys = [][2]string{
	{"up, down, j, k, ctrl+p, ctrl+n", "move"},
	{"g, G, home, end", "first row, last row"},
	{"ctrl+home, ctrl+b", "to the top, bottom"},
	{"ctrl+d, ctrl+u", "half a page, or scroll the text"},
	{"pgup, pgdown", "move a page"},
	{"/", "filter; enter keeps it, esc clears it"},
	{"space", "toggle, in toggle lists"},
	{"y, Y", "in a list, copy the row, every row"},
	{"an action's key", "act on the row, as its button does"},
	{"shift+up, shift+down", "scroll the text a line"},
}

// KeyHelp implements hostapi.UIHooks: what /help lists as keys, owned here
// because this file is what routes them.
func (a *App) KeyHelp() (keys, lists [][2]string) { return goKeys, listKeys }

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
	// A key under the chrome moves at most the chrome or a draft: the branches
	// below bump when one does, and a key that moves nothing leaves the
	// generation alone (the cleared hover repaints through frameDirty).
	if key == "ctrl+c" {
		a.bump() // every branch below acts
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
		case !a.composer.empty():
			// the draft stays one up away, and one ctrl+z
			a.clearDraft()
		case a.host.AgentActive() && a.cancelling:
			// the turn did not stop: leave anyway, past the app.quit veto;
			// children die on exit
			go a.leave()
		case a.host.AgentActive():
			a.interrupt()
		case a.arm(key, "quit"):
			a.Quit()
		}
		return nil
	}

	// Go-owned chrome and Lua overlays capture all keys (§8, §10.4).
	switch top {
	case modalBoot:
		// the shipped Lua layer failed: nothing else on this screen acts
		a.bump()
		if scrollKey(&a.bootView, key, false) {
			return nil
		}
		switch key {
		case "c":
			a.copyBootError()
		case "q":
			a.Quit()
		}
	case modalTrust:
		if a.askGuard(a.trust.opened, false) {
			return nil
		}
		tp := a.trust
		if scrollKey(&tp.view, key, false) || moveSel(&tp.sel, len(trustAnswers), len(trustAnswers), key) {
			a.bump()
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
		if pa == nil {
			return nil
		}
		if a.askGuard(pa.opened, pa.answer == nil) {
			return nil
		}
		choices := pa.choices()
		if pa.view.overflows() && scrollKey(&pa.view, key, false) || moveSel(&pa.sel, len(choices), len(choices), key) {
			a.bump()
			return nil
		}
		// a choice's key answers directly, so a new choice needs no case here
		switch key {
		case "enter":
			a.choosePerm(choices[pa.sel])
		case "esc":
			a.answerPerm(false, tools.ByUser)
		default:
			for _, c := range choices {
				if c.key == key {
					a.choosePerm(c)
					return nil
				}
			}
		}
	case modalPlan:
		if a.askGuard(a.planAsk.opened, false) {
			return nil
		}
		a.planKey(key, keyText(msg))
	case modalQuestion:
		if a.askGuard(a.question.opened, false) {
			return nil
		}
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
	// A key may move only the draft or a popup, which repaints without
	// re-rendering the widgets: typing stays off the frame generation. The
	// draft's changes mark through publishComposer, so a key that changes
	// nothing marks nothing - a flood of refused keys must not recompose the
	// screen a key.
	c := &a.composer
	if key == "ctrl+z" {
		c.undoLast()
		return a.updateCompletion()
	}
	t := keyText(msg)
	defer c.snapshot(composerSnap{text: c.text, cursor: c.cursor}, t != "" && t != " ")

	// Popups take the list moves but for the ones that edit the draft: a
	// printable key (j, k, g) types, it never jumps.
	popupMove := !slices.Contains([]string{"k", "j", "g", "G"}, key) && !lineKey(key)
	if p := &a.palette; p.open {
		if popupMove && moveSel(&p.selected, p.count(), p.view.viewH, key) {
			p.moved = true
			a.frameDirty = true
			return nil
		}
		switch key {
		case "tab":
			return a.acceptPalette()
		case "esc":
			a.dismissPalette()
			a.frameDirty = true
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
			a.frameDirty = true
			return nil
		}
		switch {
		case key == "esc":
			a.completion.close()
			a.frameDirty = true
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
		if !c.insertFits("\n") {
			a.draftCapToast()
			return nil
		}
		return a.updateCompletion()
	case "up":
		if c.empty() && a.restoreQueue() {
			return nil
		}
		if !a.composerMove(-1) {
			a.seedHistory()
			c.histPrev()
		}
	case "down":
		if !a.composerMove(1) {
			c.histNext()
		}
	case "ctrl+d", "ctrl+u":
		// Half a view, the step a list and a box prompt move on the same keys:
		// the pair scrolls wherever it lands, the draft included.
		if st := a.scrolls[a.scrollDef]; st != nil {
			step := max(st.viewH/2, 1)
			if key == "ctrl+u" {
				step = -step
			}
			a.scrollRows(a.scrollDef, step)
		}
		return nil
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
		// box, and one press too many must not kill the turn. Over the draft
		// cap esc clears: every edit there is refused, so back is the way out.
		if a.host.ViewedAgent() != "" {
			a.leaveAgentView()
		} else if label, _ := a.host.BusyNow(); a.host.AgentActive() || label != "" {
			if a.arm(key, "interrupt") {
				a.host.CancelBusy()
				a.interrupt()
			}
		} else if utf8.RuneCountInString(a.composer.text) > maxDraftChars {
			a.clearDraft()
		}
		return nil
	default:
		if !c.editKey(key) {
			if t == "" {
				return nil
			}
			if !c.insertFits(t) {
				a.draftCapToast()
				return nil // nothing changed: no completion walk, no repaint
			}
		}
	}
	return a.updateCompletion()
}

// draftCapToast refuses text past maxDraftChars, with the way out the send
// boundary's refusal names too. One showing toast is enough: a paste flood
// refused per keystroke must not pay a repaint a key, and the cooldown after
// it lets the toast fade while the refusals keep coming.
func (a *App) draftCapToast() {
	if a.now().Before(a.capToastAt) {
		return
	}
	a.capToastAt = a.now().Add(capToastTTL)
	a.pushToast(slog.LevelWarn, "Draft too long",
		fmt.Sprintf("the composer holds at most %dk characters; save the text to a file and attach it with @path", maxDraftChars/1000))
}

// capToastTTL is how long a draft-cap toast lives and how long the next one
// waits: pushToast's warn ttl.
const capToastTTL = 8 * time.Second

// clearDraft empties the draft the way ctrl+c does: one history entry up and
// one ctrl+z back.
func (a *App) clearDraft() {
	a.seedHistory()
	a.composer.pushHistory(a.composer.text)
	prev := composerSnap{text: a.composer.text, cursor: a.composer.cursor}
	a.clearComposer()
	a.composer.snapshot(prev, false)
}

// leaveAgentView is esc on a shown run: it restores the menu the view was
// raised from, shows the session again and runs the back command that
// reopens the screen the view came from over it.
func (a *App) leaveAgentView() {
	if menu := a.host.ViewedAgentMenu(); menu != "" {
		a.runCommand(menu, "", false)
	}
	a.armed = armed{}
	if back := a.host.LeaveAgentView(); back != "" {
		name, args := hostapi.SplitCommand(back)
		a.runCommand(name, args, false)
	}
}

// interrupt cancels the running turn and hands queued messages back to the
// composer; the working row keeps its ctrl+c quit hint until the turn ends.
func (a *App) interrupt() {
	// a turn parked in a plan or question ask waits on a channel no cancel
	// reaches; dismiss it with the verdict a cancelled turn discards anyway
	if a.planAsk != nil {
		a.finishPlan(tools.PlanVerdict{}, false)
	}
	if a.question != nil {
		a.finishQuestion(false)
	}
	a.host.AgentCancel()
	a.restoreQueue()
	a.cancelling = a.host.AgentActive()
}

// moveSel applies a list key to a selection over n items: the list moves
// one item, ctrl+d/u half a page, the page keys page items, home/end and
// g/G the ends. It reports whether the key was a move.
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
	case "home", "ctrl+home", "g":
		*sel = 0
	case "end", "ctrl+b", "G":
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
	what  string // what the second press does: an esc armed to interrupt stays one when the turn ends before the second press
	until time.Time
}

// arm reports whether key is armed for what, disarming it: the second press
// acts. Otherwise it arms key for 2s.
func (a *App) arm(key, what string) bool {
	if a.isArmed(key) && a.armed.what == what {
		a.armed = armed{}
		return true
	}
	a.armed = armed{key: key, what: what, until: a.now().Add(2 * time.Second)}
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
// 0 means the key is not a move. ctrl+j and ctrl+k scroll the chat, so a
// list takes them only through a list alias.
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
// which the line keeps: home and end. ctrl+d and ctrl+u are never the line's,
// so the half-page pair scrolls whatever stands above it, typed or not.
func lineKey(key string) bool { return key == "home" || key == "end" }

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

// askGuard is the guard the four blocking asks share (DESIGN §8): keys still
// in flight as the box opened, and keys on an answered permission box, are
// swallowed; past the grace every key is the ask's, draft or not.
func (a *App) askGuard(opened time.Time, answered bool) bool {
	return answered || a.fresh(opened)
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

// runCommand runs a slash command fire-and-forget: Lua and Go alike on the
// background worker, so a Go command that loads the store (/messages, /usage)
// never blocks a key. confirm says an armed danger key ran it.
func (a *App) runCommand(name, args string, confirm bool) {
	if c := a.host.Commands.Lookup(name); c != nil && c.GoFn != nil {
		a.background(func() { _ = a.runGo(name, args, confirm) })
		return
	}
	if err := a.runCommandErr(name, args, confirm); err != nil {
		a.host.CommandFailed(name, err)
	}
}

// runCommandErr is runCommand for a caller that shows a Go command's error
// itself: an input overlay keeps its box open and names the failure where
// the value was typed, so a Go command runs inline. Lua commands report
// through CommandFailed as always.
func (a *App) runCommandErr(name, args string, confirm bool) error {
	if c := a.host.Commands.Lookup(name); c != nil && c.GoFn == nil {
		a.background(func() { _ = a.runGo(name, args, confirm) })
		return nil
	}
	return a.host.RunCommand(context.Background(), name, args, confirm)
}

// runGo runs a Go command, reporting a failure the way any command's is. It
// returns the failure for a caller that acts on it.
func (a *App) runGo(name, args string, confirm bool) error {
	err := a.host.RunCommand(context.Background(), name, args, confirm)
	if err != nil {
		a.host.CommandFailed(name, err)
	}
	return err
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

// modal is a prompt drawn over the composer, which takes keys, the wheel,
// pastes and clicks while it is on top.
type modal int

const (
	noModal modal = iota
	modalBoot
	modalPerm
	modalPlan
	modalQuestion
	modalTrust
	modalOverlay
)

// topChrome is the prompt drawn on top. It mirrors the splice order in
// compose: boot over the modals, then perm over plan, question, trust.
func (a *App) topChrome() modal {
	switch {
	case a.bootErr != nil:
		return modalBoot
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
// moves one item. A wheel that moves nothing bumps no generation: the
// cleared hover repaints through frameDirty.
func (a *App) wheel(x, y, step int) {
	a.frameDirty = true
	text, sel, n, ok := a.boxScroll()
	onRow := false // over a list row or a choice, which carry a Hit
	if y >= 0 && y < len(a.lastRows) {
		rest := dropCells(a.lastRows[y], x)
		onRow = len(rest) > 0 && rest[0].Hit > 0
	}
	switch {
	case !ok:
		a.scrollRows(a.scrollAt(x, y), step)
		return
	case text != nil && text.overflows() && !(sel != nil && onRow):
		top := text.top
		text.scroll(step)
		if text.top != top {
			a.bump()
		}
	case sel != nil:
		if at := max(0, min(*sel+step, n-1)); at != *sel {
			*sel = at
			a.bump()
		}
	}
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
		if qp := a.question; qp.reviewing() {
			text = &qp.review
		} else {
			st := &qp.states[qp.at]
			text, sel, n = &st.ask, &st.sel, qp.qs[qp.at].ListRows()
		}
	case modalOverlay:
		if st := &a.overlays[len(a.overlays)-1]; !st.o.Input {
			text, sel, n = &st.body, &st.selected, len(st.visible)
		}
	case modalTrust:
		text = &a.trust.view
	case modalBoot:
		text = &a.bootView
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
		p.typing = true
		p.input.insert(line)
	case modalOverlay:
		st := &a.overlays[len(a.overlays)-1]
		if st.o.Input {
			st.input.insert(line)
		} else if st.filterable {
			st.edit("", line)
		}
	case modalPerm, modalTrust, modalBoot: // no text input: swallowed
	}
	a.bump()
	return true
}
