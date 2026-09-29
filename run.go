package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/internal/ui"
)

// event is one line of zemi run --json but the last (README.md has the
// table); a field its type does not carry is left out.
type event struct {
	Type    string `json:"type"`
	Session string `json:"session,omitempty"`
	Model   string `json:"model,omitempty"`
	Profile string `json:"profile,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Trusted *bool  `json:"trusted,omitzero"`
	CWD     string `json:"cwd,omitempty"`
	Text    string `json:"text,omitempty"`
	MS      int64  `json:"ms,omitzero"`
	ID      string `json:"id,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Input   string `json:"input,omitempty"`
	Label   string `json:"label,omitempty"`
	Status  string `json:"status,omitempty"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
	Diff    string `json:"diff,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Subject string `json:"subject,omitempty"`
	Arg     string `json:"arg,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Why     string `json:"why,omitempty"`
	Attempt int    `json:"attempt,omitzero"`
	InMS    int64  `json:"in_ms,omitzero"`
}

// doneEvent is the last line, a failed boot or send's too: what a script
// that reads one line (tail -n1) needs.
type doneEvent struct {
	Type        string     `json:"type"`
	Result      string     `json:"result"` // the last message
	IsError     bool       `json:"is_error"`
	Error       string     `json:"error,omitempty"`
	Session     string     `json:"session"`
	DurationMS  int64      `json:"duration_ms"`
	Usage       runUsage   `json:"usage"`
	Compactions int        `json:"compactions"`
	Denied      []denial   `json:"denied"`
	Tools       []doneTool `json:"tools"`
}

type runUsage struct {
	Input     int64   `json:"input_tokens"`
	Output    int64   `json:"output_tokens"`
	CacheRead int64   `json:"cache_read_tokens"`
	CostUSD   float64 `json:"cost_usd"`
}

// denial is an ask nobody could answer, as done.denied lists it.
type denial struct {
	Tool    string `json:"tool"`
	Subject string `json:"subject"`
	Arg     string `json:"arg"`
	Reason  string `json:"reason"`
	Why     string `json:"why"`
}

type doneTool struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// follower turns what a run shows into events, each once.
type follower struct {
	started map[string]bool // call ids a tool_start went out for
	ended   map[string]bool // call ids a tool_end went out for
	retries int
	done    doneEvent
}

func newFollower() *follower {
	return &follower{started: map[string]bool{}, ended: map[string]bool{}, done: doneEvent{Type: "done"}}
}

// step reads one frame and the run's denials so far. A call's status and
// output change together and a retry restores finished calls whole, so a
// finished call's tool_end goes out when a read first sees it, its row
// settled or not; a waiting call can still vanish in a retry and gets
// nothing.
func (f *follower) step(fr hostapi.RunFrame, denials []hostapi.Denial) []event {
	var evs []event
	for _, d := range denials[len(f.done.Denied):] {
		dn := denial{Tool: d.Tool, Subject: d.Subject, Arg: sanitize.String(d.Arg), Reason: sanitize.String(d.Reason), Why: string(d.Why)}
		f.done.Denied = append(f.done.Denied, dn)
		evs = append(evs, event{Type: "ask", Tool: dn.Tool, Subject: dn.Subject, Arg: dn.Arg, Reason: dn.Reason, Why: dn.Why})
	}
	if s := fr.Stream; s.Retries > f.retries {
		evs = append(evs, event{Type: "retry", Attempt: s.Retries, InMS: s.RetryIn.Milliseconds(), Error: sanitize.String(s.RetryWhy)})
	}
	f.retries = fr.Stream.Retries
	for _, m := range fr.Settled {
		switch {
		case m.Kind == hostapi.RowTool:
			evs = f.call(evs, hostapi.StreamTool{ID: m.ID, Name: m.Tool, Input: m.Text, Label: m.Label, Status: m.Status, Output: m.Output, Diff: m.Diff, Detail: m.Detail})
		case m.Role != fantasy.MessageRoleAssistant:
		case m.Kind == hostapi.RowMessage:
			f.done.Result = m.Text
			evs = append(evs, event{Type: "message", Text: m.Text})
		case m.Kind == hostapi.RowReasoning:
			ev := event{Type: "reasoning", Text: m.Text}
			if m.Stat != nil {
				ev.MS = m.Stat.ThoughtMS
			}
			evs = append(evs, ev)
		case m.Kind == hostapi.RowNotice:
			evs = append(evs, event{Type: "notice", Text: m.Text})
		case m.Kind == hostapi.RowCompact:
			f.done.Compactions++
			evs = append(evs, event{Type: "compact", Text: m.Text})
		case m.Kind == hostapi.RowError:
			f.done.IsError, f.done.Error = true, m.Text
			evs = append(evs, event{Type: "error", Text: m.Text})
		}
	}
	for _, m := range fr.Pending {
		if m.Kind == hostapi.RowTool && m.ID != "" {
			evs = f.call(evs, hostapi.StreamTool{ID: m.ID, Name: m.Tool, Input: m.Text, Label: m.Label, Status: m.Status, Output: m.Output, Diff: m.Diff, Detail: m.Detail})
		}
	}
	for _, t := range fr.Stream.Tools {
		evs = f.call(evs, t)
	}
	return evs
}

// call sends a call's tool_start once it runs and its tool_end once it
// finished, the start first when no read saw it run.
func (f *follower) call(evs []event, t hostapi.StreamTool) []event {
	if t.Status == hostapi.CallWait || t.ID != "" && f.ended[t.ID] {
		return evs
	}
	ev := event{Type: "tool_start", ID: t.ID, Tool: t.Name, Input: t.Input, Label: t.Label}
	if !f.started[t.ID] || t.ID == "" {
		f.started[t.ID] = true
		evs = append(evs, ev)
	}
	if t.Status == hostapi.CallRun {
		return evs
	}
	f.ended[t.ID] = true
	ev.Type, ev.Status, ev.Output, ev.Diff, ev.Detail = "tool_end", string(t.Status), t.Output, t.Diff, t.Detail
	if t.Status != hostapi.CallOK {
		ev.Error = failureLine(t.Name, t.Output)
	}
	f.done.Tools = append(f.done.Tools, doneTool{ID: t.ID, Tool: t.Name, Input: t.Input, Status: ev.Status, Error: ev.Error})
	return append(evs, ev)
}

// failureLine is why a failed call failed: a bash result ends in its exit
// status, signal or "stopped:" line under the command's output, so the last
// such line (the output may hold one too), else the last line; other tools
// lead with it.
func failureLine(tool, output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if tool != tools.ToolBash {
		return lines[0]
	}
	for _, l := range slices.Backward(lines) {
		if strings.HasPrefix(l, "exit status ") || strings.HasPrefix(l, "signal: ") || strings.HasPrefix(l, "stopped: ") || strings.HasPrefix(l, "timed out ") {
			return l
		}
	}
	return lines[len(lines)-1]
}

// runFailed reports a zemi run that failed before its turn: a script
// reading --json gets the done line it parses.
func runFailed(err error, jsonOut bool, stdout, stderr io.Writer) int {
	if jsonOut {
		writeEvent(stdout, doneEvent{Type: "done", IsError: true, Error: err.Error()})
		return 1
	}
	fmt.Fprintln(stderr, "zemi:", hostapi.Headline(err.Error()))
	return 1
}

func writeEvent(w io.Writer, ev any) {
	b, err := json.Marshal(ev)
	if err == nil {
		_, _ = w.Write(append(b, '\n'))
	}
}

// runHeadless runs o.prompt with no TUI: the run view on stderr, and on
// stdout the last message or, with --json, the events. It returns the exit
// status: 1 when the send or the turn failed, 130 on a signal.
func runHeadless(h *hostapi.Host, view *ui.RunView, o options, stdout io.Writer) int {
	if o.reasoning && !h.ReasoningShown() {
		h.ToggleReasoning()
	}
	// the first signal stops the turn, the second or 5s later leaves
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	view.Start(h)
	defer view.Close()

	sid, next := h.AgentSessionID(), len(h.Session().Transcript)
	began, before, asked := time.Now(), h.AgentStream(), len(h.HeadlessDenials())
	f, emit := newFollower(), func(ev any) {}
	if o.json {
		emit = func(ev any) { writeEvent(stdout, ev) }
	}
	sendCtx, cancelSend := context.WithCancel(context.Background())
	defer cancelSend()
	sent := make(chan error, 1)
	h.GoSafe("zemi run", func() {
		err := errors.New("the send panicked; the log has the stack")
		defer func() { sent <- err }() // a panic still ends the wait
		err = sendPrompt(sendCtx, h, o.prompt)
	})

	sending, interrupted, started := true, false, false
	var sendErr error
	var leave <-chan time.Time
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for over := false; !over; {
		select {
		case <-sig:
			if interrupted {
				proc.KillAll()
				return 130
			}
			interrupted = true
			cancelSend()
			h.AgentCancel()
			h.CancelBusy()
			leave = time.After(5 * time.Second)
		case <-leave:
			proc.KillAll()
			return 130
		case sendErr = <-sent:
			sending = false
			// a command that swapped the session (/new, /resume) ran no turn,
			// and none of the rows or usage it brought are this run's.
			// NOTE: a command whose turn starts a session and ends before this
			// check (a markdown command's quick reply) reads as a swap and
			// prints nothing; a swap the command reported itself would tell
			// them apart.
			if strings.HasPrefix(o.prompt, "/") && h.AgentSessionID() != sid && !h.AgentActive() {
				next, before = len(h.Session().Transcript), h.AgentStream()
			}
		case <-tick.C:
		}
		over = !sending && !h.AgentActive()
		fr := h.RunFrame(next)
		if sending { // rows a command swaps in are not this run's
			fr.Settled, fr.Pending, fr.Next = nil, nil, next
		}
		if id := h.AgentSessionID(); !started && id != "" && !sending {
			started = true
			emit(event{Type: "start", Session: id, Model: fr.Stream.Model, Profile: fr.Stream.Profile, Mode: h.PermMode(), Trusted: new(h.ProjectTrusted()), CWD: h.Session().CWD})
		}
		retries := f.retries
		evs := f.step(fr, h.HeadlessDenials()[asked:])
		if s := fr.Stream; s.Retries > retries {
			fmt.Fprintf(view, "zemi: retrying in %s: %s\n", s.RetryIn.Round(time.Second), sanitize.String(s.RetryWhy))
		}
		next = fr.Next
		view.Show(fr, interrupted)
		for _, ev := range evs {
			emit(ev)
		}
	}

	d := &f.done
	switch {
	case interrupted:
		proc.KillAll()
		d.IsError, d.Error = true, "interrupted"
	case sendErr != nil && errors.Is(sendErr, context.Canceled):
		return 130
	case sendErr != nil:
		h.Log.Error("run", sendErr.Error(), nil) // stderr gets the headline, the log the traceback
		if !o.json && errors.Is(sendErr, hostapi.ErrNoModel) && h.InitError() != "" {
			return 1 // the init failure on stderr is the cause
		}
		d.IsError, d.Error = true, sendErr.Error()
	}
	after := h.AgentStream()
	d.Session, d.DurationMS = h.AgentSessionID(), time.Since(began).Milliseconds()
	d.Usage = runUsage{Input: after.SessionIn - before.SessionIn, Output: after.SessionOut - before.SessionOut, CacheRead: after.CacheRead - before.CacheRead, CostUSD: after.SessionCost - before.SessionCost}
	on, onTerminal := view.On()
	view.Close()
	switch {
	case o.json:
		emit(*d)
	case d.IsError && (!on || sendErr != nil): // no row shows it
		head, detail, _ := strings.Cut(d.Error, "\n")
		fmt.Fprintln(view, "zemi:", hostapi.Headline(head))
		if detail != "" {
			fmt.Fprintln(view, "  "+detail)
		}
	}
	// where both streams are terminals the view drew the reply already
	if out, ok := stdout.(*os.File); !o.json && d.Result != "" && !(onTerminal && ok && term.IsTerminal(out.Fd())) {
		fmt.Fprintln(stdout, sanitize.String(d.Result))
	}
	printDenied(view, d.Denied, h.ProjectTrusted())
	if d.IsError {
		if interrupted {
			return 130
		}
		return 1
	}
	return 0
}

// printDenied lists the asks nobody could answer, subagents' included, which
// no row shows, and the one change that would let them through.
func printDenied(w io.Writer, denied []denial, trusted bool) {
	if len(denied) == 0 {
		return
	}
	fmt.Fprintf(w, "zemi: %d %s nobody could answer in a headless run:\n", len(denied), plural(len(denied), "ask", "asks"))
	shown := denied[:min(len(denied), 5)]
	clip := func(s string, n int) string { return ansi.Truncate(strings.Join(strings.Fields(s), " "), n, "…") }
	toolW, argW := 0, 0
	for _, d := range shown {
		toolW, argW = max(toolW, len(d.Tool)), max(argW, ansi.StringWidth(clip(d.Arg, 40)))
	}
	for _, d := range shown {
		arg := clip(d.Arg, 40)
		fmt.Fprintf(w, "  %-*s  %s%s  %s\n", toolW, d.Tool, arg, strings.Repeat(" ", argW-ansi.StringWidth(arg)), clip(d.Reason, 60))
	}
	if n := len(denied) - len(shown); n > 0 {
		fmt.Fprintf(w, "  … %d more\n", n)
	}
	fmt.Fprintln(w, "  "+deniedHint(denied, trusted))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// deniedHint says what would let zemi run's denied calls through.
func deniedHint(denied []denial, trusted bool) string {
	const rule = "an allow rule (zemi.permission.rule.add in init.lua) lets a call through"
	has := func(why hostapi.DenialWhy) bool {
		return slices.ContainsFunc(denied, func(d denial) bool { return d.Why == string(why) })
	}
	switch {
	case has(hostapi.DenyOutsideSandbox):
		return "running outside the sandbox needs the TUI; " + rule
	case has(hostapi.DenyNeedsTUI):
		return "only the TUI approves a Lua script or a memory edit; " + rule
	case !trusted:
		return "--trust trusts this project for the run; " + rule
	}
	return "the mode left it to the user; " + rule
}
