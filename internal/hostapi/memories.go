// memories.go is /memories: the browser over the eternal chat. It shows the
// live view - exactly the lines the next model call sends, placeholders
// included - and zooms a line into its two children in place, the model's
// own zoom, interactive. It is read-only: no key here mutates the log or the
// tree.

package hostapi

import (
	"errors"
	"fmt"
	"slices"
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
		GoComplete: SubcommandCompleter([]string{"zoom", "pop", "view"}, nil),
	}}
}

// MemoriesCommand is /memories: no argument opens the browser, "zoom l i"
// opens a line's two children in its place, "pop" closes one zoom, "view l i"
// reads the messages a line covers.
func (h *Host) MemoriesCommand(args string) error {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	switch verb {
	case "":
		h.msgMu.Lock()
		h.memZoom = nil
		h.msgMu.Unlock()
		return h.openMemories()
	case "zoom", "view":
		l, i, err := parseNodeArgs(rest)
		if err != nil {
			return Usagef("/memories %s <level> <index> (a line the list shows)", verb)
		}
		if verb == "view" {
			return h.openMemoriesRange(l, i)
		}
		return h.zoomMemories(nodeID{l, i})
	case "pop":
		h.msgMu.Lock()
		if n := len(h.memZoom); n > 0 {
			h.memZoom = h.memZoom[:n-1]
		}
		h.msgMu.Unlock()
		return h.openMemories()
	}
	return Usagef("usage: /memories [zoom <l> <i> | pop | view <l> <i>]")
}

// parseNodeArgs reads a tree node's "l i" arguments.
func parseNodeArgs(rest string) (int64, int64, error) {
	l, i, _ := strings.Cut(strings.TrimSpace(rest), " ")
	li, err1 := strconv.ParseInt(strings.TrimSpace(l), 10, 64)
	ii, err2 := strconv.ParseInt(strings.TrimSpace(i), 10, 64)
	if err1 != nil || err2 != nil {
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

// zoomMemories pushes one zoom onto the stack and reopens the browser.
func (h *Host) zoomMemories(id nodeID) error {
	if id.l == 0 {
		return Usagef("%s is a leaf: enter takes the chat to the message", id.id())
	}
	if h.DB == nil {
		return errors.New("no chat log in this boot")
	}
	lines, err := h.DB.ViewLines()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(lines, func(v store.ViewLineText) bool { return v.L == id.l && v.I == id.i }) {
		return Usagef("no line %s in the view right now", id.id())
	}
	h.msgMu.Lock()
	h.memZoom = append(h.memZoom, id)
	h.msgMu.Unlock()
	return h.openMemories()
}

// memoriesOverlay is the browser: one row per line of the live view, the
// zoom stack's expansions applied, oldest first. The rows the chat draws the
// line's first covered message at are the anchors, so moving the selection
// takes the chat there (/messages' jump path) and enter on a leaf keeps it.
func (h *Host) memoriesOverlay() Overlay {
	h.msgMu.Lock()
	zoom := slices.Clone(h.memZoom)
	h.msgMu.Unlock()
	o := Overlay{
		ID: "memories", Title: "Memories", Origin: lua.OriginBuiltin,
		Anchor: OverlayBottomRight, Follow: true, Empty: "The chat's view is empty.",
	}
	if h.DB == nil {
		return o
	}
	lines, err := h.DB.ViewLines()
	if err != nil {
		return o
	}
	b, n, levels, unbuilt := viewStatsOf(lines)
	for _, z := range zoom {
		k := slices.IndexFunc(lines, func(v store.ViewLineText) bool { return v.L == z.l && v.I == z.i })
		if k < 0 || z.l == 0 { // the line merged away or the zoom was popped past
			continue
		}
		a, b := store.ViewLineText{L: z.l - 1, I: z.i * 2}, store.ViewLineText{L: z.l - 1, I: z.i*2 + 1}
		for _, c := range []*store.ViewLineText{&a, &b} {
			if n, err := h.DB.NodeAt(c.L, c.I); err == nil {
				c.Text, c.Size = n.Text, n.Size
			}
		}
		lines = slices.Replace(lines, k, k+1, a, b)
	}
	jumps := h.chatJumpRows()
	for _, ln := range lines {
		id := nodeID{ln.L, ln.I}
		item := OverlayItem{
			ID:   id.key(),
			Text: renderLine(ln.L, ln.I, ln.Text),
			// the line's own id is what zoom and the model name it by
			Copy: id.id(),
		}
		if at := jumps.at(id.first(), id.last()); at > 0 {
			item.Anchor = at
		}
		if ln.L > 0 {
			item.Run, item.Verb = "memories zoom "+id.args(), VerbOpen
		}
		// l zooms the line the footer names it by; on a leaf the zoom refuses,
		// which is the toast that says enter takes the chat to the message
		item.Keys = append(item.Keys, ItemKey{Key: "l", Desc: "zoom", Run: "memories zoom " + id.args(), Verb: VerbOpen})
		item.Keys = append(item.Keys, ItemKey{Key: "v", Desc: "preview", Run: "memories view " + id.args(), Verb: VerbOpen})
		if len(zoom) > 0 {
			item.Keys = append(item.Keys, ItemKey{Key: "u", Desc: "zoom out", Run: "memories pop", Verb: VerbOpen})
		}
		o.Items = append(o.Items, item)
	}
	if n > 0 {
		o.Foot = fmt.Sprintf("%s · %s · %s · %s unbuilt", fmtBytes(int64(b)), pluralCount(n, "line", "lines"),
			pluralCount(levels, "level", "levels"), strconv.Itoa(unbuilt))
	}
	return o
}

// chatJumps is where the chat draws the logged messages now: the stamped
// rows' seqs in order, each with its window row, so a browser row finds the
// first message its range holds that the chat shows.
type chatJumps struct {
	seqs []int64
	rows map[int64]int
}

// chatJumpRows reads the jump map from the transcript window: a row whose
// seq the stamping filled names the row the chat draws it at; a row the
// pairing marked NoSeq (a failed log write, or not stamped yet) names
// nothing.
func (h *Host) chatJumpRows() chatJumps {
	h.tMu.RLock()
	tr := h.session.Transcript
	h.tMu.RUnlock()
	j := chatJumps{rows: make(map[int64]int, len(tr))}
	for i, m := range tr {
		if m.Seq != NoSeq {
			j.seqs = append(j.seqs, m.Seq)
			j.rows[m.Seq] = i + 1
		}
	}
	return j
}

// at is the window row of the first message in [first, last] the chat holds,
// 0 when it holds none of them.
func (j chatJumps) at(first, last int64) int {
	k, _ := slices.BinarySearch(j.seqs, first)
	if k >= len(j.seqs) {
		return 0
	}
	if seq := j.seqs[k]; seq <= last {
		return j.rows[seq]
	}
	return 0
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
		head := row.Kind
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
	}
	return strconv.FormatInt(n, 10) + " B"
}
