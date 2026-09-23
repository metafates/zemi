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
	items  []hostapi.MemoryRow
	gone   []hostapi.MemoryRow // dropped rows, still answered
	sel    int
	view   viewport
	undo   []memUndo
	answer chan []hostapi.MemoryRow
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
		a.memReview, a.memEdited = &memReviewPrompt{items: rows, sel: sel, answer: ch}, ""
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
	if mr == nil || mr.sel >= len(mr.items) || mr.items[mr.sel].Forget {
		return
	}
	mr.items[mr.sel].Edit = true
	a.memEdited = mr.items[mr.sel].ID
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
	if mr == nil || mr.sel >= len(mr.items) {
		return
	}
	it := mr.items[mr.sel]
	if key == "keep" && !suggested(it) || key == "scope" && it.Forget {
		return
	}
	mr.undo = append(mr.undo, memUndo{mr.sel, it})
	switch key {
	case "scope":
		mr.items[mr.sel].Scope = map[bool]string{true: "project", false: "user"}[it.Scope == "user"]
		return
	case "keep":
		it.Action = "accept"
		mr.items = slices.Delete(mr.items, mr.sel, mr.sel+1)
		if it.Forget {
			mr.gone = append(mr.gone, it) // a kept forget remembers nothing
		} else if at := rememberedAt(mr.items); at <= mr.sel {
			mr.items = slices.Insert(mr.items, at, it)
			mr.sel++ // the row moved past the cursor
		} else {
			mr.items = slices.Insert(mr.items, at, it)
		}
		if n := rememberedAt(mr.items); mr.sel >= n && n > 0 {
			mr.sel = 0 // past the last suggestion: back to the first one left
		}
	default:
		// a suggestion kept a moment ago was never stored, so dropping it
		// rejects it; there is nothing to forget
		it.Action = map[bool]string{true: "reject", false: "delete"}[it.Queued]
		mr.items = slices.Delete(mr.items, mr.sel, mr.sel+1)
		mr.gone = append(mr.gone, it)
	}
	mr.sel = max(0, min(mr.sel, len(mr.items)-1))
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
	mr.sel = u.at
}

// memReviewBox shows every memory whole, as plain text since markdown could
// hide part of what gets stored, with the quote it rests on and its scope,
// origin and age under it; the Remembered title says how much of each
// scope's prompt budget the stored rows take.
func (a *App) memReviewBox(w, chatH int) [][]hostapi.Segment {
	mr := a.memReview
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
	items := make([]listItem, 0, len(mr.items))
	for _, it := range mr.items {
		var meta []hostapi.Segment
		if it.Action == "accept" {
			meta = append(meta, hostapi.Segment{Text: "kept · ", HL: "Host:Ok"})
		}
		info := []string{memScope(it), map[bool]string{true: "learned", false: "saved by you"}[it.Origin == "learned"]}
		if it.Updated != 0 {
			info = append(info, strings.TrimSpace(learn.Age(it.Updated, a.now())))
		}
		meta = append(meta, hostapi.Segment{Text: strings.Join(slices.DeleteFunc(info, func(s string) bool { return s == "" }), " · "), HL: "Host:Hint"})
		// the marker's two cells and the bar's one
		more := hostapi.WrapSegs([]hostapi.Segment{{Text: sanitize.String(it.Text)}}, boxInner(w)-3)
		if it.Quote != "" {
			more = append(more, hostapi.WrapSegs([]hostapi.Segment{{Text: `"` + sanitize.String(it.Quote) + `"`, HL: "Host:Hint"}}, boxInner(w)-3)...)
		}
		items = append(items, listItem{section: sections[suggested(it)], more: append(more, meta)})
	}
	// borders, the blank row and the hint take four rows
	lines := listRows(items, mr.sel, &mr.view, max(chatH-4, 3), boxInner(w))
	if len(mr.items) == 0 {
		lines = hostapi.WrapSegs([]hostapi.Segment{{Text: "Nothing remembered yet. Type /memory <note> to save one.", HL: "Comment"}}, boxInner(w))
	}
	// the hint pads to the list, so changing hints never resizes the box
	widest := 0
	for _, l := range lines {
		widest = max(widest, segsWidth(l))
	}

	var hints []string
	if mr.sel < len(mr.items) {
		it := mr.items[mr.sel]
		switch {
		case suggested(it) && it.Forget:
			hints = append(hints, "a", "forget", "d", "keep")
		case suggested(it):
			hints = append(hints, "a", "keep", "d", "drop")
		case it.Queued:
			hints = append(hints, "d", "drop")
		default:
			hints = append(hints, "d", "forget")
		}
		if !it.Forget {
			hints = append(hints, "s", map[bool]string{true: "only this project", false: "use everywhere"}[it.Scope == "user"], "e", "edit")
		}
	}
	if len(mr.undo) > 0 {
		hints = append(hints, "u", "undo")
	}
	hints = append(hints, scrolls(mr.view)...)
	lines = append(lines, nil, padSegs(hintLine(boxInner(w), append(hints, "esc", "close")...), widest))
	return hostBox(" Memory ", "Host:Title", "", lines, w)
}

func memScope(it hostapi.MemoryRow) string {
	if it.Scope == "user" {
		return "everywhere"
	}
	return "this project"
}
