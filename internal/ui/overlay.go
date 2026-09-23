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
	back  bool // a detail view: esc and its actions return to the overlay under it
	item  any  // a detail view's item: a reload can swap the parent's under it
	// listH is the rows the unfiltered list took at width listW, which the
	// box keeps while it is open
	listH, listW int
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
	width    int      // the widest the list has been, so filtering never narrows its box
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

// filterRows renders the query line above rows, a hint while no query is
// open so opening one adds no row, and a no-match note. rows come from
// listRowsW with f.width.
func (f *listFilter) filterRows(rows [][]hostapi.Segment) [][]hostapi.Segment {
	for _, r := range rows {
		f.width = max(f.width, segsWidth(r))
	}
	query := []hostapi.Segment{{Text: "/", HL: "Host:HintKey"}, {Text: " filter", HL: "Comment"}}
	if f.active {
		query = append([]hostapi.Segment{{Text: "/ ", HL: "Comment"}}, cursorSegs([]rune(f.query), len([]rune(f.query)), "Host:Bold")...)
	}
	out := append([][]hostapi.Segment{query}, rows...)
	if len(f.visible) == 0 {
		out = append(out, padSegs([]hostapi.Segment{{Text: "  no matches", HL: "Comment"}}, f.width))
	}
	return out
}

// openOverlay pushes an overlay; Lua rendered its items when it opened, so
// the key loop never calls Lua for them.
func (a *App) openOverlay(o hostapi.Overlay) {
	a.overlays = append(a.overlays, a.overlayState(o))
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
// the overlay's own verb ("select" by default), "back" from a detail view,
// or "" when it does nothing.
func (st *overlayState) enterVerb(idx int) string {
	m, _ := st.o.Items[idx].(map[string]any)
	has := func(k string) bool { s, _ := m[k].(string); return s != "" }
	switch {
	case has("detail") || m["actions"] != nil:
		return "open"
	case has("fill") || rowRun(st.o, m) != "" || st.o.OnSelect != nil || st.back && has("id") && st.o.OnAction != nil:
		return cmp.Or(st.o.Verb, "select")
	case has("copy"):
		return "copy"
	case st.back:
		return "back"
	}
	return ""
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
		title = parent.o.Title + " › " + title
	}
	p := parent.o
	st := a.overlayState(hostapi.Overlay{ID: p.ID + "#detail", Title: title, Origin: p.Origin, Path: p.Path, Gen: p.Gen, Anchor: p.Anchor, Body: body, BodyMD: p.BodyMD, Items: actions, OnAction: p.OnAction})
	st.back, st.item, st.width = true, parent.o.Items[idx], parent.width
	a.overlays = append(a.overlays, st)
}

// detailAction runs action idx of the detail view on top. An action that
// runs or fills something ends the parent too, unless it only runs a
// command in a toggle list, which rebuilds; one with an id goes to
// on_action; one that does none of it goes back.
func (a *App) detailAction(idx int) {
	d := a.overlays[len(a.overlays)-1]
	a.closeOverlayByID(d.o.ID)
	m, _ := d.o.Items[idx].(map[string]any)
	run := rowRun(d.o, m)
	fill, _ := m["fill"].(string)
	pi := len(a.overlays) - 1 // the parent: a detail view opens right on top of it
	if id, _ := m["id"].(string); id != "" && d.o.OnAction != nil {
		p := a.overlays[pi].o
		a.overlayCall(p, func() { a.host.OverlayAction(context.Background(), p, d.item, m) })
		return
	}
	switch {
	case run == "" && fill == "":
		return
	case fill == "" && a.overlays[pi].o.Reload != nil:
		a.runOverlayRow(d.o, idx)
		a.reloadOverlay(a.overlays[pi].o, nil)
	default:
		a.cancelOverlay(a.overlays[pi].o)
		a.runOverlayRow(d.o, idx)
	}
}

