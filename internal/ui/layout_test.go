package ui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

func cells(n int) hostapi.Size   { return hostapi.Size{Kind: hostapi.SizeCells, N: n} }
func percent(n int) hostapi.Size { return hostapi.Size{Kind: hostapi.SizePercent, N: n} }
func fill(n int) hostapi.Size    { return hostapi.Size{Kind: hostapi.SizeFill, N: n} }

var contentSize = hostapi.Size{Kind: hostapi.SizeContent}

func TestSpan(t *testing.T) {
	for _, c := range []struct {
		name  string
		avail int
		kids  []spanKid
		want  []int
	}{
		{"fill takes the rest", 10, []spanKid{{size: cells(3)}, {size: fill(0)}}, []int{3, 7}},
		{"percent of avail", 50, []spanKid{{size: percent(30)}, {size: fill(1)}}, []int{15, 35}},
		{"fills share by weight", 9, []spanKid{{size: fill(2)}, {size: fill(1)}}, []int{6, 3}},
		{"a capped fill gives the rest to other fills", 20, []spanKid{{size: fill(1), ceil: 5}, {size: fill(1)}}, []int{5, 15}},
		{"room no fill takes stays blank", 20, []spanKid{{size: fill(1), ceil: 5}, {size: cells(2)}}, []int{5, 2}},
		{"content clamps to max", 20, []spanKid{{size: contentSize, want: 12, ceil: 8}, {size: fill(1)}}, []int{8, 12}},
		{"too small: content gives before a fill below its min", 4, []spanKid{{size: contentSize, want: 3}, {size: fill(1), floor: 2}}, []int{2, 2}},
		{"too small: last child first within a rank", 3, []spanKid{{size: cells(2)}, {size: cells(2)}}, []int{2, 1}},
		{"below every min: fill goes first", 1, []spanKid{{size: fill(1), floor: 3}, {size: cells(1), floor: 1}}, []int{0, 1}},
		{"no room hands out nothing", -5, []spanKid{{size: cells(3)}, {size: fill(1)}}, []int{0, 0}},
	} {
		if got := span(c.avail, c.kids); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// FuzzSpan: whatever the children ask for, the sizes fit avail, respect max,
// respect min whenever the mins fit, and fill avail when an uncapped fill or
// an overfull box leaves nothing blank.
func FuzzSpan(f *testing.F) {
	f.Add(80, []byte{3, 1, 30, 0, 1, 20, 20, 0})
	f.Add(3, []byte{0, 5, 2, 8, 2, 50, 1, 0, 3, 2, 0, 4})
	f.Fuzz(func(t *testing.T, avail int, data []byte) {
		avail %= 2000
		var kids []spanKid
		for b := range slices.Chunk(data, 4) {
			if len(b) < 4 {
				break
			}
			k := spanKid{size: hostapi.Size{Kind: hostapi.SizeKind(b[0] % 4), N: int(b[1])}, want: int(b[1]), floor: int(b[2] % 40)}
			if b[3] > 0 {
				k.ceil = k.floor + int(b[3]%60)
			}
			kids = append(kids, k)
		}
		out := span(avail, kids)
		avail = max(avail, 0)
		mins, sum, openFill := 0, 0, false
		for _, k := range kids {
			mins += k.floor
			openFill = openFill || k.size.Kind == hostapi.SizeFill && k.ceil == 0
		}
		for i, k := range kids {
			sum += out[i]
			if out[i] < 0 || k.ceil > 0 && out[i] > k.ceil || mins <= avail && out[i] < k.floor {
				t.Fatalf("avail %d, child %d %+v got %d (mins %d)", avail, i, k, out[i], mins)
			}
		}
		// a box whose mins alone overflow has nothing blank either
		if sum > avail || (openFill || mins > avail) && sum != avail {
			t.Fatalf("avail %d, kids %+v: %v sums to %d", avail, kids, out, sum)
		}
	})
}

func TestViewport(t *testing.T) {
	for _, c := range []struct {
		name        string
		v           viewport
		rows, width int
		move        func(v *viewport)
		top, shown  int
		bar         bool
	}{
		{"fits: no bar", viewport{viewH: 5}, 3, 12, func(v *viewport) { v.page(1) }, 0, 3, false},
		{"line down", viewport{viewH: 5}, 20, 12, func(v *viewport) { v.scroll(1) }, 1, 5, true},
		{"line up at the top stays", viewport{viewH: 5}, 20, 12, func(v *viewport) { v.scroll(-1) }, 0, 5, true},
		{"page keeps one row of overlap", viewport{viewH: 5}, 20, 12, func(v *viewport) { v.page(1) }, 4, 5, true},
		{"page at the bottom clamps", viewport{top: 14, viewH: 5}, 20, 12, func(v *viewport) { v.page(1) }, 15, 5, true},
		{"edges", viewport{top: 7, viewH: 5}, 20, 12, func(v *viewport) { v.edge(-1); v.edge(1) }, 15, 5, true},
		{"reveal below moves the least", viewport{viewH: 5}, 20, 12, func(v *viewport) { v.reveal(8, 10) }, 5, 5, true},
		{"reveal above goes to its top", viewport{top: 10, viewH: 5}, 20, 12, func(v *viewport) { v.reveal(3, 4) }, 3, 5, true},
		{"reveal taller than the view shows its top", viewport{viewH: 5}, 20, 12, func(v *viewport) { v.reveal(6, 16) }, 6, 5, true},
		{"follow starts short content at its first row", viewport{viewH: 5, follow: true}, 3, 12, nil, 0, 3, false},
		{"scrolling up leaves the tail", viewport{viewH: 5, follow: true}, 20, 12, func(v *viewport) { v.scroll(-3) }, 12, 5, true},
		{"under 7 cells draws no bar", viewport{viewH: 5}, 20, 6, nil, 0, 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := make([][]hostapi.Segment, c.rows)
			for i := range lines {
				lines[i] = []hostapi.Segment{{Text: fmt.Sprintf("%0*d", c.width, i)}}
			}
			v := c.v
			v.window(lines, c.width, boxFade) // a frame drew it
			if c.move != nil {
				c.move(&v)
			}
			out := v.window(lines, c.width, boxFade)
			bar := false
			for i, r := range out {
				if j := v.top + i; !strings.HasPrefix(hostapi.SegsText(r), lines[j][0].Text) {
					t.Fatalf("row %d cut: %q", j, hostapi.SegsText(r))
				}
				bar = bar || len(r) > 0 && strings.HasPrefix(r[len(r)-1].HL, "Scrollbar")
			}
			if v.top != c.top || len(out) != c.shown || bar != c.bar {
				t.Fatalf("top %d, %d rows, bar %v; want %d, %d, %v", v.top, len(out), bar, c.top, c.shown, c.bar)
			}
		})
	}
}

func TestPinned(t *testing.T) {
	for _, c := range []struct {
		name, rows string // t a turn header row, s a section header, e an end mark, . any other
		top        int
		want       []int
	}{
		{"top of content", "t . . . .", 0, nil},
		{"header at top", ". t . . .", 1, nil},
		{"header close under", "t . . . t .", 2, nil},
		{"header three under", "t . . . . t", 2, []int{0}},
		{"block above", ". t t . . . .", 4, []int{1}},
		{"no header above", ". . . . . . t", 2, nil},
		{"a section under its turn pins both, in order", "t . s . . . e . .", 4, []int{0, 2}},
		{"a section on its pin's row draws as itself", "t . . s . . . e .", 2, []int{0}},
		{"a section above its pin's row pins", "t . . s . . . e .", 3, []int{0, 3}},
		{"the section's end on its pin's row ends it", "t . s . . e . . . .", 4, []int{0}},
		{"a second section after the first's end", "t . s . e . s . . . .", 8, []int{0, 6}},
		{"a new turn resets", "t . s . . . t . . . . .", 10, []int{6}},
		{"a wrapped end drops one section", "t . s . s . e e . . . .", 10, []int{0, 2}},
		{"sections without a turn", ". s . . . .", 3, []int{1}},
	} {
		var lines [][]hostapi.Segment
		pins := map[string]hostapi.Pin{"t": hostapi.PinTurn, "s": hostapi.PinSection, "e": hostapi.PinEnd}
		for f := range strings.FieldsSeq(c.rows) {
			lines = append(lines, []hostapi.Segment{{Text: f, Pin: pins[f]}})
		}
		if got := pinned(nil, lines, c.top); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// BenchmarkPin is the pin's share of a painted frame: nothing for a view at
// the content's top, a scan back for one far below its header, and no
// allocation while the pinned row stays the same.
func BenchmarkPin(b *testing.B) {
	lines := make([][]hostapi.Segment, 2000)
	for i := range lines {
		lines[i] = []hostapi.Segment{{Text: "▌ "}, {Text: "a row of text"}}
	}
	lines[0][0].Pin = hostapi.PinTurn
	for _, top := range []int{0, len(lines) - 10} {
		b.Run(strconv.Itoa(top), func(b *testing.B) {
			st := &scrollState{top: top}
			st.pinRows(lines, 80)
			b.ReportAllocs()
			for b.Loop() {
				st.pinRows(lines, 80)
			}
		})
	}
}

func TestJoin(t *testing.T) {
	for _, c := range []struct {
		name string
		dim  bool
		// a row starting with "!" is content, no Line; elsewhere every light
		// box rune is a Line cell
		rows, want []string
	}{
		{
			"a box on the composer rule joins at its corner and its edge", false,
			[]string{"┌──┐   ", "│ab│   ", "┌─────┐"},
			[]string{"┌──┐   ", "│ab│   ", "├──┴──┐"},
		},
		{
			"a right-anchored box joins at the rule's other corner", false,
			[]string{"   ┌──┐", "   │ab│", "┌─────┐"},
			[]string{"   ┌──┐", "   │ab│", "┌──┴──┤"},
		},
		{
			"dimmed rows join nothing", true,
			[]string{"┌──┐   ", "│ab│   ", "┌─────┐"},
			[]string{"┌──┐   ", "│ab│   ", "┌─────┐"},
		},
		{
			"a narrower box under a wider one shares its border", false,
			[]string{"┌─────┐", "│abcde│", "└─┌───┐", "  │abc│", "  └───┘"},
			[]string{"┌─────┐", "│abcde│", "└─┬───┤", "  │abc│", "  └───┘"},
		},
		{
			"a wider box under a narrower one shares its border", false,
			[]string{"  ┌───┐", "  │abc│", "┌─────┐", "│abcde│", "└─────┘"},
			[]string{"  ┌───┐", "  │abc│", "┌─┴───┤", "│abcde│", "└─────┘"},
		},
		{"a rule ending at a separator", false, []string{"a│c", "─│c", "b│c"}, []string{"a│c", "─┤c", "b│c"}},
		{"rules on both sides of a separator cross it", false, []string{"a│c", "─│─", "b│c"}, []string{"a│c", "─┼─", "b│c"}},
		{"parallel lines stay", false, []string{"││", "└┘", "┌┐"}, []string{"││", "└┘", "┌┐"}},
		{"text and the snail's art beside a line stay", false, []string{"─x─╭"}, []string{"─x─╭"}},
		{"content lines under a border stay", false, []string{"┌─┐", "!│a│"}, []string{"┌─┐", "│a│"}},
	} {
		w := 0
		rows := make([][]hostapi.Segment, len(c.rows))
		for i, r := range c.rows {
			r, content := strings.CutPrefix(r, "!")
			for _, ch := range r {
				line := !content && armsOf(ch) != 0
				if n := len(rows[i]); n > 0 && rows[i][n-1].Line == line {
					rows[i][n-1].Text += string(ch)
				} else {
					rows[i] = append(rows[i], hostapi.Segment{Text: string(ch), Line: line})
				}
			}
			w = max(w, len([]rune(r)))
		}
		if c.dim {
			dimRows(rows)
		}
		(&App{}).join(rows, w)
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = hostapi.SegsText(r)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
}

// TestUnitSpans: the one-walk index answers what unitRows(unitAt()) scans
// for, an undrawn anchor's fallback included.
func TestUnitSpans(t *testing.T) {
	row := func(anchor, call int) []hostapi.Segment {
		return []hostapi.Segment{{Text: "row", Anchor: anchor, Call: call}}
	}
	lines := make([][]hostapi.Segment, 12)
	lines[1] = row(1, 0)
	for i := 4; i <= 6; i++ { // a rail: every row carries the run's anchor, its own call
		lines[i] = row(4, i)
	}
	lines[8], lines[9] = row(8, 0), row(8, 0)
	lines[10] = row(10, 0)
	sp := indexUnits(lines)
	for _, n := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} {
		wantFrom, wantTo := unitRows(lines, unitAt(lines, n))
		from, to := sp.rows(n)
		if from != wantFrom || to != wantTo {
			t.Errorf("anchor %d: rows(%d, %d), want (%d, %d)", n, from, to, wantFrom, wantTo)
		}
	}
}
