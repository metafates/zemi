package hostapi

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// TestGoalLoop drives /goal against a model that works and, asked to
// judge, answers from the case's verdicts: what each check decides, what
// stops the chain, the notice rows it leaves, and that a resume draws the
// same rows and restores the same goal. An interrupt of the goal's own work
// stops a loop too; one of a typed turn does not.
func TestGoalLoop(t *testing.T) {
	paused := append(slices.Repeat([]string{"Goal not met yet: x"}, _goalMaxRuns), "Goal paused: after 20 turns without you; send a message to go on")
	for _, c := range []struct {
		name    string
		verdict func(h *Host, check int) string // the judge's reply to the check-th check, from 0
		fail    bool                            // every turn fails
		cancel  bool                            // the first turn is interrupted
		turns   int
		goal    bool     // left set
		loopOff bool     // the loop set before it was stopped
		notices []string // the notice rows, in order
		saw     string   // what the last turn read
	}{
		{name: "met after two not yet", verdict: func(_ *Host, n int) string {
			return []string{"NOT YET: a", "**NOT YET**: b", "Checked.\nMET: done"}[n]
		}, turns: 3, notices: []string{"Goal not met yet: a", "Goal not met yet: b", "Goal met: done"}, saw: "Not met yet: b"},
		{
			name: "blocked keeps the goal", verdict: func(*Host, int) string { return "BLOCKED: needs a token" }, turns: 1, goal: true,
			notices: []string{"Goal blocked: needs a token"},
		},
		{
			name: "no verdict pauses", verdict: func(*Host, int) string { return "sure" }, turns: 1, goal: true,
			notices: []string{"Goal check gave no verdict; still watching"},
		},
		{
			name: "a pause that answers the same draws once", verdict: func(h *Host, n int) string {
				if n == 0 { // a queued user turn re-checks after the pause
					if err := h.AgentSend("again"); err != nil {
						panic(err)
					}
				}
				return "sure"
			}, turns: 2, goal: true,
			notices: []string{"Goal check gave no verdict; still watching"},
		},
		{name: "the cap pauses", turns: _goalMaxRuns + 1, goal: true, notices: paused},
		{name: "a failed turn is not checked", fail: true, turns: 1, goal: true},
		{name: "an interrupted turn is not checked", cancel: true, turns: 1, goal: true},
		{name: "an interrupted check stops the loop", verdict: func(h *Host, _ int) string {
			h.AgentCancel()
			return "NOT YET: a"
		}, turns: 1, goal: true, loopOff: true},
		{name: "a message queued during the check goes next", verdict: func(h *Host, n int) string {
			if n == 0 {
				if err := h.AgentSend("also this"); err != nil {
					panic(err)
				}
				return "NOT YET: a"
			}
			return "MET: done"
		}, turns: 2, notices: []string{"Goal not met yet: a", "Goal met: done"}, saw: "also this"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			var turns, checks atomic.Int32
			var saw atomic.Value
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
					if strings.HasPrefix(last, "Judge whether") {
						if n := int(checks.Add(1)) - 1; c.verdict != nil {
							return textParts(c.verdict(h, n))
						}
						return textParts("NOT YET: x") // never met, so a check that should not run shows
					}
					saw.Store(last)
					switch turns.Add(1); {
					case c.fail:
						return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: errors.New("boom")}}
					case c.cancel:
						return nil // waits for the interrupt
					default:
						return textParts("worked")
					}
				}}, nil
			}
			h.agents.mu.Lock()
			h.agents.loop = &Loop{Prompt: "watch"}
			h.agents.mu.Unlock()
			if err := h.SetGoal("the  tests\npass"); err != nil {
				t.Fatal(err)
			}
			if c.cancel {
				waitFor(t, func() error {
					if turns.Load() == 0 {
						return errors.New("no turn yet")
					}
					return nil
				})
				h.AgentCancel()
			}
			waitIdle(t, h)
			if got := int(turns.Load()); got != c.turns {
				t.Errorf("turns = %d, want %d", got, c.turns)
			}
			if s, _ := saw.Load().(string); !strings.Contains(s, c.saw) {
				t.Errorf("the last turn read %q, want %q in it", s, c.saw)
			}
			var g Goal
			var set bool
			var notices []string
			waitFor(t, func() error {
				if _, on := h.Loop(); on == c.loopOff {
					return fmt.Errorf("loop on %v, want %v", on, !c.loopOff)
				}
				g, set = h.Goal()
				if set != c.goal || set && g.Condition != "the tests pass" {
					return fmt.Errorf("goal = %+v %v, want set %v", g, set, c.goal)
				}
				notices = goalRows(h.Session().Transcript)
				if !slices.Equal(notices, c.notices) {
					return fmt.Errorf("notices = %q, want %q", notices, c.notices)
				}
				return nil
			})
		})
	}
}

// goalRows are the goal notices a transcript draws.
func goalRows(tr []ChatMessage) []string {
	var out []string
	for _, m := range tr {
		if m.Kind == RowNotice && strings.HasPrefix(m.Text, "Goal ") {
			out = append(out, m.Text)
		}
	}
	return out
}

// TestSetGoalRefuses: what /goal refuses before anything is sent.
func TestSetGoalRefuses(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	for _, c := range []struct{ args, want string }{
		{"clear", "a goal names an end state"},
		{"stop", "no goal to stop"},
		{"", "needs the TUI"},
	} {
		if err := h.SetGoal(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("/goal %s: %v, want %q", c.args, err, c.want)
		}
	}
	h.modelFn = func() (fantasy.LanguageModel, error) { return nil, errors.New("no model") }
	if err := h.SetGoal("the tests pass"); err == nil {
		t.Fatal("a goal with no model was sent")
	}
	if _, ok := h.Goal(); ok {
		t.Fatal("a goal that was never sent is set")
	}
}
