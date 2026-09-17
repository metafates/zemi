package hostapi

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

// LayoutNode is one node of the screen's layout tree (DESIGN §8): a box
// lays its children out along Dir, a leaf names what fills its cells.
type LayoutNode struct {
	Kind     string // box, region, composer, bar, windows or spacer
	Dir      string // box: row or col
	Sep      bool   // box: a one-cell separator between children
	Children []*LayoutNode

	Region      string // region: the widgets drawn in it
	Scroll      string // region: "bottom" scrolls it with the chat scroll state, tail followed
	Left, Right string // bar: the regions of its left and right clusters
	Slot        string // windows: the kind of window spliced in here, split or vsplit

	Stage    bool // prompts, overlays, toasts, popups and centered floats go over this node
	Size     Size
	Min, Max int // Max 0 means none
}

// DefaultLayout is the screen the shipped UI draws: the main column beside
// the vsplit windows; in it the header, the stage (the chat over the split
// windows), a blank row, the working row, the composer, a blank row and the
// statusline.
var DefaultLayout = &LayoutNode{Kind: "box", Dir: "row", Sep: true, Children: []*LayoutNode{
	{Kind: "box", Dir: "col", Size: Size{Kind: SizeFill}, Min: 30, Children: []*LayoutNode{
		{Kind: "region", Region: "header"},
		{Kind: "box", Dir: "col", Stage: true, Size: Size{Kind: SizeFill}, Min: 1, Children: []*LayoutNode{
			{Kind: "region", Region: "chat", Scroll: "bottom", Size: Size{Kind: SizeFill}, Min: 3},
			{Kind: "windows", Slot: "split"},
		}},
		{Kind: "spacer", Size: Size{Kind: SizeCells, N: 1}},
		{Kind: "region", Region: "working"},
		{Kind: "composer"},
		{Kind: "spacer", Size: Size{Kind: SizeCells, N: 1}},
		{Kind: "bar", Left: "statusline.left", Right: "statusline.right", Size: Size{Kind: SizeCells, N: 1}},
	}},
	{Kind: "windows", Slot: "vsplit"},
}}
