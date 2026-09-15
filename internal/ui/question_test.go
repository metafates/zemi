package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/metafates/zemi/internal/tools"
)

// TestGoldenQuestionOverlay pins the Go-owned question overlay (DESIGN
// §8): fixed chrome, cursor movement, and the chosen label returned to the
// tool goroutine.
func TestGoldenQuestionOverlay(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	done := make(chan string, 1)
	go func() {
		ans, ok := app.AskQuestion(tools.Question{
			Question:    "Apply the mined rules to this project?",
			Description: "Two proposals came out of the last session",
			Options: []tools.QuestionOption{
				{Label: "apply", Description: "store both as project memories"},
				{Label: "skip", Description: "leave the proposals unapplied"},
			},
		})
		if !ok {
			done <- ""
			return
		}
		done <- ans
	}()
	waitPrompt(t, app, func() bool { return app.question != nil })

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	app.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) // sel -> option 2
	if app.question.sel != 1 {
		t.Fatalf("sel = %d, want 1", app.question.sel)
	}
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "leave the proposals unapplied") {
		t.Fatalf("option 2 missing from frame:\n%s", frame)
	}

	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case ans := <-done:
		if ans != "skip" {
			t.Fatalf("answer %q, want skip", ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("question answer not delivered")
	}
	if app.question != nil {
		t.Fatal("overlay should be closed")
	}
}

// TestQuestionDeclineEsc covers esc: ok=false with an empty label.
func TestQuestionDeclineEsc(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	type result struct {
		ans string
		ok  bool
	}
	done := make(chan result, 1)
	go func() {
		ans, ok := app.AskQuestion(tools.Question{
			Question: "Proceed?",
			Options:  []tools.QuestionOption{{Label: "yes"}},
		})
		done <- result{ans, ok}
	}()
	waitPrompt(t, app, func() bool { return app.question != nil })

	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case r := <-done:
		if r.ok || r.ans != "" {
			t.Fatalf("decline produced (%q, %v)", r.ans, r.ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("question answer not delivered")
	}
	if app.question != nil {
		t.Fatal("overlay should be closed")
	}
}

// TestQuestionCtrlCDecline covers the Go-reserved cancel trigger: it must
// decline the question before any other handler sees the key.
func TestQuestionCtrlCDecline(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	done := make(chan bool, 1)
	go func() {
		_, ok := app.AskQuestion(tools.Question{
			Question: "Proceed?",
			Options:  []tools.QuestionOption{{Label: "yes"}},
		})
		done <- ok
	}()
	waitPrompt(t, app, func() bool { return app.question != nil })

	app.Update(tea.KeyPressMsg{Mod: uv.ModCtrl, Code: 'c'})
	select {
	case ok := <-done:
		if ok {
			t.Fatal("ctrl+c must decline the question")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("question answer not delivered")
	}
	if app.question != nil {
		t.Fatal("overlay should be closed")
	}
}
