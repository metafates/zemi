package ui

import (
	"fmt"
	"regexp"
	"strconv"
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
	answers []tools.Answer
	ok      bool
}

// askAsync opens the question overlay from a tool goroutine, as the
// question tool does.
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

// pressKeys presses each key: a named one, ctrl and a letter, or else its
// runes typed one by one.
func pressKeys(app *App, keys ...string) {
	named := map[string]tea.KeyPressMsg{
		"enter": {Code: tea.KeyEnter}, "esc": {Code: tea.KeyEscape}, "space": {Code: tea.KeySpace, Text: " "},
		"backspace": {Code: tea.KeyBackspace}, "shift+down": {Code: tea.KeyDown, Mod: uv.ModShift},
		"left": {Code: tea.KeyLeft},
	}
	for _, k := range keys {
		msg, ok := named[k]
		if c, ctrl := strings.CutPrefix(k, "ctrl+"); ctrl && len(c) == 1 {
			msg, ok = tea.KeyPressMsg{Mod: uv.ModCtrl, Code: rune(c[0])}, true
		}
		if ok {
			app.Update(msg)
			continue
		}
		for _, r := range k {
			app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
}

// TestGoldenQuestionOverlay pins the Go-owned question overlay (DESIGN
// §8): a batch shows tabs, answering moves on to the Submit tab, whose
// summary shows a note, and the answers come back in order.
func TestGoldenQuestionOverlay(t *testing.T) {
	app, _ := bootAgentAppPinned(t, 70, 18)
	done := askAsync(t, app,
		tools.Question{Type: tools.TypeSingle, Question: "Apply the mined rules to this project?", Header: "Rules", Options: []tools.QuestionOption{
			{Label: "apply", Description: "store both as project memories", Recommended: true},
			{Label: "skip", Description: "leave the proposals unapplied"},
		}},
		tools.Question{Type: tools.TypeMulti, Question: "Which checks run before commit?", Header: "Checks", Options: []tools.QuestionOption{
			{Label: "tests"}, {Label: "vet"}, {Label: "lint"},
		}},
	)
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	// every tab, the field focused and the Submit tab draw in the one box: its top row, height and width
	box := func() string {
		app.renderSync(t)
		f := ansi.Strip(app.Frame())
		top := strings.Index(f, "┌ Questions")
		return strconv.Itoa(strings.Count(f[:top], "\n")) + strconv.Itoa(strings.Count(f[top:top+strings.Index(f[top:], "┘")], "\n")) + strconv.Itoa(ansi.StringWidth(strings.Split(f[top:], "\n")[0]))
	}
	want := box()
	// focusing the field and scrolling the summary keep the footer's keys in place
	escAt := func() int {
		app.renderSync(t)
		f := ansi.Strip(app.Frame())
		return strings.Index(f[strings.LastIndex(f, "enter "):], "esc ")
	}
	footer := func(keys, back []string) {
		at := escAt()
		if pressKeys(app, keys...); escAt() != at {
			t.Fatalf("after %v the footer moved:\n%s", keys, ansi.Strip(app.Frame()))
		}
		pressKeys(app, back...)
	}
	footer([]string{"j", "j", "enter"}, []string{"esc", "k", "k"})
	footer([]string{"n"}, []string{"esc"})
	for _, keys := range [][]string{{"j", "n", "only in this repo", "enter"}, {"j", "j", "j", "enter"}, {"esc", "k", "k", "k", "space", "j", "j", "space", "enter"}} {
		if pressKeys(app, keys...); box() != want {
			t.Fatalf("after %v the box moved from %s to %s:\n%s", keys, want, box(), ansi.Strip(app.Frame()))
		}
	}
	t.Run("submit", func(t *testing.T) { golden.RequireEqual(t, app.Frame()) })
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "[s] Submit answers") || !strings.Contains(frame, "tests, lint") || !strings.Contains(frame, "note: only in this repo") {
		t.Fatalf("Submit tab missing:\n%s", frame)
	}
	footer([]string{"k"}, []string{"j"})
	pressKeys(app, "enter")
	if r := awaitAnswer(t, app, done); !r.ok || fmt.Sprint(r.answers) != "[{[skip]  only in this repo} {[tests lint]  }]" {
		t.Fatalf("answers %+v ok=%v", r.answers, r.ok)
	}
}

// TestQuestionTypingKeepsBox: typing past the field, a cursor moved into
// the text and a kept note cut to the field never widen the question's box.
func TestQuestionTypingKeepsBox(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 70, 18)
	done := askAsync(t, app, tools.Question{Type: tools.TypeSingle, Question: "Proceed?", Options: []tools.QuestionOption{{Label: "yes"}, {Label: "no"}}})
	width := func() int {
		app.renderSync(t)
		f := ansi.Strip(app.Frame())
		top := strings.Index(f, "┌ Question")
		if top < 0 {
			t.Fatalf("no question box:\n%s", f)
		}
		return ansi.StringWidth(strings.Split(f[top:], "\n")[0])
	}
	want := width()
	pressKeys(app, "j", "j", "enter") // the typed-answer row, focused
	for i := range 3 {
		pressKeys(app, strings.Repeat("0123456789", 4))
		if got := width(); got != want {
			t.Fatalf("typing %d chars widened the box from %d to %d", (i+1)*40, want, got)
		}
	}
	pressKeys(app, "left", "left", "left", "left", "left", "x", "y", "z")
	if got := width(); got != want {
		t.Fatalf("typing at a moved cursor widened the box from %d to %d", want, got)
	}
	pressKeys(app, "esc") // the draft is kept and shown on the field row
	if got := width(); got != want {
		t.Fatalf("a kept note widened the box from %d to %d", want, got)
	}
	pressKeys(app, "enter", "enter")
	awaitAnswer(t, app, done)
}

