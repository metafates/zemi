package ui

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
)

// overlayState is one entry of the overlay stack (DESIGN §8): a Lua (or
// Go-built hostapi.Overlay) plus local list selection.
type overlayState struct {
	o     hostapi.Overlay
	items []listItem // render_item's row when given, else the item's text
	listFilter
	input composerState // text of an input overlay
	body  viewport
	text  textRows // the body's rows
	back  bool     // a detail view: esc and its actions return to the overlay under it
	item  any      // a detail view's item: a reload can swap the parent's under it
	size  boxSize
	// the selected row's danger act waits for a second press; a key or a
	// click that moves the selection drops it
	armed bool
	// the danger list key that waits for its second press, "" for none
	keyArmed string
	// followed is the anchor this following list last moved the region to,
	// so a frame that moved nothing moves it again
	followed int
	// restore is where a following list found the chat, put back when it
	// closes without committing
	restore *followMark
	// the preview pane's rows at paneW cells: the most any item's preview
	// wraps to, 4 at most, so moving or filtering never resizes the box
	paneW, paneN int
	// the list rows as last built and what they were built from: items live
	// as long as the state and visible follows query, so a frame that moved
	// nothing measures no item
	rowsKey listKey
	rows    [][]hostapi.Segment
}

type listKey struct {
	query                      string
	sel, top, room, minW, maxW int
}

// listFilter narrows a list as the user types, through hostapi.Match over
// each row's label and text: visible holds the rows it keeps in list order,
// so sections stay whole, match their labels' matched offsets, and selected
// indexes visible.
type listFilter struct {
	// the list opened with filterMin rows or more; a rebuild keeps it, so a
	// row going or coming never adds or drops the filter row
	filterable    bool
	active        bool // "/" was pressed: keys type into query
	query         string
	labels, texts []string // per row
	visible       []int
	match         [][]int
	selected      int
	view          viewport // the list, or a detail view's actions
}

func (f *listFilter) apply() {
	f.visible, f.match = f.visible[:0], f.match[:0]
	for _, h := range hostapi.Match(f.query, f.labels, f.texts) {
		f.visible, f.match = append(f.visible, h.Index), append(f.match, h.Pos)
	}
	f.selected = max(0, min(f.selected, len(f.visible)-1))
}

// edit applies a filtering key. "/" starts typing a query on a list with
// rows; typed text and the composer's deletion keys edit it; enter keeps it
// and gives the keys back to the list, where "/" types on and esc clears it;
// esc while typing, or backspace on an empty query, clears it too. It
// reports whether the key was used.
func (f *listFilter) edit(key, text string) bool {
	if !f.active {
		switch {
		case text == "/" && len(f.labels) > 0:
			f.active = true
		case key == "esc" && f.query != "":
			f.set("")
		default:
			return false
		}
		return true
	}
	q := composerState{text: f.query, cursor: len([]rune(f.query))}
	switch {
	case key == "enter":
		f.active = false
	case key == "esc" || key == "backspace" && f.query == "":
		f.active = false
		f.set("")
	case key == "backspace":
		q.backspace()
		f.set(q.text)
	case key == "ctrl+w" || key == "alt+backspace":
		q.delWord()
		f.set(q.text)
	case text != "":
		f.set(f.query + text)
	default:
		return false
	}
	return true
}

// set narrows to query: a new query selects its first match, a cleared one
// keeps the selected row.
func (f *listFilter) set(query string) {
	prev := -1
	if f.selected < len(f.visible) {
		prev = f.visible[f.selected]
	}
	changed := query != f.query
	f.query = query
	f.apply()
	if changed {
		f.selected, f.view.top = 0, 0
		if query == "" {
			f.selected = max(0, slices.Index(f.visible, prev))
		}
	}
}

// filterMin is the fewest rows a list filters: a shorter one reads at a
// glance, so it shows no filter row and "/" does nothing there.
const filterMin = 5

// filterRow is the query line above a list, a hint while no query is open
// so opening one adds no row. It is cut to the box's inner width, so a long
// query widens nothing: an active one keeps its end, the cursor included.
func (f *listFilter) filterRow(w int) []hostapi.Segment {
	slash := hostapi.Segment{Text: "/ ", HL: hostapi.ChromeHintKey}
	switch r := []rune(f.query); {
	case f.active:
		return tailSegs(append([]hostapi.Segment{slash}, cursorSegs(r, len(r), hostapi.ChromeBold)...), w)
	case f.query != "":
		return ellipsizeSegs([]hostapi.Segment{slash, {Text: f.query, HL: hostapi.ChromeValue}}, w)
	}
	return []hostapi.Segment{{Text: "/", HL: hostapi.ChromeHintKey}, {Text: " filter", HL: hostapi.ChromeHint}}
}

// filterHints are the footer's keys while a filter types, since every
// one-character key types; esc says what it does to a kept query otherwise.
func (f *listFilter) filterHints(hints []hintPair, esc string) []hintPair {
	switch {
	case f.active:
		return []hintPair{{"enter", "done"}, {"esc", "clear"}}
	case f.query != "":
		esc = "clear"
	}
	return append(hints, hintPair{"esc", esc})
}

// noMatches is how a filtered list with no hit reads, everywhere one reports it.
const noMatches = "No matches."

// emptyRows is how every empty list or view reads: a sentence in the hint
// grey, wrapped to w.
func emptyRows(text string, w int) [][]hostapi.Segment { return wrapText(text, hostapi.ChromeHint, w) }

// openOverlay pushes an overlay, or replaces the open one with its id in
// place; Lua rendered its items when it opened, so the key loop never calls
// Lua for them.
func (a *App) openOverlay(o hostapi.Overlay) {
	if i := a.overlayIndex(o.ID); i >= 0 {
		a.replaceOverlay(i, o)
		a.refreshDetail(i)
		return
	}
	if o.Refresh {
		return
	}
	st := a.overlayState(o)
	// a following list opens where the reader is, not on its first row; any
	// other menu reopens on the row it was left on
	if o.Follow {
		st.selected = a.followRow(&st)
		st.followed = st.selectedAnchor()
		if chat := a.scrolls[a.scrollDef]; chat != nil {
			// a pick whose row the view does not show (a turn's prompt over
			// the tail) is brought into view as any move would
			if from, to := unitRows(chat.lines, st.followed); from < 0 || from < chat.top || to >= chat.top+chat.viewH {
				a.pendingAnchor[a.scrollDef] = st.followed
			}
			st.restore = &followMark{top: chat.top, focus: chat.focus, follow: chat.follow}
		}
	} else if sel := a.lastSel[o.ID]; sel != "" {
		if n := slices.IndexFunc(st.visible, func(idx int) bool { return itemKey(st.o.Items[idx]) == sel }); n >= 0 {
			st.selected = n
		}
	}
	if n := len(a.overlays); n > 0 && !o.Input { // stacked lists line up, as a detail view does
		st.size.w = a.overlays[n-1].size.w
	}
	a.overlays = append(a.overlays, st)
	if o.Confirm == "" {
		return
	}
	top := &a.overlays[len(a.overlays)-1]
	for idx, it := range top.items {
		for n, act := range it.row.Actions {
			if r, _ := hostapi.ParseOverlayItem(act); r.Run == o.Confirm {
				top.selected = max(slices.Index(top.visible, idx), 0)
				// sized as a drawn list, since its detail view opens at its width
				area := boxArea(a.stage, a.width, a.height)
				a.overlayBox(top, area.Dx(), area.Dy())
				a.openDetail(top, idx, n).armed = true
				return
			}
		}
	}
	a.pushToast(slog.LevelError, "Not in "+o.Title, "/"+o.Confirm+" runs it without asking")
}

