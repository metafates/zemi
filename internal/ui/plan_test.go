package ui

import (
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
		{"typing requests changes", "", []string{"u", "s", "e", " ", "s", "q", "l", "enter"}, tools.PlanVerdict{Feedback: "use sql"}, true},
		{"esc dismisses", "", []string{"esc"}, tools.PlanVerdict{}, false},
		{"q dismisses", "", []string{"q"}, tools.PlanVerdict{}, false},
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
