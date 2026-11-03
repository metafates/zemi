package ui

import (
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
