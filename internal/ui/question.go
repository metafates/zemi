// question.go is the Go-owned question overlay (DESIGN §6.4, §8): the
// question and plan tools ask one or more questions, each answered by
// picking options or typing an answer on the last row. A batch shows the
// questions as tabs plus a review tab, so answers can be changed before
// they are submitted together.
package ui

import (
	"cmp"
	"fmt"
	"slices"
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
	width     int // the widest body so far: the box only grows between tabs
	answer    chan qAnswer
}

// qState is one question's selection, kept while the user moves between tabs.
type qState struct {
	sel      int          // an option row, or len(options) for the typed answer
	picked   map[int]bool // multi-select marks
	typing   bool
	input    composerState
	top      int    // first option row shown
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
		// the marks are the answer, so leaving the tab with tab keeps them
		st.picked[st.sel] = !st.picked[st.sel]
		st.answered = st.marked(q)
	case q.Multi && key == "enter":
		a.answerQuestion(cmp.Or(st.marked(q), q.Options[st.sel].Label))
	case key == "enter":
		a.answerQuestion(q.Options[st.sel].Label)
	}
}

func (st *qState) marked(q tools.Question) string {
	var labels []string
	for i, o := range q.Options {
		if st.picked[i] {
			labels = append(labels, o.Label)
		}
	}
	return strings.Join(labels, ", ")
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

// questionTabs is the tab row of a batch: the questions, a check on the
// answered ones, then the review tab.
func questionTabs(qp *questionPrompt) []hostapi.Segment {
	var row []hostapi.Segment
	for i := range len(qp.qs) + 1 {
		label := "Review"
		if i < len(qp.qs) {
			label = cmp.Or(qp.qs[i].Header, fmt.Sprintf("Q%d", i+1))
			if qp.states[i].answered != "" {
				label = "✓ " + label
			}
		}
		if i > 0 {
			row = append(row, hostapi.Segment{Text: "  ·  ", HL: "Host:Hint"})
		}
		row = append(row, hostapi.Segment{Text: label, HL: map[bool]string{true: "Host:Title", false: "Host:Hint"}[i == qp.at]})
	}
	return row
}

func questionBox(qp *questionPrompt, w, chatH int) [][]hostapi.Segment {
	blank := []hostapi.Segment{{Text: ""}}
	var body [][]hostapi.Segment
	title := " Question "
	tabs := []string{}
	if len(qp.qs) > 1 {
		title = " Questions "
		body = append(body, questionTabs(qp), blank)
		tabs = []string{"tab", "next"}
	}
	box := func() [][]hostapi.Segment {
		for _, r := range body {
			qp.width = max(qp.width, segsWidth(r))
		}
		body[0] = padSegs(body[0], min(boxInner(w), max(qp.width, 56)))
		return hostBox(title, "Host:Title", "", body, w)
	}
	if qp.reviewing() {
		items := make([]listItem, 0, len(qp.qs)+1)
		for i, q := range qp.qs {
			ans := hostapi.Segment{Text: "  " + strings.ReplaceAll(qp.states[i].answered, " (Recommended)", ""), HL: "Host:Value"}
			if qp.states[i].answered == "" {
				ans = hostapi.Segment{Text: "  not answered", HL: "Host:Warn"}
			}
			items = append(items, listItem{label: strings.ReplaceAll(q.Question, "\n", " "), more: [][]hostapi.Segment{{ans}}})
		}
		// borders, the blank row and the hint take four rows; each answer
		// takes three with its gap, the submit row one
		limit := chatH - len(body) - 4
		if len(items)*3+1 > limit {
			limit = max(limit-1, 3) // the i/n counter row
		}
		body = append(body, listRows(append(items, listItem{label: "Submit answers"}), qp.reviewSel, nil, limit)...)
		enter := map[bool]string{true: "submit", false: "change"}[qp.reviewSel == len(qp.qs)]
		body = append(body, blank, hintLine(boxInner(w), "enter", enter, "shift+tab", "back", "esc", "cancel"))
		return box()
	}

	q, st := qp.qs[qp.at], &qp.states[qp.at]
	items := make([]listItem, 0, len(q.Options)+1)
	for i, opt := range q.Options {
		it := listItem{label: opt.Label, desc: opt.Description}
		// the model marks its pick in the label; the tool strips it from the answer
		if l, ok := strings.CutSuffix(it.label, " (Recommended)"); ok {
			it.label, it.desc = l, strings.TrimSuffix("recommended · "+it.desc, " · ")
		}
		if q.Multi {
			it.label = map[bool]string{true: "[x] ", false: "[ ] "}[st.picked[i]] + it.label
		}
		items = append(items, it)
	}
	items = append(items, listItem{label: cmp.Or(q.Other, "Type your own answer")})
	// wrapped: an ellipsis would hide what is asked, unless the question
	// would push the first options and the hint off screen
	var ask [][]hostapi.Segment
	for l := range strings.SplitSeq(q.Question, "\n") {
		ask = append(ask, hostapi.WrapSegs([]hostapi.Segment{{Text: l}}, boxInner(w))...)
	}
	// borders, two blank rows, the hint and three options with their i/n row
	keep := chatH - len(body) - 6 - min(len(items), 4)
	if st.typing {
		keep--
	}
	body = append(append(body, capRows(ask, max(keep, 1), boxInner(w))...), blank)
	// borders, the blank row and the hint take four rows, typing one more
	limit := chatH - len(body) - 4
	if st.typing {
		limit--
	}
	if len(items) > limit {
		limit = max(limit-1, 3) // the i/n counter row
	}
	body = append(body, listRows(items, st.sel, &st.top, limit)...)
	var hints []string
	switch {
	case st.typing:
		body = append(body, st.input.inputRow(boxInner(w)))
		hints = []string{"enter", "answer", "esc", "back"}
	case q.Multi:
		hints = slices.Concat([]string{"space", "mark", "enter", "answer"}, tabs, []string{"esc", "cancel"})
	default:
		hints = slices.Concat([]string{"enter", "answer"}, tabs, []string{"esc", "cancel"})
	}
	body = append(body, blank, hintLine(boxInner(w), hints...))
	return box()
}
