package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestAnchorRow: a jump lands on the first row carrying the anchor, else on
// the first row of the nearest one before it (a message folded into a burst,
// a call inside a run's digest), and nowhere while the rows above it are
// still to be drawn.
func TestAnchorRow(t *testing.T) {
	lines := [][]hostapi.Segment{
		{{Text: ""}},
		{{Text: "a", Anchor: 1}},
		{{Text: "a", Anchor: 1}}, // a wrapped continuation carries it again
		{{Text: ""}},
		{{Text: "b", Anchor: 4}},
	}
	for _, c := range []struct{ at, want int }{
		{1, 1}, {2, 1}, {3, 1}, {4, 4}, {5, 4}, {0, -1},
	} {
		if got := anchorRow(lines, c.at); got != c.want {
			t.Errorf("anchor %d: row %d, want %d", c.at, got, c.want)
		}
	}
	if got := anchorRow(nil, 3); got != -1 {
		t.Fatalf("nothing drawn: row %d", got)
	}
}

// bandedChatRows is the text of the chat rows the picker's message is banded
// on, as the last frame drew them.
func bandedChatRows(app *App) []string {
	chat := app.scrolls["chat"]
	var out []string
	for y := chat.rect.Min.Y; y < min(chat.rect.Max.Y, len(app.lastRows)); y++ {
		if row := app.lastRows[y]; len(row) > 0 && row[0].Fill == hostapi.FillBand {
			out = append(out, strings.TrimSpace(hostapi.SegsText(row)))
		}
	}
	return out
}

// TestMessagesFollow: the messages picker opens on the row the reader is at,
// moving its selection brings that message's first row to the top of the
// chat and bands it, and a row in view at the tail leaves the chat following
// it.
func TestMessagesFollow(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	if chat == nil || !chat.follow {
		t.Fatal("the chat should follow its tail")
	}
	open := func() *overlayState {
		t.Helper()
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		if len(app.overlays) != 1 || !app.overlays[0].o.Follow {
			t.Fatalf("overlays: %+v", app.overlays)
		}
		return &app.overlays[0]
	}
	ov := open()
	if got := ov.selectedAnchor(); got != 60 { // the last reply, at the tail
		t.Fatalf("opened on anchor %d", got)
	}
	if top := chat.top; top != chat.rows-chat.viewH {
		t.Fatalf("opening moved the chat: top %d of %d, view %d", top, chat.rows, chat.viewH)
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	if chat.follow {
		t.Fatal("a selection above the tail should leave it")
	}
	if got, want := rowAnchor(chat.lines[chat.top]), ov.selectedAnchor(); got != want || got != 1 {
		t.Fatalf("top row anchor %d, selection %d", got, want)
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "message 1") {
		t.Fatalf("banded rows %q", got)
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnd})
	app.Frame()
	if !chat.follow || chat.top != chat.rows-chat.viewH {
		t.Fatal("the last row should hand the chat back to its tail")
	}
	// the reply, the gap under it and its readout are one banded block
	if got := bandedChatRows(app); len(got) != 3 || !strings.Contains(got[0], "reply 30") || !strings.Contains(got[2], "100 out") {
		t.Fatalf("banded rows %q", got)
	}
	// a row already in the tail's view keeps the chat on the tail
	press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
	app.Frame()
	if !chat.follow {
		t.Fatal("a row in view at the tail should keep following it")
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	app.Frame()
	if !chat.follow || chat.top != chat.rows-chat.viewH {
		t.Fatalf("esc should put a following chat back on its tail: top %d of %d", chat.top, chat.rows)
	}

	// scrolled up, the picker opens on the message the top row belongs to and
	// esc puts the chat back where it was
	for range 3 {
		press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	app.Frame()
	top := chat.top
	ov = open()
	if got, want := ov.selectedAnchor(), anchorAt(chat); got != want || got == 0 {
		t.Fatalf("opened on anchor %d, the chat is at %d", got, want)
	}
	app.Frame()
	if chat.top != top {
		t.Fatalf("opening moved the chat: %d -> %d", top, chat.top)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	if chat.top == top {
		t.Fatal("a jump should move the chat")
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "message 1") {
		t.Fatalf("banded rows %q", got)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	app.Frame()
	if chat.top != top || len(app.overlays) != 0 {
		t.Fatalf("esc should cancel: top %d, want %d, %d overlays", chat.top, top, len(app.overlays))
	}
	if got := bandedChatRows(app); len(got) != 0 {
		t.Fatalf("banded rows after esc: %q", got)
	}

	// enter keeps the place instead
	open()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	jumped := chat.top
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	app.Frame()
	if len(app.overlays) != 0 {
		t.Fatalf("%d overlays after enter", len(app.overlays))
	}
	if chat.top != jumped || chat.top == top {
		t.Fatalf("enter should keep the jump: top %d, jumped %d, was %d", chat.top, jumped, top)
	}
	if got := bandedChatRows(app); len(got) != 0 {
		t.Fatalf("banded rows after enter: %q", got)
	}
}
