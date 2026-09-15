// memory.go is the Go-owned memory review overlay (DESIGN §8, §10.4):
// fixed chrome, no Lua participates in drawing or answering it. Rows list
// pending proposals and stored memories from both scopes; a accepts a
// proposal, d rejects a proposal or deletes a memory (again to unmark),
// enter applies the marks by row id, esc closes without changes.
package ui

import (
	"strings"

	"github.com/metafates/zemi/internal/hostapi"
)

type memReviewPrompt struct {
	items   []hostapi.MemoryRow
	sel     int
	actions map[string]string // row id -> accept | reject | delete
	answer  chan map[string]string
}

// ReviewMemories implements hostapi.UIHooks: blocks the caller (the
// host's review goroutine) until the overlay closes.
func (a *App) ReviewMemories(items []hostapi.MemoryRow) map[string]string {
	ch := make(chan map[string]string, 1)
	a.dispatch(memReviewMsg{items: items, answer: ch})
	return <-ch
}

// answerMemReview closes the overlay, handing back the marks on apply.
func (a *App) answerMemReview(apply bool) {
	mr := a.memReview
	a.memReview = nil
	if mr != nil && mr.answer != nil {
		out := map[string]string{}
		if apply {
			out = mr.actions
		}
		mr.answer <- out
	}
	a.frameGen++
}

// markMemory toggles an action on the selected row: accept applies to
// proposals only; d rejects a proposal or deletes a stored memory.
func (a *App) markMemory(accept bool) {
	mr := a.memReview
	if mr == nil || mr.sel >= len(mr.items) {
		return
	}
	it := mr.items[mr.sel]
	action := "delete"
	switch {
	case accept && !it.Queued:
		return
	case accept:
		action = "accept"
	case it.Queued:
		action = "reject"
	}
	if mr.actions[it.ID] == action {
		delete(mr.actions, it.ID)
	} else {
		mr.actions[it.ID] = action
	}
}

// spliceMemReview composes the review overlay over the chat area: one
// header line and one text line per row.
func (a *App) spliceMemReview(rows [][]hostapi.Segment, mr *memReviewPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	if len(mr.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "no memories", HL: "Comment"}})
	}
	for i, it := range mr.items {
		cur, hl := "  ", ""
		if i == mr.sel {
			cur, hl = "› ", "Host:Bold"
		}
		mark := map[string]string{"accept": "[+] ", "reject": "[-] ", "delete": "[-] "}[mr.actions[it.ID]]
		if mark == "" {
			mark = "[ ] "
		}
		meta := it.Kind + " · " + it.Origin + " · " + it.Scope
		if it.Queued {
			meta += " · proposal"
		}
		if len(it.Tags) > 0 {
			meta += " (" + strings.Join(it.Tags, ", ") + ")"
		}
		body = append(body, []hostapi.Segment{
			{Text: cur, HL: "Host:Bold"},
			{Text: mark, HL: "Host:Bold"},
			{Text: meta, HL: hl},
		}, []hostapi.Segment{
			{Text: "    "},
			{Text: it.Text, HL: hl},
		})
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, keyHints("a", "accept", "d", "reject/delete", "enter", "apply", "esc", "cancel"))
	box := hostBox(" Memory review ", "Host:MemTitle", "", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}
