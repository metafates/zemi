// agent.go is the Go-owned agent chrome (DESIGN §6.3, §8): the streaming
// event pump, the interactive permission overlay, and the model picker.
// Their chrome uses fixed "Host:*" styles no theme group can restyle, and
// no Lua participates in drawing or answering them.
package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
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

// hostBox draws a Go-owned chrome box: fixed-style titled top border,
// body rows already laid out as segments, padded to one width.
func hostBox(title, titleHL string, body [][]hostapi.Segment, w int) [][]hostapi.Segment {
	tw := ansi.StringWidth(title)
	bw := tw + 4
	for _, r := range body {
		if n := segsWidth(r) + 4; n > bw {
			bw = n
		}
	}
	if bw > w-4 {
		bw = w - 4
	}
	if bw < 12 {
		bw = 12
	}
	var box [][]hostapi.Segment
	box = append(box, []hostapi.Segment{
		{Text: "┌"},
		{Text: title, HL: titleHL},
		{Text: strings.Repeat("─", max0(bw-2-tw)) + "┐"},
	})
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

// splicePerm composes the permission overlay over the chat area.
func (a *App) splicePerm(rows [][]hostapi.Segment, pa *permAskPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	body = append(body, []hostapi.Segment{{Text: pa.req.Subject, HL: "Host:PermKey"}})
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
	body = append(body, []hostapi.Segment{{Text: ""}})
	body = append(body, []hostapi.Segment{
		{Text: "[o]", HL: "Host:PermKey"}, {Text: " once   "},
		{Text: "[a]", HL: "Host:PermKey"}, {Text: " always   "},
		{Text: "[n]", HL: "Host:PermKey"}, {Text: " no"},
	})
	box := hostBox(" Permission required ", "Host:PermTitle", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}

// splicePick composes the model picker over the chat area.
func (a *App) splicePick(rows [][]hostapi.Segment, mp *modelPickPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	if len(mp.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "no models registered", HL: "Comment"}})
	}
	for i, it := range mp.items {
		mark, hl := "  ", ""
		if i == mp.sel {
			mark, hl = "> ", "Host:PickSel"
		}
		name := it.ID
		if it.Display != "" && it.Display != it.ID {
			name = it.ID + "  " + it.Display
		}
		if !it.HasKey {
			name += "  (no api key)"
		}
		body = append(body, []hostapi.Segment{
			{Text: mark, HL: "Host:PickSel"},
			{Text: name, HL: hl},
		})
	}
	body = append(body, []hostapi.Segment{{Text: ""}})
	body = append(body, []hostapi.Segment{
		{Text: "[up/down]", HL: "Host:PermKey"}, {Text: " move   "},
		{Text: "[enter]", HL: "Host:PermKey"}, {Text: " pick   "},
		{Text: "[esc]", HL: "Host:PermKey"}, {Text: " cancel"},
	})
	box := hostBox(" Select model ", "Host:PickTitle", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}
