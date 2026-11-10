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

// TestChatCursorForgetsAnOffScreenMessage: a memo the view scrolled away from
// is dropped, so tab takes the message the drawn view is at and a second tab
// re-selects that one, never the old place.
func TestChatCursorForgetsAnOffScreenMessage(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	press(t, app, tab)
	held := app.chatSel
	press(t, app, tab) // away
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	press(t, app, tab) // back over a view that no longer holds the memo
	st := aScroll(app)
	bottom := min(st.top+st.viewH, len(st.lines)) - 1 - st.viewH/10
	want := 0
	for i := bottom; i >= st.top && want == 0; i-- {
		want = rowAnchor(st.lines[i])
	}
	if app.chatSel != want || app.chatSel == held {
		t.Fatalf("tab: sel %d, want %d (the view's own message), not the memo's %d", app.chatSel, want, held)
	}
	press(t, app, tab) // away
	press(t, app, tab) // back: the pick is the memo now
	if app.chatSel != want {
		t.Fatalf("tab twice: sel %d, want the picked %d", app.chatSel, want)
	}
}

// TestChatCursorPages: ctrl+u and ctrl+d walk the cursor a view up and down
// with the view following it, the way the bare composer scrolls on them.
func TestChatCursorPages(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyTab})
	units := app.chatUnits()
	tail := len(units) - 1
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	up := slices.Index(units, app.chatSel-1)
	if up < 0 || up >= tail {
		t.Fatalf("ctrl+u: sel %d at unit %d, want a message above the tail %d", app.chatSel, up, tail)
	}
	if tail-up < 2 {
		t.Fatalf("ctrl+u moved %d unit(s), want a view's worth", tail-up)
	}
	press(t, app, tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	if down := slices.Index(units, app.chatSel-1); down <= up {
		t.Fatalf("ctrl+d: unit %d, want one below %d", down, up)
	}
	app.Frame()
	if st := aScroll(app); st.bandAt != app.chatSel {
		t.Fatalf("ctrl+d: the band is on %d, not on the cursor's %d", st.bandAt, app.chatSel)
	}
}

// TestChatCursorFoldsALongPrompt: a prompt past the paste threshold draws its
// head under a fold row whose glyph says the state; l opens it whole, h folds
// it back and a click on the row toggles it the same way.
func TestChatCursorFoldsALongPrompt(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 30)
	host.AppendMessage("user", strings.Repeat("a line of the prompt\n", 20)+"the end")
	app.renderSync(t)
	app.Frame()
	rows := func() []string { return chatRows(t, app, 78) }
	prompt := func() int {
		n := 0
		for _, r := range rows() {
			if strings.HasPrefix(r, "▌ ") {
				n++
			}
		}
		return n
	}
	folded := func(glyph string) bool {
		return slices.ContainsFunc(rows(), func(r string) bool { return strings.HasPrefix(r, "▌ "+glyph) })
	}
	foldKey := func(key tea.KeyPressMsg) {
		press(t, app, key)
		app.renderSync(t) // the toggle's rows land in the cache the cursor reads
		app.Frame()
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyTab}) // the prompt is the only message
	if !folded("▸") || folded("▾") {
		t.Fatalf("the long prompt draws unfolded:\n%s", strings.Join(rows(), "\n"))
	}
	if n := prompt(); n > 10 {
		t.Fatalf("the folded prompt drew %d rows, want its head", n)
	}
	head := prompt()
	foldKey(tea.KeyPressMsg{Code: 'l'})
	if !folded("▴") || folded("▸") {
		t.Fatalf("l did not open the prompt:\n%s", strings.Join(rows(), "\n"))
	}
	if n := prompt(); n <= head+1 {
		t.Fatalf("the opened prompt drew %d rows, want its whole text over the foot", n)
	}
	foldKey(tea.KeyPressMsg{Code: 'h'})
	if !folded("▸") {
		t.Fatalf("h did not fold the prompt back:\n%s", strings.Join(rows(), "\n"))
	}
	// a key inside the frame after a toggle reads no paint: h on a prompt l
	// just opened still folds it, the state, not the glyph, says what it does
	foldKey(tea.KeyPressMsg{Code: 'l'})
	press(t, app, tea.KeyPressMsg{Code: 'h'})
	if !folded("▸") {
		t.Fatalf("h inside the frame after l did not fold the prompt back:\n%s", strings.Join(rows(), "\n"))
	}
	app.renderSync(t) // the frame the click below reads
	app.Frame()
	x, y := findCell(t, app, "▸ +")
	clickAt(app, x, y)
	if !folded("▴") {
		t.Fatalf("a click on the fold row did not open the prompt:\n%s", strings.Join(rows(), "\n"))
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
	const digest = "1 command: make · thought 6.4s"
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
