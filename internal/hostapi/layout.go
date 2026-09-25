package hostapi

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
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

// LayoutNode is one node of the screen's layout tree (DESIGN §8): a box
// lays its children out along Dir, a leaf names what fills its cells.
type LayoutNode struct {
	ID       string
	Kind     string // box, region, composer, bar or spacer
	Dir      string // box: row or col
	Sep      bool   // box: a one-cell separator between children
	Children []*LayoutNode

	Region      string // region: the widgets drawn in it
	Scroll      string // region: "top" or "bottom" (follows the tail) makes it scrollable
	Focusable   bool   // region: can take keys (zemi.ui.focus), by its id
	Left, Right string // bar, framed composer: the regions of its left and right clusters

	Frame bool   // composer: a square frame around it, two rows taller
	Top   string // framed composer: the region drawn in its top rule

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
	Anchor        string // NW, NE, SW or SE
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

// Regions are the region names the layout places, hidden nodes included.
func (l *Layout) Regions() map[string]bool {
	out := map[string]bool{}
	var walk func(n *LayoutNode)
	walk = func(n *LayoutNode) {
		for _, r := range []string{n.Region, n.Left, n.Right, n.Top} {
			if r != "" {
				out[r] = true
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(l.Root)
	for _, f := range l.Floats {
		out[f.Region] = true
	}
	return out
}

// Focusables are the ids of the focusable regions and floats, in tree
// order and then float order: the order focus cycles in.
func (l *Layout) Focusables() []string {
	var out []string
	var walk func(n *LayoutNode)
	walk = func(n *LayoutNode) {
		if n.Focusable {
			out = append(out, n.ID)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(l.Root)
	for _, f := range l.Floats {
		if f.Focusable {
			out = append(out, f.ID)
		}
	}
	return out
}

// Rect is a node's cells on the last frame, 0-based.
type Rect struct{ X, Y, Width, Height int }

// DefaultLayout is what zemi draws when no config set a layout (before the
// shipped UI loads, or after a reload that set none). It matches the
// shipped tui/layout.lua: the main column; in it the header, the
// stage (the chat), a blank row and the framed composer, whose rules carry
// the mascot and the statusline.
var DefaultLayout = &Layout{Root: &LayoutNode{Kind: "box", Dir: "row", Sep: true, Size: Size{Kind: SizeFill}, Children: []*LayoutNode{
	{Kind: "box", Dir: "col", Size: Size{Kind: SizeFill}, Min: 30, Children: []*LayoutNode{
		{Kind: "region", Region: "header"},
		{Kind: "box", Dir: "col", Stage: true, Size: Size{Kind: SizeFill}, Min: 1, Children: []*LayoutNode{
			{ID: "chat", Kind: "region", Region: "chat", Scroll: "bottom", Size: Size{Kind: SizeFill}, Min: 3},
		}},
		{Kind: "spacer", Size: Size{Kind: SizeCells, N: 1}},
		{Kind: "composer", Frame: true, Top: "working", Left: "statusline.left", Right: "statusline.right"},
	}},
}}}

const (
	maxLayoutNodes = 64
	maxLayoutDepth = 8
)

// layoutSet is the layout config set, kept whole for zemi.ui.layout.get.
type layoutSet struct {
	layout *Layout
	raw    any
	origin lua.Origin
	gen    Gen
}

// Layout is the layout in force: the one config set, else DefaultLayout.
func (h *Host) Layout() *Layout {
	if s := h.layout.Load(); s != nil {
		return s.layout
	}
	return DefaultLayout
}

// layoutNamespace is zemi.ui.layout: set, get and rects. One layout exists
// at a time. The shipped UI sets it and user config may replace it; lower
// origins are refused, since a layout can hide everything they would show.
// A reload keeps drawing the old layout until the replay sets one, and one
// that sets none falls back to DefaultLayout (ForceReload).
func (h *Host) layoutNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		o := h.RT.CurrentOrigin(L)
		if o != lua.OriginUser && o != lua.OriginBuiltin {
			L.RaiseError("zemi.ui.layout.set: only user config may set the layout, not %s code", o)
		}
		raw := luaToGoChecked(L, 1, "zemi.ui.layout.set")
		lay, err := ParseLayout(raw)
		if err != nil {
			L.RaiseError("zemi.ui.layout.set: %v", err)
		}
		if cur := h.layout.Load(); cur != nil && cur.gen == h.currentGen() && o.Rank() < cur.origin.Rank() {
			L.RaiseError("zemi.ui.layout.set: %s config set the layout; %s code may not replace it", cur.origin, o)
		}
		h.layout.Store(&layoutSet{layout: lay, raw: raw, origin: o, gen: h.currentGen()})
		h.dropStaleFocus()
		h.refresh()
		return 0
	}))
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		s := h.layout.Load()
		if s == nil {
			L.Push(glua.LNil)
			return 1
		}
		L.Push(lua.FromGo(L, s.raw))
		return 1
	}))
	tb.RawSetString("rects", L.NewFunction(func(L *glua.LState) int {
		out := map[string]any{}
		if ui := h.hooks(); ui != nil {
			for id, r := range ui.LayoutRects() {
				out[id] = map[string]any{"x": r.X, "y": r.Y, "width": r.Width, "height": r.Height}
			}
		}
		L.Push(lua.FromGo(L, out))
		return 1
	}))
	return tb
}

