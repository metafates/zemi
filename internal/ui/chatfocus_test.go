package ui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestChatCursor: tab gives the chat the keys, the cursor walks the
// messages, the paint bands the selected one, y copies it and esc hands the
// keys back to the composer.
func TestChatCursor(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.AppendChat(hostapi.ChatMessage{Role: "user", Text: "first"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: "one two three four five six"})
	host.AppendChat(hostapi.ChatMessage{Role: "user", Text: "second"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: "hi there"})
	app.renderSync(t)

	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	app.Update(tab)
	if !app.chatFocus || app.chatSel != 4 {
		t.Fatalf("tab: focus %v, sel %d, want true 4 (the message at the tail)", app.chatFocus, app.chatSel)
	}
	app.Frame()
	st := aScroll(app)
	if st.bandAt != 4 || !banded(st) {
		t.Fatalf("tab: the selected message draws unbanded (bandAt %d)", st.bandAt)
	}

	app.Update(tea.KeyPressMsg{Code: 'k'})
	if app.chatSel != 3 {
		t.Fatalf("k: sel %d, want 3", app.chatSel)
	}
	app.Frame()
	if st.bandAt != 3 || !banded(st) {
		t.Fatalf("k: the band did not follow (bandAt %d)", st.bandAt)
	}
	app.Update(tea.KeyPressMsg{Code: 'j'})
	if app.chatSel != 4 {
		t.Fatalf("j: sel %d, want 4", app.chatSel)
	}

	// a key the cursor does not take is ignored: focus and draft untouched
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if !app.chatFocus || app.composer.text != "" {
		t.Fatalf("x on the focused chat: focus %v, draft %q", app.chatFocus, app.composer.text)
	}

	app.Update(tea.KeyPressMsg{Code: 'y'})
	if body := lastToast(app); body != "8 chars" {
		t.Fatalf("y: toast %q, want 8 chars", body)
	}

	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.chatFocus {
		t.Fatal("esc: the chat kept the keys")
	}
	if app.composer.text != "" {
		t.Fatalf("esc: the draft changed: %q", app.composer.text)
	}

	// focus gone, tab offers the composer's popups nothing and the draft
	// takes letters again
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if app.composer.text != "x" {
		t.Fatalf("a letter after esc: draft %q, want x", app.composer.text)
	}
	app.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if app.composer.text != "xj" || app.chatFocus {
		t.Fatalf("j on the bare composer: draft %q, focus %v", app.composer.text, app.chatFocus)
	}
}

// aScroll is the chat's scroll state after a paint.
func aScroll(app *App) *scrollState {
	return app.scrolls[app.scrollDef]
}

// TestChatCursorEntersWhereTheReaderIs: tab takes the message at the
// viewport's bottom row, so a view scrolled away from the tail selects
// what is on screen, not the newest message.
func TestChatCursorEntersWhereTheReaderIs(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyTab})
	if !app.chatFocus {
		t.Fatal("tab: the chat took no focus")
	}
	st := aScroll(app)
	bottom := min(st.top+st.viewH, len(st.lines)) - 1
	want := 0
	for i := bottom; i >= st.top && want == 0; i-- {
		want = rowAnchor(st.lines[i])
	}
	if app.chatSel != want {
		t.Fatalf("tab: sel %d, want %d, the anchor at the view's bottom row", app.chatSel, want)
	}
}

// TestChatCursorBandsItsOwnRows: the first message bands itself, not the
// whole window (its rows are the only ones carrying its anchor).
func TestChatCursorBandsItsOwnRows(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyTab})
	for app.chatSel != 1 {
		before := app.chatSel
		app.Update(tea.KeyPressMsg{Code: 'k'})
		app.Pump()
		if app.chatSel == before {
			t.Fatalf("k never reached the first message (stuck at %d)", before)
		}
	}
	app.Frame()
	st := aScroll(app)
	from, to := unitSpan(st.lines, 1)
	if from < 0 {
		t.Fatal("the first message drew no rows")
	}
	banded := 0
	for _, row := range st.band {
		for _, s := range row {
			if s.Fill == hostapi.FillBand {
				banded++
				break
			}
		}
	}
	if banded != to-from+1 {
		t.Fatalf("the first message banded %d rows, want %d", banded, to-from+1)
	}
}

