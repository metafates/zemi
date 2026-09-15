// memory.go is the Go-owned memory review overlay (DESIGN §8, §10.4):
// fixed chrome, no Lua participates in drawing or answering it. Rows list
// queued proposals (flagged "queued") and stored memories from both
// scopes; d/space toggle the delete mark, enter/esc/q close and return
// the marked rows to the host.
package ui

import (
	"github.com/metafates/zemi/internal/hostapi"
)

// memReviewPrompt is one open review overlay. marked holds per-row delete
// marks; the answer is the marked indices in row order.
type memReviewPrompt struct {
	items  []hostapi.MemoryRow
	sel    int
	marked map[int]bool
	answer chan []int
}

// ReviewMemories implements hostapi.UIHooks: blocks the caller (the
// host's review goroutine) until the user closes the overlay; returns the
// marked row indices to delete.
func (a *App) ReviewMemories(items []hostapi.MemoryRow) []int {
	ch := make(chan []int, 1)
	a.dispatch(memReviewMsg{items: items, answer: ch})
	return <-ch
}

func (a *App) answerMemReview() {
	mr := a.memReview
	a.memReview = nil
	var del []int
	if mr != nil {
		for i := range mr.items {
			if mr.marked[i] {
				del = append(del, i)
			}
		}
		if mr.answer != nil {
			mr.answer <- del
		}
	}
	a.frameGen++
}

// spliceMemReview composes the review overlay over the chat area: one
// header line and one text line per row, queued proposals flagged.
func (a *App) spliceMemReview(rows [][]hostapi.Segment, mr *memReviewPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	if len(mr.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "no memories", HL: "Comment"}})
	}
	for i, it := range mr.items {
		cur, hl := "  ", ""
		if i == mr.sel {
			cur, hl = "> ", "Host:PickSel"
		}
		mark := "[ ] "
		if mr.marked[i] {
			mark = "[x] "
		}
		meta := it.Kind + " · " + it.Origin + " · " + it.Scope
		if it.Queued {
			meta += " · queued"
		}
		if len(it.Tags) > 0 {
			meta += " (" + joinTags(it.Tags) + ")"
		}
		body = append(body, []hostapi.Segment{
			{Text: cur, HL: "Host:PickSel"},
			{Text: mark, HL: "Host:MemMark"},
			{Text: meta, HL: hl},
		})
		body = append(body, []hostapi.Segment{
			{Text: "    "},
			{Text: it.Text, HL: hl},
		})
	}
	body = append(body, []hostapi.Segment{{Text: ""}})
	body = append(body, []hostapi.Segment{
		{Text: "[d]", HL: "Host:PermKey"}, {Text: " mark   "},
		{Text: "[enter]", HL: "Host:PermKey"}, {Text: " apply   "},
		{Text: "[esc]", HL: "Host:PermKey"}, {Text: " cancel"},
	})
	box := hostBox(" Memory review ", "Host:MemTitle", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}

func joinTags(tags []string) string {
	out := ""
	for i, t := range tags {
		if i > 0 {
			out += ", "
		}
		out += t
	}
	return out
}