// followRow is the row a following list opens on: the message the view was
// placed at, if it still is; else the one the view's top row shows, or the
// next one under it, or the last at the tail.
//
// A viewport cannot say which message it is when a centered placement clamps
// at the top or the tail, where many messages share one top row, so the chat
// keeps the row it was placed at (scrollState.focus). The list remembers
// nothing: any move the reader makes by hand drops that place.
func (a *App) followRow(st *overlayState) int {
	n := len(st.visible)
	if n == 0 {
		return 0
	}
	chat := a.scrolls[a.scrollDef]
	if chat == nil {
		return n - 1
	}
	// the place a jump left holds only while the view sits where it left it:
	// any scroll moves the top, and the view's middle names the message then,
	// so a message the reader scrolled away from the middle stops being the
	// pick
	if chat.focus > 0 && chat.top == chat.placed {
		for k, idx := range st.visible {
			if st.items[idx].row.Anchor == chat.focus {
				return k
			}
		}
	}
	// the view shows the end of the transcript: the reader is at the newest
	// message, whichever way the region got there
	if chat.follow || chat.top+chat.viewH >= chat.rows {
		return n - 1
	}
	// the message over the middle of the view, else the last one above it: a
	// blank line or a run of calls is no message the list names, and one the
	// pick centered still holds after a wheel line or two
	mid := chat.top
	if chat.viewH > 0 {
		mid += (chat.viewH - 1) / 2
	}
	above := -1
	for k, idx := range st.visible {
		from, to := unitRows(chat.lines, unitAt(chat.lines, st.items[idx].row.Anchor))
		switch {
		case from < 0:
		case from > mid:
			// no later message can cover the middle
			if above < 0 {
				return k
			}
			return above
		case to >= mid:
			return k
		default:
			above = k
		}
	}
	if above >= 0 {
		return above
	}
	return n - 1
}

// selectedAnchor is the transcript row the selected item stands for, 0 when
// it names none.
func (st *overlayState) selectedAnchor() int {
	if st.selected >= len(st.visible) {
		return 0
	}
	return st.items[st.visible[st.selected]].row.Anchor
}

// syncFollow moves the stage's scrolled region to the row the top following
// list's selection names, once per selection change: keys, clicks and the
// wheel all land here, and the paint carries it out. Its selection is also
// the message the paint bands.
func (a *App) syncFollow() {
	a.highlight = 0
	i := len(a.overlays) - 1
	if i < 0 || !a.overlays[i].o.Follow {
		return
	}
	st := &a.overlays[i]
	if a.highlight = st.selectedAnchor(); a.highlight == 0 {
		return
	}
	if a.highlight != st.followed {
		st.followed = a.highlight
		a.pendingAnchor[a.scrollDef] = a.highlight
	}
}

// overlayIndex is the stack index of the open list or view with id, -1 when
// none is open.
func (a *App) overlayIndex(id string) int {
	return slices.IndexFunc(a.overlays, func(st overlayState) bool { return st.o.ID == id && !st.back })
}

// replaceOverlay swaps o in at stack index i, keeping the filter and the
// box's size, the scroll while the body is the same, and the selection on
// its row wherever the rebuild moved it, else at its place.
func (a *App) replaceOverlay(i int, o hostapi.Overlay) {
	old := a.overlays[i]
	// read before apply, which reuses old.visible's array
	sel, was := "", old.selectedRow()
	if old.selected < len(old.visible) {
		sel = itemKey(old.o.Items[old.visible[old.selected]])
	}
	st := a.overlayState(o)
	f := old.listFilter
	f.labels, f.texts = st.labels, st.texts
	st.listFilter, st.size = f, old.size
	// a live body moves under the reader as its work writes: the view stays
	// where they scrolled, and on its tail while they did not
	if st.o.Live || st.o.Body == old.o.Body && slices.Equal(st.o.Parts, old.o.Parts) {
		st.body = old.body
	}
	st.apply()
	if n := slices.IndexFunc(st.visible, func(idx int) bool { return sel != "" && itemKey(st.o.Items[idx]) == sel }); n >= 0 {
		st.selected = n
	}
	st.armed = old.armed && sameDanger(was, st.selectedRow())
	st.followed, st.restore = old.followed, old.restore
	// the same danger key running the same command keeps its press
	if k := slices.IndexFunc(st.o.Keys, func(k hostapi.OverlayKey) bool { return k.Key == old.keyArmed }); k >= 0 && st.o.Keys[k].Danger &&
		slices.ContainsFunc(old.o.Keys, func(o hostapi.OverlayKey) bool { return o.Key == st.o.Keys[k].Key }) {
		st.keyArmed = old.keyArmed
	}
	a.overlays[i] = st
}

// selectedRow is the selected row's fields, zero when nothing is selected.
func (st *overlayState) selectedRow() hostapi.OverlayItem {
	if st.selected >= len(st.visible) {
		return hostapi.OverlayItem{}
	}
	return st.items[st.visible[st.selected]].row
}

// sameDanger reports whether the selected row n is still the danger row o,
// so a rebuild keeps the press it waits for.
func sameDanger(o, n hostapi.OverlayItem) bool {
	return n.Danger && n.Run == o.Run && cmp.Or(n.ID, n.Text) == cmp.Or(o.ID, o.Text)
}

// refreshDetail rebuilds the detail view open over list i from its item
// in the list's new items, keeping the selected action and the scroll, or
// drops it with its item: a screen a Go job refreshes (a server starting, a
// run moving) would otherwise show a stale detail.
func (a *App) refreshDetail(i int) {
	j := i + 1
	if j >= len(a.overlays) || !a.overlays[j].back || a.overlays[j].o.ID != a.overlays[i].o.ID+"#detail" {
		return
	}
	d := a.overlays[j]
	idx := slices.IndexFunc(a.overlays[i].o.Items, func(it any) bool { return itemKey(it) == itemKey(d.item) })
	if idx < 0 {
		a.overlays = slices.Delete(a.overlays, j, j+1)
		return
	}
	n := a.detailState(&a.overlays[i], idx)
	n.selected, n.view, n.body, n.size = min(d.selected, max(0, len(n.visible)-1)), d.view, d.body, d.size
	n.armed = d.armed && sameDanger(d.selectedRow(), n.selectedRow())
	a.overlays[j] = n
}

