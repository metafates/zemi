// memories.go is /memories: the browser over the eternal chat. It shows the
// live view - the lines the next model call sends, placeholders included,
// each without its id prefix - one row per line, the model's id for it as
// the right-hand tag. l drills into a line's two children as an overlay of
// its own, the way /messages descends into turns and rails, and h climbs
// back. It is read-only: no key here mutates the log or the tree.

package hostapi

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// memoriesCommands is /memories and the levels under it.
func (h *Host) memoriesCommands() []*Command {
	return []*Command{{
		Name: "memories", NArgs: NArgsAny, Desc: "browse the chat view the model reads",
		GoFn:       func(h *Host, args string, confirm bool) error { return h.MemoriesCommand(args) },
		GoComplete: SubcommandCompleter([]string{"zoom", "view"}, nil),
	}}
}

// MemoriesCommand is /memories: no argument opens the browser, "zoom l i"
// drills into a line's two children as an overlay of its own, "view l i"
// reads the messages a line covers.
func (h *Host) MemoriesCommand(args string) error {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	switch verb {
	case "":
		return h.openMemories()
	case "zoom", "view":
		l, i, err := parseNodeArgs(rest)
		if err != nil {
			return Usagef("/memories %s <level> <index> (a line the list shows)", verb)
		}
		if verb == "view" {
			return h.openMemoriesRange(l, i)
		}
		return h.openMemoriesZoom(nodeID{l, i})
	default:
		return Usagef("usage: /memories [zoom <l> <i> | view <l> <i>]")
	}
}

// parseNodeArgs reads a tree node's "l i" arguments. The level is a shift
// count downstream, so it stays in the range a tree over any log can hold.
func parseNodeArgs(rest string) (int64, int64, error) {
	l, i, _ := strings.Cut(strings.TrimSpace(rest), " ")
	li, err1 := strconv.ParseInt(strings.TrimSpace(l), 10, 64)
	ii, err2 := strconv.ParseInt(strings.TrimSpace(i), 10, 64)
	if err1 != nil || err2 != nil || li < 0 || ii < 0 || li > 62 {
		return 0, 0, errors.New("not a line")
	}
	return li, ii, nil
}

// openMemories opens the browser and tracks it, so a flush rebuilds it the
// way the messages picker's levels are.
func (h *Host) openMemories() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(h.memoriesOverlay())
	h.trackMessages("memories")
	return nil
}

