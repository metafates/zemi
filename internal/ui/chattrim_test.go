package ui

// The chat past its window: a settled append trims the head, which the chat
// absorbs by shifting its keys, so no unit is rebuilt, a fold the reader
// opened stays open and the rows' anchors move with the cut.

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

// seedTrimChat fills the window with turns whose activity run is long enough
// to fold, so the view holds a digest row to click.
func seedTrimChat(host *hostapi.Host, turns int) {
	for i := range turns {
		host.AppendMessage("user", fmt.Sprintf("question %d", i))
		appendReasoning(host, "thinking about it", hostapi.TurnStat{})
		for j := range 10 {
			host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: fmt.Sprintf("f%d.go", j), Status: "ok"})
		}
		appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{})
	}
}

func TestChatTrimKeepsTheCache(t *testing.T) {
	t.Parallel()
	app, host := bootAgentAppOn(t, 140, 40, &lagClock{}, "")
	seedTrimChat(host, 250) // 13 rows a turn: the window trims to its 1000
	chat := host.Widgets.Get("chat")
	render := func() ([][]hostapi.Segment, bool) {
		t.Helper()
		lines, more, err := host.RenderWidget(t.Context(), chat, 138, 40, app.uiState())
		if err != nil {
			t.Fatal(err)
		}
		return lines, more
	}
	// the lag clock spends the budget on every unit build, so more is true
	// until every unit the render walks is cached
	for more := true; more; {
		if _, more = render(); !more {
			break
		}
	}
	anchors := func(lines [][]hostapi.Segment) map[int]bool {
		out := map[int]bool{}
		for _, row := range lines {
			for _, s := range row {
				if s.Anchor > 0 {
					out[s.Anchor] = true
				}
			}
		}
		return out
	}
	var lines [][]hostapi.Segment
	lines, _ = render()
	before := anchors(lines)
	// a folded run's digest in the view: click it open
	turn := ""
	for _, row := range lines {
		for _, s := range row {
			if strings.HasPrefix(s.Click, "turn:") {
				turn = s.Click
			}
		}
	}
	if turn == "" {
		t.Fatal("no folded run's digest in the view")
	}
	if err := host.ClickWidget(t.Context(), "chat", map[string]any{"id": turn}); err != nil {
		t.Fatal(err)
	}
	open := false
	for {
		var m bool
		lines, m = render()
		open = open || slices.ContainsFunc(lines, func(l []hostapi.Segment) bool {
			return strings.HasPrefix(hostapi.SegsText(l), "│ ✻ thought")
		})
		if !m {
			break
		}
	}
	if !open {
		t.Fatal("the digest's click did not open the run")
	}
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: "one more settled row"})
	// the trim fills: the rows the viewport lost shift down and the units
	// above restamp a view at a time, like a cold render does
	open = false
	for {
		var m bool
		lines, m = render()
		open = open || slices.ContainsFunc(lines, func(l []hostapi.Segment) bool {
			return strings.HasPrefix(hostapi.SegsText(l), "│ ✻ thought")
		})
		if !m {
			break
		}
	}
	if !open {
		t.Fatal("the trim closed the fold the reader had open")
	}
	// every row that drew before the trim sits one row lower now: the anchors
	// moved with the cut instead of going stale. The head unit is aside - the
	// cut took its first row, so it rebuilt and re-anchors at 1.
	after := anchors(lines)
	for a := range after {
		if a == 1 || a == 1000 { // the head unit's and the appended row's
			continue
		}
		if !before[a+1] {
			t.Fatalf("anchor %d drew after the trim, but no row drew at %d before it", a, a+1)
		}
	}
}