// dropStaleLayout runs after a replay: a layout the new generation did not
// set again belongs to config that is gone.
func (h *Host) dropStaleLayout() {
	if s := h.layout.Load(); s != nil && s.gen != h.currentGen() {
		h.layout.CompareAndSwap(s, nil)
		h.dropStaleFocus()
		h.refresh()
	}
}

// Focus is the id of the layout node taking keys, "" for the composer.
func (h *Host) Focus() string {
	if p := h.focus.Load(); p != nil {
		return *p
	}
	return ""
}

// SetFocus gives the keys to a node by id, or to the composer with "". The
// UI calls it as it is; Lua goes through focusNode, which checks the id.
func (h *Host) SetFocus(id string) {
	if h.Focus() != id {
		h.focus.Store(&id)
		h.refresh()
	}
}

// focusNode moves the focus to a focusable node of the layout in force, or
// to the composer with "".
func (h *Host) focusNode(id string) error {
	if id != "" && !slices.Contains(h.Layout().Focusables(), id) {
		return fmt.Errorf("the layout has no focusable node %q", id)
	}
	h.SetFocus(id)
	return nil
}

// cycleFocus moves the focus step places through the composer and the
// focusable nodes the last frame drew.
func (h *Host) cycleFocus(step int) error {
	ui := h.hooks()
	if ui == nil {
		return nil
	}
	drawn := ui.LayoutRects()
	ids := []string{""}
	for _, id := range h.Layout().Focusables() {
		if _, ok := drawn[id]; ok {
			ids = append(ids, id)
		}
	}
	i := max(slices.Index(ids, h.Focus()), 0)
	return h.focusNode(ids[((i+step)%len(ids)+len(ids))%len(ids)])
}

// dropStaleFocus hands the keys back to the composer when the layout in
// force no longer has the focused node.
func (h *Host) dropStaleFocus() {
	if f := h.Focus(); f != "" && !slices.Contains(h.Layout().Focusables(), f) {
		h.SetFocus("")
	}
}

// focusNamespace is zemi.ui.focus: set(id?), get(), next() and prev().
// Moving the focus is gated like zemi.ui.layout.set: the focused node takes
// every key, so lower origins could otherwise route the user's typing to
// their own keymaps. Only user config sets the layout the ids come from.
func (h *Host) focusNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "ui.focus.set")
		if err := h.focusNode(L.OptString(1, "")); err != nil {
			L.RaiseError("zemi.ui.focus.set: %v", err)
		}
		return 0
	}))
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		if f := h.Focus(); f != "" {
			L.Push(glua.LString(f))
		} else {
			L.Push(glua.LNil)
		}
		return 1
	}))
	for name, step := range map[string]int{"next": 1, "prev": -1} {
		tb.RawSetString(name, L.NewFunction(func(L *glua.LState) int {
			h.refuseBelowUser(L, "ui.focus."+name)
			_ = h.cycleFocus(step)
			return 0
		}))
	}
	return tb
}