// overlayCall runs a Lua callback of a list on the worker: a toggle list
// stays open and rebuilds, any other closes first, like on_select.
func (a *App) overlayCall(o hostapi.Overlay, call func()) {
	if o.Reload != nil {
		a.reloadOverlay(o, call)
		return
	}
	a.closeOverlayByID(o.ID)
	a.background(call)
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
			i := slices.IndexFunc(a.overlays, func(st overlayState) bool { return st.o.ID == o.ID && st.o.Gen == o.Gen && !st.back })
			if i < 0 {
				return
			}
			st := &a.overlays[i]
			f, next := st.listFilter, st.o
			next.Items, next.ItemText = items, text
			*st = a.overlayState(next)
			st.listFilter = f
			st.refilter()
		})
	})
}

// closeOverlayByID closes an overlay and the detail views over it.
func (a *App) closeOverlayByID(id string) {
	a.overlays = slices.DeleteFunc(a.overlays, func(st overlayState) bool {
		return st.o.ID == id || strings.HasPrefix(st.o.ID, id+"#detail")
	})
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
	if len(o.Items) > 0 && st.edit(key, text, st.texts) || moveSel(&st.selected, len(st.visible), st.view.pageItems(len(st.visible)), key) {
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
		verb := st.enterVerb(idx)
		switch {
		case key == "space" && o.Reload == nil:
		case key == "enter" && verb == "open":
			a.openDetail(st, idx)
		case verb == "copy":
			m, _ := o.Items[idx].(map[string]any)
			text, _ := m["copy"].(string)
			return a.copied(text)
		case st.back:
			a.detailAction(idx)
		case o.Reload != nil:
			a.runOverlayRow(o, idx)
			a.reloadOverlay(o, func() { a.host.SelectOverlay(context.Background(), o, idx) })
		case verb == "":
		default:
			a.closeOverlayByID(o.ID)
			if o.OnSelect != nil {
				a.background(func() { a.host.SelectOverlay(context.Background(), o, idx) })
			} else {
				a.runOverlayRow(o, idx)
			}
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
		a.closeOverlayByID(o.ID)
		a.background(func() { a.host.SubmitOverlayInput(context.Background(), o, value) })
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
	a.closeOverlayByID(o.ID)
	a.bump()
	if o.OnClose != nil {
		a.background(func() { a.host.CancelOverlay(context.Background(), o) })
	}
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
		name, args := splitCommand(run)
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
		badge = " " + string(st.o.Origin)
		// the file name, not its path: a full path leaves no room for the title
		if st.o.Path != "" {
			badge += ":" + filepath.Base(st.o.Path)
		}
		badge += " "
	}
	if st.o.Input {
		body := [][]hostapi.Segment{st.input.inputRow(boxInner(w)), {{Text: ""}}, hintLine(boxInner(w), "enter", "confirm", "esc", "cancel")}
		return hostBox(" "+st.o.Title+" ", "Host:Title", badge, body, w)
	}
	// borders, the blank row and the hint, the filter line (a detail view's
	// actions have none), and a body's shortest: 3 rows and a blank row
	limit := h - 5
	if st.back {
		limit++
	}
	if st.o.Body != "" {
		limit -= 4
	}
	limit = max(limit, 3)
	var list [][]hostapi.Segment
	var hints []string
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
		list = st.view.wrap(func(w int) [][]hostapi.Segment { return choiceRows(choices, st.selected, w) }, boxInner(w))
		st.view.rows = len(list)
		sel, end := len(list), 0 // the selected action's rows
		for y, r := range list {
			if len(r) > 0 && r[0].Hit == st.selected+1 {
				sel, end = min(sel, y), y+1
			}
		}
		st.view.reveal(sel, end)
		list = st.view.window(list, boxInner(w))
		if len(st.visible) > 0 {
			hints = append(hints, "enter", cmp.Or(st.enterVerb(st.visible[st.selected]), "back"))
		}
	case len(st.o.Items) > 0:
		if key := (listKey{st.query, st.selected, st.view.top, limit, st.width, boxInner(w)}); st.rows == nil || key != st.rowsKey {
			items := make([]listItem, 0, len(st.visible))
			for _, i := range st.visible {
				items = append(items, st.items[i])
			}
			st.rows = listRowsW(items, st.items, st.selected, &st.view, limit, st.width, boxInner(w))
			key.top = st.view.top
			st.rowsKey = key
		}
		list = st.rows
		if len(st.visible) > 0 {
			verb := st.enterVerb(st.visible[st.selected])
			if verb != "" {
				hints = append(hints, "enter", verb)
			}
			m, _ := st.o.Items[st.visible[st.selected]].(map[string]any)
			if st.o.Reload != nil && (st.o.OnSelect != nil || rowRun(st.o, m) != "") {
				hints = append(hints, "space", "toggle")
			}
		}
		for _, k := range st.o.Keys {
			hints = append(hints, k.Key, k.Desc)
		}
		list = st.filterRows(list)
		// The list keeps the height it had unfiltered, so the box does not
		// resize under the pointer on every keystroke of a filter; only a
		// terminal resize measures it again.
		if st.listH == 0 || st.listW != w {
			st.listH, st.listW = len(list), w
		}
		for n := min(st.listH, limit+1); len(list) < n; {
			list = append(list, padSegs(nil, st.width))
		}
	case st.o.Body == "" || st.o.Empty != "":
		for l := range strings.SplitSeq(cmp.Or(st.o.Empty, "Nothing here."), "\n") {
			list = append(list, hostapi.WrapSegs([]hostapi.Segment{{Text: l, HL: "Comment"}}, boxInner(w))...)
		}
	}
	var body [][]hostapi.Segment
	if st.o.Body != "" {
		// a detail view keeps about the width of the list it opened from
		bodyW, padW := boxInner(w), min(boxInner(w), 72)
		if st.back {
			bodyW, padW = min(bodyW, max(st.width, 56)), min(bodyW, st.width)
		}
		// borders, the hint line and a blank row before it, and the list
		st.body.viewH = h - 4
		if len(list) > 0 {
			st.body.viewH -= len(list) + 1
		}
		st.body.viewH = max(st.body.viewH, 3)
		body = st.body.window(st.body.wrap(func(w int) (rows [][]hostapi.Segment) {
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
		}, bodyW), bodyW)
		body[0] = padSegs(body[0], padW)
		if len(list) > 0 {
			body = append(body, nil)
		}
	}
	hints = append(hints, scrolls(st.body, st.view)...)
	if st.o.Body != "" && !st.active { // y types into an open filter
		hints = append(hints, "y", "copy")
	}
	esc := "close"
	switch {
	case st.active: // esc clears an open filter first
		esc = "clear"
	case st.back:
		esc = "back"
	case st.o.OnSelect != nil:
		esc = "cancel"
	}
	// the box keeps the widest hint it showed, so moving between rows with
	// different hints never resizes it under the pointer
	hint := hintLine(boxInner(w), append(hints, "esc", esc)...)
	st.width = max(st.width, segsWidth(hint))
	body = append(append(body, list...), nil, padSegs(hint, min(st.width, boxInner(w))))
	return hostBox(" "+st.o.Title+" ", "Host:Title", badge, body, w)
}

// fieldLine is a "key: value" line of a detail body.
var fieldLine = regexp.MustCompile(`^([a-z][a-z -]{0,14}): (.*)$`)

// fieldRows wraps a plain body, setting its "key: value" lines as a table:
// dim keys in one column, each value wrapped beside its key, an error's in
// the danger color.
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
		switch {
		case m[1] == "error", l == "status: failed":
			hl = "Host:Danger"
		case l == "status: ready":
			hl = "Host:Ok"
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

// anchorBottom lays box over the bottom rows of the chat area at the left
// edge, just above the statusline that opened it; what it does not cover
// stays visible. It reports the cells the box took.
func anchorBottom(rows, box [][]hostapi.Segment, area uv.Rectangle, w int) ([][]hostapi.Segment, uv.Rectangle) {
	bottom := min(area.Max.Y, len(rows))
	y0 := max(area.Min.Y, bottom-len(box))
	width := 0
	for i := y0; i < bottom; i++ {
		rows = placeBox(rows, [][]hostapi.Segment{margined(box[i-y0])}, area.Min.X, i, w)
		width = max(width, segsWidth(box[i-y0])+2)
	}
	return rows, uv.Rect(area.Min.X, y0, width, max(bottom-y0, 0))
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