// openMemoriesZoom drills into one view node: an overlay of its own holding
// the node's two children under its own line. The gate reaches from the
// view: the node must be a view line or hang under one - zoom climbs down
// from what the model reads, never above it. A leaf has no children and
// refuses, which is the toast that says enter reads the messages it covers.
func (h *Host) openMemoriesZoom(id nodeID) error {
	if id.l == 0 {
		return Usagef("%s is a leaf: enter reads the messages it covers", id.id())
	}
	if h.DB == nil {
		return errors.New("no chat log in this boot")
	}
	lines, err := h.DB.ViewLines()
	if err != nil {
		return err
	}
	above := -1 // the view line the node's first message falls in
	for k, ln := range lines {
		if (nodeID{ln.L, ln.I}).first() > id.first() {
			break
		}
		above = k
	}
	if above < 0 || id.last() > (nodeID{lines[above].L, lines[above].I}).last() {
		return Usagef("%s is above the view: zoom reaches down from it", id.id())
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	o, ok := h.memoriesZoomOverlay(id)
	if !ok {
		return errors.New("no chat log in this boot")
	}
	ui.OpenOverlay(o)
	h.trackMessages(o.ID)
	return nil
}

// memoriesOverlay is the browser: one row per line of the live view, oldest
// first, an overview of what the model reads. The chat stays where it is:
// finding a turn's place on the screen is /messages' job.
func (h *Host) memoriesOverlay() Overlay {
	o := Overlay{
		ID: "memories", Title: "Memories", Origin: lua.OriginBuiltin,
		// Follow holds the browsing-menu identity (esc closes the lot); the
		// rows carry no anchor, so nothing follows the selection
		Anchor: OverlayBottomRight, Follow: true, KeepOrder: true, Empty: "The chat's view is empty.",
	}
	if h.DB == nil {
		return o
	}
	lines, err := h.DB.ViewLines()
	if err != nil {
		return o
	}
	b, n, levels, unbuilt := viewStatsOf(lines)
	lo := h.chatWindowLo()
	for _, ln := range lines {
		o.Items = append(o.Items, h.memoriesRow(nodeID{ln.L, ln.I}, ln.Text, lo))
	}
	if n > 0 {
		foot := fmt.Sprintf("%s  %s  %s", fmtBytes(int64(b)), pluralCount(n, "line", "lines"), pluralCount(levels, "level", "levels"))
		if unbuilt > 0 {
			foot += fmt.Sprintf("  %d not summarized yet", unbuilt)
		}
		foot += "  tags are line ids"
		o.Foot = foot
	}
	return o
}

// memoriesZoomOverlay is one drill-in level: the opened line's own text and
// its two children, placeholders included. ok false once the log no longer
// reaches the node, so a flush closes the level; a node hanging from message
// 0 outlives a reset until the builder re-merges.
func (h *Host) memoriesZoomOverlay(id nodeID) (Overlay, bool) {
	o := Overlay{
		ID: "memories zoom " + id.args(), Title: "Memories / " + id.id(), Origin: lua.OriginBuiltin,
		Anchor: OverlayBottomRight, Follow: true,
	}
	if h.DB == nil {
		return o, false
	}
	count, err := h.DB.LogCount()
	if err != nil || id.first() >= count {
		return o, false
	}
	if n, err := h.DB.NodeAt(id.l, id.i); err == nil && n.Text != "" {
		o.Body = n.Text
		o.Foot = fmt.Sprintf("%s  %s", fmtBytes(int64(n.Size)), pluralCount(int(id.span()), "message", "messages"))
	} else {
		o.Foot = pluralCount(int(id.span()), "message", "messages")
	}
	lo := h.chatWindowLo()
	for _, child := range []nodeID{{id.l - 1, id.i * 2}, {id.l - 1, id.i*2 + 1}} {
		text := ""
		if n, err := h.DB.NodeAt(child.l, child.i); err == nil {
			text = n.Text
		}
		o.Items = append(o.Items, h.memoriesRow(child, text, lo))
	}
	return o, true
}

// memoriesRow is one browser row: the line's text as the model reads it, the
// model's id for the line as the right-hand tag. A line the chat window no
// longer holds says so; enter reads the messages either way.
func (h *Host) memoriesRow(id nodeID, text string, winLo int64) OverlayItem {
	if text == "" {
		text = placeholder
	}
	item := OverlayItem{
		ID:   id.key(),
		Text: text,
		// the line's own id is what zoom and the model name it by
		Copy: id.id(),
		Tag:  id.id(),
	}
	if id.last() < winLo {
		// the window's oldest row is younger than everything the line covers
		item.Desc = "older than the chat window"
	}
	if id.l > 0 {
		item.Run, item.Verb = "memories zoom "+id.args(), VerbOpen
	} else {
		item.Run, item.Verb = "memories view "+id.args(), VerbOpen
	}
	if id.l > 0 {
		item.Keys = append(item.Keys, ItemKey{Key: "l", Desc: "zoom", Run: "memories zoom " + id.args(), Verb: VerbOpen})
	}
	item.Keys = append(item.Keys, ItemKey{Key: "v", Desc: "preview", Run: "memories view " + id.args(), Verb: VerbOpen})
	return item
}

// chatWindowLo is the seq of the oldest logged message the chat window
// holds: the log is append-only and the window is its suffix, so every
// message from here on is on the screen. -1 when the window holds none.
func (h *Host) chatWindowLo() int64 {
	h.tMu.RLock()
	tr := h.session.Transcript
	h.tMu.RUnlock()
	for _, m := range tr {
		if m.Seq != NoSeq {
			return m.Seq
		}
	}
	return -1
}

// rangePreviewRows bounds one preview body: a top-level line covers the
// whole log, and a body view that read it would too.
const rangePreviewRows = 500

// openMemoriesRange reads the messages one view line covers: the log's raw
// rows, oldest first, in a read-only body.
func (h *Host) openMemoriesRange(l, i int64) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if h.DB == nil {
		return errors.New("no chat log in this boot")
	}
	id := nodeID{l, i}
	count, err := h.DB.LogCount()
	if err != nil {
		return err
	}
	if id.first() >= count {
		return Usagef("no line %s in a chat of %d messages", id.id(), count)
	}
	// NOTE: a top-level line covers the whole log; the preview reads its head
	// and says so, the way a body view stays scrollable but bounded
	rows, err := h.DB.LogFrom(id.first(), int(min(id.span(), count-id.first(), rangePreviewRows)))
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, row := range rows {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		head := string(row.Kind)
		if name := rowTool(row); name != "" {
			head += " " + name
		}
		// the date a non-coder cannot reach anywhere else: zoom names ids,
		// the log row's stamp is the only when
		if ms := stampMS(row.CreatedAt); ms > 0 {
			head += " " + time.UnixMilli(ms).Format(previewStamp)
		}
		b.WriteString(head + ": " + row.Text)
	}
	if covered := id.span(); covered > int64(len(rows)) {
		fmt.Fprintf(&b, "\n\n... %d more messages: zoom the line open for them", covered-int64(len(rows)))
	}
	o := Overlay{
		ID:    "memories range " + id.args(),
		Title: "Memories / " + id.id(), Origin: lua.OriginBuiltin,
		Body: b.String(),
	}
	if b.Len() == 0 { // the rows were gone before the read (a reset raced)
		o.Empty = "the log rows are gone from this store"
	}
	ui.OpenOverlay(o)
	return nil
}

// previewStamp is a preview row's head date, local wall clock.
const previewStamp = "2006-01-02 15:04"

// viewStats sizes the live view from the store: the same snapshot feeds the
// /usage row.
func (h *Host) viewStats() (bytes, lines, levels, unbuilt int) {
	if h.DB == nil {
		return 0, 0, 0, 0
	}
	ls, err := h.DB.ViewLines()
	if err != nil {
		return 0, 0, 0, 0
	}
	return viewStatsOf(ls)
}

// viewStatsOf sizes a view: the bytes its lines fill, how many lines and
// levels it spans, and how many are still placeholders.
func viewStatsOf(ls []store.ViewLineText) (bytes, lines, levels, unbuilt int) {
	seen := map[int64]bool{}
	for _, ln := range ls {
		lines++
		if ln.Text == "" {
			unbuilt++
			bytes += len(placeholder)
		} else {
			bytes += ln.Size
		}
		if !seen[ln.L] {
			seen[ln.L] = true
			levels++
		}
	}
	return bytes, lines, levels, unbuilt
}

// fmtBytes reads a byte count the way fmtTokens reads tokens: 18432 is
// 18.4k, 150000000 is 150M.
func fmtBytes(n int64) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	case n >= 1000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e3), ".0") + "k"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}
