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
	top     int               // first listed item
	scroll  int               // first preview line
	opened  int               // items when the screen opened
	shown   string            // id of the previewed memory; another one starts unscrolled
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
	a.do(func() {
		a.memReview = &memReviewPrompt{items: rows, opened: len(rows), actions: map[string]string{}, answer: ch}
	})
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

// memReviewBox is a fixed-size box, so moving the selection never resizes
// it: one line per memory on top, then the selected memory rendered as
// markdown under a rule naming its kind, scope, origin and tags.
func (a *App) memReviewBox(w, chatH int) [][]hostapi.Segment {
	mr := a.memReview
	if len(mr.items) == 0 {
		body := [][]hostapi.Segment{{{Text: "Nothing here. /memory <note> saves one.", HL: "Host:Hint"}}, nil, hintLine("esc", "done")}
		return hostBox(" Memory ", "Host:Title", "", body, w)
	}
	inner := max(min(w-4, 100)-4, 12)
	// borders, the rule, the blank row and the hint take five rows
	rows := max(min(chatH, 36)-5, 2)
	// sized by the count at open, so a forgotten row does not resize the box
	listH := max(min(mr.opened, rows/2), 1)
	previewH := max(rows-listH, 1)

	mr.top = max(min(mr.top, mr.sel), mr.sel-listH+1, 0)
	var body [][]hostapi.Segment
	for i := mr.top; i < mr.top+listH; i++ {
		if i >= len(mr.items) {
			body = append(body, nil)
			continue
		}
		it := mr.items[i]
		cur, hl := "  ", "Host:Value"
		if i == mr.sel {
			cur, hl = "› ", "Host:Bold"
		}
		mark := map[bool]string{true: "● ", false: "  "}[it.Queued]
		markHL := "Host:Warn"
		if mr.actions[it.ID] == "accept" {
			mark, markHL = "+ ", "Host:Ok"
		}
		scope := fmt.Sprintf("%12s", memScope(it))
		row := []hostapi.Segment{{Text: cur, HL: "Host:HintKey"}, {Text: mark, HL: markHL}}
		// the list reads the rendered text on one line, without markup
		var plain []string
		for _, l := range a.host.Markdown(it.Text, inner) {
			plain = append(plain, plainSegs(l))
		}
		text := strings.Join(strings.Fields(strings.Join(plain, " ")), " ")
		row = append(row, ellipsizeSegs([]hostapi.Segment{{Text: text, HL: hl}}, inner-4-2-len(scope))...)
		row = append(row, hostapi.Segment{Text: strings.Repeat(" ", inner-segsWidth(row)-len(scope)) + scope, HL: "Host:Hint"})
		body = append(body, hitRow(row, i+1))
	}

	it := mr.items[mr.sel]
	meta := []string{map[bool]string{true: "suggested", false: "remembered"}[it.Queued], it.Kind, memScope(it), map[bool]string{true: "learned", false: "saved by you"}[it.Origin == "learned"]}
	for _, t := range it.Tags {
		meta = append(meta, "#"+t)
	}
	label := "── " + strings.Join(slices.DeleteFunc(meta, func(s string) bool { return s == "" }), " · ") + " "
	label = ansi.Truncate(label, inner, "…")
	body = append(body, []hostapi.Segment{{Text: label + strings.Repeat("─", inner-ansi.StringWidth(label)), HL: "Host:Hint"}})

	if mr.shown != it.ID {
		mr.shown, mr.scroll = it.ID, 0
	}
	preview := a.host.Markdown(it.Text, inner)
	mr.scroll = max(min(mr.scroll, len(preview)-previewH), 0)
	for i := mr.scroll; i < mr.scroll+previewH; i++ {
		switch {
		case i == mr.scroll+previewH-1 && i < len(preview)-1:
			body = append(body, []hostapi.Segment{{Text: fmt.Sprintf("%d more lines · shift+↓ scroll", len(preview)-i), HL: "Host:Hint"}})
		case i < len(preview):
			body = append(body, preview[i])
		default:
			body = append(body, nil)
		}
	}

	var hints []string
	switch {
	case it.Queued:
		hints = append(hints, "a", "keep", "d", "drop")
	case mr.actions[it.ID] == "accept":
		hints = append(hints, "d", "drop")
	default:
		hints = append(hints, "d", "forget")
	}
	if len(mr.undo) > 0 {
		hints = append(hints, "u", "undo")
	}
	body = append(body, nil, hintLine(append(hints, "esc", "done")...))
	return hostBox(" Memory ", "Host:Title", fmt.Sprintf(" %d of %d ", mr.sel+1, len(mr.items)), body, w)
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
