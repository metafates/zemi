// agent.go is the Go-owned agent chrome (DESIGN §6.3, §8): permission
// prompt, model picker, working row, and the shared box and list renderers.
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

type modelPickPrompt struct {
	items   []hostapi.ModelOption
	current string
	texts   []string // filter text per item: id and display name
	answer  chan modelPick
	listFilter
}

// AgentEvent implements hostapi.UIHooks (called from the turn goroutine).
// Stream events only invalidate the frame, so one waits in the queue at a
// time: a token per message would crowd out hooks that carry state.
func (a *App) AgentEvent(ev hostapi.AgentEvent) {
	if ev.Kind != "turn_done" && a.streamPending.Swap(true) {
		return
	}
	if !a.dispatch(agentEventMsg{ev: ev}) && ev.Kind != "turn_done" {
		a.streamPending.Store(false)
	}
}

// AskPermission implements hostapi.UIHooks. It blocks the caller (a tool
// run on the turn goroutine) until the user answers the overlay; safe from
// any goroutine via the hook channel + buffered answer, mirroring AskTrust.
func (a *App) AskPermission(req hostapi.PermRequest) (bool, string) {
	// Every field carries model input: strip escapes before the chrome.
	req.Subject, req.Arg = sanitize.String(req.Subject), sanitize.String(req.Arg)
	req.Reason, req.Preview = sanitize.String(req.Reason), sanitize.String(req.Preview)
	req.Description, req.Always = sanitize.String(req.Description), sanitize.String(req.Always)
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

// PickModel implements hostapi.UIHooks: blocks until the user picks a
// model or cancels (ok=false on cancel).
func (a *App) PickModel(items []hostapi.ModelOption, current string) (string, bool) {
	ch := make(chan modelPick, 1)
	a.await(func() tea.Cmd { a.openModelPick(items, current, ch); return nil })
	res := <-ch
	return res.id, res.ok
}

func (a *App) openModelPick(items []hostapi.ModelOption, current string, answer chan modelPick) {
	mp := &modelPickPrompt{items: items, current: current, answer: answer}
	mp.selected = max(0, slices.IndexFunc(items, func(it hostapi.ModelOption) bool { return it.ID == current }))
	for _, it := range items {
		mp.texts = append(mp.texts, it.ID+" "+it.Display)
	}
	mp.apply(mp.texts)
	a.modelPick = mp
}

func (a *App) answerPick(id string, ok bool) {
	mp := a.modelPick
	a.modelPick = nil
	if mp != nil && mp.answer != nil {
		mp.answer <- modelPick{id: id, ok: ok}
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
		{Text: "╭"},
		{Text: title, HL: titleHL},
		{Text: strings.Repeat("─", fill)},
		{Text: badge, HL: "Comment"},
		{Text: "╮"},
	}}
	for _, r := range body {
		line := ellipsizeSegs(r, bw-4)
		pad := bw - 4 - segsWidth(line)
		row := []hostapi.Segment{{Text: "│ "}}
		row = append(row, line...)
		row = append(row, hostapi.Segment{Text: strings.Repeat(" ", max(0, pad)) + " │"})
		box = append(box, row)
	}
	box = append(box, []hostapi.Segment{{Text: "╰" + strings.Repeat("─", bw-2) + "╯"}})
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
		out = append(out[:n-1:n-1], last, hostapi.Segment{Text: "…", HL: last.HL, Hit: last.Hit})
	}
	return out
}

