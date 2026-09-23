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
		{"approve outside plan mode", "", []string{"enter"}, tools.PlanVerdict{Approved: true}, true},
		{"typing requests changes", "", []string{"e", "u", "s", "e", " ", "s", "q", "l", "enter"}, tools.PlanVerdict{Feedback: "use sql"}, true},
		{"esc dismisses", "", []string{"esc"}, tools.PlanVerdict{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 70, 18)
			done := planAsync(t, app, "# Plan\n\n- one\n- two", c.mode)
			pressKeys(app, c.keys...)
			if r := awaitPlan(t, app, done); r.ok != c.want || r.v.Approved != c.wantV.Approved || r.v.Feedback != c.wantV.Feedback {
				t.Fatalf("verdict %+v ok=%v, want %+v ok=%v", r.v, r.ok, c.wantV, c.want)
			}
		})
	}
}

func TestPlanEditKeys(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)
	done := planAsync(t, app, "# Plan", "")
	pressKeys(app, "e", "j", "k", "enter") // inside edit j/k type, not scroll
	if r := awaitPlan(t, app, done); !r.ok || r.v.Feedback != "jk" {
		t.Fatalf("e then j/k should send feedback \"jk\", got %+v ok=%v", r.v, r.ok)
	}
}

func TestPlanScroll(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)
	done := planAsync(t, app, strings.Repeat("# h\n\nline\n", 30), "")
	app.Frame()
	pressKeys(app, "j")
	if app.planAsk.view.top != 1 || app.planAsk.typing {
		t.Fatalf("j should scroll the plan, got top=%d typing=%v", app.planAsk.view.top, app.planAsk.typing)
	}
	pressKeys(app, "k")
	pressKeys(app, "enter")
	if r := awaitPlan(t, app, done); !r.ok || !r.v.Approved {
		t.Fatalf("j/k scroll then enter should approve, got %+v ok=%v", r.v, r.ok)
	}
}

func TestPlanBoxFits(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)
	box := app.planBox(&planAskPrompt{plan: strings.Repeat("line\n", 200)}, 70, 18)
	if len(box) > 18 {
		t.Fatalf("plan box is %d rows, taller than the %d the screen clips to", len(box), 18)
	}
}
