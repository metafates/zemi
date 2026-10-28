package hostapi

// The ctx's head cut: a window past chatWindowRows sheds its head rows per
// flush, which the chat absorbs by shifting its keys, so the cut reports the
// shift while the epoch holds and no flush wipes the chat's cache.

import (
	"context"
	"fmt"
	"strings"
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
// TestDropHeldRowAfterTheTrimShedsIt: the held row's slot is its landing
// index minus the head cut. Once the trim sheds the row itself, no index is
// the hold's to remove - a re-sent same-text message at the stale slot
// survives.
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

// TestLiveAppendTrimKeepsTheChat: the plan row's appendAfterLive trims
// mid-stream, while the live stretch is open - the interleaving the settled
// trim pin does not cover. The render walks the window top-down, so the top
// row names the transcript head: after the trim the cached units must name
// the rows the shifted keys now index, not the rows they indexed before.
func TestLiveAppendTrimKeepsTheChat(t *testing.T) {
	h, _ := newWindow(t, t.TempDir(), t.TempDir())
	for i := range 600 {
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: fmt.Sprintf("question %d", i)})
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: fmt.Sprintf("reply %d", i)})
	}
	chat := h.Widgets.Get("chat")
	if chat == nil {
		t.Fatal("no chat widget after boot")
	}
	render := func() []string {
		t.Helper()
		lines, more, err := h.RenderWidget(context.Background(), chat, 138, 40, UIState{Time: "12:00", Frame: 1})
		if err != nil {
			t.Fatal(err)
		}
		for more {
			if lines, more, err = h.RenderWidget(context.Background(), chat, 138, 40, UIState{Time: "12:00", Frame: 1}); err != nil {
				t.Fatal(err)
			}
		}
		var out []string
		for _, row := range lines {
			if s := strings.TrimSpace(SegsText(row)); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	first := func(rows []string) string {
		for _, s := range rows {
			return s
		}
		return ""
	}
	before := first(render())
	tr := h.Session().Transcript
	if want := tr[0].Text; before != want && !strings.Contains(before, want) {
		t.Fatalf("the top row drew %q, want the transcript head %q", before, want)
	}
	// the live stretch is open while the plan ask's row lands
	ag := h.agents
	ag.mu.Lock()
	ag.textBuf = "streaming reply text"
	ag.mu.Unlock()
	h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "the plan row"})
	after := first(render())
	tr = h.Session().Transcript
	if want := tr[0].Text; after != want && !strings.Contains(after, want) {
		t.Fatalf("after the mid-stream trim the top row drew %q, want the new head %q", after, want)
	}
}

// TestLiveAppendTrims: the live append past the bound trims through the same
// helper as the settled path - the window holds the bound and the cut moves.
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
