// question.go is the Go-owned question overlay (DESIGN §6.4, §8): the
// question and plan tools ask one or more questions, each answered by
// picking options or typing an answer on the last row. A batch shows the
// questions as tabs plus a review tab, so answers can be changed before
// they are submitted together.
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
	qs        []tools.Question
	states    []qState
	at        int // the tab shown: a question, or len(qs) for the review tab
	reviewSel int // a question row, or len(qs) for submit
	answer    chan qAnswer
}

// qState is one question's selection, kept while the user moves between tabs.
type qState struct {
	sel      int          // an option row, or len(options) for the typed answer
	picked   map[int]bool // multi-select marks
	typing   bool
	input    composerState
	answered string // "" until answered
}

type qAnswer struct {
	answers []string
	ok      bool
}

// AskQuestion implements hostapi.UIHooks: blocks the tool's goroutine until
// the answers are submitted (ok) or the user dismisses them. A question
// left unanswered in a submitted batch comes back as "".
func (a *App) AskQuestion(qs []tools.Question) ([]string, bool) {
	if len(qs) == 0 {
		return nil, true
	}
	// every string is model input: strip escapes before the chrome
	clean := make([]tools.Question, len(qs))
	states := make([]qState, len(qs))
	for i, q := range qs {
		clean[i] = tools.Question{Question: sanitize.String(q.Question), Header: sanitize.String(q.Header), Multi: q.Multi, Other: sanitize.String(q.Other)}
		for _, o := range q.Options {
			clean[i].Options = append(clean[i].Options, tools.QuestionOption{Label: sanitize.String(o.Label), Description: sanitize.String(o.Description)})
		}
		states[i] = qState{picked: map[int]bool{}, typing: len(q.Options) == 0}
	}
	ch := make(chan qAnswer, 1)
	a.await(func() tea.Cmd {
		a.question = &questionPrompt{qs: clean, states: states, reviewSel: len(clean), answer: ch}
		return a.notifyIfAway("zemi has a question: " + clean[0].Question)
	})
	ans := <-ch
	return ans.answers, ans.ok
}

// finishQuestion closes the overlay and hands the answers, or a dismissal,
// to the asking tool.
func (a *App) finishQuestion(ok bool) {
	qp := a.question
	a.question = nil
	if qp != nil && qp.answer != nil {
		ans := qAnswer{ok: ok}
		for _, st := range qp.states {
			if ok {
				ans.answers = append(ans.answers, st.answered)
			}
		}
		qp.answer <- ans
	}
	a.frameGen++
}

func (qp *questionPrompt) reviewing() bool { return qp.at == len(qp.qs) }

// sel is the moving selection of the shown tab and its row count.
func (qp *questionPrompt) sel() (*int, int) {
	if qp.reviewing() {
		return &qp.reviewSel, len(qp.qs) + 1
	}
	return &qp.states[qp.at].sel, len(qp.qs[qp.at].Options) + 1
}

// switchTab moves between the questions and, in a batch, the review tab.
func (qp *questionPrompt) switchTab(d int) {
	last := len(qp.qs) - 1
	if len(qp.qs) > 1 {
		last++
	}
	qp.at = max(0, min(qp.at+d, last))
}

// questionKey handles a key on the shown tab: enter answers the question
// (the marked options in a multi-select) or starts typing on the last row.
func (a *App) questionKey(key, text string) {
	qp := a.question
	defer func() { a.frameGen++ }()
	switch key {
	case "tab":
		qp.switchTab(1)
		return
	case "shift+tab":
		qp.switchTab(-1)
		return
	}
	if qp.reviewing() {
		a.reviewKey(key)
		return
	}
	q, st := qp.qs[qp.at], &qp.states[qp.at]
	if st.typing {
		switch key {
		case "enter":
			if s := strings.TrimSpace(st.input.text); s != "" {
				a.answerQuestion(s)
			}
		case "esc":
			st.typing = false
		default:
			if !st.input.editKey(key) && text != "" {
				st.input.insert(strings.ReplaceAll(text, "\n", " "))
			}
		}
		return
	}
	if moveSel(&st.sel, len(q.Options)+1, key) {
		return
	}
	switch {
	case key == "esc":
		a.finishQuestion(false)
	case key == "left":
		qp.switchTab(-1)
	case key == "right":
		qp.switchTab(1)
	case st.sel == len(q.Options) && key == "enter":
		st.typing = true
	case st.sel == len(q.Options) && text != "" && key != "space":
		st.typing = true
		st.input.insert(text)
	case q.Multi && key == "space":
		st.picked[st.sel] = !st.picked[st.sel]
	case q.Multi && key == "enter":
		var labels []string
		for i, o := range q.Options {
			if st.picked[i] {
				labels = append(labels, o.Label)
			}
		}
		if len(labels) == 0 {
			labels = []string{q.Options[st.sel].Label}
		}
		a.answerQuestion(strings.Join(labels, ", "))
	case key == "enter":
		a.answerQuestion(q.Options[st.sel].Label)
	}
}

