package ui

import (
	"strings"

	"github.com/metafates/zemi/internal/hostapi"
)

// The splash fills the empty chat until the first message: the whorl knot
// centered, a little below the middle, near the composer.

// splashWidget is the empty chat's screen.
func splashWidget() *widget {
	return &widget{
		id:       "splash",
		region:   _regionChat,
		priority: 5,
		onClick:  nil, // the app runs it, since it edits the composer and runs commands
		render: func(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
			if !emptyChat(st) {
				return nil, false
			}
			return splashRows(st), false
		},
	}
}

// splashRows is the knot centered in the region, with the region's last row
// left to the pending work the chat yields to the splash.
func splashRows(st *hostapi.ScreenState) [][]hostapi.Segment {
	width, fit := st.Width, st.Height
	room := max(min(width-4, 72), 1)
	logo := knotLogo(room)
	if len(logo)+1 > fit { // the pending row keeps one below the art
		logo = knotLogo(0)
	}
	w := 0
	for _, r := range logo {
		w = max(w, segsCells(r))
	}
	pad := hostapi.Segment{Text: strings.Repeat(" ", max((width-min(w, room))/2, 0)), NoCopy: true}
	out := make([][]hostapi.Segment, 0, max((fit-len(logo))/2, 0)+len(logo)+1)
	for range max((fit-len(logo))/2, 0) {
		out = append(out, []hostapi.Segment{{Text: ""}})
	}
	for _, r := range logo {
		// clipped so a narrow window never wraps a row
		line := []hostapi.Segment{pad}
		used := 0
		for _, s := range r {
			if used >= room {
				break
			}
			text := clipText(s.Text, room-used)
			s.Text = text
			line = append(line, s)
			used += ansiWidth(text)
		}
		out = append(out, line)
	}
	// the region's last row, where the chat draws its pending row: a block
	// that already fills the region leaves it none
	if fit > 0 && len(out) < fit {
		for len(out) < fit-1 {
			out = append(out, []hostapi.Segment{{Text: ""}})
		}
		out = append(out, splashLastRow(st, room))
	}
	return out
}

// splashLastRow is the row the block ends with: the pending row the chat
// yields to the splash, clipped to room, else a blank one. Kept either way, so
// the content above it never moves as boot work comes and goes.
func splashLastRow(st *hostapi.ScreenState, room int) []hostapi.Segment {
	var pending []hostapi.Segment
	if st.View == nil {
		pending = pendingLine(st)
	}
	if pending == nil {
		return []hostapi.Segment{{Text: ""}}
	}
	var row []hostapi.Segment
	used := 0
	for _, s := range pending {
		if used >= room {
			break
		}
		s.Text = clipText(s.Text, room-used)
		used += ansiWidth(s.Text)
		row = append(row, s)
	}
	return row
}

// ansiWidth is the cells text draws in.
func ansiWidth(text string) int { return hostapi.SegsWidth([]hostapi.Segment{{Text: text}}) }
