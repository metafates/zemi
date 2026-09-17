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
	axis int  // a window: the cells its box shares out, what its frac is of
	kids []laid
}

// layout solves the layout tree for a w by h frame and finds the stage, the
// whole frame when no node claims it.
func (a *App) layout(w, h int) (laid, uv.Rectangle) {
	root := a.solve(hostapi.DefaultLayout, uv.Rect(0, 0, w, h))
	stage := uv.Rect(0, 0, w, h)
	var find func(l laid) bool
	find = func(l laid) bool {
		if l.node.Stage && l.wn == nil {
			stage = l.rect
			return true
		}
		return slices.ContainsFunc(l.kids, find)
	}
	find(root)
	return root, stage
}

// solve places n's children inside r. A windows slot becomes one child per
// shown window of its kind: its frac of the box's cells, else a fill share
// weighing the same as its fill siblings.
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
		if c.Kind != "windows" {
			items = append(items, item{node: c})
			continue
		}
		for _, wn := range a.shownWins(c.Slot) {
			items = append(items, item{node: c, wn: wn})
		}
	}
	avail := length
	if n.Sep {
		avail -= max(len(items)-1, 0)
	}
	avail = max(avail, 0)
	kids := make([]spanKid, len(items))
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
		if f := it.wn.Layout().Frac; f > 0 {
			kids[i].size = hostapi.Size{Kind: hostapi.SizeCells, N: int(math.Round(f * float64(avail)))}
		}
	}
	pos := r.Min.Y
	if row {
		pos = r.Min.X
	}
	for i, size := range span(avail, kids) {
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
		l.kids = append(l.kids, k)
		pos += size
	}
	return l
}

// measure is the rows a content-sized node draws at width.
// NOTE: content is measured along a column only; a content node in a row
// takes no cells.
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
	root, _ := a.layout(a.width, a.height)
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
	walk(root)
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
					rows[j] = slices.Concat(padSegs(rows[j], x), []hostapi.Segment{{Text: "│", HL: "Comment"}})
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
				rows = append(rows, []hostapi.Segment{{Text: strings.Repeat("─", w), HL: "Comment"}})
			}
			rows = append(rows, a.paint(k)...)
		}
	case n.Kind == "region" && n.Scroll != "":
		rows = a.paintChat(n.Region, w, h)
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

// paintChat draws the scrolled region h rows tall (DESIGN §8 scrollback):
// follow pins the window to the tail (chatTop may go negative when content
// is shorter than the viewport, which bottom-anchors it); otherwise chatTop
// is the absolute first visible row, clamped so content growth or a
// narrower rewrap never shows blank rows.
func (a *App) paintChat(region string, w, h int) [][]hostapi.Segment {
	chat := a.regionLines(region, w)
	a.chatRows, a.chatViewH = len(chat), h
	if a.chatFollow {
		a.chatTop = a.chatRows - h
	} else {
		a.chatTop = min(a.chatTop, max(0, a.chatRows-h))
	}
	rows := make([][]hostapi.Segment, h)
	for i := range rows {
		var row []hostapi.Segment
		if j := a.chatTop + i; j >= 0 && j < len(chat) {
			row = truncateSegs(chat[j], w)
		}
		rows[i] = padSegs(row, w)
	}
	return rows
}

// paintWindow draws a window a slot expanded into: its title row, then its
// cells, recorded for the mouse.
func (a *App) paintWindow(l laid) [][]hostapi.Segment {
	r := l.rect
	if r.Dx() < 1 || r.Dy() < 2 {
		return nil
	}
	sepX := -1
	if l.sep {
		sepX = r.Min.X - 1
	}
	l.wn.Resize(r.Dx(), r.Dy()-1)
	a.winRects = append(a.winRects, winRect{id: l.wn.ID, x: r.Min.X, y: r.Min.Y + 1, w: r.Dx(), h: r.Dy() - 1, titleY: r.Min.Y, sepX: sepX, axis: l.axis})
	return a.windowRows(l.wn, r.Dx(), r.Dy()-1)
}
