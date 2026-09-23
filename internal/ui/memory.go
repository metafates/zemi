// memory.go is the Go-owned memory screen (DESIGN §8, §10.4): fixed
// chrome, no Lua participates in drawing or answering it. Suggestions from
// the reflector come first (a keeps one, d drops it), then what zemi
// remembers (d forgets); s moves a row between this project and everywhere,
// e closes the screen to edit a row in the user's editor.
// A key acts at once and u undoes; the edited rows reach the host when the
// screen closes.
package ui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/sanitize"
)

type memReviewPrompt struct {
	items []hostapi.MemoryRow
	gone  []hostapi.MemoryRow // dropped rows, still answered
	listFilter
	size   boxSize
	undo   []memUndo
	answer chan []hostapi.MemoryRow
}

// at is the item under the selection, len(items) with none.
func (mr *memReviewPrompt) at() int {
	if mr.selected < len(mr.visible) {
		return mr.visible[mr.selected]
	}
	return len(mr.items)
}

func (mr *memReviewPrompt) texts() []string {
	out := make([]string, len(mr.items))
	for i, it := range mr.items {
		out[i] = it.Text + "  " + it.Quote
	}
	return out
}

// seek filters the rows again and selects item i, or the first shown after it.
func (mr *memReviewPrompt) seek(i int) {
	mr.apply(mr.texts())
	j, _ := slices.BinarySearch(mr.visible, i)
	mr.selected = max(0, min(len(mr.visible)-1, j))
}

// memUndo is a row as it was before a key changed it, and where it was.
type memUndo struct {
	at   int
	item hostapi.MemoryRow
}

// ReviewMemories implements hostapi.UIHooks: blocks the caller (the
// host's review goroutine) until the screen closes.
func (a *App) ReviewMemories(items []hostapi.MemoryRow) []hostapi.MemoryRow {
	ch := make(chan []hostapi.MemoryRow, 1)
	// the caller keeps reading its own slice after this returns, so the
	// screen edits a copy
	rows := slices.Clone(items)
	a.await(func() tea.Cmd {
		// the host reopens the screen after an edit, on the edited row
		sel := max(0, slices.IndexFunc(rows, func(r hostapi.MemoryRow) bool { return r.ID == a.memEdited }))
		a.memReview, a.memEdited = &memReviewPrompt{items: rows, answer: ch}, ""
		a.memReview.seek(sel)
		return nil
	})
	return <-ch
}

// answerMemReview closes the screen and hands back every row as edited.
func (a *App) answerMemReview() {
	mr := a.memReview
	a.memReview = nil
	if mr != nil && mr.answer != nil {
		mr.answer <- append(mr.items, mr.gone...)
	}
	a.bump()
}

// editMemory hands the selected row to the host for editing.
func (a *App) editMemory() {
	mr := a.memReview
	if mr == nil || mr.at() >= len(mr.items) || mr.items[mr.at()].Forget {
		return
	}
	mr.items[mr.at()].Edit = true
	a.memEdited = mr.items[mr.at()].ID
	a.answerMemReview()
}

// suggested reports whether a row still waits in the Suggested section.
func suggested(r hostapi.MemoryRow) bool { return r.Queued && r.Action != "accept" }

// markMemory acts on the selected row: keep moves a suggestion into
// Remembered so the screen shows what zemi will know without reopening it;
// drop rejects a suggestion or forgets a memory, taking the row off the
// list; scope flips between this project and everywhere.
func (a *App) markMemory(key string) {
	mr := a.memReview
	sel := mr.at()
	if sel >= len(mr.items) {
		return
	}
	it := mr.items[sel]
	if key == "keep" && !suggested(it) || key == "scope" && it.Forget {
		return
	}
	mr.undo = append(mr.undo, memUndo{sel, it})
	switch key {
	case "scope":
		mr.items[sel].Scope = map[bool]string{true: "project", false: "user"}[it.Scope == "user"]
		return
	case "keep":
		it.Action = "accept"
		mr.items = slices.Delete(mr.items, sel, sel+1)
		if it.Forget {
			mr.gone = append(mr.gone, it) // a kept forget remembers nothing
		} else if at := rememberedAt(mr.items); at <= sel {
			mr.items = slices.Insert(mr.items, at, it)
			sel++ // the row moved past the cursor
		} else {
			mr.items = slices.Insert(mr.items, at, it)
		}
		if n := rememberedAt(mr.items); sel >= n && n > 0 {
			sel = 0 // past the last suggestion: back to the first one left
		}
	default:
		// a suggestion kept a moment ago was never stored, so dropping it
		// rejects it; there is nothing to forget
		it.Action = map[bool]string{true: "reject", false: "delete"}[it.Queued]
		mr.items = slices.Delete(mr.items, sel, sel+1)
		mr.gone = append(mr.gone, it)
	}
	mr.seek(sel)
}

