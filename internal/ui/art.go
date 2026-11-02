package ui

import (
	"strconv"

	"github.com/metafates/zemi/internal/hostapi"
)

// The knot of the splash: zemi's whorl knot, baked from docs/whorl-knot.js
// (the torus-knot piece by @bas3line, MIT) on a 28x14 grid, frame 1. Each cell
// keeps the renderer's shade, the 0-31 index the audited ramp turns into the
// theme's colors. Static art: an idle zemi redraws nothing, so it does not
// turn.
type knotRow struct{ glyphs, shades string }

var knotArt = []knotRow{
	{"    #%@@%#+            ", "0000mrtuqoh000000000000"},
	{"  #%##%#@@@%#+ @%#     ", "00nqopqovvuqofvvqo00000"},
	{" #*=::   =*###* #%#%#  ", "0nlc42000bjpomiupqoto00"},
	{"+*=:    %   ***- -#%@@ ", "hid20000qpbjiki809mqvv0"},
	{"++=:  ##*:-  ++-   +###", "efa200ooj5700gg9000gnpm"},
	{" ++ @@*+-:   =-:    #%#", "0gf9uvlga2000b940000nsn"},
	{"   %@#*:     :::-  =@@*", "00gsumi2ng000222600ctvi"},
	{"  #%#-  **%%#     %@%*=", "00prn7sokiqpn0dge0qvrjd"},
	{"  #*=    +#%#@%@%%#%#: ", "00nlc0000fnsousvqppqm60"},
	{"  **==-     *##%**+    ", "00jicb80000ninnrlle0000"},
	{"   ++=++++**           ", "000efdefhhikik500000000"},
	{"      --==-            ", "0000008acc9000000000000"},
}

// knotWidth is the art's widest row, in cells.
var knotWidth = func() int {
	w := 0
	for _, r := range knotArt {
		w = max(w, len([]rune(r.glyphs)))
	}
	return w
}()

// knotRowSegs is one art row as segments, trailing blanks dropped and runs of
// one shade coalesced.
func knotRowSegs(r knotRow) []hostapi.Segment {
	glyphs, shades := []rune(r.glyphs), []rune(r.shades)
	last := len(glyphs)
	for last > 0 && glyphs[last-1] == ' ' {
		last--
	}
	var out []hostapi.Segment
	for at := 0; at < last; {
		j := at
		for j+1 < last && shades[j+1] == shades[at] {
			j++
		}
		out = append(out, hostapi.Segment{
			Text: string(glyphs[at : j+1]),
			HL:   "Knot" + knotShade(shades[at]),
		})
		at = j + 1
	}
	return out
}

// knotShade is a base-32 shade digit as its decimal index.
func knotShade(r rune) string {
	switch {
	case r >= '0' && r <= '9':
		return string(r)
	case r >= 'a' && r <= 'v':
		return strconv.Itoa(int(r-'a') + 10)
	}
	return "0"
}

// knotLogo is the knot, or one "zemi" row where it does not fit.
func knotLogo(room int) [][]hostapi.Segment {
	if room < knotWidth {
		return [][]hostapi.Segment{{{Text: "zemi", HL: "Title"}}}
	}
	out := make([][]hostapi.Segment, 0, len(knotArt))
	for _, r := range knotArt {
		out = append(out, knotRowSegs(r))
	}
	return out
}
