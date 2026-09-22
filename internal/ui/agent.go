// agent.go is the Go-owned agent chrome (DESIGN §6.3, §8): permission
// prompt, working row, and the shared box and list renderers.
// Their chrome uses fixed "Host:*" styles no theme group can restyle, and
// no Lua participates in drawing or answering them.
package ui

import (
	"cmp"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
)

// permAskPrompt is one open permission overlay; queued asks wait in
// permQueue and open when the current one is answered (parallel tool runs
// may ask concurrently; each must be answered exactly once).
type permAskPrompt struct {
	req    hostapi.PermRequest
	answer chan permAnswer
	opened time.Time // keys typed just before it appeared must not answer it
	sel    int       // highlighted choice
	scroll int       // first preview row shown
}

// permGrace swallows keys right after a prompt opens: someone typing a
// message should not approve a command by accident.
const permGrace = 400 * time.Millisecond

// permChoice is one answer of the permission prompt.
type permChoice struct {
	label, key string
	by         string
	ok         bool
	tell       bool // deny and stop the turn so the user can say what to do instead
}

func (pa *permAskPrompt) choices() []permChoice {
	out := []permChoice{{"Yes", "y", "user:once", true, false}}
	if pa.req.Always != "" {
		out = append(out, permChoice{"Yes, and " + pa.req.Always + " for this session", "a", "user:always", true, false})
	}
	if pa.req.Wider != "" {
		out = append(out, permChoice{"Yes, and " + pa.req.Wider + " for this session", "r", "user:wider", true, false})
	}
	return append(out, permChoice{"No", "n", "user", false, false}, permChoice{"No, and tell zemi what to do instead", "t", "user", false, true})
}

// choosePerm answers the prompt with c. Telling zemi what to do also denies
// the queued asks and interrupts the turn; the composer already has focus.
func (a *App) choosePerm(c permChoice) {
	a.answerPerm(c.ok, c.by)
	if !c.tell {
		return
	}
	for a.permAsk != nil {
		a.answerPerm(false, "user")
	}
	if a.host.AgentActive() {
		a.interrupt()
	}
}

// AgentStream implements hostapi.UIHooks (called from the turn goroutine).
// Stream events only invalidate the frame, so one waits in the queue at a
// time: a token per message would crowd out hooks that carry state.
func (a *App) AgentStream() {
	if a.streamPending.Swap(true) {
		return
	}
	if !a.dispatch(agentStreamMsg{}) {
		a.streamPending.Store(false)
	}
}

// TurnDone implements hostapi.UIHooks: the end of a turn always gets in.
func (a *App) TurnDone(err string) { a.dispatch(turnDoneMsg{err: err}) }

// AskPermission implements hostapi.UIHooks. It blocks the caller (a tool
// run on the turn goroutine) until the user answers the overlay; safe from
// any goroutine via the hook channel + buffered answer, mirroring AskTrust.
func (a *App) AskPermission(req hostapi.PermRequest) (bool, string) {
	// Every field carries model input: strip escapes before the chrome.
	req.Subject, req.Arg = sanitize.String(req.Subject), sanitize.String(req.Arg)
	req.Reason, req.Preview = sanitize.String(req.Reason), sanitize.String(req.Preview)
	req.Description, req.Always = sanitize.String(req.Description), sanitize.String(req.Always)
	req.Wider = sanitize.String(req.Wider)
	ch := make(chan permAnswer, 1)
	a.await(func() tea.Cmd {
		p := &permAskPrompt{req: req, answer: ch, opened: a.now()}
		if a.permAsk != nil {
			a.permQueue = append(a.permQueue, p)
		} else {
			a.permAsk = p
		}
		return a.notifyIfAway("zemi needs your approval: " + req.Subject)
	})
	ans := <-ch
	return ans.ok, ans.by
}

func (a *App) answerPerm(ok bool, by string) {
	pa := a.permAsk
	a.permAsk = nil
	if pa != nil && pa.answer != nil {
		pa.answer <- permAnswer{ok: ok, by: by}
	}
	if len(a.permQueue) > 0 {
		a.permAsk, a.permQueue = a.permQueue[0], a.permQueue[1:]
		a.permAsk.opened = a.now()
	}
	a.frameGen++
}

// ---------------------------------------------------------------------------
// chrome