// layoutParser checks a layout table as it builds the Go tree.
type layoutParser struct {
	nodes     int
	ids       map[string]bool
	regions   map[string]bool
	composers int
	stages    int
}

// ParseLayout turns zemi.ui.layout.set's table into a Layout, or says what
// is wrong with it (DESIGN §6.0: misuse raises).
func ParseLayout(v any) (*Layout, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a layout table")
	}
	if err := onlyKeys(m, "layout", "root", "floats"); err != nil {
		return nil, err
	}
	p := &layoutParser{ids: map[string]bool{}, regions: map[string]bool{}}
	root, err := p.node(m["root"], "root", 1, "col", false)
	if err != nil {
		return nil, err
	}
	if root.Kind != "box" {
		return nil, fmt.Errorf("root: must be a box")
	}
	if p.composers != 1 {
		return nil, fmt.Errorf("the layout needs exactly one composer, found %d", p.composers)
	}
	if p.stages > 1 {
		return nil, fmt.Errorf("at most one node may be the stage, found %d", p.stages)
	}
	lay, nodeIDs := &Layout{Root: root}, maps.Clone(p.ids)
	floats, err := list(m["floats"], "floats")
	if err != nil {
		return nil, err
	}
	for i, fv := range floats {
		f, err := p.float(fv, fmt.Sprintf("floats[%d]", i+1))
		if err != nil {
			return nil, err
		}
		lay.Floats = append(lay.Floats, f)
	}
	for _, f := range lay.Floats {
		if r := f.Relative; r != "stage" && r != "screen" && !nodeIDs[r] {
			return nil, fmt.Errorf("float %s: relative %q names no node", f.Region, r)
		}
	}
	return lay, nil
}

