package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/tools"
)

type planResult struct {
	v  tools.PlanVerdict
	ok bool
}

// planAsync opens the plan approval from a tool goroutine, as the plan tool
// does.
func planAsync(t *testing.T, app *App, plan, mode string) chan planResult {
	t.Helper()
	done := make(chan planResult, 1)
	go func() {
		v, ok := app.AskPlan(plan, mode)
		done <- planResult{v, ok}
	}()
	waitPrompt(t, app, func() bool { return app.planAsk != nil })
	return done
}

func awaitPlan(t *testing.T, app *App, done chan planResult) planResult {
	t.Helper()
	select {
	case r := <-done:
		if app.planAsk != nil {
			t.Fatal("plan overlay should be closed")
		}
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("plan answer not delivered")
	}
	return planResult{}
}

func TestPlanApproval(t *testing.T) {
	cases := []struct {
		name  string
		mode  string
		keys  []string
		wantV tools.PlanVerdict
		want  bool
	}{
		{"enter approves", "auto", []string{"enter"}, tools.PlanVerdict{Approved: true}, true},
		{"a approves outside plan mode", "", []string{"a"}, tools.PlanVerdict{Approved: true}, true},
		{"s saves without starting it", "", []string{"s"}, tools.PlanVerdict{Approved: true, Hold: true}, true},
		{"down down enter opens the changes input", "", []string{"j", "j", "enter", "x", "enter"}, tools.PlanVerdict{Feedback: "x"}, true},
		{"typing requests changes", "", []string{"e", "u", "s", "e", " ", "s", "q", "l", "enter"}, tools.PlanVerdict{Feedback: "use sql"}, true},
		{"esc dismisses", "", []string{"esc"}, tools.PlanVerdict{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 70, 18)
			done := planAsync(t, app, "# Plan\n\n- one\n- two", c.mode)
			pressKeys(app, c.keys...)
			if r := awaitPlan(t, app, done); r.ok != c.want || r.v != c.wantV {
				t.Fatalf("verdict %+v ok=%v, want %+v ok=%v", r.v, r.ok, c.wantV, c.want)
			}
		})
	}
}

// TestPlanEditKeys: e swaps the buttons for the changes input row for row,
// where j and k type instead of scrolling.
func TestPlanEditKeys(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)
	done := planAsync(t, app, "# Plan", "")
	before := len(app.planBox(app.planAsk, 70, 18))
	if pressKeys(app, "e"); len(app.planBox(app.planAsk, 70, 18)) != before {
		t.Fatalf("e moved the box from %d rows", before)
	}
	pressKeys(app, "j", "k", "enter")
	if r := awaitPlan(t, app, done); !r.ok || r.v.Feedback != "jk" {
		t.Fatalf("e then j/k should send feedback \"jk\", got %+v ok=%v", r.v, r.ok)
	}
}

// TestPlanScroll: the plan's text scrolls a line on shift+arrows and half
// the view on ctrl+d/u, the list keys move the buttons, and while a change
// is typed neither half-page key scrolls and esc then e keeps the draft. top
// counts lines plus halves of the view.
func TestPlanScroll(t *testing.T) {
	for _, c := range []struct {
		name               string
		keys               []string
		lines, halves, sel int
		input              string
	}{
		{"shift+down scrolls a line", []string{"shift+down"}, 1, 0, 0, ""},
		{"ctrl+d scrolls half the view", []string{"ctrl+d"}, 0, 1, 0, ""},
		{"ctrl+u goes back", []string{"ctrl+d", "ctrl+d", "ctrl+u"}, 0, 1, 0, ""},
		{"j moves the button, not the text", []string{"j"}, 0, 0, 1, ""},
		{"ctrl+u while typing changes scrolls the text", []string{"e", "xy", "ctrl+d", "ctrl+d", "ctrl+u", "z"}, 0, 1, 0, "xyz"},
		{"ctrl+d while typing changes scrolls the text", []string{"e", "x", "ctrl+d"}, 0, 1, 0, "x"},
		{"esc then e keeps the draft", []string{"e", "x", "esc", "e", "y"}, 0, 0, 0, "xy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 70, 18)
			done := planAsync(t, app, strings.Repeat("# h\n\nline\n", 30), "")
			app.Frame()
			pressKeys(app, c.keys...)
			p := app.planAsk
			if want := c.lines + c.halves*max(p.view.viewH/2, 1); p.view.top != want || p.sel != c.sel || p.input.text != c.input {
				t.Errorf("top=%d sel=%d input=%q, want %d %d %q", p.view.top, p.sel, p.input.text, want, c.sel, c.input)
			}
			app.finishPlan(tools.PlanVerdict{}, false)
			awaitPlan(t, app, done)
		})
	}
}

// TestPlanClickButtons: the hit test reads the buttons the rows draw, so a
// click on the second one opens the changes input.
func TestPlanClickButtons(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)
	done := planAsync(t, app, "# Plan", "")
	app.renderSync(t)
	x, y := findCell(t, app, "Request changes")
	clickAt(app, x+2, y)
	if !app.planAsk.typing {
		t.Fatal("a click on Request changes should open the input")
	}
	pressKeys(app, "more", "enter")
	if r := awaitPlan(t, app, done); !r.ok || r.v.Feedback != "more" {
		t.Fatalf("verdict %+v ok=%v, want feedback \"more\"", r.v, r.ok)
	}
}

func TestPlanBoxFits(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)
	box := app.planBox(&planAskPrompt{plan: strings.Repeat("line\n", 200)}, 70, 18)
	if len(box) > 18 {
		t.Fatalf("plan box is %d rows, taller than the %d the screen clips to", len(box), 18)
	}
}
