package hostapi

// The ctx's head cut: a window past chatWindowRows sheds its head rows per
// flush, which the chat absorbs by shifting its keys, so the cut reports the
// shift while the epoch holds and no flush wipes the chat's cache.

import (
	"testing"

	"charm.land/fantasy"
)

// TestAlignTrimStartsOnATurn: the window's head never opens on an
// assistant row - a bound landing mid-turn sheds the rest of that turn,
// so the chat's oldest visible row is a question, not a bare answer.
func TestAlignTrimStartsOnATurn(t *testing.T) {
	t.Parallel()
	user := ChatMessage{Role: fantasy.MessageRoleUser}
	assistant := ChatMessage{Role: fantasy.MessageRoleAssistant}
	tool := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool}
	t.Run("within the bound the slice passes through", func(t *testing.T) {
		tr := make([]ChatMessage, _chatWindowRows)
		if got := alignTrim(tr); &got[0] != &tr[0] {
			t.Fatal("a window inside the bound was rebuilt")
		}
	})
	t.Run("a bound landing mid-turn sheds to the turn start", func(t *testing.T) {
		tr := make([]ChatMessage, _chatWindowRows+5)
		for i := range tr {
			tr[i] = ChatMessage{Role: fantasy.MessageRoleUser}
		}
		tr[5] = tool      // the bound's row: mid-turn, shed
		tr[6] = assistant // shed
		tr[7] = user      // the turn the head must land on
		got := alignTrim(tr)
		if len(got) != _chatWindowRows-2 {
			t.Fatalf("kept %d rows, want %d", len(got), _chatWindowRows-2)
		}
		if !turnStart(got[0]) {
			t.Fatalf("the head reads %v/%q, want a turn start", got[0].Kind, got[0].Role)
		}
	})
	t.Run("a span with no turn start keeps the plain clip", func(t *testing.T) {
		tr := make([]ChatMessage, _chatWindowRows+3)
		for i := range tr {
			tr[i] = assistant
		}
		if got := alignTrim(tr); len(got) != _chatWindowRows {
			t.Fatalf("kept %d rows, want the plain clip %d", len(got), _chatWindowRows)
		}
	})
}