func (p *layoutParser) node(v any, at string, depth int, dir string, hiddenAbove bool) (*LayoutNode, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected a node table", at)
	}
	if p.nodes++; p.nodes > maxLayoutNodes {
		return nil, fmt.Errorf("more than %d nodes", maxLayoutNodes)
	}
	if depth > maxLayoutDepth {
		return nil, fmt.Errorf("%s: deeper than %d levels", at, maxLayoutDepth)
	}
	n := &LayoutNode{}
	var err error
	str := func(key string) string {
		s, ok := m[key].(string)
		if !ok && m[key] != nil && err == nil {
			err = fmt.Errorf("%s: %s must be a string", at, key)
		}
		return s
	}
	flag := func(key string) bool {
		b, ok := m[key].(bool)
		if !ok && m[key] != nil && err == nil {
			err = fmt.Errorf("%s: %s must be a boolean", at, key)
		}
		return b
	}
	cells := func(key string) int {
		f, ok := m[key].(float64)
		if (!ok && m[key] != nil || f < 0 || f != float64(int(f))) && err == nil {
			err = fmt.Errorf("%s: %s must be a whole number of cells", at, key)
		}
		return int(f)
	}
	n.Kind, n.ID = str("kind"), str("id")
	n.Stage, n.Hidden, n.Collapse = flag("stage"), flag("hidden"), flag("collapse")
	n.Min, n.Max = cells("min"), cells("max")
	if err != nil {
		return nil, err
	}
	if n.ID != "" {
		at = n.ID
		if p.ids[n.ID] || n.ID == "stage" || n.ID == "screen" {
			return nil, fmt.Errorf("node id %q is taken", n.ID)
		}
		p.ids[n.ID] = true
	}
	if n.Max > 0 && n.Min > n.Max {
		return nil, fmt.Errorf("%s: min %d is over max %d", at, n.Min, n.Max)
	}
	n.Stage = n.Stage && !n.Hidden
	if n.Stage {
		p.stages++
	}
	common := []string{"kind", "id", "size", "weight", "min", "max", "hidden", "collapse", "stage"}
	region := func(key string) error {
		name := str(key)
		switch {
		case err != nil:
			return err
		case name == "":
			return fmt.Errorf("%s: %s must name a region", at, key)
		case p.regions[name]:
			return fmt.Errorf("%s: region %q is drawn twice", at, name)
		}
		p.regions[name] = true
		return nil
	}
	switch n.Kind {
	case "box":
		err = onlyKeys(m, at, append(common, "dir", "sep", "children")...)
		n.Dir, n.Sep = str("dir"), flag("sep")
		if err == nil && n.Dir != "row" && n.Dir != "col" {
			err = fmt.Errorf("%s: dir must be \"row\" or \"col\"", at)
		}
	case "region":
		if err = onlyKeys(m, at, append(common, "region", "scroll", "focusable")...); err == nil {
			err = region("region")
		}
		n.Region, n.Scroll, n.Focusable = str("region"), str("scroll"), flag("focusable")
		switch {
		case err != nil:
		case n.Scroll != "" && n.Scroll != "top" && n.Scroll != "bottom":
			err = fmt.Errorf("%s: scroll must be \"top\" or \"bottom\"", at)
		case (n.Scroll != "" || n.Focusable) && n.ID == "":
			err = fmt.Errorf("%s: a scrolled or focusable region needs an id", at)
		}
	case "bar":
		if err = onlyKeys(m, at, append(common, "left", "right")...); err == nil {
			if err = region("left"); err == nil {
				err = region("right")
			}
		}
		n.Left, n.Right = str("left"), str("right")
	case "composer":
		err = onlyKeys(m, at, append(common, "frame", "top", "left", "right")...)
		n.Frame, n.Top, n.Left, n.Right = flag("frame"), str("top"), str("left"), str("right")
		p.composers++
		switch {
		case err != nil:
		case n.Hidden || n.Collapse || hiddenAbove:
			err = fmt.Errorf("%s: the composer cannot be hidden", at)
		case !n.Frame && (n.Top != "" || n.Left != "" || n.Right != ""):
			err = fmt.Errorf("%s: top, left and right need frame = true", at)
		}
		for _, key := range []string{"top", "left", "right"} {
			if err == nil && str(key) != "" {
				err = region(key)
			}
		}
	case "spacer":
		err = onlyKeys(m, at, common...)
	default:
		err = fmt.Errorf("%s: unknown kind %q (box, region, bar, composer or spacer)", at, n.Kind)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case n.Kind == "spacer":
		n.Size = Size{Kind: SizeCells, N: 1}
	case dir == "row" || n.Kind == "box":
		n.Size = Size{Kind: SizeFill} // content is measured down a column only
	default:
		n.Size = Size{Kind: SizeContent}
	}
	if sv, ok := m["size"]; ok {
		if n.Size, err = parseSize(sv, at); err != nil {
			return nil, err
		}
		if n.Size.Kind == SizeContent && (dir == "row" || n.Kind == "box" || n.Kind == "spacer") {
			return nil, fmt.Errorf("%s: size \"content\" works only for a region, bar or composer in a col", at)
		}
	}
	if w, ok := m["weight"]; ok {
		f, isNum := w.(float64)
		if !isNum || f < 1 || f != float64(int(f)) || n.Size.Kind != SizeFill {
			return nil, fmt.Errorf("%s: weight must be a whole number from 1, on a \"fill\" node", at)
		}
		n.Size.N = int(f)
	}
	if n.Kind != "box" {
		if _, ok := m["children"]; ok {
			return nil, fmt.Errorf("%s: only a box has children", at)
		}
		return n, nil
	}
	kids, err := list(m["children"], at+".children")
	if err != nil {
		return nil, err
	}
	for i, kv := range kids {
		k, err := p.node(kv, fmt.Sprintf("%s.children[%d]", at, i+1), depth+1, n.Dir, hiddenAbove || n.Hidden || n.Collapse)
		if err != nil {
			return nil, err
		}
		n.Children = append(n.Children, k)
	}
	return n, nil
}

