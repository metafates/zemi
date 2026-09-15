package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

// overlayState is one entry of the overlay stack (DESIGN §8): a Lua (or
// Go-built hostapi.Overlay) plus local list selection.
type overlayState struct {
	o        hostapi.Overlay
	itemText []string // rendered item lines (render_item applied at open)
	listFilter
	input composerState // text of an input overlay
}

func (st *overlayState) refilter() { st.apply(st.itemText) }

// listFilter narrows a list as the user types: visible holds the indices of
// rows containing every word of query (case-insensitive), and selected
// indexes visible.
type listFilter struct {
	active   bool // "/" was pressed: keys type into query
	query    string
	visible  []int
	selected int
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
	return true
}

// filterRows renders the query line (when set) above rows, or a no-match
// note.
func (f *listFilter) filterRows(rows [][]hostapi.Segment) [][]hostapi.Segment {
	var out [][]hostapi.Segment
	if f.active {
		out = append(out, append([]hostapi.Segment{{Text: "/ ", HL: "Comment"}}, cursorSegs([]rune(f.query), len([]rune(f.query)), "Host:Bold")...))
	}
	out = append(out, rows...)
	if len(f.visible) == 0 {
		out = append(out, []hostapi.Segment{{Text: "  no matches", HL: "Comment"}})
	}
	return out
}

// openOverlay pushes an overlay, rendering its items up front (render_item
// runs once per item on the queue; the frame never calls Lua).
func (a *App) openOverlay(o hostapi.Overlay) {
	a.overlays = append(a.overlays, a.overlayState(o))
}

func (a *App) overlayState(o hostapi.Overlay) overlayState {
	// Title and rendered items are Lua strings: sanitize like widget text.
	o.Title = sanitize.String(o.Title)
	st := overlayState{o: o}
	if o.Input {
		st.input.insert(sanitize.String(o.Default))
		return st
	}
	for i, item := range o.Items {
		text, err := a.renderOverlayItem(o, i, item)
		if err != nil {
			a.log.Debug("ui", "overlay render_item failed", map[string]any{
				"id": o.ID, "error": err.Error(),
			})
			text = fmt.Sprint(item)
		}
		st.itemText = append(st.itemText, sanitize.String(text))
	}
	if len(st.itemText) == 0 {
		st.itemText = []string{"(empty)"}
	}
	st.refilter()
	return st
}

// renderOverlayItem renders one item through the overlay's render_item
// Lua callback; overlays without one fall back to plain formatting.
func (a *App) renderOverlayItem(o hostapi.Overlay, idx int, item any) (string, error) {
	if o.RenderItem == nil {
		return formatOverlayItem(item), nil
	}
	res, err := a.host.RT.Call(a.ctx(), o.Origin, o.RenderItem, item)
	if err != nil {
		return "", err
	}
	if len(res) == 0 {
		return "", nil
	}
	if s, ok := res[0].(string); ok {
		return s, nil
	}
	return fmt.Sprint(res[0]), nil
}

// formatOverlayItem renders items without render_item: strings, or
// {text, fill} maps (help, revisions).
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

func (a *App) closeOverlayByID(id string) bool {
	for i := range a.overlays {
		if a.overlays[i].o.ID == id {
			a.overlays = append(a.overlays[:i], a.overlays[i+1:]...)
			return true
		}
	}
	return false
}

// overlayKey routes keys to the topmost overlay; text is what the key types
// (printable keys filter the list).
func (a *App) overlayKey(key, text string) {
	st := &a.overlays[len(a.overlays)-1]
	o := st.o
	if o.Input {
		a.inputKey(st, key, text)
		return
	}
	if key == "space" && o.Reload != nil {
		text = "" // space toggles rows in a toggle list
	}
	if st.edit(key, text, st.itemText) || moveSel(&st.selected, len(st.visible), key) {
		return
	}
	switch key {
	case "enter", "space":
		if len(st.visible) == 0 {
			return
		}
		idx := st.visible[st.selected]
		switch {
		case o.Reload != nil:
			a.runOverlayRow(o, idx)
			sel, query := st.selected, st.query
			o.Items = o.Reload()
			*st = a.overlayState(o)
			st.query, st.selected = query, sel
			st.refilter()
			a.frameGen++
		case key == "space":
		case o.OnSelect != nil:
			// SelectOverlay runs on_select then closes via the hook.
			a.host.SelectOverlay(a.ctx(), o, idx)
		default:
			a.closeOverlayByID(o.ID)
			a.frameGen++
			a.runOverlayRow(o, idx)
		}
	case "esc":
		a.cancelOverlay(o)
	}
}