// rememberedAt is where the Remembered section starts: suggestions come
// first, so an accepted row lands right after the last of them.
func rememberedAt(items []hostapi.MemoryRow) int {
	if i := slices.IndexFunc(items, func(r hostapi.MemoryRow) bool { return !suggested(r) }); i >= 0 {
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
	other := func(r hostapi.MemoryRow) bool { return r.ID == u.item.ID }
	mr.items = slices.DeleteFunc(mr.items, other)
	mr.gone = slices.DeleteFunc(mr.gone, other)
	mr.items = slices.Insert(mr.items, min(u.at, len(mr.items)), u.item)
	mr.seek(u.at)
}

// memReviewBox shows every memory whole, as plain text since markdown could
// hide part of what gets stored, with the quote it rests on and its scope,
// origin and age under it; the Remembered title says how much of each
// scope's prompt budget the stored rows take.
func (a *App) memReviewBox(w, h int) [][]hostapi.Segment {
	mr := a.memReview
	mr.apply(mr.texts())
	n := rememberedAt(mr.items)
	used := map[string]int{}
	for _, it := range mr.items[n:] {
		if !it.Queued {
			used[it.Scope] += learn.LineSize(it.Text)
		}
	}
	kb := func(scope string) string {
		return fmt.Sprintf("%.1f/%d KB", float64(used[scope])/1024, learn.ScopeBudget/1024)
	}
	sections := map[bool]string{
		true:  fmt.Sprintf("Suggested by zemi (%d)", n),
		false: fmt.Sprintf("Remembered (%d) · this project %s · everywhere %s", len(mr.items)-n, kb("project"), kb("user")),
	}
	items := make([]listItem, 0, len(mr.visible))
	for _, i := range mr.visible {
		it := mr.items[i]
		var meta []hostapi.Segment
		if it.Action == "accept" {
			meta = append(meta, hostapi.Segment{Text: "kept · ", HL: "Host:Ok"})
		}
		info := []string{memScope(it), map[bool]string{true: "learned", false: "saved by you"}[it.Origin == "learned"]}
		if it.Updated != 0 {
			info = append(info, strings.TrimSpace(learn.Age(it.Updated, a.now())))
		}
		meta = append(meta, hostapi.Segment{Text: strings.Join(slices.DeleteFunc(info, func(s string) bool { return s == "" }), " · "), HL: "Host:Hint"})
		more := [][]hostapi.Segment{{{Text: sanitize.String(it.Text)}}}
		if it.Quote != "" {
			more = append(more, []hostapi.Segment{{Text: `"` + sanitize.String(it.Quote) + `"`, HL: "Host:Hint"}})
		}
		items = append(items, listItem{section: sections[suggested(it)], more: append(more, meta)})
	}
	inner := boxInner(w)
	mr.size.w = max(mr.size.w, 56)

	var hints []hintPair
	if i := mr.at(); i < len(mr.items) && !mr.active {
		it := mr.items[i]
		switch {
		case suggested(it) && it.Forget:
			hints = append(hints, hintPair{"a", "forget"}, hintPair{"d", "keep"})
		case suggested(it):
			hints = append(hints, hintPair{"a", "keep"}, hintPair{"d", "drop"})
		case it.Queued:
			hints = append(hints, hintPair{"d", "drop"})
		default:
			hints = append(hints, hintPair{"d", "forget"})
		}
		if !it.Forget {
			hints = append(hints, hintPair{"s", map[bool]string{true: "only this project", false: "use everywhere"}[it.Scope == "user"]}, hintPair{"e", "edit"})
		}
	}
	if len(mr.undo) > 0 && !mr.active {
		hints = append(hints, hintPair{"u", "undo"})
	}
	esc := map[bool]string{true: "back", false: "close"}[mr.active]
	body := fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		head := [][]hostapi.Segment{mr.filterRow()}
		if len(mr.items) == 0 {
			head = emptyRows("Nothing remembered yet. Type /memory <note> to save one.", inner)
		}
		room := h - 2 - len(stack(tight, slices.Concat(head, oneRow), oneRow)) + 1
		// the text wraps beside the marker's two cells, and the bar's and its
		// gap's two once the list overflows
		rows := func(textW int) [][]hostapi.Segment {
			wrapped := slices.Clone(items)
			for i, it := range wrapped {
				wrapped[i].more = nil
				for _, r := range it.more {
					wrapped[i].more = append(wrapped[i].more, hostapi.WrapSegs(r, textW)...)
				}
			}
			return listRows(wrapped, mr.selected, &mr.view, max(room, 1), inner)
		}
		var list [][]hostapi.Segment
		switch {
		case len(mr.items) == 0:
		case len(items) == 0:
			list = emptyRows("No matches.", inner)
		default:
			if list = rows(inner - 2); mr.view.overflows() {
				list = rows(inner - 4)
			}
		}
		hintRow := hint(inner, slices.Concat(hints, scrolls(mr.view), []hintPair{{"esc", esc}})...)
		return stack(tight, slices.Concat(head, list), [][]hostapi.Segment{hintRow}), len(items) > 0 && mr.view.viewH < min(mr.view.rows, 3)
	})
	return promptBox("Memory", "", body, w, h, 100, &mr.size)
}

func memScope(it hostapi.MemoryRow) string {
	if it.Scope == "user" {
		return "everywhere"
	}
	return "this project"
}
