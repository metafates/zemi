// question.go is the Go-owned question overlay (DESIGN §6.4, §8): the
// question and plan tools ask one or more questions in turn, each answered
// by picking options or typing an answer on the last row.
package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

type questionPrompt struct {
	qs      []tools.Question
	at      int          // the question shown
	sel     int          // an option row, or len(options) for the typed answer
	picked  map[int]bool // multi-select marks on the shown question
	typing  bool
	input   composerState
	answers []string
	answer  chan qAnswer
}

type qAnswer struct {
	answers []string
	ok      bool
}

// AskQuestion implements hostapi.UIHooks: blocks the tool's goroutine until
// every question is answered (ok) or the user dismisses them.
func (a *App) AskQuestion(qs []tools.Question) ([]string, bool) {
	if len(qs) == 0 {
		return nil, true
	}
	// every string is model input: strip escapes before the chrome
	clean := make([]tools.Question, len(qs))
	for i, q := range qs {
		clean[i] = tools.Question{Question: sanitize.String(q.Question), Multi: q.Multi, Other: sanitize.String(q.Other)}
		for _, o := range q.Options {
			clean[i].Options = append(clean[i].Options, tools.QuestionOption{Label: sanitize.String(o.Label), Description: sanitize.String(o.Description)})
		}
	}
	ch := make(chan qAnswer, 1)
	a.dispatch(uiFunc(func() tea.Cmd {
		a.question = &questionPrompt{qs: clean, picked: map[int]bool{}, typing: len(clean[0].Options) == 0, answer: ch}
		return a.notifyIfAway("zemi has a question: " + clean[0].Question)
	}))
	ans := <-ch
	return ans.answers, ans.ok
}

// finishQuestion closes the overlay and hands the answers, or a dismissal,
// to the asking tool.
func (a *App) finishQuestion(ok bool) {
	qp := a.question
	a.question = nil
	if qp != nil && qp.answer != nil {
		qp.answer <- qAnswer{answers: qp.answers, ok: ok}
	}
	a.frameGen++
}

// questionKey answers the shown question: enter picks the selected option
// (the marked ones in a multi-select) or starts typing on the last row.
func (a *App) questionKey(key, text string) {
	qp := a.question
	q := qp.qs[qp.at]
	defer func() { a.frameGen++ }()
	if qp.typing {
		switch key {
		case "enter":
			if s := strings.TrimSpace(qp.input.text); s != "" {
				a.nextQuestion(s)
			}
		case "esc":
			qp.typing = false
		default:
			if !qp.input.editKey(key) && text != "" {
				qp.input.insert(strings.ReplaceAll(text, "\n", " "))
			}
		}
		return
	}
	if moveSel(&qp.sel, len(q.Options)+1, key) {
		return
	}
	switch {
	case key == "esc":
		a.finishQuestion(false)
	case qp.sel == len(q.Options) && key == "enter":
		qp.typing = true
	case qp.sel == len(q.Options) && text != "" && key != "space":
		qp.typing = true
		qp.input.insert(text)
	case q.Multi && key == "space":
		qp.picked[qp.sel] = !qp.picked[qp.sel]
	case q.Multi && key == "enter":
		var labels []string
		for i, o := range q.Options {
			if qp.picked[i] {
				labels = append(labels, o.Label)
			}
		}
		if len(labels) == 0 {
			labels = []string{q.Options[qp.sel].Label}
		}
		a.nextQuestion(strings.Join(labels, ", "))
	case key == "enter":
		a.nextQuestion(q.Options[qp.sel].Label)
	}
}

// nextQuestion records an answer and shows the next question, or finishes
// after the last one.
func (a *App) nextQuestion(answer string) {
	qp := a.question
	qp.answers = append(qp.answers, answer)
	if qp.at++; qp.at == len(qp.qs) {
		a.finishQuestion(true)
		return
	}
	qp.sel, qp.picked, qp.input = 0, map[int]bool{}, composerState{}
	qp.typing = len(qp.qs[qp.at].Options) == 0
}

func questionBox(qp *questionPrompt, w int) [][]hostapi.Segment {
	q := qp.qs[qp.at]
	var body [][]hostapi.Segment
	for l := range strings.SplitSeq(q.Question, "\n") {
		body = append(body, []hostapi.Segment{{Text: l}})
	}
	body = append(body, []hostapi.Segment{{Text: ""}})
	items := make([]listItem, 0, len(q.Options)+1)
	for i, opt := range q.Options {
		label := opt.Label
		if q.Multi {
			label = map[bool]string{true: "[x] ", false: "[ ] "}[qp.picked[i]] + label
		}
		items = append(items, listItem{label, opt.Description})
	}
	other := q.Other
	if other == "" {
		other = "Type your own answer"
	}
	items = append(items, listItem{other, ""})
	body = append(body, listRows(items, qp.sel, len(items), "Host:Bold")...)
	hints := hintLine("up/down", "move", "enter", "answer", "esc", "dismiss")
	switch {
	case qp.typing:
		body = append(body, padSegs(append([]hostapi.Segment{{Text: "> ", HL: "Comment"}}, cursorSegs([]rune(qp.input.text), qp.input.cursor, "")...), 40))
		hints = hintLine("enter", "send", "esc", "back to options")
	case q.Multi:
		hints = hintLine("up/down", "move", "space", "mark", "enter", "answer", "esc", "dismiss")
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, hints)
	title := " Question "
	if len(qp.qs) > 1 {
		title = fmt.Sprintf(" Question %d/%d ", qp.at+1, len(qp.qs))
	}
	return hostBox(title, "Host:Title", "", body, w)
}
