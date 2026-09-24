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
	texts []string   // what the filter matches and y copies, per item
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

func (st *overlayState) refilter() { st.apply(st.texts) }

// listFilter narrows a list as the user types: visible holds the indices of
// rows containing every word of query (case-insensitive), and selected
// indexes visible.
type listFilter struct {
	active   bool // "/" was pressed: keys type into query
	query    string
	visible  []int
	selected int
	view     viewport // the list, or a detail view's actions
}

func (f *listFilter) apply(texts []string) {
	words := strings.Fields(strings.ToLower(f.query))
	f.visible = f.visible[:0]
	for i, text := range texts {
		lower, ok := strings.ToLower(text), true
		for _, w := range words {
			ok = ok && strings.Contains(lower, w)
		}
		if ok {
			f.visible = append(f.visible, i)
		}
	}
	f.selected = max(0, min(f.selected, len(f.visible)-1))
}

// edit applies a filtering key. Every list filters the same way: "/"
// starts a query, then typed text, the composer's deletion keys, and esc
// (or backspace on an empty query) ends it. Outside a query letters like
// j and k stay free for moving. It reports whether the key was used.
func (f *listFilter) edit(key, text string, texts []string) bool {
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
	f.apply(texts)
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
	a.overlays = append(a.overlays, a.overlayState(o))
}

// mayClose reports whether origin may close (or replace) o: user and
// builtin code, or the origin that opened it.
func mayClose(origin lua.Origin, o hostapi.Overlay) bool {
	return origin == lua.OriginUser || origin == lua.OriginBuiltin || origin == o.Origin
}

// overlayIndex is the stack index of the open list or view with id, -1 when
// none is open.
func (a *App) overlayIndex(id string) int {
	return slices.IndexFunc(a.overlays, func(st overlayState) bool { return st.o.ID == id && !st.back })
}

// replaceOverlay swaps o in at stack index i, keeping the selection, the
// filter, the scroll and the box's size.
func (a *App) replaceOverlay(i int, o hostapi.Overlay) {
	old := a.overlays[i]
	st := a.overlayState(o)
	st.listFilter, st.size, st.body = old.listFilter, old.size, old.body
	st.refilter()
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
		m, _ := item.(map[string]any)
		field := func(k string) string {
			s, _ := m[k].(string)
			return sanitize.Line(s)
		}
		it := listItem{label: formatOverlayItem(item), desc: field("desc"), key: field("key"), section: field("section")}
		if i < len(o.ItemText) && o.ItemText[i] != nil {
			// only text and hl: a click id or effect here would act as Go chrome's
			for _, seg := range o.ItemText[i] {
				it.segs = append(it.segs, hostapi.Segment{Text: oneLine.Replace(sanitize.String(seg.Text)), HL: hostapi.UserHL(seg.HL)})
			}
			it.label = plainSegs(it.segs)
		}
		it.label = sanitize.Line(it.label) // a newline would break the box
		st.items = append(st.items, it)
		st.texts = append(st.texts, strings.TrimSpace(it.label+"  "+it.desc))
	}
	st.selected = o.Selected
	st.refilter()
	return st
}

var oneLine = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

// formatOverlayItem renders items without render_item: strings, or
// {text, fill} maps.
func formatOverlayItem(item any) string {
	if m, ok := item.(map[string]any); ok {
		if s, ok := m["text"].(string); ok {
			return s
		}
	}
	if s, ok := item.(string); ok {
		return s
	}
	return fmt.Sprint(item)
}

// enterVerb names what enter does on item idx: "open" its detail view,
// "fill" the composer, the overlay's own verb ("confirm" by default), "back"
// from a detail view, or "" when it does nothing. A row's own verb names its
// run or fill in place of the overlay's.
func (st *overlayState) enterVerb(idx int) string {
	m, _ := st.o.Items[idx].(map[string]any)
	has := func(k string) bool { s, _ := m[k].(string); return s != "" }
	switch {
	case hasDetail(m):
		return "open"
	case rowRun(st.o, m) != "" || st.o.OnSelect != nil || st.back && has("id") && st.o.OnAction != nil:
		return cmp.Or(itemVerb(m), st.o.Verb, "confirm")
	case has("fill"):
		return cmp.Or(itemVerb(m), "fill")
	case has("copy"):
		return "copy"
	case st.back:
		return "back"
	}
	return ""
}

func hasDetail(m map[string]any) bool {
	s, _ := m["detail"].(string)
	return s != "" || m["actions"] != nil
}

