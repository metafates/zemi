package hostapi

// The ctx's head cut: a window past chatWindowRows sheds its head rows per
// flush, which the chat absorbs by shifting its keys, so the cut reports the
// shift while the epoch holds and no flush wipes the chat's cache.

import (
	"fmt"
	"testing"

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
