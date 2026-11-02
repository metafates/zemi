package ui

// The chat past its window: a settled append trims the head, and a fold the
// reader opened stays open across it.

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

// TestChatKeepsAFoldAcrossATrim: the head cut sheds the window's oldest rows
// and the fold a click opened stays open, its rows still drawn.
func TestChatKeepsAFoldAcrossATrim(t *testing.T) {
	t.Parallel()
	app, host := bootAgentAppOn(t, 140, 40, &lagClock{}, "")
	seedTrimChat(host, 250) // 13 rows a turn: the window trims to its 1000
	lines := func() [][]hostapi.Segment {
		t.Helper()
		lines, _ := renderChat(t, app, 138, 40, app.uiState())
		return lines
	}
	// a folded run's digest in the view: click it open
	turn := ""
	for _, row := range lines() {
		for _, s := range row {
			if strings.HasPrefix(s.Click, "turn:") {
				turn = s.Click
			}
		}
	}
	if turn == "" {
		t.Fatal("no folded run's digest in the view")
	}
	clickChat(t, app, turn)
	open := func() bool {
		return slices.ContainsFunc(lines(), func(l []hostapi.Segment) bool {
			return strings.HasPrefix(hostapi.SegsText(l), "│ ✻ thought")
		})
	}
	if !open() {
		t.Fatal("the digest's click did not open the run")
	}
	// one more exchange: 13 rows, enough to cross the window's bound and
	// trim the head to the next turn start
	for i := range 13 {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: fmt.Sprintf("one more settled row %d", i)})
	}
	if !open() {
		t.Fatal("the trim closed the fold the reader had open")
	}
}