// TestChatCursorRemembersItsMessage: tab away and back re-selects the
// message the cursor held, even after the view scrolled away from it.
func TestChatCursorRemembersItsMessage(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	press(t, app, tab)
	held := app.chatSel
	for range 3 {
		app.Update(tea.KeyPressMsg{Code: 'k'})
		if app.chatSel == held {
			t.Fatal("k did not move the cursor; the memo case would be vacuous")
		}
		held = app.chatSel
	}
	press(t, app, tab) // away
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	press(t, app, tab) // back
	if app.chatSel != held {
		t.Fatalf("tab twice: sel %d, want the held %d", app.chatSel, held)
	}
	st := aScroll(app)
	if from, _ := unitRows(st.lines, app.chatSel); from < 0 {
		t.Fatalf("the restored message %d drew no rows", app.chatSel)
	}
}

// TestChatCursorFoldsTheRun: on the selected run's digest, enter and space
// toggle the rail, l opens a folded one, h folds an open one back.
func TestChatCursorFoldsTheRun(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 140, 30)
	host.AppendMessage("user", "run it")
	appendReasoning(host, strings.Repeat("plan it word by word ", 8)+"\n\nthen build", hostapi.TurnStat{ThoughtMS: 6000})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "make", Status: "ok", Output: "built\nok"})
	appendReasoning(host, "done then", hostapi.TurnStat{ThoughtMS: 400})
	appendReply(host, "done", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyTab}) // lands on the run's digest, the unit above the reply
	rows := func() []string { return chatRows(t, app, 138) }
	const digest = "1 command: make  thought 6.4s"
	if got := rows(); !slices.Contains(got, "▸ "+digest) {
		t.Fatalf("the cursor is not on the folded run:\n%s", strings.Join(got, "\n"))
	}
	// foldKey runs a key and lands its widget render, as the loop does: the
	// toggle's re-render reaches the cache the cursor reads.
	foldKey := func(key tea.KeyPressMsg) {
		for app.Pump() {
		}
		app.Update(key)
		app.renderSync(t) // the toggle's re-render lands, as the tick's queue does
		app.Frame()       // repaint, so the scrolls the cursor reads are current
	}
	open := func() bool {
		got := rows()
		return slices.Contains(got, "▾ "+digest) && slices.ContainsFunc(got, func(r string) bool { return strings.HasPrefix(r, "│ ✓ run make") })
	}
	foldKey(tea.KeyPressMsg{Code: 'l'})
	if !open() {
		t.Fatal("l did not open the run's rail")
	}
	foldKey(tea.KeyPressMsg{Code: 'l'})
	if !open() {
		t.Fatal("l on an open rail closed it")
	}
	foldKey(tea.KeyPressMsg{Code: 'h'})
	if open() {
		t.Fatal("h did not fold the rail back")
	}
	foldKey(tea.KeyPressMsg{Code: 'h'})
	if open() {
		t.Fatal("h on a folded run opened it")
	}
	foldKey(tea.KeyPressMsg{Code: ' '})
	if !open() {
		t.Fatal("space did not open the run's rail")
	}
	foldKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if open() {
		t.Fatal("enter did not fold the rail back")
	}
	foldKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !open() {
		t.Fatal("enter did not open the run's rail")
	}
}

// banded says the scroll state's band carries the FillBand tint.
func banded(st *scrollState) bool {
	for _, row := range st.band {
		for _, s := range row {
			if s.Fill == hostapi.FillBand {
				return true
			}
		}
	}
	return false
}