// hostBox draws a chrome box: a titled top border with an optional
// right-aligned badge, body rows padded to one width. The badge (a Lua
// overlay's origin) wins over the title when space runs out.
func hostBox(title, titleHL, badge string, body [][]hostapi.Segment, w int) [][]hostapi.Segment {
	return hostBoxW(title, titleHL, badge, body, w, 100)
}

// boxInner is the widest row a hostBox on a w-cell screen shows whole.
func boxInner(w int) int { return max(min(w-4, 100)-4, 1) }

// promptBox is the frame every Go-owned prompt draws in (permission, trust,
// question): the body, then a blank row and the hint line when there is one
// (the trust prompt's answers carry their keys).
func promptBox(title, badge string, body [][]hostapi.Segment, hint []hostapi.Segment, w, maxW int) [][]hostapi.Segment {
	if len(hint) > 0 {
		body = append(body, nil, hint)
	}
	return hostBoxW(" "+title+" ", "Host:Title", badge, body, w, maxW)
}

// wrapText wraps text to w cells. It splits the line breaks text already
// holds: a newline measures as no cell, so wrapping without splitting leaves
// one long row that spills past the box.
func wrapText(text, hl string, w int) [][]hostapi.Segment {
	var rows [][]hostapi.Segment
	for l := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		rows = append(rows, hostapi.WrapSegs([]hostapi.Segment{{Text: l, HL: hl}}, w)...)
	}
	return rows
}

// promptChoice is one answer of a Go-owned prompt.
type promptChoice struct{ key, label, desc, hl string }

