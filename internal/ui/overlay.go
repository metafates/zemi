package ui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
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
	input  composerState // text of an input overlay
	scroll int           // first body row shown
	back   bool          // a detail view: esc and its actions return to the overlay under it
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
	top      int // first row shown
	width    int // the widest the list has been, so filtering never narrows its box
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
		f.selected, f.top = 0, 0 // a new query selects its first match
		if f.query == "" {
			f.selected = max(0, slices.Index(f.visible, prev)) // a cleared one keeps the row
		}
	}
	return true
}

// filterRows renders the query line (when set) above rows, or a no-match
// note. rows come from listRowsW with f.width.
func (f *listFilter) filterRows(rows [][]hostapi.Segment) [][]hostapi.Segment {
	for _, r := range rows {
		f.width = max(f.width, segsWidth(r))
	}
	var out [][]hostapi.Segment
	if f.active {
		out = append(out, append([]hostapi.Segment{{Text: "/ ", HL: "Comment"}}, cursorSegs([]rune(f.query), len([]rune(f.query)), "Host:Bold")...))
	}
	out = append(out, rows...)
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
				it.segs = append(it.segs, hostapi.Segment{Text: oneLine.Replace(sanitize.String(seg.Text)), HL: seg.HL})
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
	case has("fill") || rowRun(st.o, m) != "" || st.o.OnSelect != nil:
		return cmp.Or(st.o.Verb, "select")
	case st.back:
		return "back"
	}
	return ""
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
	st := a.overlayState(hostapi.Overlay{ID: p.ID + "#detail", Title: title, Origin: p.Origin, Path: p.Path, Gen: p.Gen, Anchor: p.Anchor, Body: body, Items: actions})
	st.back, st.width = true, parent.width
	a.overlays = append(a.overlays, st)
}

// detailAction runs action idx of the detail view on top. An action that
// runs or fills something ends the parent too, unless it only runs a
// command in a toggle list, which rebuilds; one that does neither goes back.
func (a *App) detailAction(idx int) {
	d := a.overlays[len(a.overlays)-1]
	a.closeOverlayByID(d.o.ID)
	m, _ := d.o.Items[idx].(map[string]any)
	run := rowRun(d.o, m)
	fill, _ := m["fill"].(string)
	pi := len(a.overlays) - 1 // the parent: a detail view opens right on top of it
	switch {
	case run == "" && fill == "":
		return
	case fill == "" && a.overlays[pi].o.Reload != nil:
		a.runOverlayRow(d.o, idx)
		a.reloadOverlay(a.overlays[pi].o, -1)
	default:
		a.cancelOverlay(a.overlays[pi].o)
		a.runOverlayRow(d.o, idx)
	}
}

