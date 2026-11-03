package ui

import (
	"slices"
	"strings"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
)

// region is a leaf of the screen: the widgets drawn in it. A region no
// widget fills draws nothing.
type region uint8

const (
	regionNone region = iota
	regionChat
	regionWorking
	regionStatusLeft
	regionStatusRight
)

// SizeKind is how a node claims cells along its parent box's axis.
type SizeKind uint8

const (
	sizeContent SizeKind = iota // what the node draws: region rows, composer rows, 1 for a bar
	sizeCells
	sizePercent // of the parent's cells
	sizeFill    // a share of what is left, by weight
)

// Size is a node's claim: N is the cells, the percent or the fill weight
// (0 reads as 1).
type Size struct {
	Kind SizeKind
	N    int
}

// layoutKind is what a node is: a box of children or a leaf.
type layoutKind uint8

const (
	layoutBox layoutKind = iota
	layoutRegion
	layoutBar
	layoutComposer
	layoutSpacer
)

// layoutDir is how a box lays its children out: dirRow side by side, dirCol
// stacked.
type layoutDir uint8

const (
	dirRow layoutDir = iota
	dirCol
)

// node is one node of the screen's layout: a box lays its children out along
// Dir, a leaf names what fills its cells.
type node struct {
	Kind     layoutKind
	Dir      layoutDir // box
	Children []*node

	Region region // region: the widgets drawn in it
	Scroll bool   // region: scrolls, following its tail
	Fade   int    // region: the rows at an edge content lies past that fade toward the canvas

	Left, Right region // bar: the regions of its two clusters

	Frame       bool   // composer: a square frame around it, two rows taller
	Top         region // framed composer: the region drawn in its top rule
	Placeholder string // composer: drawn dim while the draft is empty

	Stage    bool // prompts, overlays and toasts go over this node
	Size     Size
	Min, Max int // Max 0 means none
}

// screen is the one layout zemi draws: the main column centered; in it the
// stage centered inside it, and the framed composer wider than the stage, the
// statusline on a bar row under it.
var screen = node{Kind: layoutBox, Dir: dirRow, Size: Size{Kind: sizeFill}, Children: []*node{
	{Kind: layoutSpacer, Size: Size{Kind: sizeFill}},
	{Kind: layoutBox, Dir: dirCol, Size: Size{Kind: sizeFill, N: 1000}, Min: 30, Max: 124, Children: []*node{
		{Kind: layoutBox, Dir: dirRow, Size: Size{Kind: sizeFill}, Children: []*node{
			{Kind: layoutSpacer, Size: Size{Kind: sizeFill}},
			{Kind: layoutBox, Dir: dirCol, Stage: true, Size: Size{Kind: sizeFill, N: 1000}, Min: 63, Max: 100, Children: []*node{
				{Kind: layoutRegion, Region: regionChat, Scroll: true, Fade: 4, Size: Size{Kind: sizeFill}, Min: 3},
				{Kind: layoutSpacer, Size: Size{Kind: sizeCells, N: 1}},
			}},
			{Kind: layoutSpacer, Size: Size{Kind: sizeFill}},
		}},
		{Kind: layoutComposer, Placeholder: "Ask anything…", Frame: true, Top: regionWorking},
		{Kind: layoutBar, Left: regionStatusLeft, Right: regionStatusRight},
	}},
	{Kind: layoutSpacer, Size: Size{Kind: sizeFill}},
}}

// spanKid is one child of a box as the solver sees it.
type spanKid struct {
	size        Size
	want        int // a content node's measured cells
	floor, ceil int // ceil 0 means none
}

