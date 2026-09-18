package ui

import (
	"math"
	"slices"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/win"
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
		}
		return 0
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
	wn   *win.Window // the window a slot expanded into
	rect uv.Rectangle
	// rows is the height its widgets render at: the rect's, or for a
	// content node the most it could get, since a height taken from its
	// own last draw could never shrink.
	rows int
	sep  bool // a separator column left of it
	// sepAfter: a separator column right of it, which a window without
	// one on its left drags
	sepAfter bool
	axis     int // a window: the cells its box shares out, what its frac is of
	kids     []laid
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
	slots  map[string]bool // window kinds the tree has a slot for
	bar    bool
	floats []placedFloat
	// focusable is the focusable node and float ids in focus order; a float
	// that draws nothing gets no rect
	focusable []string
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
	s := screenLayout{root: a.solve(spec.Root, screen), stage: screen, rects: map[string]uv.Rectangle{"screen": screen}, slots: map[string]bool{}}
	staged := false // the scroll node chosen is inside the stage
	var walk func(l laid, inStage bool)
	walk = func(l laid, inStage bool) {
		n := l.node
		if l.wn != nil {
			return
		}
		if n.Stage {
			s.stage, inStage = l.rect, true
		}
		if n.ID != "" {
			s.rects[n.ID] = l.rect
		}
		switch n.Kind {
		case "composer":
			s.rects["composer"] = l.rect
		case "bar":
			s.bar = true
		case "region":
			if n.Scroll != "" && (s.scroll == "" || inStage && !staged) {
				s.scroll, staged = n.ID, inStage
			}
			if n.Focusable {
				s.focusable = append(s.focusable, n.ID)
			}
		}
		for _, k := range l.kids {
			walk(k, inStage)
		}
	}
	walk(s.root, false)
	// the solved tree has no node for a slot without a shown window
	var slots func(n *hostapi.LayoutNode)
	slots = func(n *hostapi.LayoutNode) {
		if n.Kind == "windows" && !n.Hidden {
			s.slots[n.Slot] = true
		}
		for _, c := range n.Children {
			if !c.Hidden {
				slots(c)
			}
		}
	}
	slots(spec.Root)
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
	}
	return rel
}

