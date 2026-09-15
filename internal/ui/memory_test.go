package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/metafates/zemi/internal/hostapi"
)

// TestGoldenMemoryReviewOverlay pins the Go-owned memory review overlay
// (DESIGN §8, §10.4): fixed chrome, queued proposals flagged, delete marks,
// and the marked indices returned to the host on apply.
func TestGoldenMemoryReviewOverlay(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	items := []hostapi.MemoryRow{
		{ID: "m1", Scope: "project", Kind: "gotcha", Origin: "learned",
			Text: "the fts join needs m-qualified columns", Queued: true},
		{ID: "m2", Scope: "user", Kind: "preference", Origin: "user",
			Text: "the user prefers short replies", Tags: []string{"style"}},
	}
	done := make(chan []int, 1)
	go func() { done <- app.ReviewMemories(items) }()
	waitPrompt(t, app, func() bool { return app.memReview != nil })

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	app.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) // sel -> row 1
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}) // mark row 1
	app.renderSync(t)
	frame := app.Frame()
	if !strings.Contains(frame, "[x]") || !strings.Contains(frame, "preference") {
		t.Fatalf("marked state missing from frame:\n%s", frame)
	}

	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case del := <-done:
		if len(del) != 1 || del[0] != 1 {
			t.Fatalf("deleted=%v", del)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review answer not delivered")
	}
	if app.memReview != nil {
		t.Fatal("overlay should be closed")
	}
}

// TestMemoryReviewCancel covers esc: the host gets no deletions and the
// overlay closes.
func TestMemoryReviewCancel(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	done := make(chan []int, 1)
	go func() {
		done <- app.ReviewMemories([]hostapi.MemoryRow{
			{ID: "m1", Scope: "project", Kind: "fact", Origin: "user", Text: "note"},
		})
	}()
	waitPrompt(t, app, func() bool { return app.memReview != nil })

	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case del := <-done:
		if len(del) != 0 {
			t.Fatalf("cancel deleted %v", del)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review answer not delivered")
	}
	if app.memReview != nil {
		t.Fatal("overlay should be closed")
	}
}

// TestMemoryReviewEmpty pins the "no memories" placeholder row.
func TestMemoryReviewEmpty(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	done := make(chan []int, 1)
	go func() { done <- app.ReviewMemories(nil) }()
	waitPrompt(t, app, func() bool { return app.memReview != nil })

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	app.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	select {
	case del := <-done:
		if len(del) != 0 {
			t.Fatalf("empty review deleted %v", del)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review answer not delivered")
	}
}
