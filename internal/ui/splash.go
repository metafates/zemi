package ui

import (
	"regexp"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

// The splash fills the empty chat until the first message.
var _splashTips = []struct{ key, click, text string }{
	{"/btw ", "fill /btw ", " asks a side question the conversation does not keep"},
	{"@", "fill @", " attaches project files, dirs and skills to the prompt"},
	{"/goal ", "fill /goal ", " keeps working until a condition holds"},
	{"/review", "fill /review", " has several agents review your changes"},
	{"alt+o", "", " browses the chat's whole memory, however old"},
}

// splashTip is the discovery tip a first session ends with, one per hour of
// the host's clock, so the row does not change between renders.
func splashTip(now time.Time) int {
	return int(now.Unix()/3600) % len(_splashTips)
}

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

// splashRows is the logo, recent projects and one tip, each group centered in
// the region and the whole a little below its middle, near the composer.
func splashRows(st *hostapi.ScreenState) [][]hostapi.Segment {
	width, fit := st.Width, st.Height
	room := max(min(width-4, 72), 1)
	// where zemi is rarely the pick (~, /, a new dir outside a repository) Go
	// sets projects to switch to
	recent := st.Projects()
	tips := splashFlow(splashTipSegs(st, len(recent) > 0), room, 7)
	tips[0] = append([]hostapi.Segment{{Text: "● Tip  ", HL: "Accent", NoCopy: true}}, tips[0]...)
	last := splashLastRow(st, room)
	version := st.Version

	// a short chat drops the recent rows, then the version, then the art
	logo := knotLogo(room)
	total := func() int {
		n := len(logo) + 1 + len(tips) + 1
		if version != "" {
			n++
		}
		if len(recent) > 0 {
			n += len(recent) + 1
		}
		return n
	}
	if total() > fit {
		recent = nil
	}
	if total() > fit {
		version = ""
	}
	if total() > fit {
		logo = knotLogo(0)
	}
	n := total()

	if version != "" {
		w := 0
		for _, r := range logo {
			w = max(w, segsCells(r))
		}
		logo = append(logo, []hostapi.Segment{
			{Text: strings.Repeat(" ", max(w-ansiWidth(version), 0)), NoCopy: true},
			{Text: version, HL: "Dim"},
		})
	}
	groups := [][][]hostapi.Segment{logo}
	if len(recent) > 0 {
		// titles left, ages right, across one width
		w := min(room, 56)
		var rows [][]hostapi.Segment
		for _, r := range recent {
			ago := ansiWidth(r.Ago)
			label := clipText(tailText(r.Path, w-ago-2), w-ago-2)
			rows = append(rows, []hostapi.Segment{
				{Text: label, HL: "Normal", Click: "projects switch " + r.Root},
				{Text: strings.Repeat(" ", w-ansiWidth(label)-ago), NoCopy: true},
				{Text: r.Ago, HL: "Dim"},
			})
		}
		groups = append(groups, rows)
	}
	groups = append(groups, tips)

	// a little below the middle, near the composer, placed as if the tip were
	// one row, so a tip that wraps moves nothing above it: its second row takes
	// the one left blank under it
	top := (fit-n+len(tips)-1)/2 + 2
	var out [][]hostapi.Segment
	for range max(min(top, fit-n+len(tips)-2), 0) {
		out = append(out, []hostapi.Segment{{Text: ""}})
	}
	for g, rows := range groups {
		if g > 0 {
			out = append(out, []hostapi.Segment{{Text: ""}})
		}
		// each group centered whole, its rows left-aligned to each other
		w := 0
		for _, r := range rows {
			w = max(w, segsCells(r))
		}
		pad := hostapi.Segment{Text: strings.Repeat(" ", max((width-min(w, room))/2, 0)), NoCopy: true}
		for _, r := range rows {
			// clipped so a narrow window never wraps a row
			used, line := 0, []hostapi.Segment{pad}
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
	}
	// the region's last row, where the chat draws its pending row: a block
	// that already fills the region leaves it none
	if fit > 0 && len(out) < fit {
		for len(out) < fit-1 {
			out = append(out, []hostapi.Segment{{Text: ""}})
		}
		out = append(out, last)
	}
	return out
}

// splashTipSegs is the one tip the splash ends with: the first that applies
// of a failed init.lua, no model, an untrusted repository, no instruction
// file, a first session, else how to go on.
func splashTipSegs(st *hostapi.ScreenState, listed bool) []hostapi.Segment {
	key := func(text, click string) hostapi.Segment {
		return hostapi.Segment{Text: text, HL: "SplashKey", Click: click}
	}
	dim := func(text string) hostapi.Segment { return hostapi.Segment{Text: text, HL: "Dim"} }
	tip := _splashTips[splashTip(st.Now)]
	switch {
	case st.InitError != "":
		return []hostapi.Segment{{Text: initAt(st.InitError) + " failed: ", HL: "Danger"}, key("/errors", "errors")}
	case st.Model == "":
		return []hostapi.Segment{dim("No model yet: " + st.ModelHint)}
	case !st.Trusted && st.Trustable:
		return []hostapi.Segment{key("/trust", "trust"), dim(" lets zemi read this project's instructions and run code")}
	default:
		if files, ok := st.Instructions(); ok && len(files) == 0 {
			return []hostapi.Segment{key("/init", "fill /init"), dim(" writes an AGENTS.md, so zemi knows this project")}
		}
		if listed {
			return []hostapi.Segment{key("/projects", "projects"), dim(" switches to one, or type to stay here")}
		}
		return []hostapi.Segment{key(tip.key, tip.click), dim(tip.text)}
	}
}

// initAt is the file and line an init failure names, for a one-line tip.
func initAt(err string) string {
	for f := range strings.FieldsSeq(err) {
		if i := strings.Index(f, ":"); i > 0 && i+1 < len(f) && f[i+1] >= '0' && f[i+1] <= '9' {
			f = strings.TrimSuffix(f, ":")
			if j := strings.LastIndexByte(f, '/'); j >= 0 {
				f = f[j+1:]
			}
			return f
		}
	}
	return "init.lua"
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

// splashFlow flows segments after an indent cells wide into at most two rows
// of room cells, breaking between words; the second row starts at the indent.
func splashFlow(segs []hostapi.Segment, room, indent int) [][]hostapi.Segment {
	rows := [][]hostapi.Segment{nil}
	w := indent
	for _, s := range segs {
		for _, word := range splitWords(s.Text) {
			seg := word
			if w > indent && w+ansiWidth(strings.TrimRight(word, " \t\n")) > room {
				if len(rows) == 2 {
					last := len(rows[1]) - 1
					rows[1][last].Text = strings.TrimRight(rows[1][last].Text, " \t\n") + "…"
					return rows
				}
				rows = append(rows, []hostapi.Segment{{Text: strings.Repeat(" ", indent), NoCopy: true}})
				w = indent
				seg = strings.TrimLeft(word, " \t\n")
			}
			row := &rows[len(rows)-1]
			*row = append(*row, hostapi.Segment{Text: seg, HL: s.HL, Click: s.Click})
			w += ansiWidth(seg)
		}
	}
	return rows
}

// _splashWords is a run of "spaces, a word, spaces", the unit the splash
// wraps.
var _splashWords = regexp.MustCompile(`\s*\S+\s*`)

func splitWords(s string) []string { return _splashWords.FindAllString(s, -1) }

// ansiWidth is the cells text draws in.
func ansiWidth(text string) int { return hostapi.SegsWidth([]hostapi.Segment{{Text: text}}) }