// itemKey names an item across rebuilds: its id, else its text.
func itemKey(item any) string {
	r, _ := hostapi.ParseOverlayItem(item)
	return cmp.Or(r.ID, r.Text)
}

func (a *App) overlayState(o hostapi.Overlay) overlayState {
	// Every string here is Lua's (or a model's, through a Go builder):
	// sanitize like widget text.
	o.Title, o.Body, o.Empty = sanitize.Line(o.Title), sanitize.String(o.Body), sanitize.String(o.Empty)
	for _, row := range o.BodySegs { // drawn in Body's place, so it needs Body's scrubbing
		for i := range row {
			// only text and hl: a click id or effect here would act as Go chrome's
			row[i] = hostapi.Segment{Text: sanitize.String(row[i].Text), HL: row[i].HL}
		}
	}
	// only builtin and user lists run commands, so a plugin's label cannot
	// disguise one
	for i := range o.Keys { // a desc keeps its spaces, as a row's does: "pause " is as wide as "resume"
		o.Keys[i].Key, o.Keys[i].Desc = sanitize.Line(o.Keys[i].Key), oneLine.Replace(sanitize.String(o.Keys[i].Desc))
	}
	// an item's fields reach the screen only as its detail view's, which is here
	o.Fields = slices.Clone(o.Fields)
	for i, f := range o.Fields {
		o.Fields[i] = hostapi.Field{Name: sanitize.Line(f.Name), Value: sanitize.String(f.Value)}
	}
	st := overlayState{o: o, filterable: len(o.Items) >= filterMin}
	if o.Input {
		st.input.insert(sanitize.String(o.Default))
		return st
	}
	for i, item := range o.Items {
		// zemi.ui.overlay.open raised on a Lua item's wrong type; a Go screen's
		// or a select's item keeps the fields that parse
		r, _ := hostapi.ParseOverlayItem(item)
		// a desc keeps its runs of spaces: padded columns (a status word) stay put
		r.Desc, r.Key, r.Section, r.Verb = oneLine.Replace(sanitize.String(r.Desc)), sanitize.Line(r.Key), sanitize.Line(r.Section), sanitize.Line(r.Verb)
		r.Hotkey = sanitize.Line(r.Hotkey)
		r.Fill, r.Copy, r.Preview = sanitize.String(r.Fill), sanitize.String(r.Copy), sanitize.String(r.Preview)                          // Detail is the detail view's Body, sanitized there
		it := listItem{label: sanitize.Line(r.Text), desc: r.Desc, key: tagSeg(r.Key), section: r.Section, mark: markSeg(r.Mark), row: r} // a newline would break the box
		if r.Switch || r.Locked {
			it.key = switchSeg(r.On, r.Locked)
		}
		if i < len(o.ItemText) && o.ItemText[i] != nil {
			// only text and hl: a click id or effect here would act as Go chrome's
			for _, seg := range o.ItemText[i] {
				it.segs = append(it.segs, hostapi.Segment{Text: oneLine.Replace(sanitize.String(seg.Text)), HL: hostapi.UserHL(seg.HL)})
			}
			it.label = hostapi.SegsText(it.segs) // spaces kept: a filter's offsets index the segments
		} else if r.Danger {
			it.segs = []hostapi.Segment{{Text: it.label, HL: hostapi.ChromeDanger}}
		}
		st.items = append(st.items, it)
		st.labels = append(st.labels, it.label)
		st.texts = append(st.texts, it.section+"  "+it.desc+"  "+r.Copy+"  "+r.Preview)
	}
	if o.Phases {
		marks := map[string][]string{}
		for _, it := range st.items {
			marks[it.section] = append(marks[it.section], it.row.Mark)
		}
		for i, it := range st.items {
			st.items[i].head = markSeg(phaseMark(marks[it.section]))
		}
	}
	st.selected = o.Selected
	st.body.follow = o.Live // a live body opens on its tail
	st.apply()
	return st
}

// actionAt is the index of the selected row's action whose hotkey is key,
// -1 in a detail view, with no row, or when none has it.
func (st *overlayState) actionAt(key string) int {
	if st.back || len(st.visible) == 0 || len([]rune(key)) != 1 {
		return -1
	}
	return slices.IndexFunc(st.items[st.visible[st.selected]].row.Actions, func(a any) bool {
		r, _ := hostapi.ParseOverlayItem(a)
		return sanitize.Line(r.Hotkey) == key
	})
}

var oneLine = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

// enterVerb names what enter does on item idx, one word per act in every
// list: "open" a detail view or another screen, "pick" a value, "run" a
// command, "toggle" a toggle list's row, "fill" the composer, "copy", "back"
// from a detail view, or "" when it does nothing. A row's own verb, else the
// overlay's, names its run, select or fill where the default would not; a
// row with one is not a toggle: space passes it by in a toggle list.
func (st *overlayState) enterVerb(idx int) string {
	r := st.items[idx].row
	switch {
	case hasDetail(r) && st.o.Follow:
		// the list already moved the chat to the row: enter keeps the place
		return "close"
	case hasDetail(r):
		return "open"
	case r.Run != "" || st.o.OnSelect != nil || st.back && r.ID != "" && st.o.OnAction != nil:
		act := "run"
		if st.o.Reload != nil {
			act = "toggle"
		} else if st.o.OnSelect != nil {
			act = "pick"
		}
		return cmp.Or(r.Verb, st.o.Verb, act)
	case r.Fill != "":
		return cmp.Or(r.Verb, "fill")
	case r.Copy != "":
		if st.o.Follow {
			// the list already scrolled the chat there: enter keeps it, and y
			// is what copies
			return "close"
		}
		return "copy"
	case st.back:
		return "back"
	}
	return ""
}

func hasDetail(r hostapi.OverlayItem) bool {
	return r.Detail != "" || r.Fields != nil || r.Actions != nil
}

// actionKey is the key that acts on visible row n: the item's own hotkey,
// and in a detail view, whose rows are actions, its position when it has
// none, so every action reads as a button with a key.
func (st *overlayState) actionKey(n int) string {
	if k := st.items[st.visible[n]].row.Hotkey; k != "" || !st.back {
		return k
	}
	return strconv.Itoa(n + 1)
}

// openDetail pushes the detail view of item idx with action n selected: its
// detail text as the body and its actions as the list, under the parent's
// origin so the badge and the run gate stay the parent's.
func (a *App) openDetail(parent *overlayState, idx, n int) *overlayState {
	a.overlays = append(a.overlays, a.detailState(parent, idx))
	d := &a.overlays[len(a.overlays)-1]
	d.selected = n
	return d
}

