package hostapi

import (
	"fmt"
	"slices"

	"github.com/metafates/zemi/internal/set"
)

// SizeKind is how a layout node claims cells along its parent box's axis.
type SizeKind uint8

const (
	SizeContent SizeKind = iota // what the node draws: region rows, composer rows, 1 for a bar
	SizeCells
	SizePercent // of the parent's cells after separators
	SizeFill    // a share of what is left, by weight
)

// Size is a node's claim: N is the cells, the percent or the fill weight
// (0 reads as 1).
type Size struct {
	Kind SizeKind
	N    int
}

// LayoutKind is what a layout node is: a box of children or a leaf.
type LayoutKind string

const (
	LayoutBox      LayoutKind = "box"
	LayoutRegion   LayoutKind = "region"
	LayoutBar      LayoutKind = "bar"
	LayoutComposer LayoutKind = "composer"
	LayoutSpacer   LayoutKind = "spacer"
)

// LayoutDir is how a box lays out its children: DirRow side by side,
// DirCol stacked.
type LayoutDir string

const (
	DirRow LayoutDir = "row"
	DirCol LayoutDir = "col"
)

// ScrollEdge is the edge a scrollable region starts at; EdgeBottom follows
// the tail.
type ScrollEdge string

const (
	EdgeTop    ScrollEdge = "top"
	EdgeBottom ScrollEdge = "bottom"
)

// FloatAnchor is the corner of a float pinned to the same corner of the
// node it floats over.
type FloatAnchor string

const (
	AnchorNW FloatAnchor = "NW"
	AnchorNE FloatAnchor = "NE"
	AnchorSW FloatAnchor = "SW"
	AnchorSE FloatAnchor = "SE"
)

// LayoutNode is one node of the screen's layout tree (DESIGN §8): a box
// lays its children out along Dir, a leaf names what fills its cells.
type LayoutNode struct {
	ID       string
	Kind     LayoutKind
	Dir      LayoutDir // box
	Sep      bool      // box: a one-cell separator between children
	Children []*LayoutNode

	Region      string     // region: the widgets drawn in it
	Scroll      ScrollEdge // region: makes it scrollable
	Focusable   bool       // region: can take keys (zemi.ui.focus), by its id
	Fade        int        // region: the rows at an edge content lies past that fade toward the canvas
	Left, Right string     // bar, framed composer: the regions of its left and right clusters

	Frame       bool   // composer: a square frame around it, two rows taller
	Top         string // framed composer: the region drawn in its top rule
	Placeholder string // composer: drawn dim while the draft is empty

	Stage    bool // prompts, overlays, toasts, popups and centered floats go over this node
	Hidden   bool
	Collapse bool // hidden while its box cannot give every child its Min
	Size     Size
	Min, Max int // Max 0 means none
}

// LayoutFloat is a region drawn over the tree, placed like a neovim float:
// its Anchor corner sits at the same corner of the Relative node, moved
// Row rows down and Col columns right.
type LayoutFloat struct {
	ID, Region    string
	Relative      string // a node id, "stage" or "screen"
	Anchor        FloatAnchor
	Row, Col      int
	Width, Height Size // cells, a percent of the relative node, or content
	Hidden        bool
	Focusable     bool
}

// Layout is the screen: the tree, then the floats in drawing order.
type Layout struct {
	Root   *LayoutNode
	Floats []LayoutFloat
}

// walkNodes visits the layout's tree nodes, parents before children.
func (l *Layout) walkNodes(visit func(*LayoutNode)) {
	var walk func(n *LayoutNode)
	walk = func(n *LayoutNode) {
		visit(n)
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(l.Root)
}

// regions are the region names the layout places, hidden nodes included.
func (l *Layout) regions() set.Set[string] {
	out := set.Set[string]{}
	l.walkNodes(func(n *LayoutNode) {
		for _, r := range []string{n.Region, n.Left, n.Right, n.Top} {
			if r != "" {
				out.Add(r)
			}
		}
	})
	for _, f := range l.Floats {
		out.Add(f.Region)
	}
	return out
}

// Rect is a node's cells on the last frame, 0-based.
type Rect struct{ X, Y, Width, Height int }

// DefaultLayout is what zemi draws when no config set a layout (before the
// shipped UI loads, or after a reload that set none). It matches the
// shipped tui/layout.lua as it boots: the main column centered; in it the
// header, the stage centered inside it, and the framed composer wider than
// the stage, the statusline on a bar row under it.
var DefaultLayout = &Layout{Root: &LayoutNode{Kind: LayoutBox, Dir: DirRow, Size: Size{Kind: SizeFill}, Children: []*LayoutNode{
	{Kind: LayoutSpacer, Size: Size{Kind: SizeFill}},
	{Kind: LayoutBox, Dir: DirCol, Size: Size{Kind: SizeFill, N: 1000}, Min: 30, Max: 124, Children: []*LayoutNode{
		{Kind: LayoutRegion, Region: "header"},
		{Kind: LayoutBox, Dir: DirRow, Size: Size{Kind: SizeFill}, Children: []*LayoutNode{
			{Kind: LayoutSpacer, Size: Size{Kind: SizeFill}},
			{Kind: LayoutBox, Dir: DirCol, Stage: true, Size: Size{Kind: SizeFill, N: 1000}, Min: 63, Max: 100, Children: []*LayoutNode{
				{ID: "chat", Kind: LayoutRegion, Region: "chat", Scroll: EdgeBottom, Fade: 4, Size: Size{Kind: SizeFill}, Min: 3},
				{Kind: LayoutSpacer, Size: Size{Kind: SizeCells, N: 1}},
			}},
			{Kind: LayoutSpacer, Size: Size{Kind: SizeFill}},
		}},
		{Kind: LayoutComposer, Placeholder: "Ask anything…", Frame: true, Top: "working"},
		{Kind: LayoutBar, Left: "statusline.left", Right: "statusline.right"},
	}},
	{Kind: LayoutSpacer, Size: Size{Kind: SizeFill}},
}}}

func luaList(v any) ([]any, bool) {
	switch l := v.(type) {
	case nil:
		return nil, true
	case []any:
		return l, true
	case map[string]any:
		return nil, len(l) == 0
	default:
		return nil, false
	}
}

// onlyKeys refuses a field the table's kind does not take, so a typo is an
// error rather than a node that silently ignores it.
func onlyKeys(m map[string]any, at string, keys ...string) error {
	for k := range m {
		if !slices.Contains(keys, k) {
			return fmt.Errorf("%s: unknown field %q", at, k)
		}
	}
	return nil
}
