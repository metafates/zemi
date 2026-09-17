package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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

// TestGoldenMemoryReviewOverlay pins the Go-owned memory screen (DESIGN §8,
// §10.4): a keeps the suggestion, d forgets the stored memory, u undoes,
// and closing hands the decisions back by row id.
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

	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // keep the suggestion
	app.renderSync(t)
	// the kept suggestion reads as remembered at once, without reopening
	if frame := app.Frame(); !strings.Contains(frame, "fts join") || strings.Index(frame, "Remembered") > strings.Index(frame, "fts join") {
		t.Fatalf("a kept suggestion belongs under Remembered:\n%s", frame)
	}

	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // no-op on a stored memory
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}) // forget it
	app.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "short replies") {
		t.Fatalf("undo should bring the forgotten memory back:\n%s", frame)
	}
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})

	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := reviewAnswer(t, done); len(got) != 2 || got["m1"] != "accept" || got["m2"] != "delete" {
		t.Fatalf("decisions = %v", got)
	}
	if app.memReview != nil {
		t.Fatal("overlay should be closed")
	}
}

// TestMemoryReviewDropAccepted: a suggestion kept and then dropped was
// never stored, so it goes back as a rejection, not a delete of a memory
// that does not exist.
func TestMemoryReviewDropAccepted(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	done := make(chan map[string]string, 1)
	go func() {
		done <- app.ReviewMemories([]hostapi.MemoryRow{{ID: "m1", Scope: "project", Kind: "fact", Origin: "learned", Text: "note", Queued: true}})
	}()
	waitPrompt(t, app, func() bool { return app.memReview != nil })
	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := reviewAnswer(t, done); got["m1"] != "reject" {
		t.Fatalf("decisions = %v", got)
	}
}

// TestMemoryReviewUndoAll: undoing every action closes with no decisions.
func TestMemoryReviewUndoAll(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	done := make(chan map[string]string, 1)
	go func() {
		done <- app.ReviewMemories([]hostapi.MemoryRow{{ID: "m1", Scope: "project", Kind: "fact", Origin: "user", Text: "note"}})
	}()
	waitPrompt(t, app, func() bool { return app.memReview != nil })
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	app.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
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

// TestMemoryReviewLongList: memories read whole and rendered as markdown,
// the window scrolls to the selection, and the box keeps its place.
func TestMemoryReviewLongList(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 24)
	var items []hostapi.MemoryRow
	for i := range 12 {
		items = append(items, hostapi.MemoryRow{ID: string(rune('a' + i)), Scope: "user", Kind: "fact", Origin: "user", Text: "short note"})
	}
	items = append(items, hostapi.MemoryRow{ID: "z", Scope: "project", Kind: "gotcha", Origin: "learned",
		Text: "run **go test** before `make golden`; " + strings.Repeat("word ", 30) + "tail-marker", Tags: []string{"db"}})
	done := make(chan map[string]string, 1)
	go func() { done <- app.ReviewMemories(items) }()
	waitPrompt(t, app, func() bool { return app.memReview != nil })

	app.renderSync(t)
	first := app.Frame()
	for range items {
		app.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"tail-marker", "run go test before", "make golden", "gotcha · this project · learned · #db", "13/13"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("frame lacks %q:\n%s", want, frame)
		}
	}
	edges := func(frame string) string {
		var out []string
		for i, l := range strings.Split(frame, "\n") {
			if strings.Contains(l, "╭") || strings.Contains(l, "╰") {
				out = append(out, fmt.Sprint(i, ansi.StringWidth(strings.TrimRight(l, " \r"))))
			}
		}
		return strings.Join(out, "|")
	}
	if a, b := edges(ansi.Strip(first)), edges(frame); a != b {
		t.Fatalf("the box moved: %q vs %q", a, b)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	reviewAnswer(t, done)
}