// itemVerb is a row's own enter verb. A row with one is not a toggle: space
// passes it by in a toggle list.
func itemVerb(m map[string]any) string {
	s, _ := m["verb"].(string)
	return sanitize.Line(s)
}

// actionKey is the key that acts on visible row n: the item's own hotkey,
// and in a detail view, whose rows are actions, its position when it has
// none, so every action reads as a button with a key.
func (st *overlayState) actionKey(n int) string {
	m, _ := st.o.Items[st.visible[n]].(map[string]any)
	if k, _ := m["hotkey"].(string); k != "" || !st.back {
		return k
	}
	return strconv.Itoa(n + 1)
}

// openDetail pushes the detail view of item idx: its detail text as the
// body and its actions as the list, under the parent's origin so the badge
// and the run gate stay the parent's.
func (a *App) openDetail(parent *overlayState, idx int) {
	m, _ := parent.o.Items[idx].(map[string]any)
	body, _ := m["detail"].(string)
	actions, _ := m["actions"].([]any)
	title := parent.items[idx].label
	if parent.o.Title != "" {
		title = parent.o.Title + " / " + title
	}
	p := parent.o
	st := a.overlayState(hostapi.Overlay{ID: p.ID + "#detail", Title: title, Origin: p.Origin, Path: p.Path, Gen: p.Gen, Anchor: p.Anchor, Body: body, BodyMD: p.BodyMD, Items: actions, OnAction: p.OnAction})
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
	m, _ := d.o.Items[idx].(map[string]any)
	fill, _ := m["fill"].(string)
	id, _ := m["id"].(string)
	switch {
	case id != "" && d.o.OnAction != nil:
		a.background(func() { a.host.OverlayAction(context.Background(), p, d.item, m) })
		if p.Reload == nil {
			a.closeOverlayByID(p.ID) // the detail view goes with it
			return
		}
	case rowRun(d.o, m) == "" && fill == "":
	case fill == "" && p.Reload != nil:
		a.runOverlayRow(d.o, idx)
	default:
		a.runOverlayRow(d.o, idx)
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
	if key == "space" && o.Reload != nil {
		text = "" // space toggles rows in a toggle list
	}
	// a body that overflows takes the scroll keys, and the list keys too
	// when there is no list to move in
	if st.body.overflows() && scrollKey(&st.body, key, len(o.Items) == 0) {
		return nil
	}
	if len(o.Items) > 0 && !st.back && st.edit(key, text, st.texts) || moveSel(&st.selected, len(st.visible), st.view.pageItems(len(st.visible)), key) {
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
		m, _ := o.Items[idx].(map[string]any)
		verb := st.enterVerb(idx)
		switch {
		case key == "space" && (o.Reload == nil || itemVerb(m) != ""):
		case key == "enter" && hasDetail(m):
			a.openDetail(st, idx)
		case verb == "copy":
			text, _ := m["copy"].(string)
			return a.copied(text)
		case st.back:
			a.detailAction(idx)
		case o.Reload != nil:
			a.runOverlayRow(o, idx)
			a.reloadOverlay(o, func() { a.host.SelectOverlay(context.Background(), o, idx) })
		case verb == "":
		default:
			if o.OnSelect != nil {
				a.background(func() { a.host.SelectOverlay(context.Background(), o, idx) })
			} else {
				a.runOverlayRow(o, idx)
			}
			a.closeOverlayByID(o.ID)
		}
	case "esc":
		if st.back {
			a.closeOverlayByID(o.ID)
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
			lines = append(lines, st.texts[i])
		}
	case len(st.visible) > 0:
		lines = append(lines, st.texts[st.visible[st.selected]])
	}
	if len(lines) == 0 {
		return nil
	}
	return a.copied(strings.Join(lines, "\n"))
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

// rowRun is the command a row runs: honored only for builtin and user
// overlays so a plugin row label cannot disguise a command.
func rowRun(o hostapi.Overlay, m map[string]any) string {
	if o.Origin != lua.OriginBuiltin && o.Origin != lua.OriginUser {
		return ""
	}
	run, _ := m["run"].(string)
	return run
}

// runOverlayRow applies a row's "fill" (into the composer, like tab in the
// palette) and "run" (rowRun).
func (a *App) runOverlayRow(o hostapi.Overlay, idx int) {
	if idx >= len(o.Items) {
		return
	}
	m, ok := o.Items[idx].(map[string]any)
	if !ok {
		return
	}
	if fill, _ := m["fill"].(string); fill != "" {
		a.composer.text = sanitize.String(fill)
		a.composer.moveEnd()
	}
	if run := rowRun(o, m); run != "" {
		name, args := SplitCommand(run)
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
		// the file name, not its path: a full path leaves no room for the title
		if st.o.Path != "" {
			badge += ":" + filepath.Base(st.o.Path)
		}
	}
	inner := boxInner(w)
	if st.o.Input {
		body := [][]hostapi.Segment{st.input.inputRow(inner), nil, hint(inner, hintPair{"enter", "confirm"}, hintPair{"esc", "close"})}
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
		default:
			rows = fieldRows(st.o.Body, w)
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
				m, _ := st.o.Items[i].(map[string]any)
				hl, _ := m["hl"].(string)
				choices = append(choices, promptChoice{key: st.actionKey(n), label: st.items[i].label, desc: st.items[i].desc, hl: hostapi.UserHL(hl)})
			}
			st.view.viewH = limit
			list = choiceView(choices, st.selected, &st.view, inner, level > 1)
			if len(st.visible) > 0 { // one verb for every button: a changing one shifts the row
				hints = append(hints, hintPair{"enter", "confirm"})
			}
		case len(st.visible) > 0:
			if key := (listKey{st.query, st.selected, st.view.top, limit, st.size.w, inner}); st.rows == nil || key != st.rowsKey {
				items := make([]listItem, 0, len(st.visible))
				for _, i := range st.visible {
					items = append(items, st.items[i])
				}
				st.rows = listRowsW(items, st.items, st.selected, &st.view, limit, st.size.w, inner)
				key.top = st.view.top
				st.rowsKey = key
			}
			list = st.rows
			if verb := st.enterVerb(st.visible[st.selected]); verb != "" {
				hints = append(hints, hintPair{"enter", verb})
				enterCopies = verb == "copy"
			}
			m, _ := st.o.Items[st.visible[st.selected]].(map[string]any)
			if st.o.Reload != nil && itemVerb(m) == "" && (st.o.OnSelect != nil || rowRun(st.o, m) != "") {
				hints = append(hints, hintPair{"space", "toggle"})
			}
			for _, k := range st.o.Keys {
				hints = append(hints, hintPair{k.Key, k.Desc})
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
var statusHL = map[string]string{"starting": "Host:Warn", "ready": "Host:Ok", "failed": "Host:Danger", "stopped": "Host:Hint", "off": "Host:Hint"}

// fieldRows wraps a plain body, setting its "key: value" lines as a table:
// dim keys in one column, each value wrapped beside its key, an error's in
// the danger color and a status word in its own.
func fieldRows(body string, w int) [][]hostapi.Segment {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	kw := 0
	for _, l := range lines {
		if m := fieldLine.FindStringSubmatch(l); m != nil {
			kw = max(kw, len(m[1])+2)
		}
	}
	var rows [][]hostapi.Segment
	for _, l := range lines {
		m := fieldLine.FindStringSubmatch(l)
		if m == nil || w-kw < 20 {
			rows = append(rows, hostapi.WrapSegs([]hostapi.Segment{{Text: l}}, w)...)
			continue
		}
		hl := ""
		switch m[1] {
		case "error":
			hl = "Host:Danger"
		case "status":
			hl = statusHL[m[2]]
		}
		for i, r := range hostapi.WrapSegs([]hostapi.Segment{{Text: m[2], HL: hl}}, w-kw) {
			key := ""
			if i == 0 {
				key = m[1]
			}
			rows = append(rows, append([]hostapi.Segment{{Text: fmt.Sprintf("%-*s", kw, key), HL: "Host:Hint"}}, r...))
		}
	}
	return rows
}

// spliceBox composites box rows centered over the chat area; what the box
// does not cover stays visible. It reports the cells the box took, which a
// click outside closes it on.
func spliceBox(rows [][]hostapi.Segment, box [][]hostapi.Segment, area uv.Rectangle, w int) ([][]hostapi.Segment, uv.Rectangle) {
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
		x0 := area.Min.X + max((area.Dx()-segsWidth(box[i]))/2, 1)
		rows = placeBox(rows, [][]hostapi.Segment{margined(box[i])}, x0-1, y0+i, w)
		lo, hi = min(lo, x0-1), max(hi, x0+segsWidth(box[i])+1)
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
	bw := segsWidth(box[0])
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