func (a *App) detailState(parent *overlayState, idx int) overlayState {
	r := parent.items[idx].row
	title := parent.items[idx].label
	if parent.o.Title != "" {
		title = parent.o.Title + " / " + title
	}
	p := parent.o
	st := a.overlayState(hostapi.Overlay{ID: p.ID + "#detail", Title: title, Origin: p.Origin, Path: p.Path, Gen: p.Gen, Anchor: p.Anchor, Body: r.Detail, BodyMD: p.BodyMD, Fields: r.Fields, Mark: r.Mark, Items: r.Actions, OnAction: p.OnAction, Follow: p.Follow})
	// opening a view is navigation: it keeps the list's width and takes its
	// own height, which it then keeps while open
	st.back, st.item, st.size = true, parent.o.Items[idx], boxSize{w: parent.size.w}
	return st
}

// detailAction runs action idx of the detail view on top, then closes it.
// An action with an id goes to on_action; one that runs or fills something
// ends the parent too, unless a toggle list runs it, which rebuilds as the
// detail view closes; one that does none of it goes back.
func (a *App) detailAction(idx int) {
	d := a.overlays[len(a.overlays)-1]
	p := a.overlays[len(a.overlays)-2].o // a detail view opens right on top of its parent
	r := d.items[idx].row
	switch {
	case r.ID != "" && d.o.OnAction != nil:
		a.background(func() { a.host.OverlayAction(context.Background(), p, d.item, d.o.Items[idx]) })
		if p.Reload == nil {
			a.closeOverlayByID(p.ID) // the detail view goes with it
			return
		}
	case r.Run == "" && r.Fill == "":
		a.overlays = a.overlays[:len(a.overlays)-1] // back, with nothing to rebuild
		return
	case r.Fill == "" && p.Reload != nil:
		a.runOverlayRow(r)
	default:
		a.runOverlayRow(r)
		a.cancelOverlay(p)
		return
	}
	a.closeOverlayByID(d.o.ID)
}

// reloadOverlay runs call (nil for none) and rebuilds a toggle list's items,
// both on the worker, then swaps the items in while it is still open,
// keeping selection and filter.
func (a *App) reloadOverlay(o hostapi.Overlay, call func()) {
	a.background(func() {
		if call != nil {
			call()
		}
		items, text := o.Reload()
		a.do(func() {
			if i := a.overlayIndex(o.ID); i >= 0 && a.overlays[i].o.Gen == o.Gen {
				next := a.overlays[i].o
				next.Items, next.ItemText = items, text
				a.replaceOverlay(i, next)
			}
		})
	})
}

// closeOverlayByID closes an overlay and the detail views over it. A toggle
// list it uncovers rebuilds on the worker, so callers act before they close:
// the rebuild then shows what the closed overlay changed.
func (a *App) closeOverlayByID(id string) {
	closes := func(st overlayState) bool { return st.o.ID == id || strings.HasPrefix(st.o.ID, id+"#detail") }
	for _, st := range a.overlays { // where each closed list sat, so coming back reopens on its row
		if closes(st) && !st.back && st.selected < len(st.visible) {
			a.lastSel[st.o.ID] = itemKey(st.o.Items[st.visible[st.selected]])
		}
	}
	top := len(a.overlays) > 0 && closes(a.overlays[len(a.overlays)-1])
	a.overlays = slices.DeleteFunc(a.overlays, closes)
	if n := len(a.overlays); top && n > 0 && a.overlays[n-1].o.Reload != nil {
		a.reloadOverlay(a.overlays[n-1].o, nil)
	}
}

