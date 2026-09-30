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
		{"a capped fill gives the rest to other fills", 20, []spanKid{{size: fill(1), max: 5}, {size: fill(1)}}, []int{5, 15}},
		{"room no fill takes stays blank", 20, []spanKid{{size: fill(1), max: 5}, {size: cells(2)}}, []int{5, 2}},
		{"content clamps to max", 20, []spanKid{{size: contentSize, want: 12, max: 8}, {size: fill(1)}}, []int{8, 12}},
		{"too small: content gives before a fill below its min", 4, []spanKid{{size: contentSize, want: 3}, {size: fill(1), min: 2}}, []int{2, 2}},
		{"too small: last child first within a rank", 3, []spanKid{{size: cells(2)}, {size: cells(2)}}, []int{2, 1}},
		{"below every min: fill goes first", 1, []spanKid{{size: fill(1), min: 3}, {size: cells(1), min: 1}}, []int{0, 1}},
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
			k := spanKid{size: hostapi.Size{Kind: hostapi.SizeKind(b[0] % 4), N: int(b[1])}, want: int(b[1]), min: int(b[2] % 40)}
			if b[3] > 0 {
				k.max = k.min + int(b[3]%60)
			}
			kids = append(kids, k)
		}
		out := span(avail, kids)
		avail = max(avail, 0)
		mins, sum, openFill := 0, 0, false
		for _, k := range kids {
			mins += k.min
			openFill = openFill || k.size.Kind == hostapi.SizeFill && k.max == 0
		}
		for i, k := range kids {
			sum += out[i]
			if out[i] < 0 || k.max > 0 && out[i] > k.max || mins <= avail && out[i] < k.min {
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
		{"follow bottom-anchors short content", viewport{viewH: 5, follow: true}, 3, 12, nil, -2, 5, false},
		{"scrolling up leaves the tail", viewport{viewH: 5, follow: true}, 20, 12, func(v *viewport) { v.scroll(-3) }, 12, 5, true},
		{"under 7 cells draws no bar", viewport{viewH: 5}, 20, 6, nil, 0, 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := make([][]hostapi.Segment, c.rows)
			for i := range lines {
				lines[i] = []hostapi.Segment{{Text: fmt.Sprintf("%0*d", c.width, i)}}
			}
			v := c.v
			v.window(lines, c.width) // a frame drew it
			if c.move != nil {
				c.move(&v)
			}
			out := v.window(lines, c.width)
			bar := false
			for i, r := range out {
				if j := v.top + i; j >= 0 && !strings.HasPrefix(plainSegs(r), lines[j][0].Text) {
					t.Fatalf("row %d cut: %q", j, plainSegs(r))
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
		name, rows string // s a sticky row, . any other
		top, want  int
	}{
		{"top of content", "s . . . .", 0, -1},
		{"header at top", ". s . . .", 1, -1},
		{"header close under", "s . . . s .", 2, -1},
		{"header three under", "s . . . . s", 2, 0},
		{"block above", ". s s . . . .", 4, 1},
		{"no header above", ". . . . . . s", 2, -1},
	} {
		var lines [][]hostapi.Segment
		for f := range strings.FieldsSeq(c.rows) {
			lines = append(lines, []hostapi.Segment{{Text: f, Sticky: f == "s"}})
		}
		if got := pinned(lines, c.top); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// BenchmarkPin is the pin's share of a painted frame: nothing for a view at
// the content's top, a scan back for one far below its header, and no
// allocation while the pinned row stays the same.
func BenchmarkPin(b *testing.B) {
	lines := make([][]hostapi.Segment, 2000)
	for i := range lines {
		lines[i] = []hostapi.Segment{{Text: "▌ ", Sticky: i == 0}, {Text: "a row of text"}}
	}
	for _, top := range []int{0, len(lines) - 10} {
		b.Run(strconv.Itoa(top), func(b *testing.B) {
			st := &scrollState{top: top}
			st.pinRow(lines, 80)
			b.ReportAllocs()
			for b.Loop() {
				st.pinRow(lines, 80)
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
			"dimmed rows still join", true,
			[]string{"┌──┐   ", "│ab│   ", "┌─────┐"},
			[]string{"┌──┐   ", "│ab│   ", "├──┴──┐"},
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
			got[i] = plainSegs(r)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
}
