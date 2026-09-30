// plan.go is the Go-owned plan approval overlay (DESIGN §6.4, §8): the plan
// tool shows its plan as a scrollable markdown "Plan" popup the user approves
// (a), asks to change (e, then type), or dismisses (esc). The agent to return to
// on approval is remembered when the read-only agent started, so approval
// never asks which agent or mode.

package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

type planAskPrompt struct {
	plan   string
	view   viewport
	sel    int // the highlighted button: approve, then request changes
	typing bool
	input  composerState
	mode   string // "in <mode> mode" when a mode switch follows approval
	size   boxSize
	text   textRows
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
	a.bump()
}

// planKey handles a key on the plan popup: scrolling, the buttons, or
// dismiss. Only in the changes input do keys type.
func (a *App) planKey(key, text string) {
	p := a.planAsk
	defer a.bump()
	if !(p.typing && typedKey(key)) && scrollKey(&p.view, key, false) || !p.typing && moveSel(&p.sel, 2, 2, key) {
		return
	}
	if p.typing {
		switch key {
		case "enter":
			if s := strings.TrimSpace(p.input.text); s != "" {
				a.finishPlan(tools.PlanVerdict{Feedback: s}, true)
			}
		case "esc":
			p.typing = false // back to the plan, draft kept
		default:
			p.input.typeKey(key, text)
		}
		return
	}
	if key == "enter" {
		key = []string{"a", "e"}[p.sel]
	}
	switch key {
	case "a":
		a.finishPlan(tools.PlanVerdict{Approved: true}, true)
	case "e": // edit: every key, j/k included, types from here on
		p.typing = true
	case "esc":
		a.finishPlan(tools.PlanVerdict{}, false)
	}
}

// planBox draws the plan popup: the plan as scrollable markdown, then its
// two buttons, which the changes input replaces row for row, so e moves
// nothing.
func (a *App) planBox(p *planAskPrompt, w, h int) [][]hostapi.Segment {
	inner := boxInner(w)
	act := choiceRows([]promptChoice{{key: "a", label: "Approve"}, {key: "e", label: "Request changes"}}, p.sel, inner, true)
	hints := []hintPair{pickHint, {"esc", "dismiss"}}
	if p.typing {
		act = [][]hostapi.Segment{{{Text: "What should change?", HL: hostapi.ChromeHint}}, p.input.inputRow(inner)}
		hints = []hintPair{sendHint, {"esc", "back"}}
	}
	p.size.w = max(p.size.w, textBoxW)
	planW := min(inner, p.size.w)
	md := p.text.at(func(w int) [][]hostapi.Segment {
		if rows := a.host.Markdown(p.plan, w); len(rows) > 0 {
			return rows
		}
		return emptyRows("The plan is empty.", w)
	})
	body := scrollChoiceBody(h, inner, nil, &p.view, md, planW, func(bool) [][]hostapi.Segment { return act }, p.typing, hints...)
	badge := ""
	if p.mode != "" {
		badge = "back to " + p.mode + " on approve"
	}
	return promptBox("Plan", badge, body, w, h, 100, &p.size)
}
