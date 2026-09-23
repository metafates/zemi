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
	review    viewport
	size      boxSize
	answer    chan qAnswer
}

// qState is one question's selection, kept while the user moves between tabs.
type qState struct {
	sel      int          // an option row, or len(options) for the typed answer
	picked   map[int]bool // multi-select marks
	typing   bool
	input    composerState
	view     viewport // the options
	ask      viewport // the question text
	text     textRows
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
		clean[i] = tools.Question{Type: q.Type, Question: sanitize.String(q.Question), Description: sanitize.String(q.Description), Header: sanitize.String(q.Header)}
		for _, o := range q.Options {
			clean[i].Options = append(clean[i].Options, tools.QuestionOption{Label: sanitize.String(o.Label), Description: sanitize.String(o.Description)})
		}
		states[i] = qState{picked: map[int]bool{}, typing: q.Type == tools.TypeFreeText || len(clean[i].Options) == 0}
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
	a.bump()
}

func (qp *questionPrompt) reviewing() bool { return qp.at == len(qp.qs) }

// sel is the moving selection of the shown tab and its row count.
func (qp *questionPrompt) sel() (*int, int) {
	if qp.reviewing() {
		return &qp.reviewSel, len(qp.qs) + 1
	}
	return &qp.states[qp.at].sel, qp.qs[qp.at].ListRows()
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
	defer func() { a.bump() }()
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
	if st.ask.overflows() && scrollKey(&st.ask, key, false) {
		return
	}
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
	hasTail := q.Type == tools.TypeSingle || q.Type == tools.TypeMulti
	if moveSel(&st.sel, q.ListRows(), st.view.pageItems(q.ListRows()), key) {
		return
	}
	// a digit picks the option directly; yes_no and single answer at once
	if n := optionDigit(text); n >= 0 && n < len(q.Options) {
		st.sel = n
		if !q.Multi() {
			a.answerQuestion(q.Options[n].Label)
		}
		return
	}
	switch {
	case key == "esc":
		a.finishQuestion(false)
	case key == "left":
		qp.switchTab(-1)
	case key == "right":
		qp.switchTab(1)
	case hasTail && st.sel == len(q.Options) && key == "enter":
		st.typing = true
	case hasTail && st.sel == len(q.Options) && text != "" && key != "space":
		st.typing = true
		st.input.insert(text)
	case q.Multi() && key == "space":
		// the marks are the answer, so leaving the tab with tab keeps them
		st.picked[st.sel] = !st.picked[st.sel]
		st.answered = st.marked(q)
	case q.Multi() && key == "enter":
		a.answerQuestion(cmp.Or(st.marked(q), q.Options[st.sel].Label))
	case key == "enter":
		a.answerQuestion(q.Options[st.sel].Label)
	}
}

