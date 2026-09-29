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
	"strconv"
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
	if !(st.typing && typedKey(key)) && st.ask.overflows() && scrollKey(&st.ask, key, false) {
		return
	}
	if st.typing {
		switch key {
		case "enter":
			if s := strings.TrimSpace(st.input.text); s != "" {
				a.answerQuestion(s)
			}
		case "esc":
			if len(q.Options) == 0 { // typing is all an open question does
				a.finishQuestion(false)
			}
			st.typing = false
		default:
			st.input.typeKey(key, text)
		}
		return
	}
	hasTail := q.Type == tools.TypeSingle || q.Type == tools.TypeMulti
	if moveSel(&st.sel, q.ListRows(), st.view.pageItems(q.ListRows()), key) {
		return
	}
	// a row's key picks it directly; yes_no and single answer at once, and
	// single's last digit starts the typed answer. On the typed answer row a
	// digit is its first character instead.
	n := -1
	for i := range q.ListRows() {
		if text != "" && optionKey(q, i) == text {
			n = i
		}
	}
	if n >= 0 && (n < len(q.Options) || !q.Multi()) && st.sel != len(q.Options) {
		st.sel = n
		switch {
		case n == len(q.Options):
			st.typing = true
		case !q.Multi():
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
	case key == "enter" && st.sel < len(q.Options):
		ans := q.Options[st.sel].Label
		if q.Multi() {
			ans = cmp.Or(st.marked(q), ans)
		}
		a.answerQuestion(ans)
	}
}

// optionKey is the key that picks row i: y and n on a yes/no question, as
// every prompt answers yes and no, else its digit, "" past the ninth.
func optionKey(q tools.Question, i int) string {
	switch {
	case q.Type == tools.TypeYesNo && i < 2:
		return []string{"y", "n"}[i]
	case i < 9:
		return strconv.Itoa(i + 1)
	}
	return ""
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

// questionTabs is the tab row of a batch: the questions, each marked
// waiting or answered in a cell every tab keeps so a mark never shifts the
// labels after it, then the review tab.
func questionTabs(qp *questionPrompt) []hostapi.Segment {
	var row []hostapi.Segment
	for i := range len(qp.qs) + 1 {
		if i > 0 {
			row = append(row, hostapi.Segment{Text: "   "})
		}
		label := "Review"
		if i < len(qp.qs) {
			label = cmp.Or(qp.qs[i].Header, fmt.Sprintf("Q%d", i+1))
			mark := hostapi.Segment{Text: "· ", HL: hostapi.ChromeHint}
			if qp.states[i].answered != "" {
				mark = hostapi.Segment{Text: "✓ ", HL: hostapi.ChromeOk}
			}
			row = append(row, mark)
		}
		row = append(row, hostapi.Segment{Text: label, HL: map[bool]string{true: hostapi.ChromeTitle, false: hostapi.ChromeHint}[i == qp.at]})
	}
	return row
}

// questionBox draws the shown tab. The box starts at the size of the
// largest question tab, typing included, so switching tabs or starting an
// answer never resizes or moves it; the review list scrolls within it.
func (a *App) questionBox(qp *questionPrompt, w, h int) [][]hostapi.Segment {
	title := "Question"
	if len(qp.qs) > 1 {
		title = "Questions"
	}
	if qp.size.h == 0 {
		at := qp.at
		for qp.at = range qp.qs {
			st := &qp.states[qp.at]
			typing := st.typing
			for _, st.typing = range []bool{false, qp.qs[qp.at].Type != tools.TypeYesNo} {
				promptBox(title, "", a.questionRows(qp, w, h), w, h, 100, &qp.size)
			}
			st.typing = typing
		}
		if qp.at = len(qp.qs); len(qp.qs) > 1 {
			promptBox(title, "", a.questionRows(qp, w, min(h, qp.size.h)), w, h, 100, &qp.size)
		}
		qp.at = at
	}
	if qp.reviewing() {
		h = min(h, qp.size.h)
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
	qp.size.w = max(qp.size.w, listBoxW)
	if qp.reviewing() {
		items := make([]listItem, 0, len(qp.qs)+1)
		for i, q := range qp.qs {
			ans := hostapi.Segment{Text: "  " + strings.ReplaceAll(qp.states[i].answered, " (Recommended)", ""), HL: hostapi.ChromeValue}
			if qp.states[i].answered == "" {
				ans = hostapi.Segment{Text: "  not answered", HL: hostapi.ChromeWarn}
			}
			items = append(items, listItem{label: strings.ReplaceAll(q.Question, "\n", " "), more: [][]hostapi.Segment{{ans}}})
		}
		items = append(items, listItem{label: "Submit answers"})
		enter := map[bool]hintPair{true: sendHint, false: {"enter", "open"}}[qp.reviewSel == len(qp.qs)]
		return fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
			tight := level > 0
			room := h - 2 - len(stack(tight, tabs, oneRow, oneRow)) + 1
			list := listRows(items, qp.reviewSel, &qp.review, max(room, 1), inner)
			hintRow := hint(inner, slices.Concat(moves, []hintPair{enter}, scrolls(qp.review), []hintPair{{"esc", "dismiss"}})...)
			return stack(tight, tabs, list, [][]hostapi.Segment{hintRow}), qp.review.viewH < min(qp.review.rows, 3)
		})
	}

	q, st := qp.qs[qp.at], &qp.states[qp.at]
	// a multi-select is a list of switches, the other
	// types buttons keyed by their digit
	var items []listItem
	var choices []promptChoice
	if q.Type != tools.TypeFreeText {
		for i, opt := range q.Options {
			label, desc := opt.Label, opt.Description
			// the model marks its pick in the label; the tool strips it from the answer
			if l, ok := strings.CutSuffix(label, " (Recommended)"); ok {
				label, desc = l, strings.TrimSuffix("recommended · "+desc, " · ")
			}
			items = append(items, listItem{label: label, desc: desc, key: switchSeg(st.picked[i], false)})
			choices = append(choices, promptChoice{key: optionKey(q, i), label: label, desc: desc})
		}
		if q.Type != tools.TypeYesNo {
			items = append(items, listItem{label: "Type your own answer"})
			choices = append(choices, promptChoice{key: optionKey(q, len(q.Options)), label: "Type your own answer"})
		}
	}
	askText := st.text.at(func(w int) [][]hostapi.Segment {
		ask := hostapi.WrapText(q.Question, w)
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
		hints = slices.Concat(moves, []hintPair{sendHint, {"esc", map[bool]string{true: "dismiss", false: "back"}[len(q.Options) == 0]}})
	case q.Multi():
		hints = slices.Concat(moves, []hintPair{{"space", "toggle"}, sendHint, {"esc", "dismiss"}})
	default:
		hints = slices.Concat(moves, []hintPair{pickHint, {"esc", "dismiss"}})
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
		switch {
		case q.Multi():
			opts = listRowsW(items, items, st.sel, &st.view, max(room-len(ask), 1), qp.size.w, inner)
		case len(choices) > 0:
			st.view.viewH = max(room-len(ask), 1)
			opts = choiceView(choices, st.sel, &st.view, inner, level > 1)
		}
		var views []viewport
		if !st.typing { // the typed line keeps ctrl+u, so the footer names no scroll keys
			views = []viewport{st.ask, st.view}
		}
		body := stack(tight, tabs, ask, slices.Concat(opts, input), [][]hostapi.Segment{hint(inner, slices.Concat(hints, scrolls(views...))...)})
		return body, st.ask.viewH < min(askN, 3) || len(items) > 0 && st.view.viewH < min(st.view.rows, 3)
	})
}