func TestQuestionAnswers(t *testing.T) {
	yes := tools.Question{Type: tools.TypeSingle, Question: "Proceed?", Options: []tools.QuestionOption{{Label: "yes"}}}
	two := tools.Question{Type: tools.TypeSingle, Question: "Pick?", Options: []tools.QuestionOption{{Label: "one"}, {Label: "two"}}}
	for _, c := range []struct {
		name string
		q    tools.Question
		keys []string
		want questionResult
	}{
		{"typed answer on the last row", yes, []string{"j", "enter", "use a flag", "enter"}, questionResult{[]tools.Answer{{Typed: "use a flag"}}, true}},
		{"typing on the last row starts the answer, n too", yes, []string{"j", "n2 weeks", "enter"}, questionResult{[]tools.Answer{{Typed: "n2 weeks"}}, true}},
		{"open question types at once", tools.Question{Type: tools.TypeFreeText, Question: "Name?"}, []string{"zemi", "enter"}, questionResult{[]tools.Answer{{Typed: "zemi"}}, true}},
		{"digit picks a single choice", two, []string{"2"}, questionResult{[]tools.Answer{{Picked: []string{"two"}}}, true}},
		{"n on an option writes a note, enter sends pick and note", two, []string{"j", "n", "if it builds", "enter"}, questionResult{[]tools.Answer{{Picked: []string{"two"}, Note: "if it builds"}}, true}},
		{"a digit after writing a note carries it", two, []string{"n", "later", "esc", "2"}, questionResult{[]tools.Answer{{Picked: []string{"two"}, Note: "later"}}, true}},
		{"a multi-select sends its marks with the note", tools.Question{Type: tools.TypeMulti, Question: "Which?", Options: two.Options}, []string{"space", "j", "space", "n", "both", "enter"}, questionResult{[]tools.Answer{{Picked: []string{"one", "two"}, Note: "both"}}, true}},
		{"esc leaves typing, second esc dismisses", yes, []string{"j", "enter", "x", "esc", "esc"}, questionResult{nil, false}},
		{"the typed answer's digit starts it", yes, []string{"2", "no", "enter"}, questionResult{[]tools.Answer{{Typed: "no"}}, true}},
		{"esc out of an open question's input dismisses, enter after is safe", tools.Question{Type: tools.TypeFreeText, Question: "Name?"}, []string{"enter", "esc", "enter"}, questionResult{nil, false}},
		{"ctrl+c dismisses", yes, []string{"ctrl+c"}, questionResult{nil, false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, _ := bootAgentApp(t, 70, 16)
			done := askAsync(t, app, c.q)
			pressKeys(app, c.keys...)
			if r := awaitAnswer(t, app, done); r.ok != c.want.ok || fmt.Sprint(r.answers) != fmt.Sprint(c.want.answers) {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", r.answers, r.ok, c.want.answers, c.want.ok)
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
		{"answers lead to the Submit tab, enter submits", []any{"enter", "j", "enter", "enter"}, questionResult{[]tools.Answer{{Picked: []string{"a"}}, {Picked: []string{"b2"}}}, true}},
		{"s submits a partial batch from the Submit tab", []any{"enter", tab, "s"}, questionResult{[]tools.Answer{{Picked: []string{"a"}}, {}}, true}},
		{"a question's digit changes it from the Submit tab", []any{"enter", "enter", "1", "j", "enter", "enter"}, questionResult{[]tools.Answer{{Picked: []string{"a2"}}, {Picked: []string{"b"}}}, true}},
		{"a note edited after answering counts", []any{"enter", back, "n", "first", "esc", tab, tab, "enter"}, questionResult{[]tools.Answer{{Picked: []string{"a"}, Note: "first"}, {}}, true}},
		{"a note with no pick rides the answer", []any{"n", "why", "esc", tab, tab, "enter"}, questionResult{[]tools.Answer{{Note: "why"}, {}}, true}},
		{"going back keeps the typed answer", []any{"j", "j", "hi", "enter", back, " there", "enter", tab, "enter"}, questionResult{[]tools.Answer{{Typed: "hi there"}, {}}, true}},
		{"marks count when tab leaves the question", []any{tab, "space", "j", "space", tab, "enter"}, questionResult{[]tools.Answer{{}, {Picked: []string{"b", "b2"}}}, true}},
		{"unmarking everything unanswers it", []any{tab, "space", "space", tab, "enter"}, questionResult{[]tools.Answer{{}, {}}, true}},
		{"esc on the Submit tab dismisses", []any{"enter", "enter", "esc"}, questionResult{nil, false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
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
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", r.answers, r.ok, c.want.answers, c.want.ok)
			}
		})
	}
	// esc out of an open question's input dismisses the whole batch, and the
	// enter after it reaches only the composer
	app, _ := bootAgentApp(t, 70, 20)
	done := askAsync(t, app, qs[0], tools.Question{Type: tools.TypeFreeText, Question: "Name?"})
	pressKeys(app, "enter", "enter", "esc", "enter")
	if r := awaitAnswer(t, app, done); r.ok {
		t.Fatalf("got %+v, want the batch dismissed", r.answers)
	}
}

// TestQuestionBoxShowsTyping: the typed answer shows with its end in view,
// and a question wider than the box wraps instead of losing its end.
func TestQuestionBoxShowsTyping(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestQuestionOptionsWrap: an option draws as one full-width row and the
// selected option's description shows in the pane under the options, where
// moving the selection swaps it without shifting a row.
func TestQuestionOptionsWrap(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 100, 20)
	askAsync(t, app, tools.Question{Type: tools.TypeSingle, Question: "Which storage?", Options: []tools.QuestionOption{
		{Label: "SQLite with write-ahead logging and a busy timeout", Description: "one file, no server", Recommended: true},
		{Label: "Postgres", Description: "a server to run and back up"},
	}})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !regexp.MustCompile(`\[1\] SQLite with write-ahead logging and a busy timeout`).MatchString(frame) ||
		!strings.Contains(frame, "recommended  one file, no server") || strings.Contains(frame, "a server to run") {
		t.Fatalf("the selected option's description is not alone in the pane:\n%s", frame)
	}
	rows := func() (h, w int) {
		app.renderSync(t)
		f := ansi.Strip(app.Frame())
		top := strings.Index(f, "┌ Question")
		return strings.Count(f[:top], "\n"), ansi.StringWidth(strings.Split(f[top:], "\n")[0])
	}
	h0, w0 := rows()
	h, w := func() (int, int) { pressKeys(app, "j"); return rows() }()
	if h != h0 || w != w0 {
		t.Fatalf("moving the selection moved the box:\n%s", ansi.Strip(app.Frame()))
	}
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "a server to run and back up") || strings.Contains(frame, "one file, no server") {
		t.Fatalf("the pane does not follow the selection:\n%s", frame)
	}
	pressKeys(app, "esc")
}

// TestQuestionNoteStays: a note typed past the box's width never widens it.
func TestQuestionNoteStays(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 100, 24)
	askAsync(t, app, tools.Question{Type: tools.TypeSingle, Question: "Proceed?", Options: []tools.QuestionOption{
		{Label: "yes"}, {Label: "no", Description: "stop and wait"},
	}})
	app.renderSync(t)
	f := ansi.Strip(app.Frame())
	top := strings.Index(f, "┌ Question")
	w0 := ansi.StringWidth(strings.Split(f[top:], "\n")[0])
	pressKeys(app, "n", strings.Repeat("x", 120))
	app.renderSync(t)
	f = ansi.Strip(app.Frame())
	top = strings.Index(f, "┌ Question")
	if w := ansi.StringWidth(strings.Split(f[top:], "\n")[0]); w != w0 {
		t.Fatalf("typing a long note widened the box from %d to %d:\n%s", w0, w, f)
	}
	pressKeys(app, "esc", "esc")
}

// TestQuestionMarkdown: the question and the selected option's description
// are model prose, so their backticks render as code instead of showing.
func TestQuestionMarkdown(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 70, 18)
	done := askAsync(t, app, tools.Question{
		Type:        tools.TypeSingle,
		Question:    "Use `zemi plugin` here?",
		Description: "It prints to stdout.",
		Options: []tools.QuestionOption{
			{Label: "yes", Description: "keeps `--json` working", Recommended: true},
			{Label: "no"},
		},
	})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if strings.Contains(frame, "`") {
		t.Fatalf("backticks left in the overlay:\n%s", frame)
	}
	for _, want := range []string{"zemi plugin", "--json", "recommended"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	pressKeys(app, "esc")
	awaitAnswer(t, app, done)
}
