package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/metafates/zemi/internal/hostapi"
)

// reviewAnswer waits for the host-side answer of a memory review.
func reviewAnswer(t *testing.T, done chan map[string]string) map[string]string {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("review answer not delivered")
		return nil
	}
}

// TestGoldenMemoryReviewOverlay pins the Go-owned memory review overlay
// (DESIGN §8, §10.4): a accepts the proposal, d deletes the stored memory,
// enter hands the decisions back by row id.
func TestGoldenMemoryReviewOverlay(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	items := []hostapi.MemoryRow{
		{ID: "m1", Scope: "project", Kind: "gotcha", Origin: "learned",
			Text: "the fts join needs m-qualified columns", Queued: true},
		{ID: "m2", Scope: "user", Kind: "preference", Origin: "user",
			Text: "the user prefers short replies", Tags: []string{"style"}},
	}
	done := make(chan map[string]string, 1)
	go func() { done <- app.ReviewMemories(items) }()
	waitPrompt(t, app, func() bool { return app.memReview != nil })

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // accept the proposal
	app.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // no-op on a stored memory
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}) // delete it
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "[+]") || !strings.Contains(frame, "[-]") {
		t.Fatalf("marks missing from frame:\n%s", frame)
	}

	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := reviewAnswer(t, done); len(got) != 2 || got["m1"] != "accept" || got["m2"] != "delete" {
		t.Fatalf("decisions = %v", got)
	}
	if app.memReview != nil {
		t.Fatal("overlay should be closed")
	}
}

// TestMemoryReviewCancel: esc discards marks and closes; the empty review
// pins its placeholder row.
func TestMemoryReviewCancel(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	done := make(chan map[string]string, 1)
	go func() {
		done <- app.ReviewMemories([]hostapi.MemoryRow{{ID: "m1", Scope: "project", Kind: "fact", Origin: "user", Text: "note"}})
	}()
	waitPrompt(t, app, func() bool { return app.memReview != nil })
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := reviewAnswer(t, done); len(got) != 0 || app.memReview != nil {
		t.Fatalf("cancel returned %v", got)
	}
}

func TestMemoryReviewEmpty(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	done := make(chan map[string]string, 1)
	go func() { done <- app.ReviewMemories(nil) }()
	waitPrompt(t, app, func() bool { return app.memReview != nil })

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	app.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if got := reviewAnswer(t, done); len(got) != 0 {
		t.Fatalf("empty review returned %v", got)
	}
}