// choiceRows draws a prompt's answers as buttons: "[y] Trust", the key right
// next to what it does, the selected one marked like a selected list row. Every row carries the
// choice's 1-based Hit, so a click answers it. sel of -1 selects none.
func choiceRows(choices []promptChoice, sel, w int) [][]hostapi.Segment {
	kw := 0
	for _, c := range choices {
		kw = max(kw, ansi.StringWidth(c.key))
	}
	var rows [][]hostapi.Segment
	for i, c := range choices {
		pad := strings.Repeat(" ", kw-ansi.StringWidth(c.key)+1)
		btn, textHL := []hostapi.Segment{
			{Text: "  [", HL: "Host:Hint"}, {Text: c.key, HL: "Host:HintKey"}, {Text: "]" + pad, HL: "Host:Hint"},
		}, "Normal"
		if i == sel { // marked like a selected list row
			btn, textHL = []hostapi.Segment{{Text: "› [" + c.key + "]" + pad, HL: "Host:Title"}}, "Host:Title"
		}
		line := []hostapi.Segment{{Text: c.label, HL: cmp.Or(c.hl, textHL)}}
		if c.desc != "" {
			line = append(line, hostapi.Segment{Text: "  " + c.desc, HL: "Host:Hint"})
		}
		// a label wraps instead of ending in "…": the user answers what it says
		for j, r := range hostapi.WrapSegs(line, max(w-kw-5, 8)) {
			row := slices.Concat(btn, r)
			if j > 0 {
				row = append([]hostapi.Segment{{Text: strings.Repeat(" ", kw+5)}}, r...)
			}
			for k := range row {
				row[k].Hit = i + 1
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// hostBoxW is hostBox with its own width cap.
func hostBoxW(title, titleHL, badge string, body [][]hostapi.Segment, w, maxW int) [][]hostapi.Segment {
	bw := ansi.StringWidth(title) + ansi.StringWidth(badge) + 4
	for _, r := range body {
		bw = max(bw, segsWidth(r)+4)
	}
	// wide boxes read badly; long rows end in an ellipsis instead
	bw = max(min(bw, w-4, maxW), min(12, max(w, 4)))
	badge = ansi.Truncate(badge, bw-2, "…")
	title = ansi.Truncate(title, bw-2-ansi.StringWidth(badge), "…")
	fill := bw - 2 - ansi.StringWidth(title) - ansi.StringWidth(badge)
	box := [][]hostapi.Segment{{
		{Text: "┌", HL: "Host:Rule", NoCopy: true},
		{Text: title, HL: titleHL},
		{Text: strings.Repeat("─", fill), HL: "Host:Rule", NoCopy: true},
		{Text: badge, HL: "Host:Hint"},
		{Text: "┐", HL: "Host:Rule", NoCopy: true},
	}}
	for _, r := range body {
		line := ellipsizeSegs(r, bw-4)
		pad := bw - 4 - segsWidth(line)
		row := []hostapi.Segment{{Text: "│ ", HL: "Host:Rule", NoCopy: true}}
		row = append(row, line...)
		row = append(row, hostapi.Segment{Text: strings.Repeat(" ", max(0, pad)), NoCopy: true}, hostapi.Segment{Text: " │", HL: "Host:Rule", NoCopy: true})
		box = append(box, row)
	}
	box = append(box, []hostapi.Segment{{Text: "└" + strings.Repeat("─", bw-2) + "┘", HL: "Host:Rule", NoCopy: true}})
	return box
}

// ellipsizeSegs cuts a row to w cells ending in "…", at a word boundary
// when one is close. Trailing blanks are padding and cut without one.
func ellipsizeSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	if w < 2 || ansi.StringWidth(strings.TrimRight(plainSegs(segs), " ")) <= w {
		return truncateSegs(segs, w)
	}
	out := truncateSegs(segs, w-1)
	if n := len(out); n > 0 {
		last := out[n-1]
		if i := strings.LastIndex(last.Text, " "); i > 0 && ansi.StringWidth(last.Text[i:]) <= 12 {
			last.Text = strings.TrimRight(last.Text[:i], " ,;:-")
		}
		dots := last
		dots.Text = "…"
		out = append(out[:n-1:n-1], last, dots)
	}
	return out
}

// listItem is one selectable entry of a list.
type listItem struct {
	label, desc string
	key         string // right-aligned column, in Host:HintKey
	section     string // a change of section starts with a header row
	more        [][]hostapi.Segment
	segs        []hostapi.Segment // the label styled; label keeps its plain text
}

// listRows is the one list renderer (palette, completion, pickers,
// overlays, memory, questions). The selected item gets "› " and bold text
// on all its rows; descriptions stay dim. more rows, wrapped by the caller,
// follow the label (or replace it when the label is empty). Every row of an
// item carries its 1-based Hit. Past limit rows the list scrolls with an
// "i/n" counter; top is the first row shown, kept across frames so the list
// moves only when the selection leaves it and a click never moves the item
// under the pointer (nil for a list that never scrolls).
func listRows(items []listItem, sel int, top *int, limit int) [][]hostapi.Segment {
	return listRowsW(items, items, sel, top, limit, 0, 0)
}

// listRowsW is listRows padded to at least minW cells and, when maxW is
// set, cut to at most maxW: a long label gives way to its description, and
// a key column always shows. all is the unfiltered list, which sets the
// description column, so filtering never moves it.
func listRowsW(items, all []listItem, sel int, top *int, limit, minW, maxW int) [][]hostapi.Segment {
	labelW := 0
	for _, it := range all {
		if it.desc != "" { // a row with no description needs no column
			labelW = max(labelW, ansi.StringWidth(it.label))
		}
	}
	// descriptions line up in one column unless a long label would push them
	// far out: as far as the widest row leaves room, 24 cells when unbounded
	column := 24
	if maxW > 0 {
		right := 0
		for _, it := range all {
			right = max(right, ansi.StringWidth(it.desc)+map[bool]int{true: 2 + ansi.StringWidth(it.key)}[it.key != ""])
		}
		column = max(column, maxW-4-right)
	}
	labelW = min(labelW, column)
	var rows [][]hostapi.Segment
	keys := map[int]hostapi.Segment{} // row -> key column
	section, selTop, selEnd := "", 0, 0
	cut, starts := map[int]bool{}, map[int]bool{} // rows a scrolled window may end after, and start at
	for i, it := range items {
		if len(rows) > 0 && (it.section != section || len(it.more) > 0 || len(items[i-1].more) > 0) {
			rows = append(rows, nil)
		}
		start := len(rows)
		starts[start] = true
		if it.section != section && it.section != "" {
			rows = append(rows, []hostapi.Segment{{Text: it.section, HL: "Host:Title"}})
		}
		section = it.section
		hit, hl, marker := i+1, "Normal", hostapi.Segment{Text: "  "}
		if i == sel {
			hl, marker = "Host:Title", hostapi.Segment{Text: "› ", HL: "Host:Title"}
		}
		// segments without hl take the row's, and inline colors bold with it
		styled := func(r []hostapi.Segment) []hostapi.Segment {
			row := make([]hostapi.Segment, 0, len(r))
			for _, s := range r {
				switch {
				case s.HL == "":
					s.HL = hl
				case i == sel && strings.HasPrefix(s.HL, hostapi.SGRPrefix):
					s.HL += ";1" // markdown text is colored inline: bold it the same way
				}
				row = append(row, s)
			}
			return row
		}
		var itemRows [][]hostapi.Segment
		if it.label != "" || len(it.more) == 0 {
			label, cutW := it.label, -1
			if maxW > 0 {
				// the marker and the gap take 2 cells each, a key 2 more
				room := maxW - 4 - ansi.StringWidth(it.desc) - map[bool]int{true: 2 + ansi.StringWidth(it.key)}[it.key != ""]
				cutW = max(room, labelW)
				if it.desc != "" && ansi.StringWidth(label) > room {
					cutW = labelW // a cut label keeps its description in the column
				}
				label = ansi.Truncate(label, cutW, "…")
			}
			row := []hostapi.Segment{{Text: label, HL: hl}}
			if it.segs != nil {
				row = styled(it.segs)
				if cutW >= 0 {
					row = ellipsizeSegs(row, cutW)
				}
			}
			if it.desc != "" {
				gap := strings.Repeat(" ", max(0, labelW-segsWidth(row))+2)
				row = append(row, hostapi.Segment{Text: gap + it.desc, HL: "Comment"})
			}
			if it.key != "" {
				keys[len(rows)] = hostapi.Segment{Text: it.key, HL: "Host:HintKey", Hit: hit}
			}
			itemRows = append(itemRows, row)
		}
		for _, r := range it.more {
			itemRows = append(itemRows, styled(r))
		}
		for j, r := range itemRows {
			g := hostapi.Segment{Text: "  "}
			if j == 0 {
				g = marker
			}
			row := append([]hostapi.Segment{g}, r...)
			for k := range row {
				row[k].Hit = hit
			}
			rows = append(rows, row)
		}
		cut[len(rows)-1] = true
		if i == sel {
			selTop, selEnd = start, len(rows)
		}
	}
	// Every row pads to the widest of the whole list, so a box sized from
	// the visible window keeps its width while scrolling.
	widest := minW
	if maxW > 0 { // a width remembered from a wider screen
		widest = min(minW, maxW)
	}
	for y, r := range rows {
		k, ok := keys[y]
		kw := map[bool]int{true: 2 + ansi.StringWidth(k.Text)}[ok]
		if maxW > 0 && segsWidth(r)+kw > maxW {
			r = ellipsizeSegs(r, maxW-kw)
			rows[y] = r
		}
		widest = max(widest, segsWidth(r)+kw)
	}
	for y, r := range rows {
		if k, ok := keys[y]; ok {
			pad := hostapi.Segment{Text: strings.Repeat(" ", widest-segsWidth(r)-ansi.StringWidth(k.Text)), Hit: k.Hit}
			rows[y] = append(r, pad, k)
			continue
		}
		rows[y] = append(r, hostapi.Segment{Text: strings.Repeat(" ", widest-segsWidth(r))})
	}
	if limit = max(limit, 1); len(rows) > limit {
		if top == nil {
			top = new(int)
		}
		*top = max(min(*top, selTop, len(rows)-limit), min(selEnd-limit, selTop), 0)
		for *top < selTop && !starts[*top] {
			*top++
		}
		// start and end the window on whole items, so no item shows cut, no
		// blank separator shows at an edge and no header shows without its
		// first item; an item taller than the window is cut anyway
		end := *top + limit
		for end > *top+1 && !cut[end-1] {
			end--
		}
		if !cut[end-1] {
			end = *top + limit
		}
		win := slices.Clone(rows[*top:end])
		for len(win) < limit {
			win = append(win, padSegs(nil, widest))
		}
		rows = append(win, []hostapi.Segment{{Text: fmt.Sprintf("  %d/%d", sel+1, len(items)), HL: "Comment"}})
	}
	return rows
}

// shimmer is the "shimmer" segment effect: a bright band sweeps across the
// text every 2s, each rune blending the segment's color toward Normal by
// its distance to the band center. Blends are quantized to 8 steps so the
// sgr style cache stays small.
func (a *App) shimmer(seg hostapi.Segment, now time.Time, animate bool) []hostapi.Segment {
	dim, fg := a.thm.style(seg.HL).Fg, a.thm.style("Normal").Fg
	if !animate || dim == nil || fg == nil {
		return []hostapi.Segment{{Text: seg.Text, HL: seg.HL}}
	}
	const band = 3.0
	runes := []rune(seg.Text)
	pos := float64(now.UnixMilli()%2000)/2000*(float64(len(runes))+2*band) - band
	dr, dg, db, _ := dim.RGBA()
	fr, fgn, fb, _ := fg.RGBA()
	out := make([]hostapi.Segment, 0, len(runes))
	for i, r := range runes {
		k := 0.0
		if d := math.Abs(float64(i) - pos); d < band {
			k = math.Round((1+math.Cos(math.Pi*d/band))/2*8) / 8
		}
		mix := func(x, y uint32) int { return int((float64(x)*(1-k) + float64(y)*k) / 257) }
		hl := fmt.Sprintf("%s38;2;%d;%d;%d", hostapi.SGRPrefix, mix(dr, fr), mix(dg, fgn), mix(db, fb))
		out = append(out, hostapi.Segment{Text: string(r), HL: hl})
	}
	return out
}

// permBox draws the permission prompt: what the tool wants to do, the
// command or the diff it would apply, the model's own words and the choices.
func (a *App) permBox(pa *permAskPrompt, w, chatH int) [][]hostapi.Segment {
	req := pa.req
	isDiff := strings.HasPrefix(req.Preview, "--- ")
	maxW := 96
	if isDiff {
		maxW = 120
	}
	inner := max(min(w-4, maxW)-4, 1)
	blank := []hostapi.Segment{{Text: ""}}

	title, target := permTitle(req, a.host.Dirs.ProjectDir)
	// a command that escapes the sandbox gets a full-width warning band above
	// everything, in red on the panel, so it cannot be missed
	var banner [][]hostapi.Segment
	if req.Outside {
		for _, r := range wrapText("Runs outside the sandbox - this command can reach the whole machine, unconfined", "Host:DangerPanel", inner) {
			banner = append(banner, r)
		}
	}
	var head [][]hostapi.Segment
	if target != "" {
		head = append(head, ellipsizeSegs([]hostapi.Segment{{Text: target, HL: "Host:Value"}}, inner))
	}
	var content [][]hostapi.Segment
	switch {
	case req.Subject == "tool.bash":
		content = codePanel(req.Arg, inner)
	case isDiff:
		content = diffRows(req.Preview, inner)
	case req.Preview != "":
		content = codePanel(req.Preview, inner)
	}
	// the model's words sit apart from the chrome, quoted, never in the
	// title, and a few rows at most so they cannot push the command away
	var words [][]hostapi.Segment
	if req.Description != "" {
		words = append(words, capRows(wrapText(`"`+req.Description+`"`, "Host:Hint", inner), 3, inner)...)
	}
	// the banner carries the sandbox escape, so the dim reason note is skipped
	if note, hl := permNote(req.Reason); note != "" && !req.Outside {
		words = append(words, capRows(wrapText(note, hl, inner), 3, inner)...)
	}
	choices := pa.choices()
	btns := make([]promptChoice, len(choices))
	for i, c := range choices {
		btns[i] = promptChoice{key: c.key, label: c.label}
	}
	list := choiceRows(btns, pa.sel, inner)

	// the choices and the hint always fit: borders, the blank rows around
	// the list and the hint come first, then a line of the content, then
	// the words, then the rest of the content
	fixed := len(head) + len(list) + 5
	if len(head) > 0 && len(content) > 0 {
		fixed++ // the blank row between them
	}
	if len(banner) > 0 {
		fixed += len(banner) + 1 // the sandbox band and the blank row after it
	}
	words = capRows(words, chatH-fixed-min(len(content), 2)-1, inner)
	tail := [][]hostapi.Segment{blank}
	if len(words) > 0 {
		tail = append(append(tail, words...), blank)
	}
	hints := []string{"enter", "confirm"}
	if room := chatH - fixed - len(tail) + 1; len(content) > room {
		room-- // the row that says which lines show
		if room < 1 {
			content = nil // nothing left to show it in: the choices come first
		} else {
			pa.scroll = max(0, min(pa.scroll, len(content)-room))
			more := fmt.Sprintf("  lines %d-%d of %d", pa.scroll+1, pa.scroll+room, len(content))
			content = append(content[pa.scroll:pa.scroll+room], []hostapi.Segment{{Text: more, HL: "Host:Hint"}})
			hints = append(hints, "pgup/pgdn", "scroll")
		}
	}
	hint := hintLine(inner, append(hints, "esc", "deny")...)
	tail = append(tail, list...)
	// panels span the box, and the box fits its content (at least 56 wide):
	// a padded panel counts by its text, other rows whole as hostBoxW counts them
	fit := 56
	for _, r := range content {
		fit = max(fit, ansi.StringWidth(strings.TrimRight(plainSegs(r), " ")))
	}
	for _, r := range slices.Concat(banner, head, tail, [][]hostapi.Segment{hint}) {
		fit = max(fit, segsWidth(r))
	}
	fit = min(fit, inner)
	for i := range content {
		content[i] = padPanel(content[i], fit)
	}
	for i := range banner {
		banner[i] = padPanel(banner[i], fit)
	}
	body := banner
	if len(body) > 0 {
		body = append(body, blank)
	}
	body = append(body, head...)
	if len(head) > 0 && len(content) > 0 {
		body = append(body, blank)
	}
	body = append(body, content...)
	body = append(body, tail...)

	badge := ""
	if n := len(a.permQueue); n > 0 {
		badge = fmt.Sprintf(" 1 of %d ", n+1)
	}
	return promptBox(title, badge, body, hint, w, maxW)
}

// capRows keeps the first n of wrapped rows, ending the last kept one in
// "…" when rows were dropped.
func capRows(rows [][]hostapi.Segment, n, w int) [][]hostapi.Segment {
	if n <= 0 {
		return nil
	}
	if len(rows) <= n {
		return rows
	}
	last := truncateSegs(rows[n-1], w-1)
	if k := len(last); k > 0 {
		end := last[k-1]
		end.Text = strings.TrimRight(end.Text, " ") + "…"
		last = append(last[:k-1:k-1], end)
	}
	return append(rows[:n-1:n-1], last)
}

// padPanel cuts or extends a panel row to width, keeping its last style.
func padPanel(r []hostapi.Segment, width int) []hostapi.Segment {
	r = truncateSegs(r, width)
	if n := len(r); n > 0 && segsWidth(r) < width {
		last := r[n-1]
		last.Text += strings.Repeat(" ", width-segsWidth(r))
		r = append(r[:n-1:n-1], last)
	}
	return r
}

// permTitle asks what the request would do, and names what it acts on.
func permTitle(req hostapi.PermRequest, root string) (title, target string) {
	title, target = "Allow "+req.Subject+"?", req.Arg
	switch {
	case req.Subject == "tool.bash":
		if req.Outside {
			return "Run this command outside the sandbox?", ""
		}
		return "Run this command?", ""
	case req.Tool == "write":
		title = "Write this file?"
	case req.Tool == "edit" || req.Tool == "multiedit":
		title = "Edit this file?"
	case req.Subject == "fs.write":
		title = "Change this file?"
	case req.Subject == "fs.read":
		title = "Read this file?"
	case strings.HasPrefix(req.Subject, "net."):
		title = "Fetch this URL?"
	case strings.HasPrefix(req.Subject, "mcp."):
		title = "Use a " + strings.TrimPrefix(req.Subject, "mcp.") + " tool?"
	case req.Subject == "tool.workflow":
		title = "Run this workflow?"
	case req.Subject == "tool.lua":
		title = "Run Lua inside zemi?"
	case req.Subject == "memory.write":
		title = "Change a memory?"
	}
	if target == "script" { // an inline script, shown below
		target = ""
	}
	if rel, ok := perm.Under(root, target); ok && root != "" && filepath.IsAbs(target) {
		target = rel
	}
	return title, target
}

// permNote turns a request reason into a line worth reading: generic tool
// reasons say nothing the title does not, subagent and auto-mode reasons do.
func permNote(reason string) (string, string) {
	switch {
	case reason == "":
		return "", ""
	case strings.HasPrefix(reason, "auto mode"):
		return reason, "Host:Warn"
	case strings.HasPrefix(reason, "agent "):
		who, _, _ := strings.Cut(strings.TrimPrefix(reason, "agent "), ":")
		return "asked by the " + who + " agent", "Host:Hint"
	}
	return reason, "Host:Hint"
}

// hintLine is a footer of key/label pairs: "enter confirm · esc deny". The
// pairs before the last give way, nearest to it first, until the line fits
// w cells, so the last one (esc) always shows.
func hintLine(w int, pairs ...string) []hostapi.Segment {
	var out []hostapi.Segment
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			out = append(out, hostapi.Segment{Text: " · ", HL: "Host:Hint"})
		}
		out = append(out, hostapi.Segment{Text: pairs[i], HL: "Host:HintKey"}, hostapi.Segment{Text: " " + pairs[i+1], HL: "Host:Hint"})
	}
	if n := len(pairs); n >= 4 && segsWidth(out) > w {
		return hintLine(w, append(pairs[:n-4:n-4], pairs[n-2:]...)...)
	}
	return out
}

// codePanel shows text on a panel background, wrapped to width.
func codePanel(text string, width int) [][]hostapi.Segment {
	var rows [][]hostapi.Segment
	for l := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		l = strings.ReplaceAll(l, "\t", "    ")
		for _, wrapped := range hostapi.WrapSegs([]hostapi.Segment{{Text: l}}, width-2) {
			line := plainSegs(wrapped)
			pad := width - 2 - ansi.StringWidth(line)
			rows = append(rows, []hostapi.Segment{{Text: " " + line + strings.Repeat(" ", max(0, pad)) + " ", HL: "Host:Panel"}})
		}
	}
	return rows
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)`)

// diffRows renders a unified diff with old and new line numbers, added and
// removed lines on tinted rows, and a gap marker between hunks.
func diffRows(d string, width int) [][]hostapi.Segment {
	type line struct {
		kind     byte
		old, new int
		text     string
	}
	var lines []line
	o, n := 0, 0
	for l := range strings.SplitSeq(d, "\n") {
		switch {
		case l == "", strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "+++ "), strings.HasPrefix(l, "\\ "):
		case strings.HasPrefix(l, "@@"):
			if m := hunkHeader.FindStringSubmatch(l); m != nil {
				o, _ = strconv.Atoi(m[1])
				n, _ = strconv.Atoi(m[2])
			}
			if len(lines) > 0 {
				lines = append(lines, line{kind: '~'})
			}
		case l[0] == '+':
			lines = append(lines, line{'+', 0, n, l[1:]})
			n++
		case l[0] == '-':
			lines = append(lines, line{'-', o, 0, l[1:]})
			o++
		default:
			lines = append(lines, line{' ', o, n, strings.TrimPrefix(l, " ")})
			o++
			n++
		}
	}
	nw := len(strconv.Itoa(max(o, n)))
	num := func(v int) string {
		if v == 0 {
			return strings.Repeat(" ", nw)
		}
		return fmt.Sprintf("%*d", nw, v)
	}
	var rows [][]hostapi.Segment
	for _, l := range lines {
		if l.kind == '~' {
			rows = append(rows, []hostapi.Segment{{Text: strings.Repeat(" ", 2*nw+3) + "⋯", HL: "Host:Hint"}})
			continue
		}
		// the sign carries the terminal's own green or red, the row a tint
		signHL, lineHL, sign := "Host:Hint", "", "  "
		switch l.kind {
		case '+':
			signHL, lineHL, sign = "Host:DiffAddSign", "Host:DiffAdd", "+ "
		case '-':
			signHL, lineHL, sign = "Host:DiffDelSign", "Host:DiffDel", "- "
		}
		gutter := " " + num(l.old) + " " + num(l.new) + " "
		room := max(width-ansi.StringWidth(gutter)-2, 1)
		// wrapped, never cut: the user approves every character
		for i, wrapped := range hostapi.WrapSegs([]hostapi.Segment{{Text: l.text}}, room) {
			text := plainSegs(wrapped)
			if i > 0 {
				gutter, sign = strings.Repeat(" ", len(gutter)), "  "
			}
			rows = append(rows, []hostapi.Segment{
				{Text: gutter, HL: "Host:Hint"},
				{Text: sign, HL: signHL},
				{Text: text + strings.Repeat(" ", max(0, room-ansi.StringWidth(text))), HL: lineHL},
			})
		}
	}
	return rows
}