// listItem is one selectable entry of a list.
type listItem struct {
	label, desc string
	key, keyHL  string // right-aligned column; keyHL defaults to Host:HintKey
	section     string // a change of section starts with a header row
	more        [][]hostapi.Segment
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
	labelW := 0
	for _, it := range items {
		labelW = max(labelW, ansi.StringWidth(it.label))
	}
	// descriptions line up in one column unless a long label would push them far out
	labelW = min(labelW, 24)
	var rows [][]hostapi.Segment
	keys := map[int]hostapi.Segment{} // row -> key column
	section, selTop, selEnd := "", 0, 0
	for i, it := range items {
		if len(rows) > 0 && (it.section != section || len(it.more) > 0 || len(items[i-1].more) > 0) {
			rows = append(rows, nil)
		}
		start := len(rows)
		if it.section != section && it.section != "" {
			rows = append(rows, []hostapi.Segment{{Text: it.section, HL: "Host:Title"}})
		}
		section = it.section
		hit, hl, marker := i+1, "Normal", hostapi.Segment{Text: "  "}
		if i == sel {
			hl, marker = "Host:Bold", hostapi.Segment{Text: "› ", HL: "Host:Title"}
		}
		var itemRows [][]hostapi.Segment
		if it.label != "" || len(it.more) == 0 {
			row := []hostapi.Segment{{Text: it.label, HL: hl}}
			if it.desc != "" {
				gap := strings.Repeat(" ", max(0, labelW-ansi.StringWidth(it.label))+2)
				row = append(row, hostapi.Segment{Text: gap + it.desc, HL: "Comment"})
			}
			if it.key != "" {
				keys[len(rows)] = hostapi.Segment{Text: it.key, HL: cmp.Or(it.keyHL, "Host:HintKey"), Hit: hit}
			}
			itemRows = append(itemRows, row)
		}
		for _, r := range it.more {
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
			itemRows = append(itemRows, row)
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
		if i == sel {
			selTop, selEnd = start, len(rows)
		}
	}
	// Every row pads to the widest of the whole list, so a box sized from
	// the visible window keeps its width while scrolling.
	widest := 0
	for y, r := range rows {
		n := segsWidth(r)
		if k, ok := keys[y]; ok {
			n += 2 + ansi.StringWidth(k.Text)
		}
		widest = max(widest, n)
	}
	for y, r := range rows {
		if k, ok := keys[y]; ok {
			pad := hostapi.Segment{Text: strings.Repeat(" ", widest-segsWidth(r)-ansi.StringWidth(k.Text)), Hit: k.Hit}
			rows[y] = append(r, pad, k)
			continue
		}
		rows[y] = padSegs(r, widest)
	}
	if limit = max(limit, 1); len(rows) > limit {
		if top == nil {
			top = new(int)
		}
		*top = max(min(*top, selTop, len(rows)-limit), min(selEnd-limit, selTop), 0)
		rows = append(rows[*top:*top+limit:*top+limit], []hostapi.Segment{{Text: fmt.Sprintf("  %d/%d", sel+1, len(items)), HL: "Comment"}})
	}
	return rows
}

// alignKeys moves the key column of listRows rows out to width cells, the
// right edge of the box they go in.
func alignKeys(rows [][]hostapi.Segment, width int) [][]hostapi.Segment {
	for _, r := range rows {
		if n := len(r); n > 1 && segsWidth(r) < width {
			r[n-2].Text += strings.Repeat(" ", width-segsWidth(r))
		}
	}
	return rows
}

// workingRow is the activity line above the composer while a turn runs:
// a pulsing bullet, a shimmering label naming what the agent is doing,
// elapsed time, the interrupt key and the queue depth. It stays hidden for
// the first 300ms so instant turns do not flash it.
func (a *App) workingRow() []hostapi.Segment {
	st := a.host.AgentStream()
	if st.Elapsed < 300*time.Millisecond && !a.cancelling {
		return nil
	}
	label := "Working"
	for _, t := range st.Tools {
		if t.Status == "run" {
			label = "Running " + t.Name
			if t.Label != "" {
				label = t.Label
			}
		}
	}
	if label == "Working" && st.Reasoning != "" && st.Text == "" {
		label = "Thinking"
	}
	parts := []string{fmtElapsed(st.Elapsed)}
	if st.RetryIn > 0 {
		label = "Retrying"
		parts = []string{fmt.Sprintf("attempt %d", st.Retries+1)}
		if st.RetryWhy != "" {
			parts = append(parts, sanitize.String(st.RetryWhy))
		}
		parts = append(parts, "next try in "+fmtElapsed(st.RetryIn.Round(time.Second)))
	}
	if st.Compacting {
		label = "Compacting"
	}
	if a.cancelling {
		label = "Cancelling"
	}
	if a.editing != "" {
		label = "Editing " + a.editing
		parts = []string{"save and close the editor to continue"}
	}
	// esc answers an open prompt, so the row does not offer it
	waiting := a.permAsk != nil || a.question != nil
	if waiting {
		label = map[bool]string{true: "Waiting for your approval", false: "Waiting for your answer"}[a.permAsk != nil]
	}
	now := a.now()
	animate := a.cfgBool("ui.animations", true)
	bullet := hostapi.Segment{Text: "• ", HL: "Title"}
	if a.pulse() {
		bullet = hostapi.Segment{Text: "◦ ", HL: "Comment"}
	}
	row := append([]hostapi.Segment{bullet}, a.shimmer(label, now, animate)...)
	if !waiting {
		parts = append(parts, map[bool]string{false: "esc to interrupt", true: "ctrl+c quits"}[a.cancelling])
	}
	if n := len(st.Steering) - st.Reports; n > 0 {
		parts = append(parts, fmt.Sprintf("%d queued, up to edit", n))
	}
	return append(row, hostapi.Segment{Text: "  " + strings.Join(parts, " · "), HL: "Comment"})
}

// shimmer sweeps a bright band across text every 2s: each rune blends the
// Comment color toward Normal by its distance to the band center. Blends
// are quantized to 8 steps so the sgr style cache stays small.
func (a *App) shimmer(text string, now time.Time, animate bool) []hostapi.Segment {
	dim, fg := a.thm.style("Comment").Fg, a.thm.style("Normal").Fg
	if !animate || dim == nil || fg == nil {
		return []hostapi.Segment{{Text: text, HL: "Comment"}}
	}
	const band = 3.0
	runes := []rune(text)
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

func fmtElapsed(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %02dm %02ds", s/3600, s/60%60, s%60)
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
	// the model's words sit apart from the chrome, quoted, never in the title
	var words [][]hostapi.Segment
	if req.Description != "" {
		words = append(words, wrapSegs([]hostapi.Segment{{Text: `"` + req.Description + `"`, HL: "Host:Hint"}}, inner)...)
	}
	if note, hl := permNote(req.Reason); note != "" {
		words = append(words, wrapSegs([]hostapi.Segment{{Text: note, HL: hl}}, inner)...)
	}
	choices := pa.choices()
	items := make([]listItem, len(choices))
	for i, c := range choices {
		items[i] = listItem{label: c.label, key: c.key}
	}
	list := listRows(items, pa.sel, nil, len(items))
	if segsWidth(list[0]) > inner {
		// a narrow box keeps the labels, which say what is approved; the keys still work
		for i := range items {
			items[i].key = ""
		}
		list = listRows(items, pa.sel, nil, len(items))
	}
	tail := [][]hostapi.Segment{blank}
	if len(words) > 0 {
		tail = append(append(tail, words...), blank)
	}
	tail = append(append(tail, list...), blank, hintLine("enter", "confirm", "esc", "deny"))

	// borders, a blank row above the content, and the "more" row
	if room := max(chatH-len(head)-len(tail)-4, 3); len(content) > room {
		pa.scroll = max(0, min(pa.scroll, len(content)-room))
		more := fmt.Sprintf("  %d of %d lines · pgdn for more", pa.scroll+room, len(content))
		content = append(content[pa.scroll:pa.scroll+room], []hostapi.Segment{{Text: more, HL: "Host:Hint"}})
	}
	// panels span the box, and the box fits its content (at least 56 wide)
	fit := 56
	for _, r := range slices.Concat(head, content, tail) {
		fit = max(fit, ansi.StringWidth(strings.TrimRight(plainSegs(r), " ")))
	}
	fit = min(fit, inner)
	for i := range content {
		content[i] = padPanel(content[i], fit)
	}
	list = alignKeys(list, fit)
	body := head
	if len(head) > 0 && len(content) > 0 {
		body = append(body, blank)
	}
	body = append(body, content...)
	body = append(body, tail...)

	badge := ""
	if n := len(a.permQueue); n > 0 {
		badge = fmt.Sprintf(" 1 of %d ", n+1)
	}
	return hostBoxW(" "+title+" ", "Host:Title", badge, body, w, maxW)
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
	if rel, err := filepath.Rel(root, target); err == nil && root != "" && filepath.IsAbs(target) && !strings.HasPrefix(rel, "..") {
		target = rel
	}
	return title, target
}

// permNote turns a request reason into a line worth reading: generic tool
// reasons say nothing the title does not, subagent and auto-mode reasons do.
func permNote(reason string) (string, string) {
	switch {
	case reason == "", reason == "run command", reason == "write file", reason == "edit file",
		reason == "multiedit file", reason == "read file", reason == "fetch url", reason == "glob files",
		reason == "search files":
		return "", ""
	case strings.HasPrefix(reason, "auto mode"):
		return reason, "Host:Warn"
	case strings.HasPrefix(reason, "agent "):
		who, _, _ := strings.Cut(strings.TrimPrefix(reason, "agent "), ":")
		return "asked by the " + who + " agent", "Host:Hint"
	}
	return reason, "Host:Hint"
}

// hintLine is a footer of key/label pairs: "←→ choose · enter confirm".
func hintLine(pairs ...string) []hostapi.Segment {
	var out []hostapi.Segment
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			out = append(out, hostapi.Segment{Text: " · ", HL: "Host:Hint"})
		}
		out = append(out, hostapi.Segment{Text: pairs[i], HL: "Host:HintKey"}, hostapi.Segment{Text: " " + pairs[i+1], HL: "Host:Hint"})
	}
	return out
}