func (k spanKid) clamp(n int) int {
	n = max(n, k.floor)
	if k.ceil > 0 {
		n = min(n, k.ceil)
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
		case sizeCells:
			out[i] = k.size.N
		case sizePercent:
			out[i] = (k.size.N*avail + 50) / 100
		case sizeContent:
			out[i] = k.want
		case sizeFill: // takes what is left, below
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
			if k.size.Kind != sizeFill || k.ceil > 0 && out[i] >= k.ceil {
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
		case sizeFill:
			return 3
		case sizePercent:
			return 2
		case sizeContent:
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
					lo = kids[i].floor
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
	node *node
	rect uv.Rectangle
	// rows is the height its widgets render at: the rect's, or for a
	// content node the most it could get, since a height taken from its
	// own last draw could never shrink.
	rows int
	kids []laid
}

// screenLayout is the layout solved for one frame.
type screenLayout struct {
	root     laid
	stage    uv.Rectangle
	composer uv.Rectangle
	// scroll is the region actions without one move: the first scrolled
	// region inside the stage, else the first in the tree
	scroll region
	bar    bool
	// panes are the cells a selection may not leave: the tree's leaves
	panes []uv.Rectangle
}

// layout solves the screen for a w by h frame.
func (a *App) layout(w, h int) screenLayout {
	box := uv.Rect(0, 0, w, h)
	s := screenLayout{root: a.solve(&screen, box), stage: box}
	staged := false // the scroll node chosen is inside the stage
	var walk func(l laid, inStage bool)
	walk = func(l laid, inStage bool) {
		n := l.node
		if n.Stage {
			s.stage, inStage = l.rect, true
		}
		switch n.Kind {
		case layoutComposer:
			s.composer = l.rect
		case layoutBar:
			s.bar = true
		case layoutRegion:
			if n.Scroll && (s.scroll == regionNone || inStage && !staged) {
				s.scroll, staged = n.Region, inStage
			}
		case layoutBox, layoutSpacer:
		}
		if len(l.kids) == 0 && n.Kind != layoutSpacer {
			s.panes = append(s.panes, l.rect)
		}
		for _, k := range l.kids {
			walk(k, inStage)
		}
	}
	walk(s.root, false)
	return s
}

// solve places n's children inside r.
func (a *App) solve(n *node, r uv.Rectangle) laid {
	l := laid{node: n, rect: r, rows: r.Dy()}
	if n.Kind != layoutBox {
		return l
	}
	row := n.Dir == dirRow
	length, cross := r.Dy(), r.Dx()
	if row {
		length, cross = cross, length
	}
	items := make([]*node, 0, len(n.Children))
	for _, c := range n.Children {
		// a failed boot draws a screen of its own, with no composer to type in
		if a.bootErr == nil || c.Kind != layoutComposer {
			items = append(items, c)
		}
	}
	avail := max(length, 0)
	kids := make([]spanKid, len(items))
	for i, c := range items {
		kids[i] = spanKid{size: c.Size, floor: c.Min, ceil: c.Max}
		if c.Size.Kind == sizeContent && !row {
			kids[i].want = a.measure(c, cross)
		}
	}
	sizes := span(avail, kids)
	pos := r.Min.Y
	if row {
		pos = r.Min.X
	}
	for i, size := range sizes {
		kr := uv.Rect(r.Min.X, pos, cross, size)
		if row {
			kr = uv.Rect(pos, r.Min.Y, size, cross)
		}
		k := a.solve(items[i], kr)
		if c := items[i]; c.Size.Kind == sizeContent && !row {
			k.rows = kids[i].clamp(avail)
		}
		l.kids = append(l.kids, k)
		pos += size
	}
	return l
}

// measure is the rows a content-sized node draws at width.
func (a *App) measure(n *node, width int) int {
	switch n.Kind {
	case layoutRegion:
		rows := a.regionLines(n.Region, regionWidth(n, width), 0)
		return len(rows)
	case layoutComposer:
		if n.Frame {
			return len(a.composerRows(frameInner(width), n.Placeholder)) + 2
		}
		return len(a.composerRows(width, n.Placeholder))
	case layoutBar:
		return 1
	case layoutBox, layoutSpacer:
	}
	return 0
}

// regionWidth is the cells a region's widgets render at: a scrolled one
// keeps its bar's column and a blank before it.
func regionWidth(n *node, w int) int {
	if n.Scroll {
		return barText(w)
	}
	return w
}

// shownWidget is a widget of a region the layout draws, with the cells it
// renders at.
type shownWidget struct {
	ID            string
	Region        region
	Wrap          bool
	Go            *widget
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
	add := func(region region, w, h int) {
		for _, wg := range a.regionWidgets(region) {
			out = append(out, shownWidget{ID: wg.ID, Region: wg.Region, Wrap: wg.Wrap, Go: wg.Go, width: w, height: h})
		}
	}
	var walk func(l laid)
	walk = func(l laid) {
		switch l.node.Kind {
		case layoutRegion:
			add(l.node.Region, regionWidth(l.node, l.rect.Dx()), l.rows)
		case layoutBar:
			add(l.node.Left, l.rect.Dx(), 1)
			add(l.node.Right, l.rect.Dx(), 1)
		case layoutComposer:
			if l.node.Frame {
				add(l.node.Top, max(l.rect.Dx()-2, 1), 1) // the whole rule between the corners
			}
		case layoutBox, layoutSpacer:
		}
		for _, k := range l.kids {
			walk(k)
		}
	}
	walk(s.root)
	return out
}

// paint draws a solved node: exactly one row per cell of height, a row box
// joining its children's rows left to right.
func (a *App) paint(l laid) [][]hostapi.Segment {
	n, w, h := l.node, l.rect.Dx(), l.rect.Dy()
	var rows [][]hostapi.Segment
	switch {
	case n.Kind == layoutBox && n.Dir == dirRow:
		joined := make([][]hostapi.Segment, h)
		x := 0
		for _, k := range l.kids {
			for j, kr := range a.paint(k) {
				// one make per row per kid: the row must not share a backing
				// array with a kid's or a cache's row, which in-place edits
				// below would corrupt
				p, ok := padSeg(joined[j], x)
				out := make([]hostapi.Segment, 0, len(joined[j])+len(kr)+1)
				out = append(out, joined[j]...)
				if ok {
					out = append(out, p)
				}
				joined[j] = append(out, kr...)
			}
			x += k.rect.Dx()
		}
		rows = joined
	case n.Kind == layoutBox:
		for _, k := range l.kids {
			rows = append(rows, a.paint(k)...)
		}
	case n.Kind == layoutRegion && n.Scroll:
		rows = a.paintScrolled(n, l.rect)
	case n.Kind == layoutRegion:
		rows = a.regionLines(n.Region, w, 0)
		for i, row := range rows {
			if hostapi.SegsWidth(row) == 0 {
				rows[i] = padSegs(row, w) // a blank line is a blank row
			}
		}
	case n.Kind == layoutComposer && n.Frame:
		rows = a.composerFrame(n, w)
	case n.Kind == layoutComposer:
		// the composer is on the panel, its rows padded to the node so the
		// whole block is one surface, as the frame's own rows are
		rows = a.composerRows(w, n.Placeholder)
		for i := range rows {
			rows[i] = padSegs(rows[i], w)
		}
		onPanel(rows)
	case n.Kind == layoutBar:
		// two cells of air either side, in from the edges the frame above spans
		row := append([]hostapi.Segment{{Text: "  "}}, a.statusRow(max(w-4, 1))...)
		rows = [][]hostapi.Segment{padSegs(row, w)}
	}
	for len(rows) < h {
		rows = append(rows, padSegs(nil, w))
	}
	return rows[:h]
}

// viewport is the one scroll model (the chat, box text, lists): rows of
// content, viewH of them shown from top. Content shorter than the view
// starts at its first row; follow pins it to the tail.
type viewport struct {
	top, rows, viewH int
	follow           bool
	pins             [][]hostapi.Segment // drawn in place of the first rows shown
}

func (v *viewport) clamp() {
	if v.follow {
		v.top = v.rows - v.viewH
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
// draws in its border. fade sinks the rows at an edge content lies past; see
// edgeRamp.
func (v *viewport) window(lines [][]hostapi.Segment, width, fade int) [][]hostapi.Segment {
	v.rows = len(lines)
	v.clamp()
	bar := scrollbar{h: v.viewH, top: v.top, total: v.rows, width: width}
	out := make([][]hostapi.Segment, 0, max(min(v.viewH, len(lines)), 0))
	for i := range v.viewH {
		j := v.top + i
		if j >= len(lines) {
			break
		}
		row := lines[j]
		if i < len(v.pins) {
			row = v.pins[i]
		}
		if bar.on() {
			row = bar.row(padSegs(truncateSegs(row, width), width), i)
		}
		out = append(out, row)
	}
	// the pins are drawn over the window, and a list cut to whole items pads
	// past its content with nil: neither takes a step of the ramp
	edgeRamp(out, v.top, v.rows-v.top-v.viewH, fade, func(i int) bool {
		return i < len(v.pins) || len(out[i]) == 0
	})
	return out
}

// boxFade is how many rows a box's scrolled view, or the composer's, sinks at
// an edge: the chat's four would ghost away half a menu, so both ramp two,
// eased off by how much lies past it.
const boxFade = 2

// edgeRamp sinks the rows at a window's edges while content lies past them,
// deepest at the edge, so each edge says there is more: above and below are
// how many rows lie past each one. The sink eases off as the last rows come
// into view, in step with how many lie past the edge, and is at most a third
// of the rows. skip names the rows the ramp leaves alone: a pin, the row a
// draft's cursor is on.
func edgeRamp(rows [][]hostapi.Segment, above, below, fade int, skip func(int) bool) {
	if fade = min(fade, len(rows)/3); fade <= 0 {
		return
	}
	depth := func(past int) float64 { return min(1, float64(past)/float64(fade)) }
	top, bottom := depth(above), depth(below)
	first, last := 0, len(rows)-1
	for first < len(rows) && skip(first) {
		first++
	}
	for last >= first && skip(last) {
		last--
	}
	for k := range fade {
		t := float64(fade-k) / float64(fade+1)
		if i := first + k; top > 0 && i <= last && !skip(i) {
			rows[i] = faded(rows[i], t*top)
		}
		if i := last - k; bottom > 0 && i >= first && !skip(i) {
			rows[i] = faded(rows[i], t*bottom)
		}
	}
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
	case "ctrl+b":
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
	anchor    []string
	anchorOff int
	filling   bool         // the rows drawn are a fill's latest, older ones still to land above
	rect      uv.Rectangle // where it was drawn on the last frame
	region    region
	// the last paint's rows, shared with the widget cache, which replaces
	// rows whole and never writes them in place
	lines  [][]hostapi.Segment
	pin    []int    // the header rows drawn over the first rows shown (pinned)
	pinned []pinKey // what each of pins was copied from, so an unchanged pin allocates nothing
	// focus is the transcript row a following list placed the view at, placed
	// the top its jump left, so the list opens there again only while the view
	// still sits where the jump put it; reaching the transcript's end drops
	// it, and so does a submit or a click
	focus  int
	placed int
	// band caches bandSpan's rows: two transcript scans and a clone of the
	// line slice per paint while a following list is up, for rows that change
	// only when the region re-renders (a fresh wrapped slice), the pick or the
	// width does. bandSrc holds the rows the cache was built from, so a
	// re-render's fresh slice can never sit at the old one's address.
	band, bandSrc [][]hostapi.Segment
	bandAt, bandW int
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
func (a *App) paintScrolled(n *node, r uv.Rectangle) [][]hostapi.Segment {
	w, h := r.Dx(), r.Dy()
	st := a.scrollOf(n)
	lines := a.regionLines(n.Region, regionWidth(n, w), h)
	prevRows := st.rows
	st.rows, st.viewH, st.rect, st.region = len(lines), h, r, n.Region
	a.scrollIDs = append(a.scrollIDs, n.Region)
	if !st.follow && prevRows > 0 && st.rows != prevRows {
		st.top = reanchor(lines, st, prevRows)
	}
	switch a.pendingScroll[n.Region] {
	case hostapi.ScrollTop:
		st.top, st.follow = 0, false
	case hostapi.ScrollBottom:
		st.top, st.follow = st.rows, true
		delete(a.pendingScroll, n.Region)
	default: // applyScroll holds only the edges
	}
	// a following list's jump outranks a parked edge, and leaves none behind
	jumped := 0
	if at, ok := a.pendingAnchor[n.Region]; ok {
		if unit := unitAt(lines, at); unit != 0 {
			from, to := unitSpan(lines, at)
			st.top = jumpTop(lines, from, to, st.viewH)
			st.follow = st.top >= st.rows-st.viewH
			st.focus, jumped = at, at
			delete(a.pendingAnchor, n.Region)
			delete(a.pendingScroll, n.Region)
		}
	}
	st.filling = false
	for _, wgt := range a.regionWidgets(n.Region) {
		if c := a.caches[wgt.ID]; c != nil && c.partial {
			st.filling = true
		}
	}
	st.clamp()
	// the pick of the top following list stays in view while it is up: a rail
	// its level walks grows under the row it names, and a view pinned to the
	// tail would carry that row off the top as the run opened. Only a paint
	// whose rows changed, so a scroll of the chat is never yanked back. A
	// pick taller than the view never fits: only its first row escaping the
	// top is lost, the growth below is what the reader watches.
	if a.highlight != 0 && jumped == 0 && st.rows != prevRows {
		if from, to := unitSpan(lines, a.highlight); from >= 0 {
			off := from < st.top || (to-from+1 <= st.viewH && to >= st.top+st.viewH)
			if off {
				st.top, st.follow = jumpTop(lines, from, to, st.viewH), false
			}
		}
	}
	if jumped > 0 {
		st.placed = st.top
	}
	st.pinRows(lines, w)
	// the picker's message is banded under the pins, which stay chrome
	draw := lines
	if n.Region == a.scrollDef && a.highlight != 0 {
		same := len(st.bandSrc) == len(lines) && (len(lines) == 0 || &st.bandSrc[0] == &lines[0])
		if !same || st.bandAt != a.highlight || st.bandW != w-1 {
			from, to := unitSpan(lines, a.highlight)
			st.band = bandSpan(lines, from, to, w-1)
			st.bandSrc, st.bandAt, st.bandW = lines, a.highlight, w-1
		}
		draw = st.band
	}
	rows := st.window(draw, w-1, n.Fade) // the bar keeps the region's last column
	// the scrollbar keeps its own ground: the banded message runs to it and no
	// further, so the bar reads the same over a picked row as over any other
	for _, row := range rows {
		if k := len(row); k > 0 && row[k-1].Bar && row[k-1].Fill == hostapi.FillBand {
			row[k-1].Fill = hostapi.FillAuto
		}
	}
	st.anchor, st.anchorOff = nil, 0
	for i := st.top; i < len(lines) && i < st.top+h; i++ {
		if strings.TrimSpace(hostapi.SegsText(lines[i])) == "" {
			continue
		}
		st.anchorOff = i - st.top
		for j := i; j < len(lines) && j < i+anchorRows; j++ {
			st.anchor = append(st.anchor, hostapi.SegsText(lines[j]))
		}
		break
	}
	for i := range rows {
		rows[i] = padSegs(rows[i], w)
	}
	return rows
}

// faded is row with every segment but its scrollbar cell sunk toward the
// surface it sits on by t, keeping a deeper sink a widget set itself.
func faded(row []hostapi.Segment, t float64) []hostapi.Segment {
	out := make([]hostapi.Segment, len(row))
	for i, s := range row {
		if !s.Bar {
			s.Fade = max(s.Fade, t)
		}
		out[i] = s
	}
	return out
}

func pinOf(row []hostapi.Segment) hostapi.Pin {
	if len(row) == 0 {
		return hostapi.PinNone
	}
	return row[0].Pin
}

// rowAnchor is the transcript row a drawn row carries, 0 for none. A live
// rail's rows carry their run's anchor, which is negative when the run holds
// no settled rows; callers that mean "a place the reader can be at" keep
// their own > 0 guard.
func rowAnchor(row []hostapi.Segment) int {
	for _, s := range row {
		if s.Anchor != 0 {
			return s.Anchor
		}
	}
	return 0
}

// rowCall is the transcript row of the call a rail row draws, 0 for none.
func rowCall(row []hostapi.Segment) int {
	for _, s := range row {
		if s.Call != 0 {
			return s.Call
		}
	}
	return 0
}

// bareBar is a rail's air row: the bar alone. It carries the run's anchor
// but stands between the rows that name the reader's place.
func bareBar(row []hostapi.Segment) bool {
	return len(row) == 1 && row[0].Text == "│"
}

// unitAt is the anchor of the unit a transcript row is drawn in: the row's
// own when a row carries it, else the nearest anchor before it, which is how
// a notice inside a folded burst or a call inside its run's digest finds the
// one block that draws it. 0 when nothing at or before it is drawn.
func unitAt(lines [][]hostapi.Segment, n int) int {
	if from, _ := unitRows(lines, n); from >= 0 {
		return n
	}
	best := 0
	for _, row := range lines {
		if at := rowAnchor(row); at > 0 && at < n {
			best = at
		}
	}
	return best
}

// unitSpan is the rows a unit's anchor draws: the rows carrying it, which
// the chat stamps on every row of the unit - a rail's air rows included, the
// live turn's rows its run names too. -1 when none does.
func unitSpan(lines [][]hostapi.Segment, anchor int) (from, to int) {
	from, to = -1, -1
	for i, row := range lines {
		if rowAnchor(row) != anchor {
			continue
		}
		if from < 0 {
			from = i
		}
		to = i
	}
	return from, to
}

// unitRows is the rows a named transcript row draws, first to last: the
// rows of the call it is, when a rail drew it, else the message's. -1 when
// no row carries it.
func unitRows(lines [][]hostapi.Segment, anchor int) (from, to int) {
	if from, to := callRows(lines, anchor); from >= 0 {
		return from, to
	}
	return unitSpan(lines, anchor)
}

// callRows are the rows one call of a rail draws, head and lines under it,
// or -1 when no row names that call.
func callRows(lines [][]hostapi.Segment, call int) (from, to int) {
	from, to = -1, -1
	for i, row := range lines {
		if rowCall(row) != call {
			continue
		}
		if from < 0 {
			from = i
		}
		to = i
	}
	return from, to
}

// unitSpans indexes the lines once for a list of anchors: the rows each
// anchor draws as a message and as a call of a rail, where unitRows and
// unitAt would scan the lines per anchor.
type unitSpans struct {
	msg, call map[int][2]int
	anchors   []int // the anchors with message rows, ascending
}

func indexUnits(lines [][]hostapi.Segment) unitSpans {
	sp := unitSpans{msg: map[int][2]int{}, call: map[int][2]int{}}
	for i, row := range lines {
		if at := rowAnchor(row); at > 0 {
			if s, ok := sp.msg[at]; ok {
				s[1] = i
				sp.msg[at] = s
			} else {
				sp.msg[at] = [2]int{i, i}
				sp.anchors = append(sp.anchors, at)
			}
		}
		if c := rowCall(row); c != 0 {
			if s, ok := sp.call[c]; ok {
				s[1] = i
				sp.call[c] = s
			} else {
				sp.call[c] = [2]int{i, i}
			}
		}
	}
	slices.Sort(sp.anchors)
	return sp
}

// rows is the span an anchor draws, unitRows(unitAt(lines, n)) without the
// scans: a call's rows when a rail row names it, else the message's, else
// the nearest drawn anchor before it.
func (sp unitSpans) rows(n int) (from, to int) {
	if s, ok := sp.call[n]; ok {
		return s[0], s[1]
	}
	if s, ok := sp.msg[n]; ok {
		return s[0], s[1]
	}
	pos, _ := slices.BinarySearch(sp.anchors, n)
	if pos == 0 {
		return -1, -1
	}
	if s, ok := sp.call[sp.anchors[pos-1]]; ok {
		return s[0], s[1]
	}
	s := sp.msg[sp.anchors[pos-1]]
	return s[0], s[1]
}

// jumpTop is where a following list puts the view for a unit's rows: the
// middle of the view when the whole message fits, else its first row, which
// then has to stand below the pinned headers that draw over the view's start.
func jumpTop(lines [][]hostapi.Segment, from, to, viewH int) int {
	top := from
	if height := to - from + 1; height < viewH {
		top = from - (viewH-height)/2
	}
	top = max(top, 0)
	// the header a top pins may reach further up than the last one, so the
	// first row is placed clear of it twice over
	for range 2 {
		if n := len(pinned(nil, lines, top)); from-top < n {
			top = max(from-n, 0)
		}
	}
	return top
}

// bandSpan is lines with rows from..to banded to width cells: the tint a
// picked list row gets (FillBand, which every text role is clamped to read
// on), over the rows the pick draws. Untouched rows are shared, not copied.
func bandSpan(lines [][]hostapi.Segment, from, to, width int) [][]hostapi.Segment {
	if from < 0 {
		return lines
	}
	out := slices.Clone(lines)
	for i := from; i <= to; i++ {
		row := slices.Clone(lines[i])
		for k := range row {
			row[k].Fill = hostapi.FillBand
		}
		if pad := width - hostapi.SegsWidth(row); pad > 0 {
			row = append(row, hostapi.Segment{Text: strings.Repeat(" ", pad), HL: "Normal", Fill: hostapi.FillBand})
		}
		out[i] = row
	}
	return out
}

// pinned is dst holding the header rows a view scrolled to top draws over
// its first rows, top down: the nearest turn header above top, then each
// section header of that turn that has passed the row it pins on, until its
// end mark (a run of PinEnd rows, as a wrapped row carries it down) reaches
// that row. Nothing pins while a turn header is within the first three rows
// shown, so the pins never stack two turns with a gap row and a stats line
// between.
// NOTE: a scan back to the turn header per painted frame of a view below
// one, following a turn included: a few hundred reads in a long turn.
func pinned(dst []int, lines [][]hostapi.Segment, top int) []int {
	dst = dst[:0]
	if top <= 0 {
		return dst
	}
	for i := top; i < min(top+3, len(lines)); i++ {
		if pinOf(lines[i]) == hostapi.PinTurn {
			return dst
		}
	}
	from := 0
	for i := min(top, len(lines)) - 1; i > 0; i-- {
		if pinOf(lines[i]) == hostapi.PinTurn && headerAt(lines, i) {
			from = i
			break
		}
	}
	// the next header pins on row len(dst) of the view, and an end mark
	// drops the section pinned on row len(dst)-1
	for i := from; i < min(top+len(dst), len(lines)); i++ {
		switch p := pinOf(lines[i]); {
		case p == hostapi.PinEnd && (i == 0 || pinOf(lines[i-1]) != p):
			if n := len(dst); n > 0 && pinOf(lines[dst[n-1]]) == hostapi.PinSection {
				dst = dst[:n-1]
			}
		case !headerAt(lines, i):
		case p == hostapi.PinTurn:
			dst = append(dst[:0], i)
		default:
			dst = append(dst, i)
		}
	}
	return dst
}

// pinRows sets the rows pinned over the first ones shown and their copies,
// each made again only when its row, whether more header rows follow it, or
// the width changed.
func (st *scrollState) pinRows(lines [][]hostapi.Segment, width int) {
	st.lines, st.pin = lines, pinned(st.pin, lines, st.top)
	n := min(len(st.pinned), len(st.pin))
	st.pins, st.pinned = st.pins[:n], st.pinned[:n]
	for k, i := range st.pin {
		key := pinKey{row: &lines[i][0], more: i+1 < len(lines) && pinOf(lines[i+1]) == pinOf(lines[i]), width: width}
		switch {
		case k == len(st.pinned):
			st.pins, st.pinned = append(st.pins, pinCopy(lines[i], key.more, width)), append(st.pinned, key)
		case st.pinned[k] != key:
			st.pins[k], st.pinned[k] = pinCopy(lines[i], key.more, width), key
		}
	}
}

// pinCopy is row as the pin draws it, chrome rather than content: all in
// Pinned (dimmed) on the panel, which runs on to the bar and sets it off
// the text under it, nothing in it copying or acting but the pin's own
// click, and ending in an ellipsis when the header has more rows.
func pinCopy(row []hostapi.Segment, more bool, width int) []hostapi.Segment {
	row = slices.Clone(row)
	for i := range row {
		row[i].HL, row[i].Effect, row[i].NoCopy, row[i].Soft, row[i].Click, row[i].Widget = "Pinned", "", true, false, "", nil
		row[i].Fill = hostapi.FillPanel
	}
	if more {
		for len(row) > 0 {
			last := &row[len(row)-1]
			if last.Text = strings.TrimRight(last.Text, " "); last.Text != "" {
				break
			}
			row = row[:len(row)-1]
		}
		row = append(truncateSegs(row, barText(width)-1), hostapi.Segment{Text: "…", HL: "Pinned", NoCopy: true, Fill: hostapi.FillPanel})
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
// width cells. The cell keeps the surface of the one beside it, so a banded
// row's band runs under the bar instead of stopping a cell short of it.
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
	fill := hostapi.FillAuto
	if n := len(row); n > 0 {
		fill = row[n-1].Fill
	}
	out := make([]hostapi.Segment, len(row), len(row)+1)
	copy(out, row)
	return append(out, hostapi.Segment{Text: mark, HL: hl, NoCopy: true, Bar: true, Fill: fill})
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
	if anchoredAt(lines, want, st.anchor) {
		return want - st.anchorOff
	}
	// the nearest line is the same answer a whole scan finds, and this stops
	// at the first hit: each probe costs a SegsText
	for d := 1; want-d >= 0 || want+d < len(lines); d++ {
		if hit := want - d; hit >= 0 && anchoredAt(lines, hit, st.anchor) {
			return hit - st.anchorOff
		}
		if hit := want + d; hit < len(lines) && anchoredAt(lines, hit, st.anchor) {
			return hit - st.anchorOff
		}
	}
	return want - st.anchorOff
}

func anchoredAt(lines [][]hostapi.Segment, i int, want []string) bool {
	// a shrunk transcript puts the guessed row above the first line
	if len(want) == 0 || i < 0 || i+len(want) > len(lines) {
		return false
	}
	for k, s := range want {
		if hostapi.SegsText(lines[i+k]) != s {
			return false
		}
	}
	return true
}

// scrollOf is the scrolled state of n's region.
func (a *App) scrollOf(n *node) *scrollState {
	st := a.scrolls[n.Region]
	if st == nil {
		st = &scrollState{follow: true}
		a.scrolls[n.Region] = st
	}
	return st
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

// placeBox writes box at x0, y0 over rows in place, clipped to the frame.
func placeBox(rows, box [][]hostapi.Segment, x0, y0, w int) {
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
}
