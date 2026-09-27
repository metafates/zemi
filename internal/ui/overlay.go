package ui

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
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

// edit applies a filtering key. Every list filters the same way: "/"
// starts a query, then typed text, the composer's deletion keys, and esc
// (or backspace on an empty query) ends it. Outside a query letters like
// j and k stay free for moving. It reports whether the key was used.
func (f *listFilter) edit(key, text string) bool {
	if !f.active {
		f.active = text == "/"
		return f.active
	}
	old, prev := f.query, -1
	if f.selected < len(f.visible) {
		prev = f.visible[f.selected]
	}
	q := composerState{text: f.query, cursor: len([]rune(f.query))}
	switch {
	case key == "esc" || (key == "backspace" && f.query == ""):
		f.query, f.active = "", false
	case key == "backspace":
		q.backspace()
		f.query = q.text
	case key == "ctrl+w" || key == "alt+backspace":
		q.delWord()
		f.query = q.text
	case key == "ctrl+u":
		f.query = ""
	case text != "":
		f.query += text
	default:
		return false
	}
	f.apply()
	if f.query != old {
		f.selected, f.view.top = 0, 0 // a new query selects its first match
		if f.query == "" {
			f.selected = max(0, slices.Index(f.visible, prev)) // a cleared one keeps the row
		}
	}
	return true
}

// filterRow is the query line above a list, a hint while no query is open
// so opening one adds no row.
func (f *listFilter) filterRow() []hostapi.Segment {
	if f.active {
		return append([]hostapi.Segment{{Text: "/ ", HL: "Host:Hint"}}, cursorSegs([]rune(f.query), len([]rune(f.query)), "Host:Bold")...)
	}
	return []hostapi.Segment{{Text: "/", HL: "Host:HintKey"}, {Text: " filter", HL: "Host:Hint"}}
}

// emptyRows is how every empty list or view reads: a sentence in the hint
// grey, wrapped to w.
func emptyRows(text string, w int) [][]hostapi.Segment { return wrapText(text, "Host:Hint", w) }

// openOverlay pushes an overlay, or replaces the open one with its id in
// place when o's origin may close it; Lua rendered its items when it opened,
// so the key loop never calls Lua for them.
func (a *App) openOverlay(o hostapi.Overlay) {
	if i := a.overlayIndex(o.ID); i >= 0 && mayClose(o.Origin, a.overlays[i].o) {
		a.replaceOverlay(i, o)
		return
	}
	if o.Refresh {
		return
	}
	a.overlays = append(a.overlays, a.overlayState(o))
}

// mayClose also gates replacing o by id.
func mayClose(origin lua.Origin, o hostapi.Overlay) bool {
	return origin == lua.OriginUser || origin == lua.OriginBuiltin || origin == o.Origin
}

// overlayIndex is the stack index of the open list or view with id, -1 when
// none is open.
func (a *App) overlayIndex(id string) int {
	return slices.IndexFunc(a.overlays, func(st overlayState) bool { return st.o.ID == id && !st.back })
}

