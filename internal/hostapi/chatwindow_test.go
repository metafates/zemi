package hostapi

// The ctx's head cut: a window past chatWindowRows sheds its head rows per
// flush, which the chat absorbs by shifting its keys, so the cut reports the
// shift while the epoch holds and no flush wipes the chat's cache.

import (
	"fmt"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
)

func TestHeadCut(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	if err := h.RT.ExecuteChunk(t.Context(),
		`zemi.ui.widget.add({ id = "probe", region = "statusline.right", render = function(ctx) return { { { text = string.format("%d|%d", ctx.transcript_cut, ctx.transcript_epoch) } } } end })`,
		"probe.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	probe := func() string {
		t.Helper()
		lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("probe"), 60, 1, UIState{})
		if err != nil {
			t.Fatal(err)
		}
		return SegsText(lines[0])
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

// TestLiveAppendTrims: a row appended mid-turn (the plan ask's row, through
// appendAfterLive) trims at the same bound the settled appends do and
// advances the cut with it, so the window never holds past chatWindowRows
// and the chat's rebased keys keep reading the rows they name.
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