// inputKey edits an input overlay's line; enter submits, esc cancels.
func (a *App) inputKey(st *overlayState, key, text string) {
	in := &st.input
	switch key {
	case "enter":
		o, value := st.o, in.text
		a.closeOverlayByID(o.ID)
		a.host.SubmitOverlayInput(a.ctx(), o, value)
	case "esc":
		a.cancelOverlay(st.o)
	default:
		if !in.editKey(key) && text != "" {
			in.insert(strings.ReplaceAll(text, "\n", " "))
		}
	}
	a.frameGen++
}

// cancelOverlay closes o and runs its on_close.
func (a *App) cancelOverlay(o hostapi.Overlay) {
	a.closeOverlayByID(o.ID)
	a.frameGen++
	if o.OnClose != nil {
		a.host.CancelOverlay(a.ctx(), o)
	}
}

// runOverlayRow applies a row's "fill" (into the composer, like tab in the
// palette) and "run" (a command, honored only for builtin and user
// overlays so a plugin row label cannot disguise a command).
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
	if run, _ := m["run"].(string); run != "" && (o.Origin == lua.OriginBuiltin || o.Origin == lua.OriginUser) {
		name, args := splitCommand(run)
		a.runCommand(name, args)
	}
}

// overlayBox draws a Lua overlay with the Go-stamped origin badge (DESIGN
// §8).
func overlayBox(st overlayState, w int) [][]hostapi.Segment {
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
		line := append([]hostapi.Segment{{Text: "> ", HL: "Comment"}}, cursorSegs([]rune(st.input.text), st.input.cursor, "")...)
		body := [][]hostapi.Segment{padSegs(line, 40), {{Text: ""}}, hintLine("enter", "confirm", "esc", "cancel")}
		return hostBox(" "+st.o.Title+" ", "Host:Title", badge, body, w)
	}
	items := make([]listItem, 0, len(st.visible))
	for _, i := range st.visible {
		items = append(items, listItem{label: st.itemText[i]})
	}
	body := st.filterRows(listRows(items, st.selected, 10, "Title"))
	hints := hintLine("enter", "select", "/", "filter", "esc", "close")
	if st.o.Reload != nil {
		hints = hintLine("space", "toggle", "/", "filter", "esc", "close")
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, hints)
	return hostBox(" "+st.o.Title+" ", "Host:Title", badge, body, w)
}

// spliceBox writes box rows centered over the chat area, replacing the
// covered rows entirely.
func spliceBox(rows [][]hostapi.Segment, box [][]hostapi.Segment, w, headerH, chatH int) [][]hostapi.Segment {
	if len(rows) == 0 || len(box) == 0 {
		return rows
	}
	chatBottom := min(headerH+chatH, len(rows))
	bh := min(len(box), chatBottom-headerH)
	if bh <= 0 {
		return rows
	}
	y0 := headerH + (chatBottom-headerH-bh)/2
	for i := 0; i < bh; i++ {
		bw := segsWidth(box[i])
		x0 := max((w-bw)/2, 1)
		// composite: what the box does not cover stays visible
		under := rows[y0+i]
		row := padSegs(truncateSegs(under, x0), x0)
		row = append(row, box[i]...)
		row = append(row, dropCells(under, x0+bw)...)
		rows[y0+i] = padSegs(truncateSegs(row, w), w)
	}
	return rows
}

// anchorBottom lays box over the bottom rows of the chat area at the left
// edge, just above the statusline that opened it; what it does not cover
// stays visible.
func anchorBottom(rows, box [][]hostapi.Segment, w, headerH, chatH int) [][]hostapi.Segment {
	bottom := min(headerH+chatH, len(rows))
	y0 := max(headerH, bottom-len(box))
	for i := y0; i < bottom; i++ {
		b := box[i-y0]
		row := append([]hostapi.Segment{{Text: " "}}, b...)
		rows[i] = padSegs(truncateSegs(append(row, dropCells(rows[i], segsWidth(b)+1)...), w), w)
	}
	return rows
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
