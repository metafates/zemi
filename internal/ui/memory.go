// memory.go is the Go-owned memory screen (DESIGN §8, §10.4): fixed
// chrome, no Lua participates in drawing or answering it. Suggestions from
// the reflector come first (a keeps one, d drops it), then what zemi
// remembers (d forgets). A key acts at once and u undoes; the decisions
// reach the host by row id when the screen closes.
package ui

import (
	"slices"

	"github.com/metafates/zemi/internal/hostapi"
)

type memReviewPrompt struct {
	items   []hostapi.MemoryRow
	sel     int
	actions map[string]string // row id -> accept | reject | delete
	undo    []memUndo
	answer  chan map[string]string
}

type memUndo struct {
	at   int
	item hostapi.MemoryRow
}

// ReviewMemories implements hostapi.UIHooks: blocks the caller (the
// host's review goroutine) until the screen closes.
func (a *App) ReviewMemories(items []hostapi.MemoryRow) map[string]string {
	ch := make(chan map[string]string, 1)
	// the caller keeps reading its own slice after this returns, so the
	// screen edits a copy
	rows := slices.Clone(items)
	a.do(func() { a.memReview = &memReviewPrompt{items: rows, actions: map[string]string{}, answer: ch} })
	return <-ch
}

// answerMemReview closes the screen and hands back what was done.
func (a *App) answerMemReview() {
	mr := a.memReview
	a.memReview = nil
	if mr != nil && mr.answer != nil {
		mr.answer <- mr.actions
	}
	a.frameGen++
}

// markMemory acts on the selected row: keep moves a suggestion into
// Remembered so the screen shows what zemi will know without reopening it;
// drop rejects a suggestion or forgets a memory, taking the row off the
// list. The host applies the decisions by id when the screen closes.
func (a *App) markMemory(keep bool) {
	mr := a.memReview
	if mr == nil || mr.sel >= len(mr.items) {
		return
	}
	it := mr.items[mr.sel]
	switch {
	case keep && !it.Queued:
		return
	case keep:
		mr.actions[it.ID] = "accept"
	case it.Queued, mr.actions[it.ID] == "accept":
		// dropping a row accepted a moment ago drops the suggestion; it
		// was never stored, so there is nothing to forget
		mr.actions[it.ID] = "reject"
	default:
		mr.actions[it.ID] = "delete"
	}
	mr.undo = append(mr.undo, memUndo{mr.sel, it})
	mr.items = slices.Delete(mr.items, mr.sel, mr.sel+1)
	if keep {
		it.Queued = false
		at := rememberedAt(mr.items)
		mr.items = slices.Insert(mr.items, at, it)
		if at <= mr.sel {
			mr.sel++ // the row moved past the cursor; stay on the next suggestion
		}
	}
	mr.sel = max(0, min(mr.sel, len(mr.items)-1))
}

// rememberedAt is where the Remembered section starts: suggestions come
// first, so an accepted row lands right after the last of them.
func rememberedAt(items []hostapi.MemoryRow) int {
	if i := slices.IndexFunc(items, func(r hostapi.MemoryRow) bool { return !r.Queued }); i >= 0 {
		return i
	}
	return len(items)
}

func (a *App) undoMemory() {
	mr := a.memReview
	if mr == nil || len(mr.undo) == 0 {
		return
	}
	u := mr.undo[len(mr.undo)-1]
	mr.undo = mr.undo[:len(mr.undo)-1]
	delete(mr.actions, u.item.ID)
	// an accepted row is still on the list, one section down
	if i := slices.IndexFunc(mr.items, func(r hostapi.MemoryRow) bool { return r.ID == u.item.ID }); i >= 0 {
		mr.items = slices.Delete(mr.items, i, i+1)
	}
	mr.items = slices.Insert(mr.items, min(u.at, len(mr.items)), u.item)
	mr.sel = u.at
}

func memReviewBox(mr *memReviewPrompt, w int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	section := ""
	for i, it := range mr.items {
		if s := map[bool]string{true: "Suggested by zemi", false: "Remembered"}[it.Queued]; s != section {
			if section != "" {
				body = append(body, nil)
			}
			section = s
			body = append(body, []hostapi.Segment{{Text: s, HL: "Host:Hint"}})
		}
		cur, hl := "  ", "Host:Value"
		if i == mr.sel {
			cur, hl = "› ", "Host:Bold"
		}
		where := "  this project"
		if it.Scope == "user" {
			where = "  everywhere"
		}
		row := []hostapi.Segment{{Text: cur, HL: "Host:HintKey", Hit: i + 1}}
		if mr.actions[it.ID] == "accept" {
			row = append(row, hostapi.Segment{Text: "+ ", HL: "Host:Ok", Hit: i + 1})
		}
		body = append(body, append(row, hostapi.Segment{Text: it.Text, HL: hl, Hit: i + 1}, hostapi.Segment{Text: where, HL: "Host:Hint", Hit: i + 1}))
	}
	if len(mr.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "Nothing here. /memory <note> saves one.", HL: "Host:Hint"}})
	}
	var hints []string
	if mr.sel < len(mr.items) {
		switch it := mr.items[mr.sel]; {
		case it.Queued:
			hints = append(hints, "a", "keep", "d", "drop")
		case mr.actions[it.ID] == "accept":
			hints = append(hints, "d", "drop")
		default:
			hints = append(hints, "d", "forget")
		}
	}
	if len(mr.undo) > 0 {
		hints = append(hints, "u", "undo")
	}
	body = append(body, nil, hintLine(append(hints, "esc", "done")...))
	return hostBox(" Memory ", "Host:Title", "", body, w)
}
