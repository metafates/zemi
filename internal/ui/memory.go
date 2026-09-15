// memory.go is the Go-owned memory screen (DESIGN §8, §10.4): fixed
// chrome, no Lua participates in drawing or answering it. Suggestions from
// the reflector come first (a keeps one, d drops it), then what zemi
// remembers (d forgets). A key acts at once and u undoes; the decisions
// reach the host by row id when the screen closes.
package ui

import (
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
	a.do(func() { a.memReview = &memReviewPrompt{items: items, actions: map[string]string{}, answer: ch} })
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

// markMemory acts on the selected row and takes it off the list: keep
// applies to suggestions only; drop rejects a suggestion or forgets a
// memory.
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
	case it.Queued:
		mr.actions[it.ID] = "reject"
	default:
		mr.actions[it.ID] = "delete"
	}
	mr.undo = append(mr.undo, memUndo{mr.sel, it})
	mr.items = append(mr.items[:mr.sel:mr.sel], mr.items[mr.sel+1:]...)
	mr.sel = max(0, min(mr.sel, len(mr.items)-1))
}

func (a *App) undoMemory() {
	mr := a.memReview
	if mr == nil || len(mr.undo) == 0 {
		return
	}
	u := mr.undo[len(mr.undo)-1]
	mr.undo = mr.undo[:len(mr.undo)-1]
	delete(mr.actions, u.item.ID)
	mr.items = append(mr.items[:u.at:u.at], append([]hostapi.MemoryRow{u.item}, mr.items[u.at:]...)...)
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
		body = append(body, []hostapi.Segment{{Text: cur, HL: "Host:HintKey", Hit: i + 1}, {Text: it.Text, HL: hl, Hit: i + 1}, {Text: where, HL: "Host:Hint", Hit: i + 1}})
	}
	if len(mr.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "Nothing here. /memory <note> saves one.", HL: "Host:Hint"}})
	}
	var hints []string
	if mr.sel < len(mr.items) {
		if mr.items[mr.sel].Queued {
			hints = append(hints, "a", "keep", "d", "drop")
		} else {
			hints = append(hints, "d", "forget")
		}
	}
	if len(mr.undo) > 0 {
		hints = append(hints, "u", "undo")
	}
	body = append(body, nil, hintLine(append(hints, "esc", "done")...))
	return hostBox(" Memory ", "Host:Title", "", body, w)
}