func (a *App) reviewKey(key string) {
	qp := a.question
	if moveSel(&qp.reviewSel, len(qp.qs)+1, key) {
		return
	}
	switch key {
	case "esc":
		a.finishQuestion(false)
	case "left":
		qp.switchTab(-1)
	case "enter":
		if qp.reviewSel == len(qp.qs) {
			a.finishQuestion(true)
		} else {
			qp.at = qp.reviewSel
		}
	}
}

// answerQuestion records the shown question's answer. A single question
// submits at once; a batch moves to the next unanswered question after it,
// or to the review tab.
func (a *App) answerQuestion(answer string) {
	qp := a.question
	qp.states[qp.at].answered = answer
	if len(qp.qs) == 1 {
		a.finishQuestion(true)
		return
	}
	for n := qp.at + 1; n < len(qp.qs); n++ {
		if qp.states[n].answered == "" {
			qp.at = n
			return
		}
	}
	qp.at, qp.reviewSel = len(qp.qs), len(qp.qs)
}

// questionTabs is the tab row of a batch: a check on answered questions,
// then the review tab.
func questionTabs(qp *questionPrompt) []hostapi.Segment {
	var row []hostapi.Segment
	for i := range len(qp.qs) + 1 {
		label := "Submit"
		if i < len(qp.qs) {
			label = qp.qs[i].Header
			if label == "" {
				label = fmt.Sprintf("Q%d", i+1)
			}
			label = map[bool]string{true: "✓ ", false: "○ "}[qp.states[i].answered != ""] + label
		}
		if i > 0 {
			row = append(row, hostapi.Segment{Text: " "})
		}
		hl := map[bool]string{true: "Host:BtnSel", false: "Host:Btn"}[i == qp.at]
		row = append(row, hostapi.Segment{Text: " " + label + " ", HL: hl})
	}
	return row
}

func questionBox(qp *questionPrompt, w int) [][]hostapi.Segment {
	blank := []hostapi.Segment{{Text: ""}}
	var body [][]hostapi.Segment
	title := " Question "
	if len(qp.qs) > 1 {
		title = " Questions "
		body = append(body, questionTabs(qp), blank)
	}
	if qp.reviewing() {
		body = append(body, []hostapi.Segment{{Text: "Review your answers", HL: "Host:Bold"}}, blank)
		for i, q := range qp.qs {
			marker, hl := "  ", "Normal"
			if i == qp.reviewSel {
				marker, hl = "› ", "Host:Bold"
			}
			body = append(body, []hostapi.Segment{{Text: marker + strings.ReplaceAll(q.Question, "\n", " "), HL: hl}})
			if ans := qp.states[i].answered; ans != "" {
				body = append(body, []hostapi.Segment{{Text: "    " + ans, HL: "Host:Value"}})
			} else {
				body = append(body, []hostapi.Segment{{Text: "    not answered", HL: "Host:Warn"}})
			}
		}
		submit := hostapi.Segment{Text: " Submit answers ", HL: "Host:Btn"}
		if qp.reviewSel == len(qp.qs) {
			submit.HL = "Host:BtnSel"
		}
		body = append(body, blank, []hostapi.Segment{submit}, blank,
			hintLine("↑↓", "move", "enter", "submit or edit", "←", "back", "esc", "dismiss"))
		return hostBox(title, "Host:Title", "", body, w)
	}

	q, st := qp.qs[qp.at], &qp.states[qp.at]
	for l := range strings.SplitSeq(q.Question, "\n") {
		body = append(body, []hostapi.Segment{{Text: l}})
	}
	body = append(body, blank)
	items := make([]listItem, 0, len(q.Options)+1)
	for i, opt := range q.Options {
		label := opt.Label
		if q.Multi {
			label = map[bool]string{true: "[x] ", false: "[ ] "}[st.picked[i]] + label
		}
		items = append(items, listItem{label, opt.Description})
	}
	other := q.Other
	if other == "" {
		other = "Type your own answer"
	}
	items = append(items, listItem{other, ""})
	body = append(body, listRows(items, st.sel, len(items), "Host:Bold")...)
	var hints []string
	switch {
	case st.typing:
		body = append(body, padSegs(append([]hostapi.Segment{{Text: "> ", HL: "Comment"}}, cursorSegs([]rune(st.input.text), st.input.cursor, "")...), 40))
		hints = []string{"enter", "answer", "esc", "back to options"}
	case q.Multi:
		hints = []string{"↑↓", "move", "space", "mark", "enter", "answer", "esc", "dismiss"}
	default:
		hints = []string{"↑↓", "move", "enter", "answer", "esc", "dismiss"}
	}
	if len(qp.qs) > 1 {
		hints = append(hints, "tab", "next")
	}
	body = append(body, blank, hintLine(hints...))
	return hostBox(title, "Host:Title", "", body, w)
}
