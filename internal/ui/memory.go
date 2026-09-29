// memory.go is the Go-owned memory screen (DESIGN §8, §10.4): fixed
// chrome, no Lua participates in drawing or answering it. Suggestions from
// the reflector come first (a keeps one, d drops it), then what zemi
// remembers (d forgets); s moves a row between this project and everywhere,
// o closes the screen to edit a row in the user's editor. Enter opens a row
// with those actions as buttons. A key acts at once and u undoes; the
// edited rows reach the host when the screen closes.
package ui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

type memReviewPrompt struct {
	items []hostapi.MemoryRow
	gone  []hostapi.MemoryRow // dropped rows, still answered
	listFilter
	detail bool     // the selected row is open, its actions as buttons
	act    int      // the selected button
	body   viewport // the open row's text
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

// open shows the selected row with its actions as buttons.
func (mr *memReviewPrompt) open() {
	mr.detail, mr.act, mr.body = mr.at() < len(mr.items), 0, viewport{}
}

// seek filters the rows again and selects item i, or the first shown after
// it. A memory's text is its row's label, as memMore draws it, and its quote
// is other text.
func (mr *memReviewPrompt) seek(i int) {
	mr.labels, mr.texts = mr.labels[:0], mr.texts[:0]
	for _, it := range mr.items {
		mr.labels, mr.texts = append(mr.labels, sanitize.String(it.Text)), append(mr.texts, it.Quote)
	}
	mr.apply()
	j, _ := slices.BinarySearch(mr.visible, i)
	mr.selected = max(0, min(len(mr.visible)-1, j))
}

// memUndo is a row as it was before a key changed it, and where it was,
// plus the memory keeping it took off the screen, if any.
type memUndo struct {
	at, hidAt int
	item, hid hostapi.MemoryRow
}

// ReviewMemories implements hostapi.UIHooks: blocks the caller (the
// host's review goroutine) until the screen closes.
func (a *App) ReviewMemories(items []hostapi.MemoryRow, focus string) []hostapi.MemoryRow {
	ch := make(chan []hostapi.MemoryRow, 1)
	// the caller keeps reading its own slice after this returns, so the
	// screen edits a copy
	rows := slices.Clone(items)
	a.await(func() tea.Cmd {
		// the host reopens the screen after an edit, on the edited row
		sel := slices.IndexFunc(rows, func(r hostapi.MemoryRow) bool { return r.ID == cmp.Or(focus, a.memEdited) })
		a.memReview, a.memEdited = &memReviewPrompt{items: rows, answer: ch}, ""
		a.memReview.seek(max(sel, 0))
		if focus != "" && sel >= 0 {
			a.memReview.open() // a stored memory's first action is Forget
		}
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
	mr.undo = append(mr.undo, memUndo{at: sel, item: it})
	switch key {
	case "scope":
		mr.items[sel].Scope = map[bool]store.Scope{true: store.ScopeProject, false: store.ScopeUser}[it.Scope == store.ScopeUser]
		return
	case "keep":
		it.Action = "accept"
		mr.items = slices.Delete(mr.items, sel, sel+1)
		// the memory it forgets or replaces leaves the screen, and the answer:
		// the host retires it when it stores this row
		if i := slices.IndexFunc(mr.items, func(r hostapi.MemoryRow) bool { return !r.Queued && r.ID == it.Supersedes }); i >= 0 {
			u := &mr.undo[len(mr.undo)-1]
			u.hidAt, u.hid = i, mr.items[i]
			mr.items = slices.Delete(mr.items, i, i+1)
		}
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
	if u.hid.ID != "" {
		mr.items = slices.Insert(mr.items, u.hidAt, u.hid)
	}
	mr.items = slices.Insert(mr.items, min(u.at, len(mr.items)), u.item)
	mr.seek(u.at)
}

// memActions are what the keys do to row it, as buttons: the hint names
// the same ones, so each keeps one word everywhere.
func memActions(it hostapi.MemoryRow, undo bool) []promptChoice {
	var out []promptChoice
	switch {
	case suggested(it) && it.Forget:
		out = []promptChoice{{key: "a", label: "Forget"}, {key: "d", label: "Keep"}}
	case suggested(it):
		out = []promptChoice{{key: "a", label: "Keep"}, {key: "d", label: "Drop"}}
	case it.Queued:
		out = []promptChoice{{key: "d", label: "Drop"}}
	default:
		out = []promptChoice{{key: "d", label: "Forget"}}
	}
	if !it.Forget {
		out = append(out, promptChoice{key: "s", label: map[bool]string{true: "Only this project", false: "Use everywhere"}[it.Scope == store.ScopeUser]}, promptChoice{key: "o", label: "Open in the editor"})
	}
	if undo {
		out = append(out, promptChoice{key: "u", label: "Undo"})
	}
	return out
}

// memAct runs action key on the selected row and closes an open one.
func (a *App) memAct(key string) {
	a.memReview.detail = false
	switch key {
	case "a":
		a.markMemory("keep")
	case "d":
		a.markMemory("drop")
	case "s":
		a.markMemory("scope")
	case "o":
		a.editMemory()
	case "u":
		a.undoMemory()
	}
}

// memMore is row it as the list and its open view show it: the text, the
// quote it rests on, and its scope, origin and age.
func (a *App) memMore(it hostapi.MemoryRow) [][]hostapi.Segment {
	var meta []hostapi.Segment
	if it.Action == "accept" {
		meta = append(meta, hostapi.Segment{Text: "kept · ", HL: "Host:Ok"})
	}
	info := []string{memScope(it), map[bool]string{true: "learned", false: "saved by you"}[it.Origin == store.OriginLearned]}
	if it.Updated != 0 {
		info = append(info, strings.TrimSpace(learn.Age(it.Updated, a.now())))
	}
	meta = append(meta, hostapi.Segment{Text: strings.Join(slices.DeleteFunc(info, func(s string) bool { return s == "" }), " · "), HL: "Host:Hint"})
	more := [][]hostapi.Segment{{{Text: sanitize.String(it.Text)}}}
	if it.Quote != "" {
		more = append(more, []hostapi.Segment{{Text: `"` + sanitize.String(it.Quote) + `"`, HL: "Host:Hint"}})
	}
	return append(more, meta)
}

// memReviewBox shows every memory whole, as plain text since markdown could
// hide part of what gets stored, with the quote it rests on and its scope,
// origin and age under it; the Remembered title says how much of each
// scope's prompt budget the stored rows take.
func (a *App) memReviewBox(w, h int) [][]hostapi.Segment {
	mr := a.memReview
	n := rememberedAt(mr.items)
	used := map[store.Scope]int{}
	for _, it := range mr.items[n:] {
		if !it.Queued {
			used[it.Scope] += learn.LineSize(it.Text)
		}
	}
	kb := func(scope store.Scope) string {
		return fmt.Sprintf("%.1f/%d KB", float64(used[scope])/1024, learn.ScopeBudget/1024)
	}
	sections := map[bool]string{
		true:  fmt.Sprintf("Suggested by zemi (%d)", n),
		false: fmt.Sprintf("Remembered (%d) · this project %s · everywhere %s", len(mr.items)-n, kb(store.ScopeProject), kb(store.ScopeUser)),
	}
	inner := boxInner(w)
	mr.size.w = max(mr.size.w, 56)
	if i := mr.at(); mr.detail && i < len(mr.items) {
		return a.memDetailBox(mr.items[i], w, h, inner)
	}
	mr.detail = false
	items := make([]listItem, 0, len(mr.visible))
	for n, i := range mr.visible {
		it := mr.items[i]
		more := a.memMore(it)
		more[0] = matchSegs(more[0], mr.match[n], n == mr.selected)
		items = append(items, listItem{section: sections[suggested(it)], more: more})
	}

	var hints []hintPair
	if i := mr.at(); i < len(mr.items) {
		hints = append(hints, hintPair{"enter", "open"})
		for _, c := range memActions(mr.items[i], len(mr.undo) > 0) {
			verb := strings.ToLower(c.label)
			if c.key == "o" { // the button's label does not fit a footer
				verb = "editor"
			}
			hints = append(hints, hintPair{c.key, verb})
		}
	}
	body := fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		head := [][]hostapi.Segment{mr.filterRow()}
		if len(mr.items) == 0 {
			head = emptyRows("Nothing remembered yet. Type /memory <note> to save one.", inner)
		}
		room := h - 2 - len(stack(tight, slices.Concat(head, oneRow), oneRow)) + 1
		// the text wraps beside the marker's two cells, and beside the bar once
		// the list overflows
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
				list = rows(barText(inner) - 2)
			}
		}
		hintRow := hint(inner, mr.filterHints(slices.Concat(hints, scrolls(mr.view)), "close")...)
		return stack(tight, slices.Concat(head, list), [][]hostapi.Segment{hintRow}), len(items) > 0 && mr.view.viewH < min(mr.view.rows, 3)
	})
	return promptBox("Memory", "", body, w, h, 100, &mr.size)
}

// memDetailBox is row it open: its text whole, then its actions as buttons.
func (a *App) memDetailBox(it hostapi.MemoryRow, w, h, inner int) [][]hostapi.Segment {
	mr := a.memReview
	acts := memActions(it, len(mr.undo) > 0)
	mr.act = min(mr.act, len(acts)-1)
	textW := min(inner, mr.size.w)
	text := func(w int) (rows [][]hostapi.Segment) {
		for _, r := range a.memMore(it) {
			rows = append(rows, hostapi.WrapSegs(r, w)...)
		}
		return rows
	}
	act := func(compact bool) [][]hostapi.Segment { return choiceRows(acts, mr.act, inner, compact) }
	body := scrollChoiceBody(h, inner, &mr.body, text, textW, act, false, pickHint, hintPair{"esc", "back"})
	return promptBox("Memory / "+sanitize.Line(it.Text), "", body, w, h, 100, &mr.size)
}

func memScope(it hostapi.MemoryRow) string {
	if it.Scope == store.ScopeUser {
		return "everywhere"
	}
	return "this project"
}