// optionDigit maps a "1".."9" key to its zero-based option index, or -1.
func optionDigit(text string) int {
	if len(text) != 1 {
		return -1
	}
	if text[0] >= '1' && text[0] <= '9' {
		return int(text[0] - '1')
	}
	return -1
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
	if moveSel(&qp.reviewSel, len(qp.qs)+1, qp.review.pageItems(len(qp.qs)+1), key) {
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

// questionBox draws the shown tab. The box starts at the size of the
// largest tab, so switching tabs never resizes or moves it.
func (a *App) questionBox(qp *questionPrompt, w, h int) [][]hostapi.Segment {
	title := "Question"
	if len(qp.qs) > 1 {
		title = "Questions"
	}
	if qp.size.h == 0 {
		at := qp.at
		for qp.at = range len(qp.qs) + map[bool]int{true: 1}[len(qp.qs) > 1] {
			promptBox(title, "", a.questionRows(qp, w, h), w, h, 100, &qp.size)
		}
		qp.at = at
	}
	return promptBox(title, "", a.questionRows(qp, w, h), w, h, 100, &qp.size)
}

func (a *App) questionRows(qp *questionPrompt, w, h int) [][]hostapi.Segment {
	inner := boxInner(w)
	var tabs [][]hostapi.Segment
	var moves []hintPair
	if len(qp.qs) > 1 {
		tabs = [][]hostapi.Segment{questionTabs(qp)}
		moves = []hintPair{{"tab", "next"}}
		if qp.at == len(qp.qs) {
			moves = []hintPair{{"shift+tab", "previous"}}
		}
	}
	qp.size.w = max(qp.size.w, 56)
	if qp.reviewing() {
		items := make([]listItem, 0, len(qp.qs)+1)
		for i, q := range qp.qs {
			ans := hostapi.Segment{Text: "  " + strings.ReplaceAll(qp.states[i].answered, " (Recommended)", ""), HL: "Host:Value"}
			if qp.states[i].answered == "" {
				ans = hostapi.Segment{Text: "  not answered", HL: "Host:Warn"}
			}
			items = append(items, listItem{label: strings.ReplaceAll(q.Question, "\n", " "), more: [][]hostapi.Segment{{ans}}})
		}
		items = append(items, listItem{label: "Submit answers"})
		enter := map[bool]string{true: "submit", false: "change"}[qp.reviewSel == len(qp.qs)]
		return fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
			tight := level > 0
			room := h - 2 - len(stack(tight, tabs, oneRow, oneRow)) + 1
			list := listRows(items, qp.reviewSel, &qp.review, max(room, 1), inner)
			hintRow := hint(inner, slices.Concat(moves, []hintPair{{"enter", enter}}, scrolls(qp.review), []hintPair{{"esc", "dismiss"}})...)
			return stack(tight, tabs, list, [][]hostapi.Segment{hintRow}), qp.review.viewH < min(qp.review.rows, 3)
		})
	}

	q, st := qp.qs[qp.at], &qp.states[qp.at]
	var items []listItem
	if q.Type != tools.TypeFreeText {
		items = make([]listItem, 0, len(q.Options)+1)
		for i, opt := range q.Options {
			it := listItem{label: opt.Label, desc: opt.Description}
			// the model marks its pick in the label; the tool strips it from the answer
			if l, ok := strings.CutSuffix(it.label, " (Recommended)"); ok {
				it.label, it.desc = l, strings.TrimSuffix("recommended · "+it.desc, " · ")
			}
			if q.Multi() && st.picked[i] {
				it.key = "on"
			}
			items = append(items, it)
		}
		if q.Type != tools.TypeYesNo {
			items = append(items, listItem{label: "Type your own answer"})
		}
	}
	askText := st.text.at(func(w int) [][]hostapi.Segment {
		var ask [][]hostapi.Segment
		for l := range strings.SplitSeq(q.Question, "\n") {
			ask = append(ask, hostapi.WrapSegs([]hostapi.Segment{{Text: l}}, w)...)
		}
		if q.Description != "" {
			ask = append(ask, nil)
			ask = append(ask, a.host.Markdown(q.Description, w)...)
		}
		return ask
	})
	askN := len(askText(inner))
	var hints []hintPair
	switch {
	case st.typing:
		hints = []hintPair{{"enter", "answer"}, {"esc", "back"}}
	case q.Multi():
		hints = slices.Concat(moves, []hintPair{{"space", "toggle"}, {"enter", "answer"}, {"esc", "dismiss"}})
	default:
		hints = slices.Concat(moves, []hintPair{{fmt.Sprintf("1-%d", min(len(q.Options), 9)), "pick"}, {"enter", "answer"}, {"esc", "dismiss"}})
	}
	return fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		var input [][]hostapi.Segment
		if st.typing {
			input = [][]hostapi.Segment{st.input.inputRow(inner)}
		}
		optsPH := oneRow[:min(len(items), 1)]
		// what is asked scrolls when it would push the first options off
		// screen; the rest goes to the options
		room := h - 2 - len(stack(tight, tabs, oneRow, slices.Concat(optsPH, input), oneRow)) + 1 + len(optsPH)
		st.ask.viewH = max(room-min(len(items), 4), 1)
		ask := st.ask.window(st.ask.wrap(askText, inner), inner)
		var opts [][]hostapi.Segment
		if len(items) > 0 {
			opts = listRows(items, st.sel, &st.view, max(room-len(ask), 1), inner)
		}
		views := []viewport{st.ask}
		if !st.typing { // typing, the page keys scroll only the question
			views = append(views, st.view)
		}
		body := stack(tight, tabs, ask, slices.Concat(opts, input), [][]hostapi.Segment{hint(inner, slices.Concat(hints, scrolls(views...))...)})
		return body, st.ask.viewH < min(askN, 3) || len(items) > 0 && st.view.viewH < min(st.view.rows, 3)
	})
}