// overlayKey routes keys to the topmost overlay; text is what the key types
// (printable keys filter the list).
func (a *App) overlayKey(key, text string) tea.Cmd {
	st := &a.overlays[len(a.overlays)-1]
	o := st.o
	if o.Input {
		a.inputKey(st, key, text)
		return nil
	}
	a.bump()
	confirmed, swallowed := pressArmed(&st.armed, key, func() string { return st.actionKey(st.selected) })
	keyArmed := st.keyArmed
	if st.keyArmed = ""; swallowed || keyArmed != "" && key == "esc" {
		return nil
	}
	if key == "space" && o.Reload != nil && !st.active {
		text = "" // space toggles rows in a toggle list, and types into an open filter
	}
	// a filter takes its keys first; then a body that overflows takes the
	// scroll keys, and the list keys too when there is no list to move in
	if !st.back && st.filterable && st.edit(key, text) || st.body.overflows() && scrollKey(&st.body, key, len(o.Items) == 0) ||
		moveSel(&st.selected, len(st.visible), st.view.pageItems(len(st.visible)), key) {
		return nil
	}
	// a row's key acts like enter on it; one printable character, so no
	// overlay can take esc or ctrl+c
	for n := range st.visible {
		if len([]rune(key)) == 1 && st.actionKey(n) == key {
			st.selected, key = n, "enter"
			break
		}
	}
	// a view opened from a rail steps along it in place
	if run := map[string]string{"ctrl+j": o.Next, "ctrl+k": o.Prev}[key]; run != "" {
		a.closeOverlayByID(o.ID)
		a.runOverlayRow(hostapi.OverlayItem{Run: run})
		return nil
	}
	declared := slices.IndexFunc(o.Keys, func(k hostapi.OverlayKey) bool { return k.Key == key })
	if i := declared; i >= 0 && !keyApplies(o.Keys[i], st) {
		declared = -1 // a row this key does not act on
	}
	if i := declared; i >= 0 && (o.Keys[i].Run != "" || o.Keys[i].Fn != nil || o.Keys[i].Act != nil) {
		if o.Keys[i].Danger && keyArmed != key {
			st.keyArmed = key
			return nil
		}
		if act := o.Keys[i].Act; act != nil {
			item := st.selectedRow()
			call := func() { act(item) }
			if o.Reload != nil { // the screen keeps its place and rebuilds
				a.reloadOverlay(o, call)
				return nil
			}
			// the box closes before the work, so a screen it opens is left open
			a.closeOverlayByID(o.ID)
			a.background(call)
			return nil
		}
		if run := o.Keys[i].Run; run != "" {
			a.runOverlayRow(hostapi.OverlayItem{Run: run})
			if o.Reload != nil {
				a.reloadOverlay(o, nil)
			}
			return nil
		}
		var item any
		if len(st.visible) > 0 {
			item = o.Items[st.visible[st.selected]]
		}
		call := func() { a.host.OverlayKey(context.Background(), o, o.Keys[i], item) }
		if o.Reload != nil { // a toggle list stays open and rebuilds
			a.reloadOverlay(o, call)
			return nil
		}
		a.background(call)
		a.closeOverlayByID(o.ID)
		return nil
	}
	// an action's key acts on the selected row as its button does; a danger
	// one opens the detail on it armed, so the view is its confirmation
	if n := st.actionAt(key); n >= 0 {
		d := a.openDetail(st, st.visible[st.selected], n)
		r := d.items[n].row
		if r.Danger {
			d.armed = true
			return nil
		}
		cmd := a.overlayKey("enter", "")
		if hasDetail(r) { // the action's own view: esc returns to the list
			a.overlays = slices.Delete(a.overlays, len(a.overlays)-2, len(a.overlays)-1)
		}
		return cmd
	}
	if declared >= 0 && len(st.visible) > 0 {
		a.pushToast(slog.LevelWarn, "Cannot "+o.Keys[declared].Desc, st.items[st.visible[st.selected]].row.Detail)
		return nil
	}
	switch key {
	case "y", "Y":
		return a.copyOverlay(st, key == "Y")
	case "enter", "space":
		if len(st.visible) == 0 {
			return nil
		}
		idx := st.visible[st.selected]
		r := st.items[idx].row
		verb := st.enterVerb(idx)
		sel := func() { a.host.SelectOverlay(context.Background(), o, idx) }
		switch {
		case key == "space" && !confirmed && (o.Reload == nil || r.Verb != ""):
		case key == "enter" && o.Follow && hasDetail(r):
			// a following list already moved the chat to the row found:
			// enter keeps that place, and l is the way into the row
			a.closeOverlayByID(o.ID)
		case key == "enter" && hasDetail(r) && !(r.Verb == "open" && r.Run != ""):
			// a row that says it opens a screen opens it; its actions keep
			// their own keys
			a.openDetail(st, idx, 0)
		case r.Danger && !confirmed && !(st.back && len(st.visible) == 1):
			// a detail view whose only action it is states the effect and holds
			// nothing a stray key hits: it is the confirmation
			st.armed = true
		case verb == "copy" && r.Copy != "":
			// a copy row's value goes to the clipboard and the list stays open
			return a.copied(st.value(idx))
		case st.back:
			a.detailAction(idx)
		case o.Reload != nil && r.Fill == "":
			a.runOverlayRow(r)
			a.reloadOverlay(o, sel)
		case o.Reload != nil: // a fill hands the keys to the composer, so the list closes
			a.runOverlayRow(r)
			if o.OnSelect != nil {
				a.background(sel)
			}
			a.closeOverlayByID(o.ID)
		case verb == "":
		case r.Verb == "open" && o.OnSelect == nil:
			a.runOverlayRow(r) // the screen it opens goes over this list, so esc comes back
		default:
			if o.OnSelect != nil {
				a.background(sel)
			} else {
				a.runOverlayRow(r)
			}
			a.closeOverlayByID(o.ID)
		}
	case "l":
		// one level in: a row's actions where it has them, enter's act else
		if st.back || len(st.visible) == 0 {
			return nil
		}
		if hasDetail(st.items[st.visible[st.selected]].row) {
			a.openDetail(st, st.visible[st.selected], 0)
			return nil
		}
		return a.overlayKey("enter", "")
	case "h":
		// one level back, a view stacked over another only: at the root esc
		// is the way out, and h types into a filter as it always did
		if len(a.overlays) == 1 {
			return nil
		}
		fallthrough
	case "esc":
		if st.back {
			a.overlays = a.overlays[:len(a.overlays)-1] // back, with nothing to rebuild
			return nil
		}
		a.cancelOverlay(o)
	}
	return nil
}

// disarmIfMoved returns what drops the top overlay's arm once a click, a
// wheel or a bar drag moved its selection, as any other key does, so moving
// back never finds the row armed. A click or a wheel (acts) drops an armed
// list key at once, as any other key does.
func (a *App) disarmIfMoved(acts bool) func() {
	if n := len(a.overlays); acts && n > 0 {
		a.overlays[n-1].keyArmed = ""
	}
	i := len(a.overlays) - 1
	if i < 0 || !a.overlays[i].armed {
		return func() {}
	}
	sel := a.overlays[i].selected
	return func() {
		if i < len(a.overlays) && a.overlays[i].selected != sel {
			a.overlays[i].armed = false
		}
	}
}

// copyOverlay copies the text of a view, its fields as "name: value" lines
// before its body, or the selected row (all rows): an error or a diagnostic
// is worth nothing if it cannot leave the terminal.
func (a *App) copyOverlay(st *overlayState, all bool) tea.Cmd {
	var lines []string
	for _, f := range st.o.Fields {
		if f.Value != "" {
			lines = append(lines, f.Name+": "+f.Value)
		}
	}
	if lines != nil && st.o.Body != "" {
		lines = append(lines, "")
	}
	if st.o.Body != "" {
		lines = append(lines, st.o.Body)
	}
	switch {
	case lines != nil && !all:
	case all:
		lines = nil
		for _, i := range st.visible {
			lines = append(lines, st.value(i))
		}
	case len(st.visible) > 0:
		lines = []string{st.value(st.visible[st.selected])}
	}
	if len(lines) == 0 {
		return nil
	}
	return a.copied(strings.Join(lines, "\n"))
}

// value is what y copies of item i: its copy field, else its row.
func (st *overlayState) value(i int) string {
	if s := st.items[i].row.Copy; s != "" {
		return s
	}
	return strings.TrimSpace(st.items[i].label + "  " + st.items[i].desc)
}

// inputKey edits an input overlay's line; enter submits, esc cancels. A Go
// prompt's command runs before the close, so a list under it rebuilds with
// what it changed.
func (a *App) inputKey(st *overlayState, key, text string) {
	in := &st.input
	switch key {
	case "enter":
		o, value := st.o, in.text
		if o.Run != "" {
			a.runCommand(hostapi.SplitCommand(o.Run + " " + value))
		} else {
			a.background(func() { a.host.SubmitOverlayInput(context.Background(), o, value) })
		}
		a.closeOverlayByID(o.ID)
	case "esc":
		a.cancelOverlay(st.o)
	default:
		in.typeKey(key, text)
	}
	a.bump()
}

// cancelOverlay closes o and runs its on_close. The overlay's Lua
// callbacks run on the background worker: the key loop never waits on Lua.
func (a *App) cancelOverlay(o hostapi.Overlay) {
	// esc cancels a browse: a following list puts the chat back where it
	// found it, while enter closes and keeps the place
	if i := a.overlayIndex(o.ID); i >= 0 {
		a.unfollow(a.overlays[i])
	}
	if o.OnClose != nil {
		a.background(func() { a.host.CancelOverlay(context.Background(), o) })
	}
	a.closeOverlayByID(o.ID)
	a.bump()
}

// followMark is where a following list found the chat, put back when it
// closes without committing.
type followMark struct {
	top    int
	focus  int
	follow bool
}

