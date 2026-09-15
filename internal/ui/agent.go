// agent.go is the Go-owned agent chrome (DESIGN §6.3, §8): permission
// prompt, model picker, working row, and the shared box and list renderers.
// Their chrome uses fixed "Host:*" styles no theme group can restyle, and
// no Lua participates in drawing or answering them.
package ui

import (
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

// permChoice is one button of the permission prompt.
type permChoice struct {
	label string
	by    string
	ok    bool
}

func (pa *permAskPrompt) choices() []permChoice {
	out := []permChoice{{"Yes", "user:once", true}}
	if pa.req.Always != "" {
		out = append(out, permChoice{"Yes, " + pa.req.Always, "user:always", true})
	}
	return append(out, permChoice{"No", "user", false})
}

type modelPickPrompt struct {
	items  []hostapi.ModelOption
	texts  []string // filter text per item: id and display name
	answer chan modelPick
	listFilter
}

// AgentEvent implements hostapi.UIHooks (called from the turn goroutine).
func (a *App) AgentEvent(ev hostapi.AgentEvent) {
	a.dispatch(agentEventMsg{ev: ev})
}

// AskPermission implements hostapi.UIHooks. It blocks the caller (a tool
// run on the turn goroutine) until the user answers the overlay; safe from
// any goroutine via the hook channel + buffered answer, mirroring AskTrust.
func (a *App) AskPermission(req hostapi.PermRequest) (bool, string) {
	// Every field carries model input: strip escapes before the chrome.
	req.Subject, req.Arg = sanitize.String(req.Subject), sanitize.String(req.Arg)
	req.Reason, req.Preview = sanitize.String(req.Reason), sanitize.String(req.Preview)
	ch := make(chan permAnswer, 1)
	a.dispatch(uiFunc(func() tea.Cmd {
		p := &permAskPrompt{req: req, answer: ch, opened: a.now()}
		if a.permAsk != nil {
			a.permQueue = append(a.permQueue, p)
		} else {
			a.permAsk = p
		}
		return a.notifyIfAway("zemi needs your approval: " + req.Subject)
	}))
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
	a.do(func() { a.openModelPick(items, current, ch) })
	res := <-ch
	return res.id, res.ok
}

func (a *App) openModelPick(items []hostapi.ModelOption, current string, answer chan modelPick) {
	mp := &modelPickPrompt{items: items, answer: answer}
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

// hostBoxW is hostBox with its own width cap.
func hostBoxW(title, titleHL, badge string, body [][]hostapi.Segment, w, maxW int) [][]hostapi.Segment {
	bw := ansi.StringWidth(title) + ansi.StringWidth(badge) + 4
	for _, r := range body {
		bw = max(bw, segsWidth(r)+4)
	}
	// wide boxes read badly; long rows end in an ellipsis instead
	bw = max(min(bw, w-4, maxW), 12)
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
// when one is close.
func ellipsizeSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	if segsWidth(segs) <= w || w < 2 {
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

type listItem struct{ label, desc string }

// listRows is the one list renderer (palette, completion, pickers,
// overlays): a window of at most limit rows kept around sel, "› " and
// selHL on the selected row, dim descriptions, and an "i/n" counter once
// the list scrolls.
func listRows(items []listItem, sel, limit int, selHL string) [][]hostapi.Segment {
	// Every row pads to the widest item in the whole list, so a box sized
	// from the visible window keeps its width while scrolling.
	widest, labelW := 0, 0
	for _, it := range items {
		labelW = max(labelW, ansi.StringWidth(it.label))
	}
	// descriptions line up in one column unless a long label would push them far out
	labelW = min(labelW, 24)
	itemWidth := func(it listItem) int {
		n := ansi.StringWidth(it.label)
		if it.desc != "" {
			n = max(n, labelW) + 2 + ansi.StringWidth(it.desc)
		}
		return n
	}
	for _, it := range items {
		widest = max(widest, itemWidth(it))
	}
	start := max(0, min(sel-limit/2, len(items)-limit))
	var rows [][]hostapi.Segment
	for i := start; i < len(items) && i < start+limit; i++ {
		marker, hl := "  ", "Normal"
		if i == sel {
			marker, hl = "› ", selHL
		}
		row := []hostapi.Segment{{Text: marker + items[i].label, HL: hl, Hit: i + 1}}
		if items[i].desc != "" {
			gap := strings.Repeat(" ", max(0, labelW-ansi.StringWidth(items[i].label))+2)
			row = append(row, hostapi.Segment{Text: gap + items[i].desc, HL: "Comment", Hit: i + 1})
		}
		if pad := widest - itemWidth(items[i]); pad > 0 {
			row = append(row, hostapi.Segment{Text: strings.Repeat(" ", pad), Hit: i + 1})
		}
		rows = append(rows, row)
	}
	if len(items) > limit {
		rows = append(rows, []hostapi.Segment{{Text: fmt.Sprintf("  %d/%d", sel+1, len(items)), HL: "Comment"}})
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
		}
	}
	if label == "Working" && st.Reasoning != "" && st.Text == "" {
		label = "Thinking"
	}
	timing := fmtElapsed(st.Elapsed)
	if st.RetryIn > 0 {
		label = fmt.Sprintf("Retrying (attempt %d)", st.Retries+1)
		timing = "next try in " + fmtElapsed(st.RetryIn.Round(time.Second))
		if st.RetryWhy != "" {
			timing = sanitize.String(st.RetryWhy) + ", " + timing
		}
	}
	if st.Compacting {
		label = "Compacting"
	}
	if a.cancelling {
		label = "Cancelling"
	}
	now := a.now()
	animate := a.cfgBool("ui.animations", true)
	bullet := hostapi.Segment{Text: "• ", HL: "Title"}
	if a.pulse() {
		bullet = hostapi.Segment{Text: "◦ ", HL: "Comment"}
	}
	row := append([]hostapi.Segment{bullet}, a.shimmer(label, now, animate)...)
	hint := " (" + timing + " · esc to interrupt)"
	if a.cancelling {
		hint = " (" + timing + " · ctrl+c quits)"
	}
	if n := len(st.Steering); n > 0 {
		hint += fmt.Sprintf(" · %d queued, up to edit", n)
	}
	return append(row, hostapi.Segment{Text: hint, HL: "Comment"})
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

// keyHints is the footer of every Go-drawn box: "enter select · esc close".
func keyHints(pairs ...string) []hostapi.Segment { return hintLine(pairs...) }

// splicePerm draws the permission prompt: what the tool wants to do, the
// command or the diff it would apply, and the choices.
func (a *App) splicePerm(rows [][]hostapi.Segment, pa *permAskPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	req := pa.req
	isDiff := strings.HasPrefix(req.Preview, "--- ")
	maxW := 96
	if isDiff {
		maxW = 120
	}
	inner := max(min(w-4, maxW)-4, 20)
	blank := []hostapi.Segment{{Text: ""}}

	verb, target := permTitle(req, a.host.Dirs.ProjectDir)
	head := []hostapi.Segment{{Text: verb, HL: "Host:Bold"}}
	if target != "" {
		head = append(head, hostapi.Segment{Text: "  " + target, HL: "Host:Value"})
	}
	body := [][]hostapi.Segment{ellipsizeSegs(head, inner)}
	if note, hl := permNote(req.Reason); note != "" {
		body = append(body, ellipsizeSegs([]hostapi.Segment{{Text: note, HL: hl}}, inner))
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
	if room := max(chatH-11, 3); len(content) > room {
		pa.scroll = max(0, min(pa.scroll, len(content)-room))
		more := fmt.Sprintf("  %d of %d lines · j/k scroll", pa.scroll+room, len(content))
		content = append(content[pa.scroll:pa.scroll+room], []hostapi.Segment{{Text: more, HL: "Host:Hint"}})
	}
	// panels span the box, and the box fits its content (at least 56 wide)
	fit := 56
	for _, r := range append(append([][]hostapi.Segment{}, body...), content...) {
		fit = max(fit, trimmedWidth(r))
	}
	fit = min(max(fit, segsWidth(permButtons(pa, inner))), inner)
	for i := range content {
		content[i] = padPanel(content[i], fit)
	}
	if len(content) > 0 {
		body = append(body, blank)
		body = append(body, content...)
	}
	body = append(body, blank, permButtons(pa, fit), blank,
		hintLine("←→", "choose", "enter", "confirm", "esc", "deny"))

	badge := ""
	if n := len(a.permQueue); n > 0 {
		badge = fmt.Sprintf(" 1 of %d ", n+1)
	}
	box := hostBoxW(" Permission required ", "Host:PermTitle", badge, body, w, maxW)
	return spliceBox(rows, box, w, headerH, chatH)
}

// trimmedWidth is a row's width without trailing blanks.
func trimmedWidth(r []hostapi.Segment) int {
	var b strings.Builder
	for _, s := range r {
		b.WriteString(s.Text)
	}
	return ansi.StringWidth(strings.TrimRight(b.String(), " "))
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

// permTitle names what the request does, and on what.
func permTitle(req hostapi.PermRequest, root string) (verb, target string) {
	verb, target = permTitleAbs(req)
	if rel, err := filepath.Rel(root, target); err == nil && root != "" && filepath.IsAbs(target) && !strings.HasPrefix(rel, "..") {
		target = rel
	}
	return verb, target
}

func permTitleAbs(req hostapi.PermRequest) (verb, target string) {
	switch {
	case req.Subject == "tool.bash":
		return "Run a command", ""
	case req.Tool == "write":
		return "Write", req.Arg
	case req.Tool == "edit" || req.Tool == "multiedit":
		return "Edit", req.Arg
	case req.Subject == "fs.write":
		return "Change", req.Arg
	case req.Subject == "fs.read":
		return "Read", req.Arg
	case strings.HasPrefix(req.Subject, "net."):
		return "Fetch", req.Arg
	case strings.HasPrefix(req.Subject, "mcp."):
		return "Use the " + strings.TrimPrefix(req.Subject, "mcp.") + " tool", req.Arg
	case req.Subject == "tool.workflow":
		return "Run a workflow", req.Arg
	}
	return req.Subject, req.Arg
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

func permButtons(pa *permAskPrompt, width int) []hostapi.Segment {
	choices := pa.choices()
	render := func(labels []string) []hostapi.Segment {
		var row []hostapi.Segment
		for i, c := range choices {
			if i > 0 {
				row = append(row, hostapi.Segment{Text: "  "})
			}
			hl := "Host:Btn"
			if i == pa.sel {
				hl = map[bool]string{true: "Host:BtnSel", false: "Host:BtnNo"}[c.ok]
			}
			row = append(row, hostapi.Segment{Text: " " + labels[i] + " ", HL: hl, Hit: i + 1})
		}
		return row
	}
	labels := make([]string, len(choices))
	for i, c := range choices {
		labels[i] = c.label
	}
	row := render(labels)
	if over := segsWidth(row) - width; over > 0 && len(choices) == 3 {
		// the "don't ask again" label gives way, the buttons stay
		labels[1] = ansi.Truncate(labels[1], max(ansi.StringWidth(labels[1])-over, 8), "…")
		row = render(labels)
	}
	return row
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
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		l = strings.ReplaceAll(l, "\t", "    ")
		for _, wrapped := range wrapSegs([]hostapi.Segment{{Text: l}}, width-2) {
			line := ""
			for _, sg := range wrapped {
				line += sg.Text
			}
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
	for _, l := range strings.Split(d, "\n") {
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
		text := ansi.Truncate(strings.ReplaceAll(l.text, "\t", "    "), room, "…")
		rows = append(rows, []hostapi.Segment{
			{Text: gutter, HL: numHL},
			{Text: sign + text + strings.Repeat(" ", max(0, room-ansi.StringWidth(text))), HL: lineHL},
		})
	}
	return rows
}

// splicePick composes the model picker over the chat area.
func (a *App) splicePick(rows [][]hostapi.Segment, mp *modelPickPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	if len(mp.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "no models registered", HL: "Comment"}})
	}
	items := make([]listItem, 0, len(mp.visible))
	for _, i := range mp.visible {
		it := mp.items[i]
		desc := ""
		if it.Display != "" && it.Display != it.ID {
			desc = it.Display
		}
		if !it.HasKey {
			desc = strings.TrimSpace(desc + "  (no api key)")
		}
		items = append(items, listItem{it.ID, desc})
	}
	if len(mp.items) > 0 {
		body = append(body, mp.filterRows(listRows(items, mp.selected, 12, "Host:Bold"))...)
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, keyHints("enter", "pick", "type", "filter", "esc", "cancel"))
	box := hostBox(" Select model ", "Host:PickTitle", "", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}
