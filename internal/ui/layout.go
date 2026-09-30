package ui

import (
	"slices"
	"strings"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
)

// spanKid is one child of a box as the solver sees it.
type spanKid struct {
	size     hostapi.Size
	want     int // a content node's measured cells
	min, max int // max 0 means none
}

func (k spanKid) clamp(n int) int {
	n = max(n, k.min)
	if k.max > 0 {
		n = min(n, k.max)
	}
	return n
}

// span divides a box's avail cells between its children. Fill children
// share what the others leave by weight, up to their max; room nobody
// takes stays blank. When the children do not fit, fill gives cells up
// first, then percent, content and cells, the last child first within a
// rank: down to min, then down to 0. It never fails and never hands out
// more than avail.
func span(avail int, kids []spanKid) []int {
	avail = max(avail, 0)
	out := make([]int, len(kids))
	used := 0
	for i, k := range kids {
		switch k.size.Kind {
		case hostapi.SizeCells:
			out[i] = k.size.N
		case hostapi.SizePercent:
			out[i] = (k.size.N*avail + 50) / 100
		case hostapi.SizeContent:
			out[i] = k.want
		case hostapi.SizeFill: // takes what is left, below
		}
		out[i] = k.clamp(max(out[i], 0))
		used += out[i]
	}
	// NOTE: one cell at a time to the fill with the most weight per cell
	// given (D'Hondt), O(cells x children): fine at terminal sizes.
	given := make([]int, len(kids))
	for ; used < avail; used++ {
		best := -1
		for i, k := range kids {
			if k.size.Kind != hostapi.SizeFill || k.max > 0 && out[i] >= k.max {
				continue
			}
			if best < 0 || (given[i]+1)*weight(kids[best]) < (given[best]+1)*weight(k) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		out[best]++
		given[best]++
	}
	rank := func(k spanKid) int {
		switch k.size.Kind {
		case hostapi.SizeFill:
			return 3
		case hostapi.SizePercent:
			return 2
		case hostapi.SizeContent:
			return 1
		default:
			return 0
		}
	}
	for _, floor := range []bool{true, false} {
		for r := 3; r >= 0 && used > avail; r-- {
			for i := len(kids) - 1; i >= 0 && used > avail; i-- {
				if rank(kids[i]) != r {
					continue
				}
				lo := 0
				if floor {
					lo = kids[i].min
				}
				cut := min(used-avail, max(out[i]-lo, 0))
				out[i] -= cut
				used -= cut
			}
		}
	}
	return out
}

func weight(k spanKid) int { return max(k.size.N, 1) }

// laid is a layout node solved into its cells for one frame.
type laid struct {
	node *hostapi.LayoutNode
	rect uv.Rectangle
	// rows is the height its widgets render at: the rect's, or for a
	// content node the most it could get, since a height taken from its
	// own last draw could never shrink.
	rows int
	sep  bool // a separator column left of it
	kids []laid
}

// screenLayout is the layout solved for one frame.
type screenLayout struct {
	root  laid
	stage uv.Rectangle
	// rects holds every node with an id, "stage", "screen" and "composer"
	rects map[string]uv.Rectangle
	// scroll is the scrolled region actions without an id move: the first
	// inside the stage, else the first in the tree
	scroll string
	bar    bool
	floats []placedFloat
	// focusable is the focusable node and float ids in focus order; a float
	// that draws nothing gets no rect
	focusable []string
	// panes are the cells a selection may not leave: the leaves, then the
	// floats, then the composer again, since paintFloats keeps it on top
	panes []uv.Rectangle
}

// placedFloat is a Lua float's region with the cells it renders at and
// where it goes, before its content is measured.
type placedFloat struct {
	f             hostapi.LayoutFloat
	rel           uv.Rectangle
	width, height int // render size
}

// layout solves the layout in force for a w by h frame.
func (a *App) layout(w, h int) screenLayout {
	spec := a.host.Layout()
	screen := uv.Rect(0, 0, w, h)
	s := screenLayout{root: a.solve(spec.Root, screen), stage: screen, rects: map[string]uv.Rectangle{"screen": screen}}
	staged := false // the scroll node chosen is inside the stage
	var walk func(l laid, inStage bool)
	walk = func(l laid, inStage bool) {
		n := l.node
		if n.Stage {
			s.stage, inStage = l.rect, true
		}
		if n.ID != "" {
			s.rects[n.ID] = l.rect
		}
		switch n.Kind {
		case hostapi.LayoutComposer:
			s.rects["composer"] = l.rect
			s.bar = s.bar || n.Left != "" || n.Right != ""
		case hostapi.LayoutBar:
			s.bar = true
		case hostapi.LayoutRegion:
			if n.Scroll != "" && (s.scroll == "" || inStage && !staged) {
				s.scroll, staged = n.ID, inStage
			}
			if n.Focusable {
				s.focusable = append(s.focusable, n.ID)
			}
		case hostapi.LayoutBox, hostapi.LayoutSpacer:
		}
		if len(l.kids) == 0 && n.Kind != hostapi.LayoutSpacer {
			s.panes = append(s.panes, l.rect)
		}
		for _, k := range l.kids {
			walk(k, inStage)
		}
	}
	walk(s.root, false)
	s.rects["stage"] = s.stage
	for _, f := range spec.Floats {
		rel, ok := s.rects[f.Relative]
		if f.Hidden || !ok {
			continue
		}
		pf := placedFloat{f: f, rel: rel}
		pf.width = floatAxis(f.Width, rel.Dx())
		pf.height = floatAxis(f.Height, rel.Dy())
		s.floats = append(s.floats, pf)
		if f.Focusable {
			s.focusable = append(s.focusable, f.ID)
		}
	}
	return s
}

// floatAxis is a float's render cells along one axis of its relative node:
// cells, a percent of it, or all of it for content (the content then
// takes what it draws).
func floatAxis(sz hostapi.Size, rel int) int {
	switch sz.Kind {
	case hostapi.SizeCells:
		return sz.N
	case hostapi.SizePercent:
		return (sz.N*rel + 50) / 100
	default:
		return rel
	}
}

// solve places n's children inside r. While the children's mins do not
// fit, the last collapsing child is hidden for the frame.
func (a *App) solve(n *hostapi.LayoutNode, r uv.Rectangle) laid {
	l := laid{node: n, rect: r, rows: r.Dy()}
	if n.Kind != hostapi.LayoutBox {
		return l
	}
	row := n.Dir == hostapi.DirRow
	length, cross := r.Dy(), r.Dx()
	if row {
		length, cross = cross, length
	}
	var items []*hostapi.LayoutNode
	for _, c := range n.Children {
		if !c.Hidden {
			items = append(items, c)
		}
	}
	var sizes []int
	var kids []spanKid
	var avail int
	for {
		avail = length
		if n.Sep {
			avail -= max(len(items)-1, 0)
		}
		avail = max(avail, 0)
		kids = make([]spanKid, len(items))
		for i, c := range items {
			kids[i] = spanKid{size: c.Size, min: c.Min, max: c.Max}
			if c.Size.Kind == hostapi.SizeContent && !row {
				kids[i].want = a.measure(c, cross)
			}
		}
		mins, gone := 0, -1
		for i, c := range items {
			mins += kids[i].min
			if c.Collapse {
				gone = i
			}
		}
		if mins <= avail || gone < 0 {
			sizes = span(avail, kids)
			break
		}
		items = slices.Delete(items, gone, gone+1)
	}
	pos := r.Min.Y
	if row {
		pos = r.Min.X
	}
	for i, size := range sizes {
		if i > 0 && n.Sep {
			pos++
		}
		kr := uv.Rect(r.Min.X, pos, cross, size)
		if row {
			kr = uv.Rect(pos, r.Min.Y, size, cross)
		}
		k := a.solve(items[i], kr)
		if c := items[i]; c.Size.Kind == hostapi.SizeContent && !row {
			k.rows = kids[i].clamp(avail)
		}
		k.sep = row && n.Sep && i > 0
		l.kids = append(l.kids, k)
		pos += size
	}
	return l
}

// measure is the rows a content-sized node draws at width.
func (a *App) measure(n *hostapi.LayoutNode, width int) int {
	switch n.Kind {
	case hostapi.LayoutRegion:
		return len(a.regionLines(n.Region, regionWidth(n, width)))
	case hostapi.LayoutComposer:
		if n.Frame {
			return len(a.composerRows(frameInner(width), n.Placeholder)) + 2
		}
		return len(a.composerRows(width, n.Placeholder))
	case hostapi.LayoutBar:
		return 1
	case hostapi.LayoutBox, hostapi.LayoutSpacer:
	}
	return 0
}

// regionWidth is the cells a region's widgets render at: a scrolled one
// keeps its bar's column and a blank before it.
func regionWidth(n *hostapi.LayoutNode, w int) int {
	if n.Scroll != "" {
		return barText(w)
	}
	return w
}

// shownWidget is a widget of a region the layout draws, with the cells it
// renders at.
type shownWidget struct {
	*hostapi.Widget
	width, height int
}

// shownWidgets lists the widgets of every region the layout draws; widgets
// of other regions cost no render.
func (a *App) shownWidgets() []shownWidget {
	if a.width <= 0 || a.height <= 0 {
		return nil
	}
	s := a.layout(a.width, a.height)
	var out []shownWidget
	add := func(region string, w, h int) {
		for _, wg := range a.host.Widgets.InRegion(region) {
			out = append(out, shownWidget{wg, w, h})
		}
	}
	var walk func(l laid)
	walk = func(l laid) {
		switch l.node.Kind {
		case hostapi.LayoutRegion:
			add(l.node.Region, regionWidth(l.node, l.rect.Dx()), l.rows)
		case hostapi.LayoutBar:
			add(l.node.Left, l.rect.Dx(), 1)
			add(l.node.Right, l.rect.Dx(), 1)
		case hostapi.LayoutComposer:
			if l.node.Frame {
				add(l.node.Top, max(l.rect.Dx()-2, 1), 1) // the whole rule between the corners
				add(l.node.Left, frameInner(l.rect.Dx()), 1)
				add(l.node.Right, frameInner(l.rect.Dx()), 1)
			}
		case hostapi.LayoutBox, hostapi.LayoutSpacer:
		}
		for _, k := range l.kids {
			walk(k)
		}
	}
	walk(s.root)
	for _, f := range s.floats {
		add(f.f.Region, f.width, f.height)
	}
	return out
}

// paint draws a solved node: exactly one row per cell of height, a row box
// joining its children's rows left to right.
func (a *App) paint(l laid) [][]hostapi.Segment {
	n, w, h := l.node, l.rect.Dx(), l.rect.Dy()
	var rows [][]hostapi.Segment
	switch {
	case n.Kind == hostapi.LayoutBox && n.Dir == hostapi.DirRow:
		joined := make([][]hostapi.Segment, h)
		x := 0
		for _, k := range l.kids {
			if k.sep {
				for j := range joined {
					joined[j] = slices.Concat(padSegs(joined[j], x), []hostapi.Segment{{Text: "│", HL: "Rule", NoCopy: true, Line: true}})
				}
				x++
			}
			for j, kr := range a.paint(k) {
				joined[j] = slices.Concat(padSegs(joined[j], x), kr)
			}
			x += k.rect.Dx()
		}
		rows = joined
	case n.Kind == hostapi.LayoutBox:
		for i, k := range l.kids {
			if i > 0 && n.Sep {
				rows = append(rows, []hostapi.Segment{{Text: strings.Repeat("─", w), HL: "Rule", NoCopy: true, Line: true}})
			}
			rows = append(rows, a.paint(k)...)
		}
	case n.Kind == hostapi.LayoutRegion && n.Scroll != "":
		rows = a.paintScrolled(n, l.rect)
	case n.Kind == hostapi.LayoutRegion:
		rows = a.regionLines(n.Region, w)
		for i, row := range rows {
			if hostapi.SegsWidth(row) == 0 {
				rows[i] = padSegs(row, w) // a blank line is a blank row
			}
		}
	case n.Kind == hostapi.LayoutComposer && n.Frame:
		rows = a.composerFrame(n, w)
	case n.Kind == hostapi.LayoutComposer:
		rows = a.composerRows(w, n.Placeholder)
	case n.Kind == hostapi.LayoutBar:
		rows = [][]hostapi.Segment{a.statusRow(w, n.Left, n.Right)}
	}
	for len(rows) < h {
		rows = append(rows, padSegs(nil, w))
	}
	return rows[:h]
}

// viewport is the one scroll model (chat, Lua scroll nodes, box text,
// lists): rows of content, viewH of them shown from top. follow pins it to
// the tail, where top goes negative while the content is shorter than the
// view, which bottom-anchors it.
type viewport struct {
	top, rows, viewH int
	follow           bool
	first            []hostapi.Segment // drawn in place of the first row shown, when set
}

func (v *viewport) clamp() {
	if v.follow {
		v.top = v.rows - v.viewH
		return
	}
	v.top = max(0, min(v.top, v.rows-v.viewH))
}

// scroll moves n rows (negative = up) and leaves the tail.
func (v *viewport) scroll(n int) {
	if v.follow {
		v.top, v.follow = max(v.rows-v.viewH, 0), false
	}
	v.top += n
	v.clamp()
}

// page moves a page with one row of overlap, so no row is skipped.
func (v *viewport) page(dir int) { v.scroll(dir * max(v.viewH-1, 1)) }

func (v *viewport) edge(dir int) { v.scroll(dir * max(v.rows, 1)) }

// reveal moves the least so rows [a, b) show, the top of a taller span.
func (v *viewport) reveal(a, b int) {
	v.top = max(min(v.top, a), min(b-v.viewH, a))
	v.clamp()
}

func (v viewport) overflows() bool { return v.rows > v.viewH }

// pageItems is how many items of an n-item list a page holds.
func (v viewport) pageItems(n int) int { return max(v.viewH*n/max(v.rows, 1), 1) }

// barText is what a scrolled region's text may use: its last column keeps
// the bar and the one before it a blank. A region under 8 cells draws no bar.
func barText(width int) int {
	if width >= 8 {
		return width - 2
	}
	return width
}

// window is the rows of lines the view shows, each padded to width cells
// and followed by its bar cell while the content overflows, which a box
// draws in its border; rows before the content (a bottom-anchored tail) are
// nil.
func (v *viewport) window(lines [][]hostapi.Segment, width int) [][]hostapi.Segment {
	v.rows = len(lines)
	v.clamp()
	bar := scrollbar{h: v.viewH, top: v.top, total: v.rows, width: width}
	out := make([][]hostapi.Segment, 0, max(min(v.viewH, len(lines)), 0))
	for i := range v.viewH {
		j := v.top + i
		if j >= len(lines) {
			break
		}
		var row []hostapi.Segment
		switch {
		case i == 0 && v.first != nil:
			row = v.first
		case j >= 0:
			row = lines[j]
		}
		if bar.on() {
			p := padSegs(truncateSegs(row, width), width)
			if n := len(row); n > 0 && len(p) > n {
				p[n].Fill = row[n-1].Fill // a band runs on to the border
			}
			row = bar.row(p, i)
		}
		out = append(out, row)
	}
	return out
}

// scrollKey applies a scroll key to a text viewport: shift+up/down a line,
// ctrl+d/u half the view, the page keys a page, ctrl+home/end the edges, and
// in a box with nothing to select (textOnly) the list keys a line.
func scrollKey(v *viewport, key string, textOnly bool) bool {
	switch key {
	case "shift+up":
		v.scroll(-1)
	case "shift+down":
		v.scroll(1)
	case "ctrl+d":
		v.scroll(max(v.viewH/2, 1))
	case "ctrl+u":
		v.scroll(-max(v.viewH/2, 1))
	case "pgup":
		v.page(-1)
	case "pgdown":
		v.page(1)
	case "ctrl+home":
		v.edge(-1)
	case "ctrl+end":
		v.edge(1)
	default:
		d := listMove(key)
		if !textOnly || d == 0 {
			return false
		}
		v.scroll(d)
	}
	return true
}

// scrollState is a scrolled layout region's viewport (DESIGN §8
// scrollback). While it does not follow, anchor is the text of the first
// few visible rows from the first non-blank one, anchorOff how far below
// top that one sat, so a re-render that moves rows around keeps the same
// content in view instead of the same row number.
type scrollState struct {
	viewport
	bottom    bool
	anchor    []string
	anchorOff int
	filling   bool         // the rows drawn are a fill's latest, older ones still to land above
	rect      uv.Rectangle // where it was drawn on the last frame
	region    string
	// the last paint's rows, shared with the widget cache, which replaces
	// rows whole and never writes them in place
	lines  [][]hostapi.Segment
	pin    int    // the row drawn over the first one shown (pinned), -1 for none
	pinned pinKey // what first was copied from, so an unchanged pin allocates nothing
}

// pinKey names a pinned row's copy: the row by its first segment, which the
// widget cache never reuses for another row, and what else shapes the copy.
type pinKey struct {
	row   *hostapi.Segment
	more  bool
	width int
}

// anchorRows is how many rows identify the viewport. One is not enough:
// a turn footer or a blank line repeats down the transcript.
const anchorRows = 3

// paintScrolled draws a scrolled region into r. Its widgets render at
// barText of its width whether or not the bar shows, so the bar
// appearing never rewraps them.
func (a *App) paintScrolled(n *hostapi.LayoutNode, r uv.Rectangle) [][]hostapi.Segment {
	w, h := r.Dx(), r.Dy()
	st := a.scrollOf(n)
	lines := a.regionLines(n.Region, regionWidth(n, w))
	prevRows := st.rows
	st.rows, st.viewH, st.rect, st.region = len(lines), h, r, n.Region
	a.scrollIDs = append(a.scrollIDs, n.ID)
	if !st.follow && prevRows > 0 && st.rows != prevRows {
		st.top = reanchor(lines, st, prevRows)
	}
	st.filling = false
	for _, wgt := range a.host.Widgets.InRegion(n.Region) {
		if c := a.caches[wgt.ID]; c != nil && c.partial {
			st.filling = true
		}
	}
	st.clamp()
	st.pinRow(lines, w)
	rows := st.window(lines, w-1) // the bar keeps the region's last column
	st.anchor, st.anchorOff = nil, 0
	for i := max(st.top, 0); i < len(lines) && i < st.top+h; i++ {
		if strings.TrimSpace(plainSegs(lines[i])) == "" {
			continue
		}
		st.anchorOff = i - st.top
		for j := i; j < len(lines) && j < i+anchorRows; j++ {
			st.anchor = append(st.anchor, plainSegs(lines[j]))
		}
		break
	}
	for i := range rows {
		rows[i] = padSegs(rows[i], w)
	}
	return rows
}

func sticky(row []hostapi.Segment) bool { return len(row) > 0 && row[0].Sticky }

// pinned is the row a view scrolled to top draws over its first one: the
// first row of the nearest header (a run of sticky rows) above top, or -1
// when none is, or when a header is within the first three rows shown, so
// the pin never stacks two headers with a gap row and a stats line between.
// NOTE: a scan back to the header per painted frame of a view below one,
// following a turn included: a few hundred bool reads in a long turn.
func pinned(lines [][]hostapi.Segment, top int) int {
	if top <= 0 {
		return -1
	}
	for i := top; i < min(top+3, len(lines)); i++ {
		if sticky(lines[i]) {
			return -1
		}
	}
	i := min(top, len(lines)) - 1
	for i >= 0 && !sticky(lines[i]) {
		i--
	}
	for i > 0 && sticky(lines[i-1]) {
		i--
	}
	return i
}

// pinRow sets the row pinned over the first one shown and its copy, made
// again only when the pinned row, whether more header rows follow it, or the
// width changed.
func (st *scrollState) pinRow(lines [][]hostapi.Segment, width int) {
	st.lines, st.pin = lines, pinned(lines, st.top)
	if st.pin < 0 {
		st.first, st.pinned = nil, pinKey{}
		return
	}
	k := pinKey{&lines[st.pin][0], st.pin+1 < len(lines) && sticky(lines[st.pin+1]), width}
	if k != st.pinned {
		st.first, st.pinned = pinCopy(lines[st.pin], k.more, width), k
	}
}

// pinCopy is row as the pin draws it, chrome rather than content: all in
// Pinned (dimmed), nothing in it
// copying or acting but the pin's own click, and ending in an ellipsis when
// the header has more rows.
func pinCopy(row []hostapi.Segment, more bool, width int) []hostapi.Segment {
	row = slices.Clone(row)
	for i := range row {
		row[i].HL, row[i].Effect, row[i].NoCopy, row[i].Soft, row[i].Click, row[i].Widget = "Pinned", "", true, false, "", ""
	}
	if more {
		for len(row) > 0 {
			last := &row[len(row)-1]
			if last.Text = strings.TrimRight(last.Text, " "); last.Text != "" {
				break
			}
			row = row[:len(row)-1]
		}
		row = append(truncateSegs(row, barText(width)-1), hostapi.Segment{Text: "…", HL: "Pinned", NoCopy: true})
	}
	return row
}

// scrollbar is the one-cell bar down the right edge of a viewport showing
// rows [top, top+h) of total: the thumb's size and offset say how much
// shows and where. It is decoration, so it never copies, and it draws
// nothing while everything fits. width is the text beside it, under 7
// cells too narrow for one.
type scrollbar struct{ h, top, total, width int }

func (b scrollbar) on() bool { return b.h >= 2 && b.total > b.h && b.width >= 7 }

// row appends the bar cell for viewport row i to a row already padded to
// width cells.
func (b scrollbar) row(row []hostapi.Segment, i int) []hostapi.Segment {
	if !b.on() {
		return row
	}
	size := max(b.h*b.h/b.total, 1)
	pos := max(0, min((b.h-size)*max(b.top, 0)/(b.total-b.h), b.h-size))
	mark, hl := "│", "ScrollbarTrack"
	if i >= pos && i < pos+size {
		mark, hl = "█", "ScrollbarThumb"
	}
	out := make([]hostapi.Segment, len(row), len(row)+1)
	copy(out, row)
	return append(out, hostapi.Segment{Text: mark, HL: hl, NoCopy: true, Bar: true})
}

// reanchor puts the viewport back on the content it was showing after the
// row count moved: a thought block expanding above it, or a rewrap at a new
// width, would otherwise slide unrelated lines under the user. The same
// text can appear more than once, so the remembered rows are looked for
// first where rows added below (a streaming turn) or above (a fill, first
// when one was drawn) would put them, then nearest where a proportional
// scroll would land; with no match that proportional position is the answer.
func reanchor(lines [][]hostapi.Segment, st *scrollState, prevRows int) int {
	tops := []int{st.top, st.top + st.rows - prevRows}
	if st.filling {
		slices.Reverse(tops)
	}
	for _, top := range tops {
		if anchoredAt(lines, top+st.anchorOff, st.anchor) {
			return top
		}
	}
	want := st.top*st.rows/prevRows + st.anchorOff
	best := -1
	for i := range lines {
		if !anchoredAt(lines, i, st.anchor) {
			continue
		}
		if best < 0 || abs(i-want) < abs(best-want) {
			best = i
		}
	}
	if best < 0 {
		return want - st.anchorOff
	}
	return best - st.anchorOff
}

func anchoredAt(lines [][]hostapi.Segment, i int, want []string) bool {
	// a shrunk transcript puts the guessed row above the first line
	if len(want) == 0 || i < 0 || i+len(want) > len(lines) {
		return false
	}
	for k, s := range want {
		if plainSegs(lines[i+k]) != s {
			return false
		}
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// scrollOf is a scrolled region's state, a "bottom" one following its tail
// from the start.
func (a *App) scrollOf(n *hostapi.LayoutNode) *scrollState {
	st := a.scrolls[n.ID]
	if st == nil || st.bottom != (n.Scroll == hostapi.EdgeBottom) {
		st = &scrollState{follow: n.Scroll == hostapi.EdgeBottom, bottom: n.Scroll == hostapi.EdgeBottom}
		a.scrolls[n.ID] = st
	}
	return st
}

// paintFloats draws the Lua floats over rows in list order, each opaque at
// its content's size or the size it asked for, and records the ones with
// an id. The composer and the statusline's alarm pills stay on top: they
// are how a broken layout gets fixed.
func (a *App) paintFloats(rows [][]hostapi.Segment, s *screenLayout, w, h int) {
	if len(s.floats) == 0 {
		return
	}
	type kept struct {
		x, y int
		segs []hostapi.Segment
	}
	var keep []kept
	if c, ok := s.rects["composer"]; ok {
		for y := c.Min.Y; y < c.Max.Y && y < len(rows); y++ {
			keep = append(keep, kept{c.Min.X, y, truncateSegs(dropCells(rows[y], c.Min.X), c.Dx())})
		}
	}
	for y, row := range rows {
		x := 0
		for _, sg := range row {
			if sg.Widget == "" && sg.Click != "" { // a Go pill
				keep = append(keep, kept{x, y, []hostapi.Segment{sg}})
			}
			x += hostapi.SegsWidth([]hostapi.Segment{sg})
		}
	}
	for _, pf := range s.floats {
		f := pf.f
		lines := a.regionLines(f.Region, pf.width)
		fw, fh := pf.width, pf.height
		if f.Width.Kind == hostapi.SizeContent {
			fw = 0
			for _, l := range lines {
				fw = max(fw, hostapi.SegsWidth(l))
			}
			fw = min(fw, pf.width)
		}
		if f.Height.Kind == hostapi.SizeContent {
			fh = min(len(lines), pf.height)
		}
		if fw <= 0 || fh <= 0 {
			continue
		}
		x, y := pf.rel.Min.X+f.Col, pf.rel.Min.Y+f.Row
		if f.Anchor == hostapi.AnchorNE || f.Anchor == hostapi.AnchorSE {
			x = pf.rel.Max.X - fw + f.Col
		}
		if f.Anchor == hostapi.AnchorSW || f.Anchor == hostapi.AnchorSE {
			y = pf.rel.Max.Y - fh + f.Row
		}
		x, y = max(0, min(x, w-fw)), max(0, min(y, h-fh))
		box := make([][]hostapi.Segment, fh)
		for i := range box {
			var line []hostapi.Segment
			if i < len(lines) {
				line = truncateSegs(lines[i], fw)
			}
			box[i] = padSegs(line, fw)
		}
		placeBox(rows, box, x, y, w)
		s.panes = append(s.panes, uv.Rect(x, y, fw, fh))
		if f.ID != "" {
			s.rects[f.ID] = uv.Rect(x, y, fw, fh)
		}
	}
	if c, ok := s.rects["composer"]; ok {
		s.panes = append(s.panes, c)
	}
	for _, k := range keep {
		placeBox(rows, [][]hostapi.Segment{k.segs}, k.x, k.y, w)
	}
}

// The arms of a light box glyph: the sides of its cell its lines run to.
const (
	armU = 1 << iota
	armR
	armD
	armL
)

// lightBox is the square light glyph of each set of arms. All 16 exist, so a
// union of arms always draws.
var lightBox = [16]string{" ", "╵", "╶", "└", "╷", "│", "┌", "├", "╴", "┘", "─", "┴", "┐", "┤", "┬", "┼"}

// boxArms is lightBox inverted over the box drawing block, U+2500 on.
var boxArms = func() (t [0x80]uint8) {
	for a, s := range lightBox[1:] {
		r, _ := utf8.DecodeRuneInString(s)
		t[r-0x2500] = uint8(a + 1)
	}
	return t
}()

func armsOf(r rune) uint8 {
	if r < 0x2500 || r >= 0x2580 {
		return 0
	}
	return boxArms[r-0x2500]
}

// join makes the chrome lines of a composed frame (Segment.Line) meet where
// they touch: a line cell gains the arms its neighbours point at it with, so
// a box standing on a rule draws ├ ┴ ┤ there. It only adds arms, so text,
// parallel lines and lines that end facing none stay as drawn, and a changed
// cell keeps its own style and click. An unchanged frame allocates nothing.
func (a *App) join(rows [][]hostapi.Segment, w int) {
	n := w * len(rows)
	if cap(a.arms) < n {
		a.arms = make([]uint8, n)
	}
	g := a.arms[:n]
	clear(g)
	for y, row := range rows {
		if !slices.ContainsFunc(row, func(s hostapi.Segment) bool { return s.Line }) {
			continue
		}
		x := 0
		for i := range row {
			s := &row[i]
			if !s.Line {
				x += ansi.StringWidth(s.Text)
				continue
			}
			for _, r := range s.Text {
				if x < w {
					g[y*w+x] = armsOf(r)
				}
				x++
			}
		}
	}
	for i, c := range g {
		if c == 0 {
			continue
		}
		y, x := i/w, i%w
		want := c
		if y > 0 && g[i-w]&armD != 0 {
			want |= armU
		}
		if y+1 < len(rows) && g[i+w]&armU != 0 {
			want |= armD
		}
		if x > 0 && g[i-1]&armR != 0 {
			want |= armL
		}
		if x+1 < w && g[i+1]&armL != 0 {
			want |= armR
		}
		if want != c {
			rows[y] = restyle(rows[y], x, x+1, func(s *hostapi.Segment) { s.Text = lightBox[want] })
		}
	}
}

// placeBox writes box at x0, y0 over rows, clipped to the frame.
func placeBox(rows, box [][]hostapi.Segment, x0, y0, w int) [][]hostapi.Segment {
	for i, line := range box {
		y := y0 + i
		if y < 0 || y >= len(rows) {
			continue
		}
		bw := hostapi.SegsWidth(line)
		under := rows[y]
		row := padSegs(truncateSegs(under, x0), x0)
		row = append(row, line...)
		row = append(row, dropCells(under, x0+bw)...)
		rows[y] = padSegs(truncateSegs(row, w), w)
	}
	return rows
}
