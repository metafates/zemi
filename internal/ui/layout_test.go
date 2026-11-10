package ui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

func cells(n int) Size   { return Size{Kind: _sizeCells, N: n} }
func percent(n int) Size { return Size{Kind: _sizePercent, N: n} }
func fill(n int) Size    { return Size{Kind: _sizeFill, N: n} }

var _contentSize = Size{Kind: _sizeContent}

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
		{"content clamps to max", 20, []spanKid{{size: _contentSize, want: 12, ceil: 8}, {size: fill(1)}}, []int{8, 12}},
		{"too small: content gives before a fill below its min", 4, []spanKid{{size: _contentSize, want: 3}, {size: fill(1), floor: 2}}, []int{2, 2}},
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
			k := spanKid{size: Size{Kind: SizeKind(b[0] % 4), N: int(b[1])}, want: int(b[1]), floor: int(b[2] % 40)}
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
			openFill = openFill || k.size.Kind == _sizeFill && k.ceil == 0
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
			v.window(lines, c.width, _boxFade) // a frame drew it
			if c.move != nil {
				c.move(&v)
			}
			out := v.window(lines, c.width, _boxFade)
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

func TestReanchor(t *testing.T) {
	row := func(text string, anchor int) []hostapi.Segment {
		return []hostapi.Segment{{Text: text, Anchor: anchor}}
	}
	// look-alike rows: the text probes cannot tell one from the next
	lookalike := func(n, anchorAt, anchor int) [][]hostapi.Segment {
		lines := make([][]hostapi.Segment, n)
		for i := range lines {
			if i == anchorAt {
				lines[i] = row("rail head", anchor)
				continue
			}
			lines[i] = row("word", 0)
		}
		return lines
	}
	st := func(top, anchorAt, anchor int, lines [][]hostapi.Segment) *scrollState {
		from, _ := unitSpan(lines, anchor)
		return &scrollState{
			top: top, rows: len(lines), anchorOff: top - anchorAt,
			anchor:        []string{"rail head", "word", "word"},
			anchorID:      anchor,
			anchorUnitOff: anchorAt - from,
			anchorTop:     top,
		}
	}
	for _, c := range []struct {
		name     string
		lines    [][]hostapi.Segment
		st       *scrollState
		prevRows int
		want     int
	}{
		{"a streamed tail leaves the top alone", lookalike(20, 10, 7), nil, 20, 10},
		{"a fill above and a tail below move the top exactly", nil, nil, 20, 15},
		{"the anchored unit shed falls back to the probes", nil, nil, 20, 10},
	} {
		lines := c.lines
		s := c.st
		switch c.name {
		case "a streamed tail leaves the top alone":
			s = st(10, 10, 7, lines)
			lines = append(slices.Clone(lines), make([][]hostapi.Segment, 6)...)
			for i := 20; i < 26; i++ {
				lines[i] = row("word", 0)
			}
		case "a fill above and a tail below move the top exactly":
			lines = lookalike(20, 10, 7)
			s = st(10, 10, 7, lines)
			grown := make([][]hostapi.Segment, 0, 31)
			grown = append(grown, make([][]hostapi.Segment, 5)...)
			for i := range 5 {
				grown[i] = row("word", 0)
			}
			grown = append(grown, lines...)
			for range 6 {
				grown = append(grown, row("word", 0))
			}
			lines = grown
		case "the anchored unit shed falls back to the probes":
			lines = lookalike(20, 10, 7)
			s = st(10, 10, 7, lines)
			lines = slices.Clone(lines)
			lines[10] = row("word", 0)
		}
		if got := reanchor(lines, s, c.prevRows); got != c.want {
			t.Errorf("%s: top %d, want %d", c.name, got, c.want)
		}
	}
}
