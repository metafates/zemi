// question.go is the Go-owned question overlay (DESIGN §6.4, §8): the
// question tool asks one or more questions, each answered by picking
// options or typing an answer on the last row, with a note field under the
// options. A batch shows the questions as tabs plus a Submit tab, so
// answers can be changed before they are submitted together.

package ui

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/tools"
)

type questionPrompt struct {
	qs     []tools.Question
	states []qState
	at     int      // the tab shown: a question, or len(qs) for the Submit tab
	review viewport // the Submit tab's summary
	size   boxSize
	answer chan qAnswer
}

// qState is one question's answer, kept while the user moves between tabs.
type qState struct {
	sel    int          // an option row, or len(options) for the typed answer
	picked set.Set[int] // the picked options: a single choice's one, a multi-select's marks
	typed  bool         // the answer is input's text instead of a pick
	typing bool         // the field has the keys
	input  composerState
	view   viewport // the options
	ask    viewport // the question text
	text   textRows
}

type qAnswer struct {
	answers []tools.Answer
	ok      bool
}

// AskQuestion implements hostapi.UIHooks: blocks the tool's goroutine until
// the answers are submitted (ok) or the user dismisses them. A question
// left unanswered in a submitted batch comes back as the zero Answer.
func (a *App) AskQuestion(qs []tools.Question) ([]tools.Answer, bool) {
	if len(qs) == 0 {
		return nil, true
	}
	// every string is model input: strip escapes before the chrome
	clean := make([]tools.Question, len(qs))
	states := make([]qState, len(qs))
	for i, q := range qs {
		clean[i] = tools.Question{Type: q.Type, Question: sanitize.String(q.Question), Description: sanitize.String(q.Description), Header: sanitize.String(q.Header)}
		for _, o := range q.Options {
			clean[i].Options = append(clean[i].Options, tools.QuestionOption{Label: sanitize.String(o.Label), Description: sanitize.String(o.Description), Recommended: o.Recommended})
		}
		states[i] = qState{picked: set.Set[int]{}, typing: q.Type == tools.TypeFreeText || len(clean[i].Options) == 0}
	}
	ch := make(chan qAnswer, 1)
	a.await(func() tea.Cmd {
		a.question = &questionPrompt{qs: clean, states: states, answer: ch}
		return a.notifyIfAway("zemi: a question: " + clean[0].Question)
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
		for i := range qp.states {
			if ok {
				ans.answers = append(ans.answers, qp.states[i].answer(qp.qs[i]))
			}
		}
		qp.answer <- ans
	}
	a.bump()
}

// answer is what the state answers: the typed text, or the picked options
// with the text as their note, so a note edited after the pick counts.
func (st *qState) answer(q tools.Question) tools.Answer {
	text := strings.TrimSpace(st.input.text)
	if st.typed {
		return tools.Answer{Typed: text}
	}
	var a tools.Answer
	for i, o := range q.Options {
		if st.picked.Has(i) {
			a.Picked = append(a.Picked, o.Label)
		}
	}
	if len(a.Picked) > 0 {
		a.Note = text
	}
	return a
}

// choose picks the highlighted option: a single choice's one, or a
// multi-select's marks, the highlighted option when none is marked.
func (st *qState) choose(q tools.Question) {
	if !q.Multi() || len(st.picked) == 0 {
		st.picked = set.Of(st.sel)
	}
	st.typed = false
}

func (qp *questionPrompt) reviewing() bool { return qp.at == len(qp.qs) }

// switchTab moves between the questions and, in a batch, the Submit tab.
func (qp *questionPrompt) switchTab(d int) {
	last := len(qp.qs) - 1
	if len(qp.qs) > 1 {
		last++
	}
	qp.at = max(0, min(qp.at+d, last))
}

// questionKey handles a key on the shown tab: enter answers the question
// (the marked options in a multi-select) or focuses the field on the last
// row, n focuses it as a note, and in the field enter answers with it.
func (a *App) questionKey(key, text string) {
	qp := a.question
	defer a.bump()
	switch key {
	case "tab":
		qp.switchTab(1)
		return
	case "shift+tab":
		qp.switchTab(-1)
		return
	}
	if qp.reviewing() {
		a.reviewKey(key, text)
		return
	}
	q, st := qp.qs[qp.at], &qp.states[qp.at]
	tail := st.sel == len(q.Options)
	if !(st.typing && lineKey(key)) && st.ask.overflows() && scrollKey(&st.ask, key, false) {
		return
	}
	if st.typing {
		switch key {
		case "enter":
			switch {
			case !tail:
				st.choose(q)
				a.answerQuestion()
			case strings.TrimSpace(st.input.text) != "":
				st.picked, st.typed = set.Set[int]{}, true
				a.answerQuestion()
			}
		case "esc":
			if len(q.Options) == 0 { // typing is all an open question does
				a.finishQuestion(false)
			}
			st.typing = false // the draft is kept
		default:
			st.input.typeKey(key, text)
		}
		return
	}
	if moveSel(&st.sel, q.ListRows(), st.view.pageItems(q.ListRows()), key) {
		return
	}
	// a row's digit picks it directly, carrying the note; a single choice
	// answers at once, and the last digit focuses the typed answer. On the
	// typed answer row a digit is its first character instead.
	if n, err := strconv.Atoi(text); err == nil && n >= 1 && n <= min(q.ListRows(), 9) && (n <= len(q.Options) || !q.Multi()) && !tail {
		st.sel = n - 1
		switch {
		case st.sel == len(q.Options):
			st.typing = true
		case !q.Multi():
			st.choose(q)
			a.answerQuestion()
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
	case tail && key == "enter":
		st.typing = true
	case tail && text != "" && key != "space":
		st.typing = true
		st.input.insert(text)
	case key == "n":
		st.typing = true
	case q.Multi() && key == "space" && !tail:
		// the marks are the answer, so leaving the tab with tab keeps them
		if st.picked.Has(st.sel) {
			st.picked.Remove(st.sel)
		} else {
			st.picked.Add(st.sel)
		}
		st.typed = false
	case key == "enter":
		st.choose(q)
		a.answerQuestion()
	}
}

// optionKey is the key that picks row i: its digit, "" past the ninth.
func optionKey(i int) string {
	if i < 9 {
		return strconv.Itoa(i + 1)
	}
	return ""
}

// reviewKey handles a key on the Submit tab: enter and s submit whatever is
// scrolled, a question's digit opens it.
func (a *App) reviewKey(key, text string) {
	qp := a.question
	if scrollKey(&qp.review, key, true) {
		return
	}
	if n, err := strconv.Atoi(text); err == nil && n >= 1 && n <= len(qp.qs) {
		qp.at = n - 1
		return
	}
	switch key {
	case "enter", "s":
		a.finishQuestion(true)
	case "left":
		qp.switchTab(-1)
	case "esc":
		a.finishQuestion(false)
	}
}

// answerQuestion ends the shown question. A single question submits at
// once; a batch moves to the next unanswered question after it, or to the
// Submit tab.
func (a *App) answerQuestion() {
	qp := a.question
	if len(qp.qs) == 1 {
		a.finishQuestion(true)
		return
	}
	for n := qp.at + 1; n < len(qp.qs); n++ {
		if qp.states[n].answer(qp.qs[n]).Open() {
			qp.at = n
			return
		}
	}
	qp.at = len(qp.qs)
}

// questionTabs is the tab row of a batch: the questions, each marked
// waiting or answered in a cell every tab keeps so a mark never shifts the
// labels after it, then the Submit tab.
func questionTabs(qp *questionPrompt) []hostapi.Segment {
	var row []hostapi.Segment
	for i := range len(qp.qs) + 1 {
		if i > 0 {
			row = append(row, hostapi.Segment{Text: "   "})
		}
		label := "Submit"
		if i < len(qp.qs) {
			label = cmp.Or(qp.qs[i].Header, fmt.Sprintf("Q%d", i+1))
			mark := hostapi.Segment{Text: "· ", HL: hostapi.ChromeHint}
			if !qp.states[i].answer(qp.qs[i]).Open() {
				mark = hostapi.Segment{Text: "✓ ", HL: hostapi.ChromeOk}
			}
			row = append(row, mark)
		}
		row = append(row, hostapi.Segment{Text: label, HL: map[bool]string{true: hostapi.ChromeTitle, false: hostapi.ChromeHint}[i == qp.at]})
	}
	return row
}

// questionBox draws the shown tab. The box starts at the size of the
// largest question tab, so switching tabs never resizes or moves it; the
// Submit tab's summary scrolls within it.
func (a *App) questionBox(qp *questionPrompt, w, h int) [][]hostapi.Segment {
	title := "Question"
	if len(qp.qs) > 1 {
		title = "Questions"
	}
	if qp.size.h == 0 {
		at := qp.at
		for qp.at = range qp.qs {
			promptBox(title, "", a.questionRows(qp, w, h), w, h, 100, &qp.size)
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
		moves = []hintPair{{key: "tab", verb: "next"}}
		if qp.at == len(qp.qs) {
			moves = []hintPair{{key: "shift+tab", verb: "previous"}}
		}
	}
	qp.size.w = max(qp.size.w, listBoxW)
	if qp.reviewing() {
		n, done := len(qp.qs), 0
		for i, q := range qp.qs {
			if !qp.states[i].answer(q).Open() {
				done++
			}
		}
		// the one button, pinned under the summary and always selected
		act := func(compact bool) [][]hostapi.Segment {
			rows := choiceRows([]promptChoice{{key: "s", label: "Submit answers", desc: fmt.Sprintf("%d of %d answered", done, n)}}, 0, inner, compact)
			for _, r := range rows {
				for j := range r {
					r[j].Hit = n + 1
				}
			}
			return rows
		}
		hints := slices.Concat(moves, []hintPair{sendHint, {key: fmt.Sprintf("1-%d", n), verb: "change"}, {key: "esc", verb: "dismiss"}})
		return scrollChoiceBody(h, inner, tabs, &qp.review, qp.reviewRows, inner, act, hints...)
	}

	q, st := qp.qs[qp.at], &qp.states[qp.at]
	// a multi-select is a list of switches, the other
	// types buttons keyed by their digit
	var items []listItem
	var choices []promptChoice
	if q.Type != tools.TypeFreeText {
		for i, opt := range q.Options {
			items = append(items, listItem{label: opt.Label, key: switchSeg(st.picked.Has(i), false)})
			choices = append(choices, promptChoice{key: optionKey(i), label: opt.Label})
		}
		items = append(items, listItem{label: "Type your own answer"})
		choices = append(choices, promptChoice{key: optionKey(len(q.Options)), label: "Type your own answer"})
	}
	askText := st.text.at(func(w int) [][]hostapi.Segment {
		ask := a.host.Markdown(q.Question, w)
		if q.Description != "" {
			ask = append(ask, nil)
			ask = append(ask, a.host.Markdown(q.Description, w)...)
		}
		return ask
	})
	askN := len(askText(inner))
	// the options' descriptions, shown in a pane under the list for the
	// selected option as /config shows a row's preview; paneN reserves the
	// most rows any one takes, so moving the selection never shifts a row
	paneWrapped := make([][][]hostapi.Segment, len(q.Options))
	paneN := 0
	for i, opt := range q.Options {
		if opt.Description == "" && !opt.Recommended {
			continue
		}
		desc := opt.Description
		if opt.Recommended {
			desc = strings.TrimSuffix("recommended · "+desc, " · ")
		}
		paneWrapped[i] = a.host.Markdown(desc, inner)
		paneN = max(paneN, min(len(paneWrapped[i]), 4))
	}
	var hints []hintPair
	switch {
	case st.typing:
		hints = slices.Concat(moves, []hintPair{sendHint, {key: "esc", verb: map[bool]string{true: "dismiss", false: "back"}[len(q.Options) == 0]}})
	case q.Multi():
		hints = slices.Concat(moves, []hintPair{{key: "space", verb: "toggle"}, sendHint, {key: "esc", verb: "dismiss"}})
	default:
		hints = slices.Concat(moves, []hintPair{pickHint, {key: "esc", verb: "dismiss"}})
	}
	field := [][]hostapi.Segment{st.field(q, min(inner, qp.size.w))}
	return fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		optsPH := oneRow[:min(len(items), 1)]
		paneRows := map[bool]int{true: 1, false: paneN}[level > 1]
		pane := make([][]hostapi.Segment, paneRows)
		if st.sel < len(paneWrapped) {
			copy(pane, capRows(paneWrapped[st.sel], paneRows, inner))
		}
		// what is asked scrolls when it would push the first options off
		// screen; the rest goes to the options
		room := h - 2 - len(stack(tight, tabs, oneRow, optsPH, pane, field, oneRow)) + 1 + len(optsPH)
		st.ask.viewH = max(room-min(len(items), 4), 1)
		ask := st.ask.window(askText(inner), inner, boxFade)
		var opts [][]hostapi.Segment
		switch {
		case q.Multi():
			opts = listRowsW(items, items, st.sel, &st.view, max(room-len(ask), 1), qp.size.w, inner)
		case len(choices) > 0:
			st.view.viewH = max(room-len(ask), 1)
			opts = choiceView(choices, st.sel, &st.view, inner, level > 1)
		}
		views := []viewport{st.ask, st.view} // the half-page pair scrolls a typed line's text too
		body := stack(tight, tabs, ask, opts, pane, field, [][]hostapi.Segment{hint(inner, slices.Concat(hints, scrolls(views...))...)})
		return body, st.ask.viewH < min(askN, 3) || len(items) > 0 && st.view.viewH < min(st.view.rows, 3)
	})
}

// field is the row under the options holding the question's one text: a
// note on the picks, or the typed answer while the highlight is on the last
// row. It is always drawn behind a fixed label column and cut to the field's
// width, so focusing it, moving the highlight or typing shifts nothing.
func (st *qState) field(q tools.Question, w int) []hostapi.Segment {
	name, text := "Note", []hostapi.Segment{{Text: "add a note", HL: hostapi.ChromeHint}}
	tail := st.sel == len(q.Options)
	if tail {
		name, text = "Answer", []hostapi.Segment{{Text: "type to answer", HL: hostapi.ChromeHint}}
	}
	if s := strings.Join(strings.Fields(st.input.text), " "); s != "" {
		text = []hostapi.Segment{{Text: s, HL: hostapi.ChromeValue}}
	}
	text = ellipsizeSegs(text, w-12)
	row := []hostapi.Segment{{Text: fmt.Sprintf("%-8s", name), HL: hostapi.ChromeHint}}
	switch {
	case st.typing:
		row = append(row, st.input.inputRow(w-8, "")...)
	case tail:
		row = append(row, text...)
	default:
		row = append(row, hostapi.Segment{Text: "[", HL: hostapi.ChromeHint}, hostapi.Segment{Text: "n", HL: hostapi.ChromeHintKey}, hostapi.Segment{Text: "] ", HL: hostapi.ChromeHint})
		row = append(row, text...)
	}
	for i := range row {
		row[i].Hit = q.ListRows() + 1
	}
	return row
}

// reviewRows is the Submit tab's summary at width w: each question's key
// and name, then its answer, or "not answered", with its note under it.
// A question's rows carry its Hit, so a click opens it.
func (qp *questionPrompt) reviewRows(w int) [][]hostapi.Segment {
	names := make([]string, len(qp.qs))
	nameW := 0
	for i, q := range qp.qs {
		names[i] = cmp.Or(q.Header, strings.Join(strings.Fields(q.Question), " "))
		nameW = max(nameW, ansi.StringWidth(names[i]))
	}
	textW := max(w-6, 8) // past "  [1] ", as choiceRows keys its buttons
	nameW = min(nameW, textW/2)
	ansW := max(textW-nameW-2, 1)
	var rows [][]hostapi.Segment
	for i, q := range qp.qs {
		a := qp.states[i].answer(q)
		col := wrapText("not answered", hostapi.ChromeWarn, ansW)
		if !a.Open() {
			col = wrapText(cmp.Or(a.Typed, strings.Join(a.Picked, ", ")), hostapi.ChromeValue, ansW)
		}
		if a.Note != "" {
			col = append(col, wrapText("note: "+a.Note, hostapi.ChromeHint, ansW)...)
		}
		name := ellipsizeSegs([]hostapi.Segment{{Text: names[i], HL: hostapi.ChromeHint}}, nameW)
		for j, r := range col {
			lead := []hostapi.Segment{{Text: strings.Repeat(" ", 6+nameW+2)}}
			if j == 0 {
				lead = slices.Concat([]hostapi.Segment{
					{Text: "  [", HL: hostapi.ChromeHint}, {Text: strconv.Itoa(i + 1), HL: hostapi.ChromeHintKey}, {Text: "] ", HL: hostapi.ChromeHint},
				}, name, []hostapi.Segment{{Text: strings.Repeat(" ", nameW+2-hostapi.SegsWidth(name))}})
			}
			row := slices.Concat(lead, r)
			for k := range row {
				row[k].Hit = i + 1
			}
			rows = append(rows, row)
		}
	}
	return rows
}