// replaceOverlay swaps o in at stack index i, keeping the selection, the
// filter and the box's size, and the scroll while the body is the same.
func (a *App) replaceOverlay(i int, o hostapi.Overlay) {
	old := a.overlays[i]
	st := a.overlayState(o)
	f := old.listFilter
	f.labels, f.texts = st.labels, st.texts
	st.listFilter, st.size = f, old.size
	if st.o.Body == old.o.Body {
		st.body = old.body
	}
	st.apply()
	a.overlays[i] = st
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
	for i := range o.Keys {
		o.Keys[i].Key, o.Keys[i].Desc = sanitize.Line(o.Keys[i].Key), sanitize.Line(o.Keys[i].Desc)
	}
	st := overlayState{o: o}
	if o.Input {
		st.input.insert(sanitize.String(o.Default))
		return st
	}
	for i, item := range o.Items {
		// zemi.ui.overlay.open raised on a Lua item's wrong type; a Go screen's
		// or a select's item keeps the fields that parse
		r, _ := hostapi.ParseOverlayItem(item)
		r.Desc, r.Key, r.Section, r.Verb = sanitize.Line(r.Desc), sanitize.Line(r.Key), sanitize.Line(r.Section), sanitize.Line(r.Verb)
		r.Hotkey = sanitize.Line(r.Hotkey)
		r.Fill, r.Copy = sanitize.String(r.Fill), sanitize.String(r.Copy) // Detail is the detail view's Body, sanitized there
		if o.Origin != lua.OriginBuiltin && o.Origin != lua.OriginUser {
			r.Run = "" // so a plugin row label cannot disguise a command
		}
		it := listItem{label: sanitize.Line(r.Text), desc: r.Desc, key: tagSeg(r.Key), section: r.Section, row: r} // a newline would break the box
		if r.Switch || r.Locked {
			it.key = switchSeg(r.On, r.Locked)
		}
		if i < len(o.ItemText) && o.ItemText[i] != nil {
			// only text and hl: a click id or effect here would act as Go chrome's
			for _, seg := range o.ItemText[i] {
				it.segs = append(it.segs, hostapi.Segment{Text: oneLine.Replace(sanitize.String(seg.Text)), HL: hostapi.UserHL(seg.HL)})
			}
			it.label = plainSegs(it.segs) // spaces kept: a filter's offsets index the segments
		}
		st.items = append(st.items, it)
		st.labels = append(st.labels, it.label)
		st.texts = append(st.texts, it.section+"  "+it.desc+"  "+r.Copy)
	}
	st.selected = o.Selected
	st.apply()
	return st
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
		return "copy"
	case st.back:
		return "back"
	}
	return ""
}

func hasDetail(r hostapi.OverlayItem) bool { return r.Detail != "" || r.Actions != nil }

// actionKey is the key that acts on visible row n: the item's own hotkey,
// and in a detail view, whose rows are actions, its position when it has
// none, so every action reads as a button with a key.
func (st *overlayState) actionKey(n int) string {
	if k := st.items[st.visible[n]].row.Hotkey; k != "" || !st.back {
		return k
	}
	return strconv.Itoa(n + 1)
}

