package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/metafates/zemi/internal/tools"
)

type questionResult struct {
	answers []string
	ok      bool
}

// askAsync opens the question overlay from a tool goroutine, as the
// question and plan tools do.
func askAsync(t *testing.T, app *App, qs ...tools.Question) chan questionResult {
	t.Helper()
	done := make(chan questionResult, 1)
	go func() {
		ans, ok := app.AskQuestion(qs)
		done <- questionResult{ans, ok}
	}()
	waitPrompt(t, app, func() bool { return app.question != nil })
	return done
}

func awaitAnswer(t *testing.T, app *App, done chan questionResult) questionResult {
	t.Helper()
	select {
	case r := <-done:
		if app.question != nil {
			t.Fatal("overlay should be closed")
		}
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("question answer not delivered")
	}
	return questionResult{}
}

func pressKeys(app *App, keys ...string) {
	for _, k := range keys {
		switch k {
		case "enter":
			app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		case "esc":
			app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		case "space":
			app.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
		case "ctrl+c":
			app.Update(tea.KeyPressMsg{Mod: uv.ModCtrl, Code: 'c'})
		default:
			for _, r := range k {
				app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
			}
		}
	}
}

// TestGoldenQuestionOverlay pins the Go-owned question overlay (DESIGN
// §8): a batch shows tabs, answering moves on to the review tab, and the
// answers come back in order.
func TestGoldenQuestionOverlay(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)
	done := askAsync(t, app,
		tools.Question{Question: "Apply the mined rules to this project?", Header: "Rules", Options: []tools.QuestionOption{
			{Label: "apply (Recommended)", Description: "store both as project memories"},
			{Label: "skip", Description: "leave the proposals unapplied"},
		}},
		tools.Question{Question: "Which checks run before commit?", Header: "Checks", Multi: true, Options: []tools.QuestionOption{
			{Label: "tests"}, {Label: "vet"}, {Label: "lint"},
		}},
	)
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	pressKeys(app, "j", "enter", "space", "j", "j", "space", "enter")
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "Submit answers") || !strings.Contains(frame, "tests, lint") {
		t.Fatalf("review tab missing:\n%s", frame)
	}
	pressKeys(app, "enter")
	if r := awaitAnswer(t, app, done); !r.ok || fmt.Sprint(r.answers) != "[skip tests, lint]" {
		t.Fatalf("answers %q ok=%v", r.answers, r.ok)
	}
}

func TestQuestionAnswers(t *testing.T) {
	yes := tools.Question{Question: "Proceed?", Options: []tools.QuestionOption{{Label: "yes"}}}
	for _, c := range []struct {
		name string
		q    tools.Question
		keys []string
		want questionResult
	}{
		{"typed answer on the last row", yes, []string{"j", "enter", "use a flag", "enter"}, questionResult{[]string{"use a flag"}, true}},
		{"typing on the last row starts the answer", yes, []string{"j", "no", "enter"}, questionResult{[]string{"no"}, true}},
		{"open question types at once", tools.Question{Question: "Name?"}, []string{"zemi", "enter"}, questionResult{[]string{"zemi"}, true}},
		{"esc leaves typing, second esc dismisses", yes, []string{"j", "enter", "x", "esc", "esc"}, questionResult{nil, false}},
		{"ctrl+c dismisses", yes, []string{"ctrl+c"}, questionResult{nil, false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 70, 16)
			done := askAsync(t, app, c.q)
			pressKeys(app, c.keys...)
			if r := awaitAnswer(t, app, done); r.ok != c.want.ok || fmt.Sprint(r.answers) != fmt.Sprint(c.want.answers) {
				t.Fatalf("got %q ok=%v, want %q ok=%v", r.answers, r.ok, c.want.answers, c.want.ok)
			}
		})
	}
}

func TestQuestionBatch(t *testing.T) {
	pick := func(labels ...string) tools.Question {
		q := tools.Question{Question: labels[0] + "?"}
		for _, l := range labels {
			q.Options = append(q.Options, tools.QuestionOption{Label: l})
		}
		return q
	}
	qs := []tools.Question{pick("a", "a2"), pick("b", "b2")}
	qs[1].Multi = true
	tab, back := tea.KeyPressMsg{Code: tea.KeyTab}, tea.KeyPressMsg{Code: tea.KeyTab, Mod: uv.ModShift}
	for _, c := range []struct {
		name string
		keys []any // string key names for pressKeys, or a tea.KeyPressMsg
		want questionResult
	}{
		{"answers lead to review, enter submits", []any{"enter", "j", "enter", "enter"}, questionResult{[]string{"a", "b2"}, true}},
		{"a question can be changed from review", []any{"enter", "enter", "k", "k", "enter", "j", "enter", "enter"}, questionResult{[]string{"a2", "b"}, true}},
		{"tab skips a question, it submits unanswered", []any{tab, "enter", "enter"}, questionResult{[]string{"", "b"}, true}},
		{"going back keeps the typed answer", []any{"j", "j", "hi", "enter", back, " there", "enter", tab, "enter"}, questionResult{[]string{"hi there", ""}, true}},
		{"marks count when tab leaves the question", []any{tab, "space", "j", "space", tab, "enter"}, questionResult{[]string{"", "b, b2"}, true}},
		{"unmarking everything unanswers it", []any{tab, "space", "space", tab, "enter"}, questionResult{[]string{"", ""}, true}},
		{"esc on review dismisses", []any{"enter", "enter", "esc"}, questionResult{nil, false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 70, 20)
			done := askAsync(t, app, qs...)
			for _, k := range c.keys {
				if msg, ok := k.(tea.KeyPressMsg); ok {
					app.Update(msg)
				} else {
					pressKeys(app, k.(string))
				}
			}
			if r := awaitAnswer(t, app, done); r.ok != c.want.ok || fmt.Sprint(r.answers) != fmt.Sprint(c.want.answers) {
				t.Fatalf("got %q ok=%v, want %q ok=%v", r.answers, r.ok, c.want.answers, c.want.ok)
			}
		})
	}
}

// TestQuestionBoxShowsTyping: the typed answer shows with its end in view,
// and a question wider than the box wraps instead of losing its end.
func TestQuestionBoxShowsTyping(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	askAsync(t, app, tools.Question{Question: "What should the command be called, now that it replaces both sync and fetch?"})
	pressKeys(app, "zem")
	app.Update(tea.PasteMsg{Content: "i ai"})
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "> zemi ai") || !strings.Contains(frame, "fetch?") {
		t.Fatalf("typed and pasted text or question end missing:\n%s", frame)
	}
	// a line wider than the box scrolls to keep the latest typing in view
	pressKeys(app, strings.Repeat("x", 80)+"end")
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "xend") {
		t.Fatalf("the end of a long answer is hidden:\n%s", frame)
	}
	pressKeys(app, "esc", "esc")
}

// TestQuestionBoxScrolls: options that do not fit scroll with the
// selection and a long question gives way, keeping the typed-answer row
// and the hints on screen.
func TestQuestionBoxScrolls(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	var opts []tools.QuestionOption
	for i := range 20 {
		opts = append(opts, tools.QuestionOption{Label: fmt.Sprintf("option %d", i)})
	}
	askAsync(t, app, tools.Question{Question: strings.Repeat("which one? ", 300), Options: opts})
	for range 20 {
		app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"› Type your own answer", "21/21", "esc cancel", "└"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("%q missing:\n%s", want, frame)
		}
	}
	pressKeys(app, "esc")
}