func (p *layoutParser) float(v any, at string) (LayoutFloat, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return LayoutFloat{}, fmt.Errorf("%s: expected a float table", at)
	}
	if err := onlyKeys(m, at, "id", "region", "relative", "anchor", "row", "col", "width", "height", "hidden", "focusable"); err != nil {
		return LayoutFloat{}, err
	}
	f := LayoutFloat{Relative: "stage", Anchor: "NW", Width: Size{Kind: SizeContent}, Height: Size{Kind: SizeContent}}
	for key, dst := range map[string]*string{"id": &f.ID, "region": &f.Region, "relative": &f.Relative, "anchor": &f.Anchor} {
		if s, ok := m[key].(string); ok {
			*dst = s
		} else if m[key] != nil {
			return f, fmt.Errorf("%s: %s must be a string", at, key)
		}
	}
	for key, dst := range map[string]*int{"row": &f.Row, "col": &f.Col} {
		if n, ok := m[key].(float64); ok && n == float64(int(n)) {
			*dst = int(n)
		} else if m[key] != nil {
			return f, fmt.Errorf("%s: %s must be a whole number", at, key)
		}
	}
	for key, dst := range map[string]*Size{"width": &f.Width, "height": &f.Height} {
		if sv, ok := m[key]; ok {
			s, err := parseSize(sv, at)
			if err != nil {
				return f, err
			}
			if s.Kind == SizeFill {
				return f, fmt.Errorf("%s: a float's %s takes cells, a percent or \"content\"", at, key)
			}
			*dst = s
		}
	}
	f.Hidden, _ = m["hidden"].(bool)
	f.Focusable, _ = m["focusable"].(bool)
	switch {
	case f.Focusable && f.ID == "":
		return f, fmt.Errorf("%s: a focusable float needs an id", at)
	case f.Region == "":
		return f, fmt.Errorf("%s: region must name a region", at)
	case p.regions[f.Region]:
		return f, fmt.Errorf("%s: region %q is drawn twice", at, f.Region)
	case !slices.Contains([]string{"NW", "NE", "SW", "SE"}, f.Anchor):
		return f, fmt.Errorf("%s: anchor must be NW, NE, SW or SE", at)
	case f.ID != "" && (p.ids[f.ID] || f.ID == "stage" || f.ID == "screen"):
		return f, fmt.Errorf("node id %q is taken", f.ID)
	}
	p.regions[f.Region] = true
	if f.ID != "" {
		p.ids[f.ID] = true
	}
	return f, nil
}

// parseSize reads a size: a whole number of cells, "N%", "fill" or "content".
func parseSize(v any, at string) (Size, error) {
	bad := fmt.Errorf("%s: size must be cells, \"N%%\" (1 to 100), \"fill\" or \"content\", got %v", at, v)
	switch s := v.(type) {
	case float64:
		if s < 0 || s != float64(int(s)) {
			return Size{}, bad
		}
		return Size{Kind: SizeCells, N: int(s)}, nil
	case string:
		switch s {
		case "fill":
			return Size{Kind: SizeFill}, nil
		case "content":
			return Size{Kind: SizeContent}, nil
		}
		if num, ok := strings.CutSuffix(s, "%"); ok {
			if n, err := strconv.Atoi(num); err == nil && n > 0 && n <= 100 {
				return Size{Kind: SizePercent, N: n}, nil
			}
		}
	}
	return Size{}, bad
}

// list reads an array; nil and an empty table are an empty one.
func list(v any, at string) ([]any, error) {
	switch l := v.(type) {
	case nil:
		return nil, nil
	case []any:
		return l, nil
	case map[string]any:
		if len(l) == 0 {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("%s: expected a list", at)
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
