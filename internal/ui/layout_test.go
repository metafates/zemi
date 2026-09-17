package ui

import (
	"slices"
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
