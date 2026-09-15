// agent.go is the Go-owned agent chrome (DESIGN §6.3, §8): the streaming
// event pump, the interactive permission overlay, and the model picker.
// Their chrome uses fixed "Host:*" styles no theme group can restyle, and
// no Lua participates in drawing or answering them.
package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
)

// agentEventMsg carries one streaming update from the turn goroutine. The
// stream state itself lives in the host (agentHost snapshot); the UI only
// invalidates the frame, and the chat widget re-reads ctx.agent on the
// queue (DESIGN §4: no Lua state, no rendering inside View).

// permAskPrompt is one open permission overlay; queued asks wait in
// permQueue and open when the current one is answered (parallel tool runs
// may ask concurrently; each must be answered exactly once).
type permAskPrompt struct {
	req    hostapi.PermRequest
	answer chan permAnswer
}

type modelPickPrompt struct {
	items  []hostapi.ModelOption
	sel    int
	answer chan modelPick
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
	a.dispatch(permAskMsg{req: req, answer: ch})
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
		next := a.permQueue[0]
		a.permQueue = a.permQueue[1:]
		a.permAsk = &permAskPrompt{req: next.req, answer: next.answer}
	}
	a.frameGen++
}

// PickModel implements hostapi.UIHooks: blocks until the user picks a
// model or cancels (ok=false on cancel).
func (a *App) PickModel(items []hostapi.ModelOption, current string) (string, bool) {
	ch := make(chan modelPick, 1)
	sel := 0
	for i, it := range items {
		if it.ID == current {
			sel = i
			break
		}
	}
	a.dispatch(modelPickMsg{items: items, sel: sel, current: current, answer: ch})
	res := <-ch
	return res.id, res.ok
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
	bw := ansi.StringWidth(title) + ansi.StringWidth(badge) + 4
	for _, r := range body {
		bw = max(bw, segsWidth(r)+4)
	}
	bw = max(min(bw, w-4), 12)
	badge = ansi.Truncate(badge, bw-2, "…")
	title = ansi.Truncate(title, bw-2-ansi.StringWidth(badge), "…")
	fill := bw - 2 - ansi.StringWidth(title) - ansi.StringWidth(badge)
	box := [][]hostapi.Segment{{
		{Text: "┌"},
		{Text: title, HL: titleHL},
		{Text: strings.Repeat("─", fill)},
		{Text: badge, HL: "Comment"},
		{Text: "┐"},
	}}
	for _, r := range body {
		line := truncateSegs(r, bw-4)
		pad := bw - 4 - segsWidth(line)
		row := []hostapi.Segment{{Text: "│ "}}
		row = append(row, line...)
		row = append(row, hostapi.Segment{Text: strings.Repeat(" ", max0(pad)) + " │"})
		box = append(box, row)
	}
	box = append(box, []hostapi.Segment{{Text: "└" + strings.Repeat("─", bw-2) + "┘"}})
	return box
}

type listItem struct{ label, desc string }

// listRows is the one list renderer (palette, completion, pickers,
// overlays): a window of at most limit rows kept around sel, "› " and
// selHL on the selected row, dim descriptions, and an "i/n" counter once
// the list scrolls.
func listRows(items []listItem, sel, limit int, selHL string) [][]hostapi.Segment {
	// Every row pads to the widest item in the whole list, so a box sized
	// from the visible window keeps its width while scrolling.
	itemWidth := func(it listItem) int {
		n := ansi.StringWidth(it.label)
		if it.desc != "" {
			n += 2 + ansi.StringWidth(it.desc)
		}
		return n
	}
	widest := 0
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
		row := []hostapi.Segment{{Text: marker + items[i].label, HL: hl}}
		if items[i].desc != "" {
			row = append(row, hostapi.Segment{Text: "  " + items[i].desc, HL: "Comment"})
		}
		if pad := widest - itemWidth(items[i]); pad > 0 {
			row = append(row, hostapi.Segment{Text: strings.Repeat(" ", pad)})
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

// keyHints lays out a footer of "[key] label" pairs, keys in the fixed
// host style.
func keyHints(pairs ...string) []hostapi.Segment {
	var out []hostapi.Segment
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			out = append(out, hostapi.Segment{Text: "   "})
		}
		out = append(out, hostapi.Segment{Text: "[" + pairs[i] + "]", HL: "Host:Bold"}, hostapi.Segment{Text: " " + pairs[i+1]})
	}
	return out
}

// splicePerm composes the permission overlay over the chat area.
func (a *App) splicePerm(rows [][]hostapi.Segment, pa *permAskPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	body = append(body, []hostapi.Segment{{Text: pa.req.Subject, HL: "Host:Bold"}})
	if arg := pa.req.Arg; arg != "" {
		lines := strings.Split(arg, "\n")
		if len(lines) > 8 {
			lines = append(lines[:8], "…")
		}
		for _, l := range lines {
			body = append(body, []hostapi.Segment{{Text: l}})
		}
	}
	if pa.req.Reason != "" {
		body = append(body, []hostapi.Segment{{Text: pa.req.Reason, HL: "Comment"}})
	}
	if p := pa.req.Preview; p != "" {
		const maxPreview = 12
		lines := strings.Split(p, "\n")
		more := len(lines) - maxPreview
		if more > 0 {
			lines = lines[:maxPreview]
		}
		body = append(body, []hostapi.Segment{{Text: ""}})
		for _, l := range lines {
			hl := "Host:DiffAdd"
			if strings.HasPrefix(l, "- ") {
				hl = "Host:DiffDel"
			}
			body = append(body, []hostapi.Segment{{Text: strings.ReplaceAll(l, "\t", "    "), HL: hl}})
		}
		if more > 0 {
			body = append(body, []hostapi.Segment{{Text: fmt.Sprintf("… %d more lines", more), HL: "Comment"}})
		}
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, keyHints("o", "once", "a", "always", "n", "no"))
	box := hostBox(" Permission required ", "Host:PermTitle", "", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}

// splicePick composes the model picker over the chat area.
func (a *App) splicePick(rows [][]hostapi.Segment, mp *modelPickPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	if len(mp.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "no models registered", HL: "Comment"}})
	}
	items := make([]listItem, 0, len(mp.items))
	for _, it := range mp.items {
		desc := ""
		if it.Display != "" && it.Display != it.ID {
			desc = it.Display
		}
		if !it.HasKey {
			desc = strings.TrimSpace(desc + "  (no api key)")
		}
		items = append(items, listItem{it.ID, desc})
	}
	body = append(body, listRows(items, mp.sel, len(items), "Host:Bold")...)
	body = append(body, []hostapi.Segment{{Text: ""}}, keyHints("up/down", "move", "enter", "pick", "esc", "cancel"))
	box := hostBox(" Select model ", "Host:PickTitle", "", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}
