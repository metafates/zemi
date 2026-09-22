// plan.go is the Go-owned plan approval overlay (DESIGN §6.4, §8): the plan
// tool shows its plan as a scrollable markdown "Plan" popup the user approves
// (enter), asks to change (type), or dismisses (esc). The agent to return to
// on approval is remembered when the read-only agent started, so approval
// never asks which agent or mode.
package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

type planAskPrompt struct {
	plan   string
	scroll int
	typing bool
	input  composerState
	mode   string // "in <mode> mode" when a mode switch follows approval
	answer chan planAnswer
}

type planAnswer struct {
	v  tools.PlanVerdict
	ok bool
}

// AskPlan implements hostapi.UIHooks. It blocks the caller (the plan tool on
// the turn goroutine) until the user approves, types changes, or dismisses.
func (a *App) AskPlan(plan, implementMode string) (tools.PlanVerdict, bool) {
	ch := make(chan planAnswer, 1)
	a.await(func() tea.Cmd {
		a.planAsk = &planAskPrompt{
			plan:   sanitize.String(plan),
			mode:   implementMode,
			answer: ch,
		}
		return a.notifyIfAway("zemi has a plan to review")
	})
	ans := <-ch
	return ans.v, ans.ok
}

func (a *App) finishPlan(v tools.PlanVerdict, ok bool) {
	p := a.planAsk
	a.planAsk = nil
	if p != nil && p.answer != nil {
		p.answer <- planAnswer{v, ok}
	}
	a.frameGen++
}

// planKey handles a key on the plan popup: scrolling, approve, open the
// changes input with e, or dismiss. Only in the changes input do keys type.
func (a *App) planKey(key, text string) {
	p := a.planAsk
	defer func() { a.frameGen++ }()
	if p.typing {
		switch key {
		case "enter":
			if s := strings.TrimSpace(p.input.text); s != "" {
				a.finishPlan(tools.PlanVerdict{Feedback: s}, true)
			}
		case "esc":
			p.typing = false // back to the plan, draft kept
		default:
			if !p.input.editKey(key) && text != "" {
				p.input.insert(strings.ReplaceAll(text, "\n", " "))
			}
		}
		return
	}
	switch key {
	case "shift+down":
		p.scroll++
	case "shift+up":
		p.scroll = max(0, p.scroll-1)
	case "enter":
		a.finishPlan(tools.PlanVerdict{Approved: true}, true)
	case "e": // edit: every key, j/k included, types from here on
		p.typing, p.input = true, composerState{}
	case "esc", "q":
		a.finishPlan(tools.PlanVerdict{}, false)
	default:
		if d := scrollDelta(key); d != 0 { // j/k and the page keys scroll the plan
			p.scroll = max(0, p.scroll+d)
		}
	}
}

// planBox draws the plan popup: the plan as scrollable markdown, a counter
// row when it does not fit, the changes input when typing, and the hints.
func (a *App) planBox(p *planAskPrompt, w, chatH int) [][]hostapi.Segment {
	inner := boxInner(w)
	plan := a.host.Markdown(p.plan, inner)
	var input [][]hostapi.Segment
	var hints []string
	if p.typing {
		input = [][]hostapi.Segment{
			{{Text: ""}},
			{{Text: "What should change?", HL: "Host:Hint"}},
			{{Text: ""}},
			p.input.inputRow(inner),
		}
		hints = []string{"enter", "send changes", "esc", "back"}
	} else {
		hints = []string{"enter", "approve", "e", "request changes", "esc", "dismiss"}
	}
	// borders, the two blanks and the hint hold five rows; the input takes the rest
	room := chatH - len(input) - 5
	if len(plan) == 0 {
		plan = [][]hostapi.Segment{[]hostapi.Segment{{Text: "(empty plan)"}}}
	}
	if len(plan) > room && room > 0 {
		room-- // the row that says which lines show
		p.scroll = max(0, min(p.scroll, len(plan)-room))
		more := fmt.Sprintf("  lines %d-%d of %d (j/k, shift+up/down to scroll)", p.scroll+1, p.scroll+room, len(plan))
		plan = append(plan[p.scroll:p.scroll+room], []hostapi.Segment{{Text: more, HL: "Host:Hint"}})
	}
	badge := ""
	if p.mode != "" {
		badge = "back to " + p.mode + " on approve"
	}
	return promptBox(" Plan ", badge, append(append(plan, input...), nil), hintLine(boxInner(w), hints...), w, 100)
}
