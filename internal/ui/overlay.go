package ui

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"unicode"
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
	// inputErr is why an input overlay's last submit was refused, shown
	// above the line until the next key clears it
	inputErr string
	body     viewport
	text     textRows            // the body's rows
	back     bool                // a detail view: esc and its actions return to the overlay under it
	item     hostapi.OverlayItem // a detail view's item: a reload can swap the parent's under it
	size     boxSize
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
		return []hintPair{{key: "enter", verb: "done"}, {key: "esc", verb: "clear"}}
	case f.query != "":
		esc = "clear"
	}
	return append(hints, hintPair{key: "esc", verb: esc})
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
		a.byRow = false
		a.syncRail()
		return
	}
	if o.Refresh {
		return
	}
	st := a.overlayState(o)
	// a level reopened from the row that opened it lands where it was left; a
	// list opened on its own opens where the reader is
	byRow := a.byRow
	a.byRow = false
	if o.Follow {
		if sel := a.lastSel[o.ID]; !byRow || sel == "" {
			// an open that names its row (OpenAt) brings the selection the
			// host computed for it: the levels one open puts up move the chat
			// as they open, and a level deriving the place from the moved chat
			// would land on the parent level's pick, not the reader's row
			if o.OpenAt > 0 {
				st.selected = max(0, min(o.Selected, len(st.visible)-1))
			} else {
				st.selected = a.followRow(&st)
			}
		} else if n := slices.IndexFunc(st.visible, func(idx int) bool { return itemKey(st.o.Items[idx]) == sel }); n >= 0 {
			st.selected = n
		}
		st.followed = st.selectedAnchor()
		if chat := a.scrolls[a.scrollDef]; chat != nil {
			// the level opened last says where the chat goes: a jump the level
			// under it asked for (the turn of a row, while this one's row is
			// already in view) must not land the chat on that level's row
			delete(a.pendingAnchor, a.scrollDef)
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
	if n := len(a.overlays); n > 0 && !o.Input && !o.Follow {
		// a level drawn over its list lines up with it; a browsing list hides
		// the level under it, so it takes the width of what it holds
		st.size.w = a.overlays[n-1].size.w
	}
	a.overlays = append(a.overlays, st)
	a.syncRail()
	if o.Confirm == "" {
		return
	}
	top := &a.overlays[len(a.overlays)-1]
	for idx, it := range top.items {
		for _, k := range it.row.Keys {
			if k.Run == o.Confirm {
				top.selected = max(slices.Index(top.visible, idx), 0)
				top.keyArmed = k.Key
				return
			}
		}
	}
	a.pushToast(slog.LevelError, "Nothing to confirm", "/"+o.Confirm+" runs it without asking")
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
	// the chat's place is one row, whoever asks: the host opened this level
	// there, and a level naming it lands on it, so a blank row between two
	// messages resolves the way the chat draws it, down to the next message
	if place := a.chatRow(); place > 0 {
		for k, idx := range st.visible {
			if st.items[idx].row.Anchor == place {
				return k
			}
		}
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
	spans := indexUnits(chat.lines)
	for k, idx := range st.visible {
		from, to := spans.rows(st.items[idx].row.Anchor)
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
//
// The topmost following list answers, not the top overlay: a screen a row
// opens (/call, /jobs view) sits over the list it came from, and the chat
// keeps previewing the row under it.
func (a *App) syncFollow() {
	a.highlight = 0
	i := len(a.overlays) - 1
	for i >= 0 && !a.overlays[i].o.Follow {
		i--
	}
	if i < 0 {
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

// railOf is the transcript row the topmost following list draws whole, 0 for
// none: a list on its way out already reads as closed.
func (a *App) railOf() int {
	if a.railClose != "" {
		return 0
	}
	for i := range slices.Backward(a.overlays) {
		if a.overlays[i].o.Follow {
			return a.overlays[i].o.Rail
		}
	}
	return 0
}

// syncRail keeps the chat's whole-run view in step with the overlay stack as
// it changes, never during a paint: the chat renders the run whole from the
// state it reads, so a level opening or closing lands in the same frame.
func (a *App) syncRail() {
	if r := a.railOf(); r != a.rail {
		a.rail = r
		a.bump()
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
	if arm := old.armOf(old.keyArmed); arm != "" && st.armOf(old.keyArmed) == arm {
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

// selectedItem is the item the selected row was read from, zero when nothing
// is selected.
func (st *overlayState) selectedItem() hostapi.OverlayItem {
	if st.back {
		return st.item
	}
	if st.selected >= len(st.visible) {
		return hostapi.OverlayItem{}
	}
	return st.o.Items[st.visible[st.selected]]
}

// sameDanger reports whether the selected row n is still the danger row o,
// so a rebuild keeps the press it waits for.
func sameDanger(o, n hostapi.OverlayItem) bool {
	return n.Danger && n.Run == o.Run && cmp.Or(n.ID, n.Text) == cmp.Or(o.ID, o.Text)
}

// refreshDetail rebuilds the view open over list i from its item in the
// list's new items, keeping the scroll, or drops it with its item: a screen a
// Go job refreshes (a server starting, a run moving) would otherwise show
// what it said before.
func (a *App) refreshDetail(i int) {
	j := i + 1
	if j >= len(a.overlays) || !a.overlays[j].back || a.overlays[j].o.ID != a.overlays[i].o.ID+"#detail" {
		return
	}
	d := a.overlays[j]
	idx := slices.IndexFunc(a.overlays[i].o.Items, func(it hostapi.OverlayItem) bool { return itemKey(it) == itemKey(d.item) })
	a.overlays = slices.Delete(a.overlays, j, j+1)
	if idx < 0 {
		return
	}
	a.openDetail(&a.overlays[i], idx)
	n := &a.overlays[len(a.overlays)-1]
	n.body, n.size = d.body, d.size
}

// itemKey names an item across rebuilds: its id, its transcript row (a list
// of one turn's rows repeats labels), else its text.
func itemKey(r hostapi.OverlayItem) string {
	switch {
	case r.ID != "":
		return r.ID
	case r.Anchor > 0:
		return "row " + strconv.Itoa(r.Anchor)
	}
	return r.Text
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
	for _, item := range o.Items {
		r := item
		// a desc keeps its runs of spaces: padded columns (a status word) stay put
		r.Desc, r.Key, r.Section = oneLine.Replace(sanitize.String(r.Desc)), sanitize.Line(r.Key), sanitize.Line(r.Section)
		r.Verb = hostapi.Verb(sanitize.Line(string(r.Verb)))
		r.Fill, r.Copy, r.Preview = sanitize.String(r.Fill), sanitize.String(r.Copy), sanitize.String(r.Preview)                          // Detail is the detail view's Body, sanitized there
		it := listItem{label: sanitize.Line(r.Text), desc: r.Desc, key: tagSeg(r.Key), section: r.Section, mark: markSeg(r.Mark), row: r} // a newline would break the box
		if r.On != nil || r.Locked != nil {
			it.key = switchSeg(r.On != nil && *r.On, r.Locked != nil)
		}
		if r.Row != nil {
			// only text and hl: a click id or effect here would act as Go chrome's
			for _, seg := range r.Row {
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
		marks := map[string][]hostapi.MarkKind{}
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

// rowKey is the key of the selected row that answers to key: zero (an empty
// ItemKey) when none. A detail view carries the keys of the row it opened
// from, so what acts on the row acts on its view.
func (st *overlayState) rowKey(key string) hostapi.ItemKey {
	var keys []hostapi.ItemKey
	if st.back {
		keys = st.item.Keys
	} else if len(st.visible) > 0 {
		keys = st.items[st.visible[st.selected]].row.Keys
	}
	if len([]rune(key)) != 1 {
		return hostapi.ItemKey{}
	}
	for _, k := range keys {
		if k.Key == key {
			return k
		}
	}
	return hostapi.ItemKey{}
}

var oneLine = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

// enterVerb names what enter does on item idx, one word per act in every
// list: "open" a view or another screen, "pick" a value, "run" a command,
// "toggle" a toggle list's row, "fill" the composer, "copy" the row's value,
// "go" to a browsing list's row (which closes the list keeping that place),
// or "" when it does nothing. A row's own verb, else the overlay's, names its
// run, select or fill where the default would not; a row with one is not a
// toggle: space passes it by in a toggle list.
func (st *overlayState) enterVerb(idx int) hostapi.Verb {
	r := st.items[idx].row
	switch {
	case hasView(r):
		return hostapi.VerbOpen
	case r.Run != "" || st.o.OnSelect != nil:
		if r.Verb != "" {
			return r.Verb
		}
		switch {
		case st.o.Reload != nil:
			return hostapi.VerbToggle
		case st.o.OnSelect != nil:
			return hostapi.VerbPick
		}
		return cmp.Or(st.o.Verb, hostapi.VerbRun)
	case r.Fill != "":
		return cmp.Or(r.Verb, hostapi.VerbFill)
	case st.o.Follow && st.o.Rail != 0:
		// the run closes with the list, so enter cannot leave the chat on a
		// row of it: the list stays where the preview put it
		return ""
	case st.o.Follow:
		// the list already moved the chat to the row: enter keeps that place
		return hostapi.VerbGo
	case r.Copy != "":
		return hostapi.VerbCopy
	}
	return ""
}

// hasView reports whether a row has a view of its own: text and facts, or a
// screen its run names.
func hasView(r hostapi.OverlayItem) bool { return r.Detail != "" || r.Fields != nil }

// openDetail pushes the view of item idx: its facts and text, under the
// parent's origin so the badge and the run gate stay the parent's. What acts
// on the row is a key of the row, so the view lists nothing.
func (a *App) openDetail(parent *overlayState, idx int) {
	r := parent.items[idx].row
	title := parent.items[idx].label
	if parent.o.Title != "" {
		title = parent.o.Title + " / " + title
	}
	p := parent.o
	st := a.overlayState(hostapi.Overlay{ID: p.ID + "#detail", Title: title, Origin: p.Origin, Path: p.Path, Gen: p.Gen, Anchor: p.Anchor, Body: r.Detail, BodyMD: p.BodyMD, Fields: r.Fields, Mark: r.Mark, OnAction: p.OnAction, Follow: p.Follow})
	// opening a view is navigation: it keeps the list's width and takes its
	// own height, which it then keeps while open
	st.back, st.item, st.size = true, parent.o.Items[idx], boxSize{w: parent.size.w}
	a.overlays = append(a.overlays, st)
}

// pressRowKey runs one of the selected row's keys: an id goes to on_action,
// else it fills or runs. A screen the key opens sits over the list, which
// stays so esc comes back to it; anything else closes the list, unless it
// rebuilds.
func (a *App) pressRowKey(st *overlayState, k hostapi.ItemKey) {
	o := st.o
	item := st.selectedItem()
	switch {
	case k.ID != "" && o.OnAction != nil:
		a.background(func() { a.host.OverlayAction(context.Background(), o, item, k.Raw) })
	case k.Fill != "" || k.Run != "":
		a.runOverlayRow(hostapi.OverlayItem{Run: k.Run, Fill: k.Fill})
		if k.Fill == "" && k.Verb == hostapi.VerbOpen {
			return // the screen it opened sits over this list
		}
	default:
		return
	}
	if o.Reload != nil { // the list shows what the key changed
		a.reloadOverlay(o, nil)
		return
	}
	// a view's keys act on the row from its list, so they close the list the
	// same way the key does from it; the view's id is the list's plus #detail
	a.closeOverlayByID(strings.TrimSuffix(o.ID, "#detail"))
}

// reloadOverlay runs call (nil for none) and rebuilds a toggle list's items,
// both on the worker, then swaps the items in while it is still open,
// keeping selection and filter.
func (a *App) reloadOverlay(o hostapi.Overlay, call func()) {
	a.background(func() {
		if call != nil {
			call()
		}
		items := o.Reload()
		a.do(func() {
			if i := a.overlayIndex(o.ID); i >= 0 && a.overlays[i].o.Gen == o.Gen {
				next := a.overlays[i].o
				next.Items = items
				a.replaceOverlay(i, next)
			}
		})
	})
}

// closeOverlayByID closes an overlay, the levels of its menu and the views
// over it: a level's id starts with the id of the list it opened from. A list
// that draws a run whole folds it at once and closes once the chat has
// rendered it folded, so the run is never seen open beside a closed list.
func (a *App) closeOverlayByID(id string) {
	closes := func(st overlayState) bool {
		return st.o.ID == id || strings.HasPrefix(st.o.ID, id+" ") || strings.HasPrefix(st.o.ID, id+"#detail")
	}
	if a.rail != 0 && a.railClose == "" && slices.ContainsFunc(a.overlays, func(st overlayState) bool { return st.o.Rail != 0 && closes(st) }) {
		a.railClose, a.railGen = id, a.frameGen
		a.syncRail()
		return
	}
	a.closeOverlays(id)
}

// closeOverlays is closeOverlayByID past the fold: a toggle list it uncovers
// rebuilds on the worker, so callers act before they close, and the rebuild
// then shows what the closed overlay changed.
func (a *App) closeOverlays(id string) {
	closes := func(st overlayState) bool {
		return st.o.ID == id || strings.HasPrefix(st.o.ID, id+" ") || strings.HasPrefix(st.o.ID, id+"#detail")
	}
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
	a.syncRail()
}

// cancelMenuRoot closes the outermost level of a browsing menu, so esc on any
// of its levels closes the lot; false when the level is its own root.
func (a *App) cancelMenuRoot(o hostapi.Overlay) bool {
	for _, st := range a.overlays {
		if strings.HasPrefix(o.ID, st.o.ID+" ") {
			a.cancelOverlay(st.o)
			return true
		}
	}
	return false
}

// closeMenu closes a browsing list with every level under it opened, keeping
// the chat where the selection left it: enter on a row whose place is the
// transcript is not a level down, it is the place itself.
func (a *App) closeMenu(st *overlayState) {
	// the chat stays where the menu left it, and keeps that row as its place,
	// so reopening the menu follows it back there
	if chat := a.scrolls[a.scrollDef]; chat != nil {
		if anchor := st.selectedAnchor(); anchor > 0 {
			chat.focus, chat.placed = anchor, chat.top
		}
	}
	root := st.o.ID
	for _, s := range a.overlays { // levels are outermost first
		if strings.HasPrefix(st.o.ID, s.o.ID+" ") {
			root = s.o.ID
			break
		}
	}
	a.closeOverlayByID(root)
}

// stepUnder moves the level a call view stepped over onto the row the view
// now shows, so going back lands on the call that was read, not the one the
// level was opened on.
func (a *App) stepUnder(run string) {
	row, err := strconv.Atoi(strings.TrimPrefix(run, "call "))
	if err != nil {
		return
	}
	// the step closes this view and opens the next one on a worker, so the
	// level to move is the one a following list is: the nearest below
	i := len(a.overlays) - 1
	for i >= 0 && !a.overlays[i].o.Follow {
		i--
	}
	if i < 0 {
		return
	}
	under := &a.overlays[i]
	for k, idx := range under.visible {
		if under.items[idx].row.Anchor != row {
			continue
		}
		if under.selected != k {
			under.selected, under.armed, under.keyArmed = k, false, ""
		}
		return
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
	a.byRow = false // a row's screen opens before the next key, not after it
	confirmed, swallowed := pressArmed(&st.armed, key)
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
	if !st.active { // an open filter types; until then left and right alias h and l
		switch key {
		case "left":
			key = "h"
		case "right":
			key = "l"
		}
	}
	// a view opened from a rail steps along it in place, and the level under
	// it steps with it: the view and the list name one row
	if run := map[string]string{"ctrl+j": o.Next, "ctrl+k": o.Prev}[key]; run != "" {
		a.closeOverlayByID(o.ID)
		a.runOverlayRow(hostapi.OverlayItem{Run: run})
		a.stepUnder(run)
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
		item := st.selectedItem()
		call := func() { a.host.OverlayKey(context.Background(), o, o.Keys[i], item) }
		if o.Reload != nil { // a toggle list stays open and rebuilds
			a.reloadOverlay(o, call)
			return nil
		}
		a.background(call)
		a.closeOverlayByID(o.ID)
		return nil
	}
	// a row's own key acts on it from the list, danger ones on their second
	// press as the list's do
	if k := st.rowKey(key); k.Key != "" {
		if k.Danger && keyArmed != key {
			st.keyArmed = key
			return nil
		}
		a.pressRowKey(st, k)
		return nil
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
		case key == "enter" && hasView(r) && !(r.Verb == hostapi.VerbOpen && r.Run != ""):
			// a row with a view of its own takes enter; one whose run says it
			// opens a screen opens it below
			a.openDetail(st, idx)
		case key == "enter" && verb == hostapi.VerbGo:
			// a browsing list's row stands for its place in the chat: enter
			// closes the menu there, and l is the way into a level
			a.closeMenu(st)
		case r.Danger && !confirmed:
			st.armed = true
		case verb == hostapi.VerbCopy && r.Copy != "":
			// a copy row's value goes to the clipboard and the list stays open
			return a.copied(st.value(idx))
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
		case verb == hostapi.VerbOpen && o.OnSelect == nil:
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
		// one level in, where there is one: a row's view or the screen its run
		// opens. Its operations are keys of the row, so a row with neither
		// takes nothing from l.
		if st.back || len(st.visible) == 0 {
			return nil
		}
		idx := st.visible[st.selected]
		switch r := st.items[idx].row; {
		case hasView(r):
			a.openDetail(st, idx)
		case r.Run != "" && o.OnSelect == nil:
			a.runOverlayRow(r)
		}
		return nil
	case "h":
		// one level back, a view or a level stacked over another only: at the
		// root there is nowhere back to, and h types into a filter as always
		if len(a.overlays) == 1 {
			return nil
		}
		if st.back {
			a.overlays = a.overlays[:len(a.overlays)-1] // back, with nothing to rebuild
			a.syncRail()
			return nil
		}
		a.cancelOverlay(o) // the chat back where this level found it
	case "esc":
		if st.back {
			a.overlays = a.overlays[:len(a.overlays)-1] // back, with nothing to rebuild
			a.syncRail()
			return nil
		}
		// a browsing menu closes whole, the chat back where it was found
		if o.Follow && a.cancelMenuRoot(o) {
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
	if n := len(a.overlays); acts && n > 0 && a.overlays[n-1].keyArmed != "" {
		a.overlays[n-1].keyArmed = ""
		a.frameDirty = true // a wheel with no step bumps nothing else
	}
	i := len(a.overlays) - 1
	if i < 0 || !a.overlays[i].armed {
		return func() {}
	}
	sel := a.overlays[i].selected
	return func() {
		if i < len(a.overlays) && a.overlays[i].selected != sel {
			a.overlays[i].armed = false
			a.frameDirty = true
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
// what it changed; a value the command refuses keeps the box open with the
// error it named, so the ask fixes the value instead of retyping it.
func (a *App) inputKey(st *overlayState, key, text string) {
	in := &st.input
	switch key {
	case "enter":
		o, value := st.o, in.text
		if o.Run != "" {
			name, args := hostapi.SplitCommand(o.Run + " " + value)
			if err := a.runCommandErr(name, args); err != nil {
				st.inputErr = err.Error()
				a.bump()
				return
			}
		} else {
			a.background(func() { a.host.SubmitOverlayInput(context.Background(), o, value) })
		}
		a.closeOverlayByID(o.ID)
	case "esc":
		a.cancelOverlay(st.o)
	default:
		st.inputErr = ""
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
// palette) and "run", which only builtin and user overlays keep. A screen it
// opens is a level of this list, so it reopens on the row the list left.
func (a *App) runOverlayRow(r hostapi.OverlayItem) {
	if r.Fill != "" {
		a.composer.text = r.Fill
		a.composer.moveEnd()
	}
	if r.Run != "" {
		name, args := hostapi.SplitCommand(r.Run)
		a.byRow = true // the screen it opens is a level of this list
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
// h is the height the box may take, top whether it is the stack's frontmost
// box, the only one its h back names.
func (a *App) overlayBox(st *overlayState, w, h int, top bool) [][]hostapi.Segment {
	badge := a.host.OriginBadge(st.o.Origin, st.o.Path)
	inner := boxInner(w)
	if st.o.Input {
		inW := min(inner, st.size.w)
		if st.size.w == 0 { // measuring: the line draws at its natural width
			inW = min(inner, 40)
		}
		body := [][]hostapi.Segment{st.input.inputRow(inW, st.o.Placeholder)}
		if st.inputErr != "" {
			// the error takes the blank row between the line and the hints,
			// wrapped to the width the box keeps, so a long one costs height
			// and never widens or shifts the frame
			body = append(body, hostapi.WrapSegs([]hostapi.Segment{{Text: st.inputErr, HL: hostapi.ChromeDanger}}, min(inner, max(st.size.w, 20)))...)
		} else {
			body = append(body, nil)
		}
		body = append(body, hint(inner, sendHint, hintPair{key: "esc", verb: "close"}))
		return promptBox(st.o.Title, badge, body, w, h, 100, &st.size)
	}
	// a list with previews reads as text; a detail view widens to its
	// breadcrumb title where the screen has room, and a field repeating the
	// title's item (a project's path) shows only when the title is cut
	previews := !st.back && slices.ContainsFunc(st.items, func(it listItem) bool { return it.row.Preview != "" })
	// a box takes its width when it first draws and keeps it: one that
	// resized as the selection moved would shift under the reader, so the
	// body, the rows and the hints all fit that width
	measuring := st.size.h == 0
	fields := st.o.Fields
	if st.back {
		title := ansi.StringWidth(st.o.Title) + 1 + map[bool]int{true: ansi.StringWidth(badge) + 2}[badge != ""]
		st.size.w = max(st.size.w, min(title, inner))
		if title <= inner {
			fields = slices.DeleteFunc(slices.Clone(fields), func(f hostapi.Field) bool { return strings.HasSuffix(st.o.Title, " / "+f.Value) })
		}
	} else if measuring { // the fewest cells a measuring box keeps, so it reads as a menu
		floor := textBoxW
		if len(st.o.Items) > 0 && !previews {
			// the width the list's own rows take, so the box opens at it and
			// no label or description trims when the kept width arrives
			labelW, keyW, descNeeds, rowW := listNeedW(st.items)
			keep := max(labelW+keyW+max(descNeeds, 3)+4, rowW+4)
			floor = max(listBoxW, min(keep, inner))
		}
		st.size.w = max(st.size.w, floor)
	}
	if st.o.Wide {
		st.size.w = max(st.size.w, inner)
	}
	bodyW := inner
	if !measuring || (len(st.o.Items) > 0 && !previews && st.o.Body == "" && len(st.o.Parts) == 0 && st.o.Fields == nil) {
		// a list draws at the width the box keeps, measuring pass included,
		// so the first frame and the kept one split its columns the same way
		bodyW = min(inner, st.size.w)
	}
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
			rows = append(rows, a.bodyParts(st.o.Parts, w)...)
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
				listPH, fixed = nil, emptyRows(noMatches, bodyW)
			}
		case bodyN == 0 || st.o.Empty != "":
			listPH, fixed = nil, emptyRows(cmp.Or(st.o.Empty, "Nothing here."), bodyW)
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
		case len(st.visible) > 0:
			if key := (listKey{query: st.query, sel: st.selected, top: st.view.top, room: limit, minW: st.size.w, maxW: inner}); st.rows == nil || key != st.rowsKey {
				items := make([]listItem, 0, len(st.visible))
				for n, i := range st.visible {
					it := st.items[i]
					it.match = st.match[n]
					items = append(items, it)
				}
				st.rows = listRowsW(items, st.items, st.selected, &st.view, limit, st.size.w, bodyW)
				key.top = st.view.top
				st.rowsKey = key
			}
			list = st.rows
			verb := st.enterVerb(st.visible[st.selected])
			if verb != "" {
				hints = append(hints, hintPair{key: "enter", verb: string(verb)})
				enterCopies = verb == hostapi.VerbCopy
			}
			r := st.items[st.visible[st.selected]].row
			if verb != hostapi.VerbToggle && st.o.Reload != nil && r.Verb == "" && (st.o.OnSelect != nil || r.Run != "") {
				hints = append(hints, hintPair{key: "space", verb: "toggle"})
			}
			if (verb == hostapi.VerbGo || st.o.OnSelect != nil) && (hasView(r) || r.Run != "") {
				// enter is the row itself: l is the way into what it opens
				hints = append(hints, hintPair{key: "l", verb: "open"})
			}
		}
		hints = append(hints, st.hintKeys()...)
		var body [][]hostapi.Segment
		if bodyN > 0 {
			st.body.viewH = max(room-len(list), 1)
			body = st.body.window(buildBody(bodyW), bodyW, boxFade)
		}
		// y says nothing enter does not
		if (bodyN > 0 || len(st.visible) > 0) && !enterCopies {
			hints = append(hints, hintPair{key: "y", verb: "copy"})
		}
		if st.o.Next != "" || st.o.Prev != "" {
			hints = append(hints, hintPair{key: "ctrl+j/k", verb: "next/prev"})
		}
		scroll := scrolls(st.body, st.view)
		if scroll == nil && st.o.Reload != nil {
			scroll = []hintPair{scrollGap}
		}
		esc := map[bool]string{true: "back", false: "close"}[st.back]
		if top && !st.back && len(a.overlays) > 1 {
			hints = append(hints, hintPair{key: "h", verb: "back"})
		}
		hints = st.filterHints(append(hints, scroll...), esc)
		footW := bodyW
		if measuring { // the box grows to what the footer keeps, so no hint drops
			footW = inner
		}
		foot := hint(footW, hints...)
		if st.armed {
			foot = armedHint(footW, "enter", st.items[st.visible[st.selected]].label)
		}
		if d := st.keyDesc(st.keyArmed); d != "" {
			foot = armedHint(footW, st.keyArmed, d)
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

// bodyParts lays a view's parts out: a part naming a Fill keeps a padding row
// of its own surface above and below it, so a command and what it answered
// stand as two blocks joined in one, each with air all around its text and
// nothing between them but their own padding. A part outside a fill keeps
// one blank row before it, and an empty one draws nothing: a call with no
// output yet is its command alone.
func (a *App) bodyParts(parts []hostapi.BodyPart, w int) [][]hostapi.Segment {
	var rows [][]hostapi.Segment
	for _, p := range parts {
		if strings.TrimSpace(p.Text) == "" && p.Fill != hostapi.FillAuto {
			continue
		}
		if p.Fill == hostapi.FillAuto {
			if len(rows) > 0 {
				rows = append(rows, nil)
			}
			rows = append(rows, a.bodyPart(p, w)...)
			continue
		}
		rows = append(rows, wellPad(p.Fill, w))
		rows = append(rows, a.bodyPart(p, w)...)
		rows = append(rows, wellPad(p.Fill, w))
	}
	return rows
}

// bodyPart draws one part of a view's body in the form its text is written
// in, wrapped to w: a part in a well sits on its surface, while a diff and
// markdown bring their own.
func (a *App) bodyPart(p hostapi.BodyPart, w int) [][]hostapi.Segment {
	switch p.Lang {
	case "markdown":
		return a.host.Markdown(strings.TrimRight(p.Text, "\n"), w)
	case "diff":
		return hostapi.DiffRows(p.Text, w)
	case "":
		if p.Fill != hostapi.FillAuto { // the well's own side padding, like code
			return wellRows(wrapText(p.Text, "", w-2), p.Fill, w)
		}
		return wrapText(p.Text, "", w)
	}
	if p.Fill != hostapi.FillAuto { // the well pads it, not the part
		return a.wellCode(p.Text, p.Lang, p.Fill, w)
	}
	return a.codePanel(p.Text, p.Lang, hostapi.FillCanvas, w)
}

// wellRows puts wrapped text in a well at width cells.
func wellRows(rows [][]hostapi.Segment, fill hostapi.Fill, width int) [][]hostapi.Segment {
	out := make([][]hostapi.Segment, len(rows))
	for i, r := range rows {
		out[i] = wellRow(r, fill, width)
	}
	return out
}

// keyApplies reports whether a list key acts on the selected row: a key
// whose Applies refuses it is neither hinted nor run.
func keyApplies(k hostapi.OverlayKey, st *overlayState) bool {
	return k.Applies == nil || k.Applies(st.selectedRow())
}

// hintKeys are what the footer names: the list's keys that act on the
// selected row, then the row's own, which act on it from the list and are
// the whole of what a row offers once its view opens. The row's own are
// marked, so a narrow footer gives way on the screen's keys before them.
func (st *overlayState) hintKeys() []hintPair {
	var out []hintPair
	last := ""
	add := func(key, desc string, row bool) {
		if last != "" && desc == last { // keys sharing a desc read as one hint: +/- at once
			out[len(out)-1].key += "/" + key
			return
		}
		last = desc
		out = append(out, hintPair{key: key, verb: desc, row: row})
	}
	for _, k := range st.o.Keys {
		if k.Run == "" && k.Fn == nil && k.Act == nil && len(st.visible) == 0 || !keyApplies(k, st) {
			continue // it acts on the row the list has not got
		}
		add(k.Key, k.Desc, false)
	}
	// the keys of the row: the selected one in a list, the row the view
	// opened from in a detail
	row := hostapi.OverlayItem{}
	if st.back {
		row = st.item
	} else if len(st.visible) > 0 {
		row = st.selectedRow()
	}
	for _, k := range row.Keys {
		if slices.ContainsFunc(st.o.Keys, func(o hostapi.OverlayKey) bool { return o.Key == k.Key }) {
			continue
		}
		add(k.Key, k.Desc, true)
	}
	return out
}

// armOf is the command the danger key that answers to key runs, "" for none
// or a key that cannot be undone: the list's, then the selected row's.
func (st *overlayState) armOf(key string) string {
	if key == "" {
		return ""
	}
	if i := slices.IndexFunc(st.o.Keys, func(k hostapi.OverlayKey) bool { return k.Key == key && k.Danger }); i >= 0 {
		return st.o.Keys[i].Run
	}
	if k := st.rowKey(key); k.Key != "" && k.Danger {
		return k.Run
	}
	return ""
}

// keyDesc is the label the key that answers to key carries, "" for none: the
// list's, then the selected row's.
func (st *overlayState) keyDesc(key string) string {
	if key == "" {
		return ""
	}
	if i := slices.IndexFunc(st.o.Keys, func(k hostapi.OverlayKey) bool { return k.Key == key }); i >= 0 {
		return st.o.Keys[i].Desc
	}
	return st.rowKey(key).Desc
}

// pressArmed is key on a screen whose selected danger row may be armed:
// enter and space confirm it; any other key drops the arm, and esc does only
// that.
func pressArmed(armed *bool, key string) (confirmed, swallowed bool) {
	if !*armed {
		return false, false
	}
	if key == "enter" || key == "space" {
		return true, false
	}
	*armed = false
	return false, key == "esc"
}

// armedHint is the footer of a press that waits for a second one: what the
// next press does, in the danger color, with the keys drawn as keys. The
// label reads on after "again to", so its first letter drops and an
// acronym's word stays whole (skills.go's listDesc keeps the same rule).
func armedHint(w int, key, label string) []hostapi.Segment {
	r, n := utf8.DecodeRuneInString(label)
	if next, _ := utf8.DecodeRuneInString(label[n:]); n > 0 && !unicode.IsUpper(next) {
		label = string(unicode.ToLower(r)) + label[n:]
	}
	foot := hint(w, hintPair{key: key, verb: "again to " + label}, hintPair{key: "esc", verb: "cancel"})
	foot[1].HL = hostapi.ChromeDanger
	return foot
}

// phaseMark is a phase's state from its rows' marks: failed once one
// failed, else running while one runs, waiting while one waits, stopped
// when one stopped, done when every row is.
func phaseMark(marks []hostapi.MarkKind) hostapi.MarkKind {
	for _, m := range []hostapi.MarkKind{hostapi.MarkErr, hostapi.MarkSpin, hostapi.MarkWait, hostapi.MarkStop} {
		if slices.Contains(marks, m) {
			return m
		}
	}
	return hostapi.MarkOK
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
func fieldRows(fields []hostapi.Field, mark hostapi.MarkKind, w int) [][]hostapi.Segment {
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