// unfollow puts the chat back where a following list found it: esc and a
// click outside cancel the browse, never the reader's place.
func (a *App) unfollow(st overlayState) {
	if !st.o.Follow || st.restore == nil {
		return
	}
	chat := a.scrolls[a.scrollDef]
	if chat == nil {
		return
	}
	// rows 0 skips the re-anchor the next paint would do: this place is kept
	// as it was, not re-anchored on what the jump left behind
	chat.top, chat.focus, chat.follow, chat.rows = st.restore.top, st.restore.focus, st.restore.follow, 0
	delete(a.pendingAnchor, a.scrollDef)
}

// cancelMenu cancels the top overlay's whole menu, not only a detail view
// drawn over it.
func (a *App) cancelMenu() {
	i := len(a.overlays) - 1
	for i > 0 && a.overlays[i].back {
		i--
	}
	a.cancelOverlay(a.overlays[i].o)
}

// runOverlayRow applies a row's "fill" (into the composer, like tab in the
// palette) and "run", which only builtin and user overlays keep.
func (a *App) runOverlayRow(r hostapi.OverlayItem) {
	if r.Fill != "" {
		a.composer.text = r.Fill
		a.composer.moveEnd()
	}
	if r.Run != "" {
		name, args := hostapi.SplitCommand(r.Run)
		a.runCommand(name, args)
	}
}

// listRowsMax is the most rows a list takes before it scrolls: a menu that
// fills the screen hides what it is a menu over.
const listRowsMax = 20

// listRowsCap is that cap, at most three fifths of the screen and never so
// few that nothing is a list.
func listRowsCap(h int) int { return max(min(h-4, 3*h/5, listRowsMax), 3) }

// overlayBox draws a Lua overlay with the Go-stamped origin badge (DESIGN
// §8): the body, then the list, then the keys that act on the selection.
// h is the height the box may take.
func (a *App) overlayBox(st *overlayState, w, h int) [][]hostapi.Segment {
	badge := a.host.OriginBadge(st.o.Origin, st.o.Path)
	inner := boxInner(w)
	if st.o.Input {
		body := [][]hostapi.Segment{st.input.inputRow(min(inner, st.size.w)), nil, hint(inner, sendHint, hintPair{"esc", "close"})}
		return promptBox(st.o.Title, badge, body, w, h, 100, &st.size)
	}
	// a list with previews reads as text; a detail view widens to its
	// breadcrumb title where the screen has room, and a field repeating the
	// title's item (a project's path) shows only when the title is cut
	previews := !st.back && slices.ContainsFunc(st.items, func(it listItem) bool { return it.row.Preview != "" })
	fields := st.o.Fields
	if st.back {
		title := ansi.StringWidth(st.o.Title) + 1 + map[bool]int{true: ansi.StringWidth(badge) + 2}[badge != ""]
		st.size.w = max(st.size.w, min(title, inner))
		if title <= inner {
			fields = slices.DeleteFunc(slices.Clone(fields), func(f hostapi.Field) bool { return strings.HasSuffix(st.o.Title, " / "+f.Value) })
		}
	} else {
		st.size.w = max(st.size.w, map[bool]int{true: listBoxW, false: textBoxW}[len(st.o.Items) > 0 && !previews])
	}
	if st.o.Wide {
		st.size.w = max(st.size.w, inner)
	}
	bodyW := min(inner, st.size.w)
	if previews && st.paneW != bodyW {
		st.paneW, st.paneN = bodyW, 0
		for _, it := range st.items {
			if it.row.Preview != "" {
				st.paneN = max(st.paneN, min(len(wrapText(it.row.Preview, "", bodyW)), 4))
			}
		}
	}
	buildBody := st.text.at(func(w int) (rows [][]hostapi.Segment) {
		if rows = fieldRows(fields, st.o.Mark, w); rows != nil && (st.o.Body != "" || len(st.o.Parts) > 0) {
			rows = append(rows, nil)
		}
		switch {
		case len(st.o.Parts) > 0:
			for i, p := range st.o.Parts {
				if i > 0 {
					rows = append(rows, nil)
				}
				rows = append(rows, a.bodyPart(p, w)...)
			}
		case st.o.BodySegs != nil:
			for _, row := range st.o.BodySegs {
				rows = append(rows, hostapi.WrapSegs(row, w)...)
			}
		case st.o.Body == "":
		case st.o.BodyMD:
			rows = append(rows, a.host.Markdown(strings.TrimRight(st.o.Body, "\n"), w)...)
		default:
			rows = append(rows, wrapText(st.o.Body, "", w)...)
		}
		return rows
	})
	bodyN := 0
	if st.o.Body != "" || len(st.o.Parts) > 0 || st.o.Fields != nil {
		bodyN = len(buildBody(bodyW))
	}
	tail := 1 // the rows the box's padding goes above: the footer, and a preview pane
	body := fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		var head, list [][]hostapi.Segment // the filter row, and the list's rows
		var hints []hintPair
		// the list's and the body's viewports share what the other rows leave
		bodyPH, listPH, fixed := oneRow[:min(bodyN, 1)], oneRow, [][]hostapi.Segment(nil)
		switch {
		case st.back && len(st.o.Items) > 0:
		case len(st.o.Items) > 0:
			if st.filterable {
				head = [][]hostapi.Segment{st.filterRow(bodyW)}
			}
			if len(st.visible) == 0 {
				listPH, fixed = nil, emptyRows(noMatches, inner)
			}
		case bodyN == 0 || st.o.Empty != "":
			listPH, fixed = nil, emptyRows(cmp.Or(st.o.Empty, "Nothing here."), inner)
		default:
			listPH = nil
		}
		// the selected row's preview, in rows every row's fits
		var pane [][]hostapi.Segment
		if previews {
			pane = make([][]hostapi.Segment, map[bool]int{true: 1, false: st.paneN}[level > 1])
			if len(st.visible) > 0 {
				copy(pane, capRows(wrapText(st.items[st.visible[st.selected]].row.Preview, hostapi.ChromeHint, bodyW), len(pane), bodyW))
			}
		}
		room := h - 2 - len(stack(tight, bodyPH, slices.Concat(head, listPH, fixed), pane, oneRow)) + len(bodyPH) + len(listPH)
		limit := min(max(room-min(bodyN, 3), 1), listRowsCap(h)) // the list's rows; the body keeps three
		enterCopies := false
		switch {
		case st.back && len(st.o.Items) > 0:
			// a detail view's rows are actions: draw them as the prompts' buttons
			choices := make([]promptChoice, 0, len(st.visible))
			for n, i := range st.visible {
				// an action armed without danger (Overlay.Confirm) reads as a danger one
				armed := st.armed && n == st.selected
				choices = append(choices, promptChoice{key: st.actionKey(n), label: st.items[i].label, desc: st.items[i].desc, hl: map[bool]string{true: hostapi.ChromeDanger}[st.items[i].row.Danger || armed]})
			}
			st.view.viewH = limit
			list = choiceView(choices, st.selected, &st.view, inner, level > 1)
			if len(st.visible) > 0 {
				hints = append(hints, pickHint)
			}
		case len(st.visible) > 0:
			if key := (listKey{query: st.query, sel: st.selected, top: st.view.top, room: limit, minW: st.size.w, maxW: inner}); st.rows == nil || key != st.rowsKey {
				items := make([]listItem, 0, len(st.visible))
				for n, i := range st.visible {
					it := st.items[i]
					it.match = st.match[n]
					items = append(items, it)
				}
				st.rows = listRowsW(items, st.items, st.selected, &st.view, limit, st.size.w, inner)
				key.top = st.view.top
				st.rowsKey = key
			}
			list = st.rows
			verb := st.enterVerb(st.visible[st.selected])
			if verb != "" {
				hints = append(hints, hintPair{"enter", verb})
				enterCopies = verb == "copy"
			}
			r := st.items[st.visible[st.selected]].row
			if verb != "toggle" && st.o.Reload != nil && r.Verb == "" && (st.o.OnSelect != nil || r.Run != "") {
				hints = append(hints, hintPair{"space", "toggle"})
			}
			if hasDetail(r) && verb == "close" {
				// enter keeps the place here, so l is the way into the row
				hints = append(hints, hintPair{"l", "actions"})
			}
		}
		// a key with a Run or Fn acts with no row too, so an empty list names
		// it; one naming its rows' action waits for a row
		last := -1
		for i, k := range st.o.Keys {
			if k.Run == "" && len(st.visible) == 0 || !keyApplies(k, st) {
				continue // it acts on the row the list has not got
			}
			if last >= 0 && k.Desc == st.o.Keys[last].Desc { // keys sharing a desc read as one hint: +/- at once
				hints[len(hints)-1].key += "/" + k.Key
				continue
			}
			last = i
			hints = append(hints, hintPair{k.Key, k.Desc})
		}
		var body [][]hostapi.Segment
		if bodyN > 0 {
			st.body.viewH = max(room-len(list), 1)
			body = st.body.window(buildBody(bodyW), bodyW, boxFade)
		}
		// y says nothing enter does not
		if (bodyN > 0 || len(st.visible) > 0) && !enterCopies {
			hints = append(hints, hintPair{"y", "copy"})
		}
		if st.o.Next != "" || st.o.Prev != "" {
			hints = append(hints, hintPair{"ctrl+j/k", "next/prev"})
		}
		scroll := scrolls(st.body, st.view)
		if scroll == nil && st.o.Reload != nil {
			scroll = []hintPair{scrollGap}
		}
		// a following list's esc cancels the browse: it is not closing, it
		// is putting the chat back
		esc := map[bool]string{true: "back", false: "close"}[st.back]
		if st.o.Follow {
			esc = "back"
		}
		hints = st.filterHints(append(hints, scroll...), esc)
		foot := hint(inner, hints...)
		if st.armed {
			foot = armedHint(inner, cmp.Or(st.actionKey(st.selected), "enter"), st.items[st.visible[st.selected]].label)
		}
		if i := slices.IndexFunc(st.o.Keys, func(k hostapi.OverlayKey) bool { return k.Key == st.keyArmed }); i >= 0 {
			foot = armedHint(inner, st.keyArmed, st.o.Keys[i].Desc)
		}
		tail = len(stack(tight, pane, oneRow))
		rows := stack(tight, body, slices.Concat(head, list, fixed), pane, [][]hostapi.Segment{foot})
		return rows, bodyN > 0 && st.body.viewH < min(bodyN, 3) || len(listPH) > 0 && st.view.viewH < min(st.view.rows, 3)
	})
	// a filter that shortens the list moves the preview pane no more than the footer
	if n := min(st.size.h, h) - len(body) - 2; n > 0 && tail > 1 {
		body = slices.Insert(body, len(body)-tail, make([][]hostapi.Segment, n)...)
	}
	return promptBox(st.o.Title, badge, body, w, h, 100, &st.size)
}

