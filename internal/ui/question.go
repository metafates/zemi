// question.go is the Go-owned question overlay (DESIGN §6.4, §8): the
// question tool asks the user a structured question with fixed options;
// the chrome is host drawn, the answer is the chosen option label.
package ui

import (
	"strings"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/tools"
)

type questionPrompt struct {
	q      tools.Question
	sel    int
	answer chan qAnswer
}

type qAnswer struct {
	label string
	ok    bool
}

// AskQuestion implements hostapi.UIHooks: blocks the tool's goroutine until
// the user answers; esc/ctrl+c decline (ok=false).
func (a *App) AskQuestion(q tools.Question) (string, bool) {
	ch := make(chan qAnswer, 1)
	a.dispatch(questionMsg{q: q, answer: ch})
	ans := <-ch
	return ans.label, ans.ok
}

func (a *App) answerQuestion(label string, ok bool) {
	qp := a.question
	a.question = nil
	if qp != nil && qp.answer != nil {
		qp.answer <- qAnswer{label: label, ok: ok}
	}
	a.frameGen++
}

// spliceQuestion composes the question overlay over the chat area.
func (a *App) spliceQuestion(rows [][]hostapi.Segment, qp *questionPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	for _, l := range strings.Split(qp.q.Question, "\n") {
		body = append(body, []hostapi.Segment{{Text: l}})
	}
	if qp.q.Description != "" {
		for _, l := range strings.Split(qp.q.Description, "\n") {
			body = append(body, []hostapi.Segment{{Text: l, HL: "Comment"}})
		}
	}
	body = append(body, []hostapi.Segment{{Text: ""}})
	for i, opt := range qp.q.Options {
		cur, hl := "  ", ""
		if i == qp.sel {
			cur, hl = "> ", "Host:Bold"
		}
		body = append(body, []hostapi.Segment{
			{Text: cur, HL: "Host:Bold"},
			{Text: opt.Label, HL: hl},
		})
		if opt.Description != "" {
			body = append(body, []hostapi.Segment{
				{Text: "    "},
				{Text: opt.Description, HL: "Comment"},
			})
		}
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, keyHints("up/down", "move", "enter", "answer", "esc", "decline"))
	box := hostBox(" Question ", "Host:QTitle", "", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}
