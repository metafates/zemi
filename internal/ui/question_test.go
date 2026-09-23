package ui

import (
	"fmt"
	"regexp"
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
		tools.Question{Type: tools.TypeSingle, Question: "Apply the mined rules to this project?", Header: "Rules", Options: []tools.QuestionOption{
			{Label: "apply (Recommended)", Description: "store both as project memories"},
			{Label: "skip", Description: "leave the proposals unapplied"},
		}},
		tools.Question{Type: tools.TypeMulti, Question: "Which checks run before commit?", Header: "Checks", Options: []tools.QuestionOption{
			{Label: "tests"}, {Label: "vet"}, {Label: "lint"},
		}},
	)
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	// every tab, typing and the review draw in the one box: its top row and height
	box := func() string {
		app.renderSync(t)
		f := ansi.Strip(app.Frame())
		top := strings.Index(f, "┌ Questions")
		return fmt.Sprint(strings.Count(f[:top], "\n"), strings.Count(f[top:top+strings.Index(f[top:], "┘")], "\n"))
	}
	want := box()
	for _, keys := range [][]string{{"j", "enter"}, {"j", "j", "j", "enter"}, {"esc", "k", "k", "k", "space", "j", "j", "space", "enter"}} {
		if pressKeys(app, keys...); box() != want {
			t.Fatalf("after %v the box moved from %s to %s:\n%s", keys, want, box(), ansi.Strip(app.Frame()))
		}
	}
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "Submit answers") || !strings.Contains(frame, "tests, lint") {
		t.Fatalf("review tab missing:\n%s", frame)
	}
	pressKeys(app, "enter")
	if r := awaitAnswer(t, app, done); !r.ok || fmt.Sprint(r.answers) != "[skip tests, lint]" {
		t.Fatalf("answers %q ok=%v", r.answers, r.ok)
	}
}

func TestQuestionAnswers(t *testing.T) {
	yes := tools.Question{Type: tools.TypeSingle, Question: "Proceed?", Options: []tools.QuestionOption{{Label: "yes"}}}
	for _, c := range []struct {
		name string
		q    tools.Question
		keys []string
		want questionResult
	}{
		{"typed answer on the last row", yes, []string{"j", "enter", "use a flag", "enter"}, questionResult{[]string{"use a flag"}, true}},
		{"typing on the last row starts the answer, a digit too", yes, []string{"j", "2weeks", "enter"}, questionResult{[]string{"2weeks"}, true}},
		{"open question types at once", tools.Question{Type: tools.TypeFreeText, Question: "Name?"}, []string{"zemi", "enter"}, questionResult{[]string{"zemi"}, true}},
		{"yes_no answered by n, as every prompt answers no", tools.Question{Type: tools.TypeYesNo, Question: "Go?", Options: []tools.QuestionOption{{Label: "Yes"}, {Label: "No"}}}, []string{"n"}, questionResult{[]string{"No"}, true}},
		{"digit picks a single choice", tools.Question{Type: tools.TypeSingle, Question: "Pick?", Options: []tools.QuestionOption{{Label: "one"}, {Label: "two"}}}, []string{"2"}, questionResult{[]string{"two"}, true}},
		{"esc leaves typing, second esc dismisses", yes, []string{"j", "enter", "x", "esc", "esc"}, questionResult{nil, false}},
		{"the typed answer's digit starts it", yes, []string{"2", "no", "enter"}, questionResult{[]string{"no"}, true}},
		{"esc out of an open question's input dismisses, enter after is safe", tools.Question{Type: tools.TypeFreeText, Question: "Name?"}, []string{"enter", "esc", "enter"}, questionResult{nil, false}},
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
		q := tools.Question{Type: tools.TypeSingle, Question: labels[0] + "?"}
		for _, l := range labels {
			q.Options = append(q.Options, tools.QuestionOption{Label: l})
		}
		return q
	}
	qs := []tools.Question{pick("a", "a2"), pick("b", "b2")}
	qs[1].Type = tools.TypeMulti
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
	// esc out of an open question's input dismisses the whole batch, and the
	// enter after it reaches only the composer
	app, _ := bootAgentApp(t, 70, 20)
	done := askAsync(t, app, qs[0], tools.Question{Type: tools.TypeFreeText, Question: "Name?"})
	pressKeys(app, "enter", "enter", "esc", "enter")
	if r := awaitAnswer(t, app, done); r.ok {
		t.Fatalf("got %q, want the batch dismissed", r.answers)
	}
}

// TestQuestionBoxShowsTyping: the typed answer shows with its end in view,
// and a question wider than the box wraps instead of losing its end.
func TestQuestionBoxShowsTyping(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	askAsync(t, app, tools.Question{Type: tools.TypeFreeText, Question: "What should the command be called, now that it replaces both sync and fetch?"})
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
	askAsync(t, app, tools.Question{Type: tools.TypeSingle, Question: strings.Repeat("which one? ", 300), Options: opts})
	for range 20 {
		app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"›     Type your own answer", "█", "esc dismiss", "└"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("%q missing:\n%s", want, frame)
		}
	}
	pressKeys(app, "esc")
}

// TestQuestionOptionsWrap: a long option wraps in its column, and every
// description starts at one x, beside its own label.
func TestQuestionOptionsWrap(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 20)
	askAsync(t, app, tools.Question{Type: tools.TypeSingle, Question: "Which storage?", Options: []tools.QuestionOption{
		{Label: "SQLite with write-ahead logging and a busy timeout (Recommended)", Description: "one file, no server"},
		{Label: "Postgres", Description: "a server to run"},
	}})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	x := func(desc string) int {
		for l := range strings.SplitSeq(frame, "\n") {
			if i := strings.Index(l, desc); i >= 0 {
				return ansi.StringWidth(l[:i])
			}
		}
		return -1
	}
	if x("recommended") < 0 || x("recommended") != x("a server to run") || !regexp.MustCompile(`\[2\] Postgres +a server to run`).MatchString(frame) {
		t.Fatalf("descriptions are lost or out of their column:\n%s", frame)
	}
	pressKeys(app, "esc")
}