// solve places n's children inside r. A windows slot becomes one child per
// shown window of its kind: its frac of the box's cells, else a fill share
// weighing the same as its fill siblings. While the children's mins do not
// fit, the last collapsing child is hidden for the frame.
func (a *App) solve(n *hostapi.LayoutNode, r uv.Rectangle) laid {
	l := laid{node: n, rect: r, rows: r.Dy()}
	if n.Kind != "box" {
		return l
	}
	row := n.Dir == "row"
	length, cross := r.Dy(), r.Dx()
	if row {
		length, cross = cross, length
	}
	type item struct {
		node *hostapi.LayoutNode
		wn   *win.Window
	}
	var items []item
	for _, c := range n.Children {
		switch {
		case c.Hidden:
		case c.Kind != "windows":
			items = append(items, item{node: c})
		default:
			for _, wn := range a.shownWins(c.Slot) {
				items = append(items, item{node: c, wn: wn})
			}
		}
	}
	var sizes []int
	var kids []spanKid
	avail := 0
	for {
		avail = length
		if n.Sep {
			avail -= max(len(items)-1, 0)
		}
		avail = max(avail, 0)
		kids = make([]spanKid, len(items))
		for i, it := range items {
			c := it.node
			if it.wn == nil {
				kids[i] = spanKid{size: c.Size, min: c.Min, max: c.Max}
				if c.Size.Kind == hostapi.SizeContent && !row {
					kids[i].want = a.measure(c, cross)
				}
				continue
			}
			kids[i] = spanKid{size: hostapi.Size{Kind: hostapi.SizeFill}, min: map[string]int{win.KindSplit: minSplitH, win.KindVSplit: minVsplitW}[c.Slot]}
			switch f := it.wn.Layout().Frac; {
			case f > 0:
				kids[i].size = hostapi.Size{Kind: hostapi.SizeCells, N: int(math.Round(f * float64(avail)))}
			case it.wn.IsText():
				// a panel nobody sized takes what its text needs, at most half
				// the box: a five-line result is not half the screen
				kids[i].size = hostapi.Size{Kind: hostapi.SizeContent}
				kids[i].want = a.textSpan(it.wn, row, cross, avail)
				kids[i].max = max(avail/2, kids[i].min)
			}
		}
		mins, gone := 0, -1
		for i, it := range items {
			mins += kids[i].min
			if it.wn == nil && it.node.Collapse {
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
		k := a.solve(items[i].node, kr)
		if c := items[i].node; c.Size.Kind == hostapi.SizeContent && !row {
			k.rows = kids[i].clamp(avail)
		}
		k.wn, k.axis = items[i].wn, avail
		k.sep = row && n.Sep && i > 0
		k.sepAfter = row && n.Sep && i < len(sizes)-1
		l.kids = append(l.kids, k)
		pos += size
	}
	return l
}

// measure is the rows a content-sized node draws at width.
func (a *App) measure(n *hostapi.LayoutNode, width int) int {
	switch n.Kind {
	case "region":
		return len(a.regionLines(n.Region, width))
	case "composer":
		return len(a.composerRows(width))
	case "bar":
		return 1
	}
	return 0
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
		case "region":
			add(l.node.Region, l.rect.Dx(), l.rows)
		case "bar":
			add(l.node.Left, l.rect.Dx(), 1)
			add(l.node.Right, l.rect.Dx(), 1)
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
	case l.wn != nil:
		rows = a.paintWindow(l)
	case n.Kind == "box" && n.Dir == "row":
		rows = make([][]hostapi.Segment, h)
		x := 0
		for _, k := range l.kids {
			if k.sep {
				for j := range rows {
					rows[j] = slices.Concat(padSegs(rows[j], x), []hostapi.Segment{{Text: "│", HL: "Comment", NoCopy: true}})
				}
				x++
			}
			for j, kr := range a.paint(k) {
				rows[j] = slices.Concat(padSegs(rows[j], x), kr)
			}
			x += k.rect.Dx()
		}
	case n.Kind == "box":
		for i, k := range l.kids {
			if i > 0 && n.Sep {
				rows = append(rows, []hostapi.Segment{{Text: strings.Repeat("─", w), HL: "Comment", NoCopy: true}})
			}
			rows = append(rows, a.paint(k)...)
		}
	case n.Kind == "region" && n.Scroll != "":
		rows = a.paintScrolled(n, l.rect)
	case n.Kind == "region":
		rows = a.regionLines(n.Region, w)
		for i, row := range rows {
			if segsWidth(row) == 0 {
				rows[i] = padSegs(row, w) // a blank line is a blank row
			}
		}
	case n.Kind == "composer":
		rows = a.composerRows(w)
	case n.Kind == "bar":
		rows = [][]hostapi.Segment{a.statusRow(w, n.Left, n.Right)}
	}
	for len(rows) < h {
		rows = append(rows, padSegs(nil, w))
	}
	return rows[:h]
}

// scrollState is where a scrolled region's viewport is (DESIGN §8
// scrollback). follow pins it to the tail (top may go negative when the
// content is shorter than the viewport, which bottom-anchors it);
// otherwise top is the absolute first visible row, clamped so content
// growth or a narrower rewrap never shows blank rows. anchor is the text
// of the first few visible rows from the first non-blank one, anchorOff how
// far below top that one sat, so a re-render that moves rows around keeps
// the same content in view instead of the same row number.
type scrollState struct {
	top, rows, viewH int
	follow, bottom   bool
	anchor           []string
	anchorOff        int
	rect             uv.Rectangle // where it was drawn on the last frame
}

// anchorRows is how many rows identify the viewport. One is not enough:
// a turn footer or a blank line repeats down the transcript.
const anchorRows = 3

// paintScrolled draws a scrolled region into r.
func (a *App) paintScrolled(n *hostapi.LayoutNode, r uv.Rectangle) [][]hostapi.Segment {
	w, h := r.Dx(), r.Dy()
	st := a.scrollOf(n)
	lines := a.regionLines(n.Region, w)
	prevRows := st.rows
	st.rows, st.viewH, st.rect = len(lines), h, r
	a.scrollIDs = append(a.scrollIDs, n.ID)
	if st.follow {
		st.top = st.rows - h
	} else {
		if prevRows > 0 && st.rows != prevRows {
			st.top = reanchor(lines, st, prevRows)
		}
		st.top = max(0, min(st.top, st.rows-h))
	}
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
	rows := make([][]hostapi.Segment, h)
	for i := range rows {
		var row []hostapi.Segment
		if j := st.top + i; j >= 0 && j < len(lines) {
			row = truncateSegs(lines[j], w)
		}
		rows[i] = padSegs(row, w)
	}
	return rows
}

// reanchor puts the viewport back on the content it was showing after the
// row count moved: a thought block expanding above it, or a rewrap at a new
// width, would otherwise slide unrelated lines under the user. The
// remembered row is looked for nearest where a proportional scroll would
// land, since the same text can appear more than once; with no match that
// proportional position is the answer.
func reanchor(lines [][]hostapi.Segment, st *scrollState, prevRows int) int {
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
	if len(want) == 0 || i+len(want) > len(lines) {
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
	if st == nil || st.bottom != (n.Scroll == "bottom") {
		st = &scrollState{follow: n.Scroll == "bottom", bottom: n.Scroll == "bottom"}
		a.scrolls[n.ID] = st
	}
	return st
}

// paintWindow draws a window a slot expanded into: its title row, then its
// cells, recorded for the mouse.
func (a *App) paintWindow(l laid) [][]hostapi.Segment {
	r := l.rect
	if r.Dx() < 1 || r.Dy() < 2 {
		return nil
	}
	sepX := -1
	switch {
	case l.sep:
		sepX = r.Min.X - 1
	case l.sepAfter:
		sepX = r.Max.X
	}
	l.wn.Resize(r.Dx(), r.Dy()-1)
	a.winRects = append(a.winRects, winRect{id: l.wn.ID, x: r.Min.X, y: r.Min.Y + 1, w: r.Dx(), h: r.Dy() - 1, titleY: r.Min.Y, sepX: sepX, axis: l.axis})
	return a.windowRows(l.wn, r.Dx(), r.Dy()-1)
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
			x += segsWidth([]hostapi.Segment{sg})
		}
	}
	for _, pf := range s.floats {
		f := pf.f
		lines := a.regionLines(f.Region, pf.width)
		fw, fh := pf.width, pf.height
		if f.Width.Kind == hostapi.SizeContent {
			fw = 0
			for _, l := range lines {
				fw = max(fw, segsWidth(l))
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
		if strings.HasSuffix(f.Anchor, "E") {
			x = pf.rel.Max.X - fw + f.Col
		}
		if strings.HasPrefix(f.Anchor, "S") {
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
		if f.ID != "" {
			s.rects[f.ID] = uv.Rect(x, y, fw, fh)
		}
	}
	for _, k := range keep {
		placeBox(rows, [][]hostapi.Segment{k.segs}, k.x, k.y, w)
	}
}
