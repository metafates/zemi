// memory.go is the Go-owned memory screen (DESIGN §8, §10.4): fixed
// chrome, no Lua participates in drawing or answering it. Suggestions from
// the reflector come first (a keeps one, d drops it), then what zemi
// remembers (d forgets). A key acts at once and u undoes; the decisions
// reach the host by row id when the screen closes.
package ui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
)

type memReviewPrompt struct {
	items   []hostapi.MemoryRow
	sel     int
	top     int               // first line of the scrolled window
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

// memReviewBox lists one memory per line and opens the selected one in
// place: its whole text wrapped, then kind, scope, origin and tags. The
// window scrolls to keep that card in view inside the chat area.
func memReviewBox(mr *memReviewPrompt, w, chatH int) [][]hostapi.Segment {
	inner := max(min(w-4, 100)-4, 12)
	// one width for every selection: the widest memory on one line, capped
	fit := 56
	for _, it := range mr.items {
		fit = max(fit, 6+ansi.StringWidth(it.Text)+2+ansi.StringWidth(memScope(it)))
	}
	fit = min(fit, inner)

	var lines [][]hostapi.Segment
	selTop, selEnd := 0, 0
	section := ""
	for i, it := range mr.items {
		if s := map[bool]string{true: "Suggested by zemi", false: "Remembered"}[it.Queued]; s != section {
			if section != "" {
				lines = append(lines, nil)
			}
			section = s
			lines = append(lines, []hostapi.Segment{{Text: s, HL: "Host:Hint"}})
		}
		mark := ""
		if mr.actions[it.ID] == "accept" {
			mark = "+ "
		}
		if i != mr.sel {
			scope := memScope(it)
			text := ellipsizeSegs([]hostapi.Segment{{Text: it.Text, HL: "Host:Value"}}, fit-2-len(mark)-2-ansi.StringWidth(scope))
			row := []hostapi.Segment{{Text: "  "}, {Text: mark, HL: "Host:Ok"}}
			row = append(row, text...)
			row = append(row, hostapi.Segment{Text: strings.Repeat(" ", max(2, fit-segsWidth(row)-ansi.StringWidth(scope))) + scope, HL: "Host:Hint"})
			lines = append(lines, hitRow(row, i+1))
			continue
		}
		selTop = len(lines)
		for j, r := range wrapSegs([]hostapi.Segment{{Text: it.Text, HL: "Host:Bold"}}, fit-2-len(mark)) {
			pre := strings.Repeat(" ", len(mark))
			if j == 0 {
				pre = mark
			}
			lines = append(lines, hitRow(append([]hostapi.Segment{{Text: "▌ ", HL: "Host:HintKey"}, {Text: pre, HL: "Host:Ok"}}, r...), i+1))
		}
		meta := []string{it.Kind, memScope(it), map[bool]string{true: "learned", false: "saved by you"}[it.Origin == "learned"]}
		for _, t := range it.Tags {
			meta = append(meta, "#"+t)
		}
		lines = append(lines, hitRow([]hostapi.Segment{{Text: "▌ ", HL: "Host:HintKey"}, {Text: strings.Join(slices.DeleteFunc(meta, func(s string) bool { return s == "" }), " · "), HL: "Host:Hint"}}, i+1))
		selEnd = len(lines)
	}

	badge := ""
	// borders, the blank row and the hint take four rows
	if room := max(chatH-4, 3); len(lines) > room {
		mr.top = min(mr.top, selTop, len(lines)-room)
		mr.top = max(mr.top, min(selEnd-room, selTop), 0)
		lines = lines[mr.top : mr.top+room]
		badge = fmt.Sprintf(" %d of %d ", mr.sel+1, len(mr.items))
	}
	body := lines
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
	return hostBox(" Memory ", "Host:Title", badge, body, w)
}

func memScope(it hostapi.MemoryRow) string {
	if it.Scope == "user" {
		return "everywhere"
	}
	return "this project"
}

// hitRow makes every cell of a row click to item hit.
func hitRow(row []hostapi.Segment, hit int) []hostapi.Segment {
	for i := range row {
		row[i].Hit = hit
	}
	return row
}