// reloadOverlay runs the on_select of item idx (-1 for none) of a toggle
// list and rebuilds its items, both on the worker, then swaps the items in
// while it is still open, keeping selection and filter.
func (a *App) reloadOverlay(o hostapi.Overlay, idx int) {
	a.background(func() {
		if idx >= 0 && o.OnSelect != nil {
			a.host.SelectOverlay(context.Background(), o, idx)
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
	a.frameGen++
	if key == "space" && o.Reload != nil {
		text = "" // space toggles rows in a toggle list
	}
	// a body scrolls by page, and by line when there is no list to move in
	d := map[string]int{"pgup": -10, "pgdown": 10}[key]
	if len(o.Items) == 0 {
		d += listMove(key)
	}
	if o.Body != "" && d != 0 {
		st.scroll = max(0, st.scroll+d)
		return nil
	}
	if len(o.Items) > 0 && st.edit(key, text, st.texts) || moveSel(&st.selected, len(st.visible), key) {
		return nil
	}
	// a row's hotkey acts like enter on it; one printable character, so no
	// overlay can take esc or ctrl+c
	if len([]rune(key)) == 1 {
		if i := slices.IndexFunc(st.visible, func(i int) bool { m, _ := o.Items[i].(map[string]any); return m["hotkey"] == key }); i >= 0 {
			st.selected, key = i, "enter"
		}
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
		case st.back:
			a.detailAction(idx)
		case o.Reload != nil:
			a.runOverlayRow(o, idx)
			a.reloadOverlay(o, idx)
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
	title := "Copied to the clipboard"
	if all {
		title = fmt.Sprintf("Copied %d lines to the clipboard", len(lines))
	}
	a.pushToast("info", title, "")
	return copyCmd(strings.Join(lines, "\n"))
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
	a.frameGen++
}

// cancelOverlay closes o and runs its on_close. The overlay's Lua
// callbacks run on the background worker: the key loop never waits on Lua.
func (a *App) cancelOverlay(o hostapi.Overlay) {
	a.closeOverlayByID(o.ID)
	a.frameGen++
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
func overlayBox(st *overlayState, w, h int) [][]hostapi.Segment {
	// the origin badge marks boxes that plugins and project config draw
	badge := ""
	if st.o.Origin != lua.OriginBuiltin && st.o.Origin != lua.OriginUser {
		badge = " " + string(st.o.Origin)
		if st.o.Path != "" {
			badge += ":" + st.o.Path
		}
		badge += " "
	}
	if st.o.Input {
		body := [][]hostapi.Segment{st.input.inputRow(boxInner(w)), {{Text: ""}}, hintLine(boxInner(w), "enter", "confirm", "esc", "cancel")}
		return hostBox(" "+st.o.Title+" ", "Host:Title", badge, body, w)
	}
	// borders, the blank row, the hint, the counter and the filter line, and
	// a body's shortest: 3 rows, the row saying which show and a blank row
	limit := min(max(h-6, 3), 20)
	if st.o.Body != "" {
		limit = min(max(h-11, 3), 20)
	}
	var list [][]hostapi.Segment
	var hints []string
	switch {
	case len(st.o.Items) > 0:
		items := make([]listItem, 0, len(st.visible))
		for _, i := range st.visible {
			items = append(items, st.items[i])
		}
		list = listRowsW(items, st.selected, &st.top, limit, st.width, boxInner(w))
		if len(st.visible) > 0 {
			verb := st.enterVerb(st.visible[st.selected])
			if verb != "" {
				hints = append(hints, "enter", verb)
			}
			if st.o.Reload != nil {
				hints = append(hints, "space", "toggle")
			}
		}
		if len(list) > limit && !st.active {
			hints = append(hints, "/", "filter")
		}
		list = st.filterRows(list)
	case st.o.Body == "":
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
		for l := range strings.SplitSeq(strings.TrimRight(st.o.Body, "\n"), "\n") {
			body = append(body, hostapi.WrapSegs([]hostapi.Segment{{Text: strings.ReplaceAll(l, "\t", "    ")}}, bodyW)...)
		}
		// borders, the hint line and a blank row before it, and the list
		room := h - 4
		if len(list) > 0 {
			room -= len(list) + 1
		}
		if room = max(room, 3); len(body) > room {
			room--
			hints = append(hints, "pgup/pgdn", "scroll")
			st.scroll = min(st.scroll, len(body)-room)
			n := len(body)
			body = append(body[st.scroll:st.scroll+room:st.scroll+room], []hostapi.Segment{{Text: fmt.Sprintf("lines %d-%d of %d", st.scroll+1, st.scroll+room, n), HL: "Comment"}})
		} else {
			st.scroll = 0
		}
		hints = append(hints, "y", "copy")
		body[0] = padSegs(body[0], padW)
		if len(list) > 0 {
			body = append(body, nil)
		}
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

// spliceBox composites box rows centered over the chat area; what the box
// does not cover stays visible.
func spliceBox(rows [][]hostapi.Segment, box [][]hostapi.Segment, area uv.Rectangle, w int) [][]hostapi.Segment {
	if len(rows) == 0 || len(box) == 0 {
		return rows
	}
	bottom := min(area.Max.Y, len(rows))
	bh := min(len(box), bottom-area.Min.Y)
	if bh <= 0 {
		return rows
	}
	y0 := area.Min.Y + (bottom-area.Min.Y-bh)/2
	for i := range bh {
		x0 := area.Min.X + max((area.Dx()-segsWidth(box[i]))/2, 1)
		rows = placeBox(rows, [][]hostapi.Segment{margined(box[i])}, x0-1, y0+i, w)
	}
	return rows
}

// anchorBottom lays box over the bottom rows of the chat area at the left
// edge, just above the statusline that opened it; what it does not cover
// stays visible.
func anchorBottom(rows, box [][]hostapi.Segment, area uv.Rectangle, w int) [][]hostapi.Segment {
	bottom := min(area.Max.Y, len(rows))
	y0 := max(area.Min.Y, bottom-len(box))
	for i := y0; i < bottom; i++ {
		rows = placeBox(rows, [][]hostapi.Segment{margined(box[i-y0])}, area.Min.X, i, w)
	}
	return rows
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