// bodyPart draws one part of a view's body in the form its text is written
// in, wrapped to w: a command reads as a well of the canvas, as the prompt's
// preview does, while a diff and markdown bring their own surfaces.
func (a *App) bodyPart(p hostapi.BodyPart, w int) [][]hostapi.Segment {
	switch p.Lang {
	case "markdown":
		return a.host.Markdown(strings.TrimRight(p.Text, "\n"), w)
	case "diff":
		return hostapi.DiffRows(p.Text, w)
	case "":
		return wrapText(p.Text, "", w)
	}
	return a.codePanel(p.Text, p.Lang, w)
}

// keyApplies reports whether a list key acts on the selected row: a key
// whose Applies refuses it is neither hinted nor run.
func keyApplies(k hostapi.OverlayKey, st *overlayState) bool {
	return k.Applies == nil || k.Applies(st.selectedRow())
}

// pressArmed is key on a screen whose selected danger button may be armed:
// enter, space and the button's key confirm it; any other key drops the arm,
// and esc does only that. button is read only while armed, when a row is
// selected.
func pressArmed(armed *bool, key string, button func() string) (confirmed, swallowed bool) {
	if !*armed {
		return false, false
	}
	if key == "enter" || key == "space" || key == button() {
		return true, false
	}
	*armed = false
	return false, key == "esc"
}

// armedHint is the footer of a press that waits for a second one: what the
// next press does, in the danger color, with the keys drawn as keys.
func armedHint(w int, key, label string) []hostapi.Segment {
	foot := hint(w, hintPair{key, "again to " + strings.ToLower(label)}, hintPair{"esc", "cancel"})
	foot[1].HL = hostapi.ChromeDanger
	return foot
}

// phaseMark is a phase's state from its rows' marks: failed once one
// failed, else running while one runs, waiting while one waits, stopped
// when one stopped, done when every row is.
func phaseMark(marks []string) string {
	for _, m := range []string{"err", "spin", "wait", "stop"} {
		if slices.Contains(marks, m) {
			return m
		}
	}
	return "ok"
}

// statusHL colors the status words every screen shares.
//
//exhaustive:enforce
var statusHL = map[hostapi.ServiceStatus]string{
	hostapi.StatusIdle: hostapi.ChromeHint, hostapi.StatusStarting: hostapi.ChromeWarn, hostapi.StatusReady: hostapi.ChromeOk, hostapi.StatusFailed: hostapi.ChromeDanger,
	hostapi.StatusSignIn: hostapi.ChromeWarn, hostapi.StatusStopped: hostapi.ChromeHint, hostapi.StatusOff: hostapi.ChromeHint,
}

