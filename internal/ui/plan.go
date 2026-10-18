// plan.go is the Go-owned plan approval overlay (DESIGN §6.4, §8): the plan
// tool shows its plan as a scrollable markdown "Plan" popup the user approves
// (a), asks to change (e, then type), or dismisses (esc). The agent to return to
// on approval is remembered when the read-only agent started, so approval
// never asks which agent or profile.

package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

type planAskPrompt struct {
	plan    string
	view    viewport
	sel     int // the highlighted button: approve, then request changes
	typing  bool
	input   composerState
	profile string // the profile the "back to <profile> on approve" badge names
	size    boxSize
	text    textRows
	answer  chan planAnswer
}

type planAnswer struct {
	v  tools.PlanVerdict
	ok bool
}

// choices is the plan's buttons: the rows, the keys and the hit test all read
// them, and a button's key is what it does (planKey).
func (p *planAskPrompt) choices() []promptChoice {
	return []promptChoice{
		{key: "a", label: "Approve"},
		{key: "s", label: "Save, don't start"},
		{key: "e", label: "Request changes"},
	}
}

// AskPlan implements hostapi.UIHooks. It blocks the caller (the plan tool on
// the turn goroutine) until the user approves, types changes, or dismisses.
func (a *App) AskPlan(plan, implementProfile string) (tools.PlanVerdict, bool) {
	ch := make(chan planAnswer, 1)
	a.await(func() tea.Cmd {
		a.planAsk = &planAskPrompt{
			plan:    sanitize.String(plan),
			profile: implementProfile,
			answer:  ch,
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
	choices := p.choices()
	defer a.bump()
	if !(p.typing && lineKey(key)) && scrollKey(&p.view, key, false) || !p.typing && moveSel(&p.sel, len(choices), len(choices), key) {
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
		key = choices[p.sel].key
	}
	switch key {
	case "a":
		a.finishPlan(tools.PlanVerdict{Approved: true}, true)
	case "s": // the same approval, held: the file is saved, the work waits
		a.finishPlan(tools.PlanVerdict{Approved: true, Hold: true}, true)
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
	act := choiceRows(p.choices(), p.sel, inner, true)
	hints := []hintPair{pickHint, {"esc", "dismiss"}}
	if p.typing {
		act = [][]hostapi.Segment{{{Text: "What should change?", HL: hostapi.ChromeHint}}, p.input.inputRow(inner)}
		hints = []hintPair{sendHint, {"esc", "back"}}
	}
	// the full width a modal may take: the plan wraps there once, and typing
	// never widens the box
	p.size.w = max(p.size.w, boxInner(w))
	planW := min(inner, p.size.w)
	md := p.text.at(func(w int) [][]hostapi.Segment {
		if rows := a.host.Markdown(p.plan, w); len(rows) > 0 {
			return rows
		}
		return emptyRows("The plan is empty.", w)
	})
	body := scrollChoiceBody(h, inner, nil, &p.view, md, planW, func(bool) [][]hostapi.Segment { return act }, hints...)
	badge := ""
	if p.profile != "" {
		badge = "back to " + p.profile + " on approve"
	}
	return promptBox("Plan", badge, body, w, h, 100, &p.size)
}