// openDetail pushes the detail view of item idx: its detail text as the
// body and its actions as the list, under the parent's origin so the badge
// and the run gate stay the parent's.
func (a *App) openDetail(parent *overlayState, idx int) {
	r := parent.items[idx].row
	title := parent.items[idx].label
	if parent.o.Title != "" {
		title = parent.o.Title + " / " + title
	}
	p := parent.o
	st := a.overlayState(hostapi.Overlay{ID: p.ID + "#detail", Title: title, Origin: p.Origin, Path: p.Path, Gen: p.Gen, Anchor: p.Anchor, Body: r.Detail, BodyMD: p.BodyMD, Fields: p.Fields, Items: r.Actions, OnAction: p.OnAction})
	st.back, st.item, st.size = true, parent.o.Items[idx], parent.size
	a.overlays = append(a.overlays, st)
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

// overlayCall runs a Lua callback of a list on the worker: a toggle list
// stays open and rebuilds, any other closes, like on_select.
func (a *App) overlayCall(o hostapi.Overlay, call func()) {
	if o.Reload != nil {
		a.reloadOverlay(o, call)
		return
	}
	a.background(call)
	a.closeOverlayByID(o.ID)
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
	if key == "space" && o.Reload != nil && !st.active {
		text = "" // space toggles rows in a toggle list, and types into an open filter
	}
	// a body that overflows takes the scroll keys, and the list keys too
	// when there is no list to move in
	if st.body.overflows() && scrollKey(&st.body, key, len(o.Items) == 0) {
		return nil
	}
	if len(o.Items) > 0 && !st.back && st.edit(key, text) || moveSel(&st.selected, len(st.visible), st.view.pageItems(len(st.visible)), key) {
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
	if i := slices.IndexFunc(o.Keys, func(k hostapi.OverlayKey) bool { return k.Key == key }); i >= 0 {
		var item any
		if len(st.visible) > 0 {
			item = o.Items[st.visible[st.selected]]
		}
		a.overlayCall(o, func() { a.host.OverlayKey(context.Background(), o, o.Keys[i], item) })
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
		switch {
		case key == "space" && (o.Reload == nil || r.Verb != ""):
		case key == "enter" && hasDetail(r):
			a.openDetail(st, idx)
		case verb == "copy":
			return a.copied(st.value(idx))
		case st.back:
			a.detailAction(idx)
		case o.Reload != nil && r.Fill == "":
			a.runOverlayRow(r)
			a.reloadOverlay(o, func() { a.host.SelectOverlay(context.Background(), o, idx) })
		case o.Reload != nil: // a fill hands the keys to the composer, so the list closes
			a.runOverlayRow(r)
			if o.OnSelect != nil {
				a.background(func() { a.host.SelectOverlay(context.Background(), o, idx) })
			}
			a.closeOverlayByID(o.ID)
		case verb == "":
		case r.Verb == "open" && o.OnSelect == nil:
			a.runOverlayRow(r) // the screen it opens goes over this list, so esc comes back
		default:
			if o.OnSelect != nil {
				a.background(func() { a.host.SelectOverlay(context.Background(), o, idx) })
			} else {
				a.runOverlayRow(r)
			}
			a.closeOverlayByID(o.ID)
		}
	case "esc":
		if st.back {
			a.overlays = a.overlays[:len(a.overlays)-1] // back, with nothing to rebuild
			return nil
		}
		a.cancelOverlay(o)
	}
	return nil
}

// copyOverlay copies the body of a view, or the selected row (all rows):
// an error or a diagnostic is worth nothing if it cannot leave the terminal.
func (a *App) copyOverlay(st *overlayState, all bool) tea.Cmd {
	var lines []string
	switch {
	case st.o.Body != "" && !all:
		lines = []string{st.o.Body}
	case all:
		for _, i := range st.visible {
			lines = append(lines, st.value(i))
		}
	case len(st.visible) > 0:
		lines = append(lines, st.value(st.visible[st.selected]))
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

// inputKey edits an input overlay's line; enter submits, esc cancels.
func (a *App) inputKey(st *overlayState, key, text string) {
	in := &st.input
	switch key {
	case "enter":
		o, value := st.o, in.text
		a.background(func() { a.host.SubmitOverlayInput(context.Background(), o, value) })
		a.closeOverlayByID(o.ID)
	case "esc":
		a.cancelOverlay(st.o)
	default:
		if !in.editKey(key) && text != "" {
			in.insert(strings.ReplaceAll(text, "\n", " "))
		}
	}
	a.bump()
}

// cancelOverlay closes o and runs its on_close. The overlay's Lua
// callbacks run on the background worker: the key loop never waits on Lua.
func (a *App) cancelOverlay(o hostapi.Overlay) {
	if o.OnClose != nil {
		a.background(func() { a.host.CancelOverlay(context.Background(), o) })
	}
	a.closeOverlayByID(o.ID)
	a.bump()
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

// overlayBox draws a Lua overlay with the Go-stamped origin badge (DESIGN
// §8): the body, then the list, then the keys that act on the selection.
// h is the height the box may take.
func (a *App) overlayBox(st *overlayState, w, h int) [][]hostapi.Segment {
	// the origin badge marks boxes that plugins and project config draw
	badge := ""
	if st.o.Origin != lua.OriginBuiltin && st.o.Origin != lua.OriginUser {
		badge = string(st.o.Origin)
		// a plugin's name, else the file's: a path leaves no room for the title
		if name := a.host.PluginOf(st.o.Path); name != "" {
			badge += ":" + name
		} else if st.o.Path != "" {
			badge += ":" + filepath.Base(st.o.Path)
		}
	}
	inner := boxInner(w)
	if st.o.Input {
		body := [][]hostapi.Segment{st.input.inputRow(inner), nil, hint(inner, submitHint, hintPair{"esc", "close"})}
		return promptBox(st.o.Title, badge, body, w, h, 100, &st.size)
	}
	// a list fits its content, 56 cells at least, a text view is 72, and a
	// detail view opens at its parent's size
	if !st.back {
		st.size.w = max(st.size.w, map[bool]int{true: 56, false: 72}[len(st.o.Items) > 0])
	}
	bodyW := min(inner, st.size.w)
	buildBody := st.text.at(func(w int) (rows [][]hostapi.Segment) {
		switch {
		case st.o.BodySegs != nil:
			for _, row := range st.o.BodySegs {
				rows = append(rows, hostapi.WrapSegs(row, w)...)
			}
		case st.o.BodyMD:
			rows = a.host.Markdown(strings.TrimRight(st.o.Body, "\n"), w)
		case st.o.Fields:
			rows = fieldRows(st.o.Body, w)
		default:
			for l := range strings.SplitSeq(strings.TrimRight(st.o.Body, "\n"), "\n") {
				rows = append(rows, hostapi.WrapSegs([]hostapi.Segment{{Text: l}}, w)...)
			}
		}
		return rows
	})
	bodyN := 0
	if st.o.Body != "" {
		bodyN = len(buildBody(bodyW))
	}
	body := fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		var head, list [][]hostapi.Segment // the filter row, and the list's rows
		var hints []hintPair
		// the list's and the body's viewports share what the other rows leave
		bodyPH, listPH, fixed := oneRow[:min(bodyN, 1)], oneRow, [][]hostapi.Segment(nil)
		switch {
		case st.back && len(st.o.Items) > 0:
		case len(st.o.Items) > 0:
			head = [][]hostapi.Segment{st.filterRow()}
			if len(st.visible) == 0 {
				listPH, fixed = nil, emptyRows("No matches.", inner)
			}
		case st.o.Body == "" || st.o.Empty != "":
			listPH, fixed = nil, emptyRows(cmp.Or(st.o.Empty, "Nothing here."), inner)
		default:
			listPH = nil
		}
		room := h - 2 - len(stack(tight, bodyPH, slices.Concat(head, listPH, fixed), oneRow)) + len(bodyPH) + len(listPH)
		limit := max(room-min(bodyN, 3), 1) // the list's rows; the body keeps three
		enterCopies := false
		switch {
		case st.back && len(st.o.Items) > 0:
			// a detail view's rows are actions: draw them as the prompts' buttons
			choices := make([]promptChoice, 0, len(st.visible))
			for n, i := range st.visible {
				choices = append(choices, promptChoice{key: st.actionKey(n), label: st.items[i].label, desc: st.items[i].desc, hl: hostapi.UserHL(st.items[i].row.HL)})
			}
			st.view.viewH = limit
			list = choiceView(choices, st.selected, &st.view, inner, level > 1)
			if len(st.visible) > 0 {
				hints = append(hints, pickHint)
			}
		case len(st.visible) > 0:
			if key := (listKey{st.query, st.selected, st.view.top, limit, st.size.w, inner}); st.rows == nil || key != st.rowsKey {
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
			if r := st.items[st.visible[st.selected]].row; verb != "toggle" && st.o.Reload != nil && r.Verb == "" && (st.o.OnSelect != nil || r.Run != "") && !st.active {
				hints = append(hints, hintPair{"space", "toggle"})
			}
			for _, k := range st.o.Keys {
				if !st.active { // an open filter types these one-character keys
					hints = append(hints, hintPair{k.Key, k.Desc})
				}
			}
		}
		var body [][]hostapi.Segment
		if bodyN > 0 {
			st.body.viewH = max(room-len(list), 1)
			body = st.body.window(st.body.wrap(buildBody, bodyW), bodyW)
		}
		// y types into an open filter, and says nothing enter does not
		if (bodyN > 0 || len(st.visible) > 0) && !st.active && !enterCopies {
			hints = append(hints, hintPair{"y", "copy"})
		}
		esc := "close"
		if st.active || st.back { // esc ends an open filter first
			esc = "back"
		}
		hints = append(hints, scrolls(st.body, st.view)...)
		hints = append(hints, hintPair{"esc", esc})
		rows := stack(tight, body, slices.Concat(head, list, fixed), [][]hostapi.Segment{hint(inner, hints...)})
		return rows, bodyN > 0 && st.body.viewH < min(bodyN, 3) || len(listPH) > 0 && st.view.viewH < min(st.view.rows, 3)
	})
	return promptBox(st.o.Title, badge, body, w, h, 100, &st.size)
}

// fieldLine is a "key: value" line of a detail body.
var fieldLine = regexp.MustCompile(`^([a-z][a-z -]{0,14}): (.*)$`)

// statusHL colors the status words every screen shares.
//
//exhaustive:enforce
var statusHL = map[hostapi.ServiceStatus]string{
	hostapi.StatusIdle: "Host:Hint", hostapi.StatusStarting: "Host:Warn", hostapi.StatusReady: "Host:Ok", hostapi.StatusFailed: "Host:Danger",
	hostapi.StatusSignIn: "Host:Warn", hostapi.StatusStopped: "Host:Hint", hostapi.StatusOff: "Host:Hint",
}

// fieldRows wraps a plain body, setting the "key: value" lines it opens
// with as a table: dim keys in one column, each value wrapped beside its
// key, an error's in the danger color and a status word in its own. Only the
// opening lines are fields, so prose after them never is, whatever its shape.
// A blank line ends them, and every line after an error's first continues
// it, so a traceback stays its text.
func fieldRows(body string, w int) [][]hostapi.Segment {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	block := make([][]string, len(lines)) // key and value, key "" continuing; nil is text
	kw, inErr := 0, false
fields:
	for i, l := range lines {
		switch m := fieldLine.FindStringSubmatch(l); {
		case m != nil && !inErr:
			block[i], inErr = m[1:], m[1] == "error"
			kw = max(kw, len(m[1])+2)
		case i > 0 && l != "":
			block[i] = []string{"", l}
		default:
			break fields
		}
	}
	var rows [][]hostapi.Segment
	hl := ""
	for i, l := range lines {
		if w-kw < 20 || block[i] == nil {
			rows = append(rows, hostapi.WrapSegs([]hostapi.Segment{{Text: l}}, w)...)
			continue
		}
		key, val := block[i][0], block[i][1]
		switch key {
		case "":
		case "error":
			hl = "Host:Danger"
		case "status":
			hl = statusHL[hostapi.ServiceStatus(val)]
		default:
			hl = ""
		}
		for j, r := range hostapi.WrapSegs([]hostapi.Segment{{Text: val, HL: hl}}, w-kw) {
			if j > 0 {
				key = ""
			}
			rows = append(rows, append([]hostapi.Segment{{Text: fmt.Sprintf("%-*s", kw, key), HL: "Host:Hint"}}, r...))
		}
	}
	return rows
}

// spliceBox composites box rows centered over the chat area; what the box
// does not cover stays visible. It reports the cells the box took, which a
// click outside closes it on.
func spliceBox(rows, box [][]hostapi.Segment, area uv.Rectangle, w int) ([][]hostapi.Segment, uv.Rectangle) {
	if len(rows) == 0 || len(box) == 0 {
		return rows, uv.Rectangle{}
	}
	bottom := min(area.Max.Y, len(rows))
	bh := min(len(box), bottom-area.Min.Y)
	if bh <= 0 {
		return rows, uv.Rectangle{}
	}
	y0 := area.Min.Y + (bottom-area.Min.Y-bh)/2
	lo, hi := w, 0
	for i := range bh {
		x0 := area.Min.X + max((area.Dx()-hostapi.SegsWidth(box[i]))/2, 1)
		rows = placeBox(rows, [][]hostapi.Segment{margined(box[i])}, x0-1, y0+i, w)
		lo, hi = min(lo, x0-1), max(hi, x0+hostapi.SegsWidth(box[i])+1)
	}
	return rows, uv.Rect(lo, y0, max(hi-lo, 0), bh)
}

// anchorBottom lays box over the bottom rows of area with its border in
// line with frame's left or right edge: the composer frame whose
// statusline pill opened it. What it does not cover stays visible, one
// cell off its inner side. It reports the cells the box took.
func anchorBottom(rows, box [][]hostapi.Segment, area, frame uv.Rectangle, right bool, w int) ([][]hostapi.Segment, uv.Rectangle) {
	bottom := min(area.Max.Y, len(rows))
	y0 := max(area.Min.Y, bottom-len(box))
	bw := hostapi.SegsWidth(box[0])
	x := frame.Min.X
	if right {
		x = max(frame.Max.X-bw, 0)
	}
	for i := y0; i < bottom; i++ {
		if right {
			rows = placeBox(rows, [][]hostapi.Segment{append([]hostapi.Segment{{Text: " "}}, box[i-y0]...)}, x-1, i, w)
		} else {
			rows = placeBox(rows, [][]hostapi.Segment{append(slices.Clip(box[i-y0]), hostapi.Segment{Text: " "})}, x, i, w)
		}
	}
	return rows, uv.Rect(x, y0, bw, max(bottom-y0, 0))
}

// margined holds what a box does not cover one cell off each border, so it
// does not read as part of the box.
func margined(line []hostapi.Segment) []hostapi.Segment {
	return append(append([]hostapi.Segment{{Text: " "}}, line...), hostapi.Segment{Text: " "})
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