// codePanel shows text on a panel background, wrapped to width.
func codePanel(text string, width int) [][]hostapi.Segment {
	var rows [][]hostapi.Segment
	for l := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		l = strings.ReplaceAll(l, "\t", "    ")
		for _, wrapped := range wrapSegs([]hostapi.Segment{{Text: l}}, width-2) {
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
			rows = append(rows, []hostapi.Segment{{Text: strings.Repeat(" ", 2*nw+3) + "⋯", HL: "Host:DiffCtxNum"}})
			continue
		}
		numHL, lineHL, sign := "Host:DiffCtxNum", "Host:DiffCtxLine", "  "
		switch l.kind {
		case '+':
			numHL, lineHL, sign = "Host:DiffAddNum", "Host:DiffAddLine", "+ "
		case '-':
			numHL, lineHL, sign = "Host:DiffDelNum", "Host:DiffDelLine", "- "
		}
		gutter := " " + num(l.old) + " " + num(l.new) + " "
		room := max(width-ansi.StringWidth(gutter)-2, 1)
		// wrapped, never cut: the user approves every character
		for i, wrapped := range wrapSegs([]hostapi.Segment{{Text: strings.ReplaceAll(l.text, "\t", "    ")}}, room) {
			text := plainSegs(wrapped)
			if i > 0 {
				gutter, sign = strings.Repeat(" ", len(gutter)), "  "
			}
			rows = append(rows, []hostapi.Segment{
				{Text: gutter, HL: numHL},
				{Text: sign + text + strings.Repeat(" ", max(0, room-ansi.StringWidth(text))), HL: lineHL},
			})
		}
	}
	return rows
}

func pickBox(mp *modelPickPrompt, w int) [][]hostapi.Segment {
	const limit = 12
	var body [][]hostapi.Segment
	var hints []string
	if len(mp.visible) > 0 {
		hints = []string{"enter", "switch"}
	}
	if len(mp.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "No models yet. Add a provider in init.lua.", HL: "Comment"}})
	} else {
		items := make([]listItem, 0, len(mp.visible))
		for _, i := range mp.visible {
			it := mp.items[i]
			item := listItem{label: it.ID}
			if it.Display != it.ID {
				item.desc = it.Display
			}
			switch {
			case it.ID == mp.current:
				item.key, item.keyHL = "current", "Host:Hint"
			case !it.HasKey:
				item.key, item.keyHL = "no API key", "Host:Warn"
			}
			items = append(items, item)
		}
		body = mp.filterRows(listRows(items, mp.selected, &mp.top, limit))
		if len(mp.items) > limit || mp.active {
			hints = append(hints, "/", "filter")
		}
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, hintLine(append(hints, "esc", "cancel")...))
	return hostBox(" Model ", "Host:Title", "", body, w)
}
