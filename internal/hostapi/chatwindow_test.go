package hostapi

// The ctx's head cut: a window past chatWindowRows sheds its head rows per
// flush, which the chat absorbs by shifting its keys, so the cut reports the
// shift while the epoch holds and no flush wipes the chat's cache.

import (
	"fmt"
	"testing"

	"charm.land/fantasy"
)

func TestHeadCut(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	probe := func() string {
		t.Helper()
		st := h.ScreenState(60, 1, UIState{})
		return fmt.Sprintf("%d|%d", st.TranscriptCut, st.TranscriptEpoch)
	}
	for i := range chatWindowRows + 5 {
		h.AppendMessage("user", fmt.Sprintf("row %d", i))
	}
	// the window holds its last chatWindowRows rows: 5 shed, no replacement
	if got := probe(); got != "5|1" {
		t.Fatalf("past the window the ctx reads %q, want \"5|1\"", got)
	}
	h.AppendMessage("user", "one more")
	if got := probe(); got != "6|1" {
		t.Fatalf("the next flush reads %q, want \"6|1\"", got)
	}
}

// TestDropHeldRowAfterTheTrimShedsIt: the held row's slot is its landing
// index minus the head cut. Once the trim sheds the row itself, no index is
// the hold's to remove - a re-sent same-text message at the stale slot
// survives.

// TestAlignTrimStartsOnATurn: the window's head never opens on an
// assistant row - a bound landing mid-turn sheds the rest of that turn,
// so the chat's oldest visible row is a question, not a bare answer.
func TestAlignTrimStartsOnATurn(t *testing.T) {
	t.Parallel()
	user := ChatMessage{Role: fantasy.MessageRoleUser}
	assistant := ChatMessage{Role: fantasy.MessageRoleAssistant}
	tool := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool}
	t.Run("within the bound the slice passes through", func(t *testing.T) {
		tr := make([]ChatMessage, chatWindowRows)
		if got := alignTrim(tr); &got[0] != &tr[0] {
			t.Fatal("a window inside the bound was rebuilt")
		}
	})
	t.Run("a bound landing mid-turn sheds to the turn start", func(t *testing.T) {
		tr := make([]ChatMessage, chatWindowRows+5)
		for i := range tr {
			tr[i] = ChatMessage{Role: fantasy.MessageRoleUser}
		}
		tr[5] = tool      // the bound's row: mid-turn, shed
		tr[6] = assistant // shed
		tr[7] = user      // the turn the head must land on
		got := alignTrim(tr)
		if len(got) != chatWindowRows-2 {
			t.Fatalf("kept %d rows, want %d", len(got), chatWindowRows-2)
		}
		if !turnStart(got[0]) {
			t.Fatalf("the head reads %v/%q, want a turn start", got[0].Kind, got[0].Role)
		}
	})
	t.Run("a span with no turn start keeps the plain clip", func(t *testing.T) {
		tr := make([]ChatMessage, chatWindowRows+3)
		for i := range tr {
			tr[i] = assistant
		}
		if got := alignTrim(tr); len(got) != chatWindowRows {
			t.Fatalf("kept %d rows, want the plain clip %d", len(got), chatWindowRows)
		}
	})
}

func TestDropHeldRowAfterTheTrimShedsIt(t *testing.T) {
	h, _ := newTestHost(t)
	const prompt = "deploy the staging build"
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: prompt})
	held := &heldPrompt{base: 0, text: prompt}
	// the window churns past its bound: the held row sheds
	for i := range chatWindowRows + 5 {
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: fmt.Sprintf("filler %d", i)})
	}
	// the user re-sends the same prompt, then churns it to the slot base names
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: prompt})
	for i := range chatWindowRows - 1 {
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: fmt.Sprintf("late %d", i)})
	}
	h.dropHeldRow(held)
	for _, m := range h.Session().Transcript {
		if m.Text == prompt {
			return // the resent message survived
		}
	}
	t.Fatal("the resent message died to the held prompt's stale base")
}

// TestLiveAppendTrims: a row appended mid-turn (the plan ask's row, through
// appendAfterLive) trims through the same helper as the settled path - the
// window holds the bound and the cut moves with it.
func TestLiveAppendTrims(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	for i := range chatWindowRows - 1 {
		h.AppendMessage("user", fmt.Sprintf("row %d", i))
	}
	// the live stretch still open when the plan ask lands: the flush
	// settles it, then the plan row appends, one trim past the bound
	ag := h.agents
	ag.mu.Lock()
	ag.textBuf = "live reply"
	ag.mu.Unlock()
	h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "the plan"})

	tr := h.Session().Transcript
	if len(tr) != chatWindowRows {
		t.Fatalf("after the live append the window holds %d rows, want %d", len(tr), chatWindowRows)
	}
	if cut := h.trCut.Load(); cut != 1 {
		t.Fatalf("after the live append the head cut reads %d, want 1", cut)
	}
	if got := tr[len(tr)-1].Text; got != "the plan" {
		t.Fatalf("the live row reads %q, want \"the plan\"", got)
	}
	if got := tr[len(tr)-2].Text; got != "live reply" {
		t.Fatalf("the flushed stretch reads %q, want \"live reply\"", got)
	}
	if got := tr[0].Text; got != "row 1" {
		t.Fatalf("the shed head left %q first, want \"row 1\"", got)
	}

	// a live append on a window already at the bound trims one per row
	h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "the plan again"})
	tr = h.Session().Transcript
	if len(tr) != chatWindowRows {
		t.Fatalf("the second live append left %d rows, want %d", len(tr), chatWindowRows)
	}
	if cut := h.trCut.Load(); cut != 2 {
		t.Fatalf("the second live append left the cut at %d, want 2", cut)
	}
}