// fieldRows sets a detail view's fields as a table: dim names in one
// column, each value's lines wrapped beside its name, an error's in the
// danger color and a status word in its own behind mark. A field with no
// value is left out; too narrow for the column, a field is a "name: value"
// line.
func fieldRows(fields []hostapi.Field, mark string, w int) [][]hostapi.Segment {
	kw := 0
	for _, f := range fields {
		if f.Value != "" {
			kw = max(kw, ansi.StringWidth(f.Name)+2)
		}
	}
	var rows [][]hostapi.Segment
	for _, f := range fields {
		if f.Value == "" {
			continue
		}
		hl, lead := "", hostapi.Segment{}
		switch f.Name {
		case "error":
			hl = hostapi.ChromeDanger
		case "status":
			hl, lead = statusHL[hostapi.ServiceStatus(f.Value)], markSeg(mark)
		}
		name := f.Name
		for l := range strings.SplitSeq(f.Value, "\n") {
			val := []hostapi.Segment{{Text: l, HL: hl}}
			if lead.Text != "" {
				val = []hostapi.Segment{lead, val[0]}
			}
			rows = append(rows, field(name, kw, val, w)...)
			name, lead = "", hostapi.Segment{}
		}
	}
	return rows
}

// field sets a value beside its name in a column kw wide, or as a "name:
// value" line when w leaves the value under 20 cells.
func field(name string, kw int, val []hostapi.Segment, w int) [][]hostapi.Segment {
	if w-kw < 20 {
		return fieldLine(name, val, w)
	}
	return fieldRow(name, kw, hostapi.WrapSegs(val, w-kw))
}

// fieldRow sets a field's value rows beside its name in a column kw wide;
// under the name the column is blank, and a copy rejoins the value.
func fieldRow(name string, kw int, rows [][]hostapi.Segment) [][]hostapi.Segment {
	for i, r := range rows {
		rows[i] = append([]hostapi.Segment{{Text: fmt.Sprintf("%-*s", kw, name), HL: hostapi.ChromeHint, NoCopy: name == ""}}, r...)
		name = ""
	}
	return rows
}

// fieldLine is a field too narrow for the name column: "name: value"
// wrapped to w, or the value alone under a name already drawn.
func fieldLine(name string, val []hostapi.Segment, w int) [][]hostapi.Segment {
	if name != "" {
		val = append([]hostapi.Segment{{Text: name + ": ", HL: hostapi.ChromeHint}}, val...)
	}
	return hostapi.WrapSegs(val, w)
}

// spliceBox composites box rows centered over the chat area; what the box
// does not cover stays visible. It reports the box's own cells, which a
// click outside closes it on and a drag inside selects in.
func spliceBox(rows, box [][]hostapi.Segment, area uv.Rectangle, w int) uv.Rectangle {
	if len(rows) == 0 || len(box) == 0 {
		return uv.Rectangle{}
	}
	bottom := min(area.Max.Y, len(rows))
	bh := min(len(box), bottom-area.Min.Y)
	if bh <= 0 {
		return uv.Rectangle{}
	}
	y0 := area.Min.Y + (bottom-area.Min.Y-bh)/2
	lo, hi := w, 0
	for i := range bh {
		// the box covers its own cells: the backdrop beside it keeps its own text
		bw := hostapi.SegsWidth(box[i])
		x0 := area.Min.X + max((area.Dx()-bw)/2, 2)
		placeBox(rows, [][]hostapi.Segment{box[i]}, x0, y0+i, w)
		lo, hi = min(lo, x0), max(hi, x0+bw)
	}
	return uv.Rect(lo, y0, max(hi-lo, 0), bh)
}

// anchorBottom lays box over the bottom rows of area with its border in
// line with frame's left or right edge: the composer frame whose
// statusline pill opened it. Where the frame is drawn (framed, see ruleAt)
// and its top rule is at most a row under area and wider than the box, the
// box stands on it: the rule's cells under the box become its bottom border,
// joined at its edges (App.join), and the label that rule carries gives way
// to it. The box covers its own cells: what it does not cover stays visible,
// the rule and the rows beside it included. Its lines, and the rule cells it
// stands on, take the rule group: a composer popup's is the draft's tint of
// that rule. It reports the cells the box took, that border included.
func anchorBottom(rows, box [][]hostapi.Segment, area, frame uv.Rectangle, framed, right bool, rule string, w int) uv.Rectangle {
	for _, row := range box {
		for i := range row {
			if row[i].Line && row[i].HL == hostapi.ChromeRule {
				row[i].HL = rule
			}
		}
	}
	bw := hostapi.SegsWidth(box[0])
	x := frame.Min.X
	if right {
		x = max(frame.Max.X-bw, 0)
	}
	end, drawn := min(area.Max.Y, len(rows)), len(box)
	if gap := frame.Min.Y - area.Max.Y; framed && gap >= 0 && gap <= 1 && x >= frame.Min.X && x+bw <= frame.Max.X {
		end, drawn = frame.Min.Y+1, len(box)-1
		// the rule is the box's bottom border: within its span nothing else
		// draws, so what lies under it (the working label, the snail) gives way
		// cell for cell rather than cutting the border
		rows[frame.Min.Y] = restyle(rows[frame.Min.Y], x, x+bw, func(s *hostapi.Segment) {
			s.Line, s.HL, s.Fill, s.Fade, s.Effect = true, rule, hostapi.FillPanel, 0, ""
			var b strings.Builder
			for _, r := range s.Text {
				if armsOf(r) != 0 {
					b.WriteRune(r)
					continue
				}
				b.WriteString(strings.Repeat("─", max(ansi.StringWidth(string(r)), 1)))
			}
			s.Text = b.String()
		})
	}
	y0 := max(area.Min.Y, end-len(box))
	for i := y0; i < end && i-y0 < drawn; i++ {
		placeBox(rows, [][]hostapi.Segment{box[i-y0]}, x, i, w)
	}
	return uv.Rect(x, y0, bw, max(end-y0, 0))
}

// ruleAt reports whether frame's top-left cell is a drawn corner: a framed
// composer's rule, which a box can stand on, and not a separator or text
// beside an unframed one.
func ruleAt(rows [][]hostapi.Segment, frame uv.Rectangle) bool {
	if frame.Min.Y < 0 || frame.Min.Y >= len(rows) {
		return false
	}
	c := dropCells(rows[frame.Min.Y], frame.Min.X)
	if len(c) == 0 || !c[0].Line {
		return false
	}
	r, _ := utf8.DecodeRuneInString(c[0].Text)
	return armsOf(r)&(armR|armD) == armR|armD
}

// dropCells removes the first n display cells of a row.
func dropCells(segs []hostapi.Segment, n int) []hostapi.Segment {
	var out []hostapi.Segment
	for _, s := range segs {
		sw := ansi.StringWidth(s.Text)
		if n >= sw {
			n -= sw
			continue
		}
		if n > 0 {
			s.Text = ansi.TruncateLeft(s.Text, n, "")
			n = 0
		}
		out = append(out, s)
	}
	return out
}
