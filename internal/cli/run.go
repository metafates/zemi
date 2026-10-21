package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"reflect"
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
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/internal/ui"
)

// eventType is one zemi run --json wire type (DESIGN §8 has the tables).
type eventType string

const (
	evStart     eventType = "start"
	evToolStart eventType = "tool_start"
	evToolEnd   eventType = "tool_end"
	evAsk       eventType = "ask"
	evRetry     eventType = "retry"
	evMessage   eventType = "message"
	evReasoning eventType = "reasoning"
	evNotice    eventType = "notice"
	evCompact   eventType = "compact"
	evError     eventType = "error"
	evDone      eventType = "done"
)

// event is one line of zemi run --json but the last (DESIGN §8 has the
// tables); a field its type does not carry is left out.
type event struct {
	Type    eventType `json:"type"`
	Session string    `json:"session,omitempty"`
	Model   string    `json:"model,omitempty"`
	Profile string    `json:"profile,omitempty"`
	Trusted *bool     `json:"trusted,omitzero"`
	CWD     string    `json:"cwd,omitempty"`
	Text    string    `json:"text,omitempty"`
	MS      int64     `json:"ms,omitzero"`
	ID      string    `json:"id,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	Input   string    `json:"input,omitempty"`
	Label   string    `json:"label,omitempty"`
	Status  string    `json:"status,omitempty"`
	Output  string    `json:"output,omitempty"`
	Error   string    `json:"error,omitempty"`
	Diff    string    `json:"diff,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Subject string    `json:"subject,omitempty"`
	Arg     string    `json:"arg,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Why     string    `json:"why,omitempty"`
	Attempt int       `json:"attempt,omitzero"`
	InMS    int64     `json:"in_ms,omitzero"`
}

// doneEvent is the last line, a failed boot or send's too: what a script
// that reads one line (tail -n1) needs.
type doneEvent struct {
	Type        eventType        `json:"type"`
	Result      string           `json:"result"`
	IsError     bool             `json:"is_error"`
	Error       string           `json:"error,omitempty"`
	Session     string           `json:"session"`
	DurationMS  int64            `json:"duration_ms"`
	Usage       runUsage         `json:"usage"`
	Compactions int              `json:"compactions"`
	Denied      []hostapi.Denial `json:"denied"`
	Tools       []doneTool       `json:"tools"`
}

type runUsage struct {
	Input     int64   `json:"input_tokens"`
	Output    int64   `json:"output_tokens"`
	CacheRead int64   `json:"cache_read_tokens"`
	CostUSD   float64 `json:"cost_usd"`
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
	started set.Set[string] // call ids a tool_start went out for
	ended   set.Set[string] // call ids a tool_end went out for
	turn    int             // the frame's turn: a turn chains into the next, so retries restart with it
	retries int
	done    doneEvent
}

func newFollower() *follower {
	return &follower{started: set.Set[string]{}, ended: set.Set[string]{}, done: doneEvent{Type: evDone}}
}

// step reads one frame and the run's denials so far. A call's status and
// output change together and a retry restores finished calls whole, so a
// finished call's tool_end goes out when a read first sees it, its row
// settled or not; a waiting call can still vanish in a retry and gets
// nothing.
func (f *follower) step(fr hostapi.RunFrame, denials []hostapi.Denial) []event {
	var evs []event
	for _, d := range denials[len(f.done.Denied):] {
		f.done.Denied = append(f.done.Denied, d)
		evs = append(evs, event{Type: evAsk, Tool: d.Tool, Subject: d.Subject, Arg: d.Arg, Reason: d.Reason, Why: string(d.Why)})
	}
	// beginTurn resets retries at every turn, and the turns of one run chain
	// under one active stretch, so the watermark resets with the turn.
	if fr.Stream.Turn != f.turn {
		f.turn, f.retries = fr.Stream.Turn, 0
	}
	if s := fr.Stream; s.Retries > f.retries {
		evs = append(evs, event{Type: evRetry, Attempt: s.Retries + 1, InMS: s.RetryIn.Milliseconds(), Error: s.RetryWhy})
	}
	f.retries = fr.Stream.Retries
	asCall := func(m hostapi.ChatMessage) hostapi.StreamTool {
		return hostapi.StreamTool{ID: m.CallID, Name: m.Tool, Input: m.Text, Label: m.Label, Status: m.Status, Output: m.Output, Diff: m.Diff, Detail: m.Detail}
	}
	for _, m := range fr.Settled {
		switch {
		case m.Kind == hostapi.RowTool:
			evs = f.call(evs, asCall(m))
			// a verify that passed is the turn's last word: with no message
			// after it, its output is what the run prints
			if m.Tool == tools.ToolVerify && m.Status == hostapi.CallOK {
				f.done.Result = m.Output
			}
		case m.Role != fantasy.MessageRoleAssistant:
		case m.Kind == hostapi.RowMessage:
			f.done.Result = m.Text
			evs = append(evs, event{Type: evMessage, Text: m.Text})
		case m.Kind == hostapi.RowReasoning:
			ev := event{Type: evReasoning, Text: m.Text}
			if m.Stat != nil {
				ev.MS = m.Stat.ThoughtMS
			}
			evs = append(evs, ev)
		case m.Kind == hostapi.RowNotice:
			evs = append(evs, event{Type: evNotice, Text: m.Text})
		case m.Kind == hostapi.RowCompact:
			f.done.Compactions++
			evs = append(evs, event{Type: evCompact, Text: m.Text})
		case m.Kind == hostapi.RowError:
			f.done.IsError, f.done.Error = true, m.Text
			evs = append(evs, event{Type: evError, Text: m.Text})
		}
	}
	for _, m := range fr.Pending {
		if m.Kind == hostapi.RowTool {
			evs = f.call(evs, asCall(m))
		}
	}
	for _, t := range fr.Stream.Tools {
		evs = f.call(evs, t)
	}
	return evs
}

// call sends a call's tool_start once it runs and its tool_end once it
// finished, the start first when no read saw it run. An id-less call is
// one key: a second one gets nothing after the first ended.
func (f *follower) call(evs []event, t hostapi.StreamTool) []event {
	if t.Status == hostapi.CallWait || f.ended.Has(t.ID) {
		return evs
	}
	ev := event{Type: evToolStart, ID: t.ID, Tool: t.Name, Input: t.Input, Label: t.Label}
	if !f.started.Has(t.ID) {
		f.started.Add(t.ID)
		evs = append(evs, ev)
	}
	if t.Status == hostapi.CallRun {
		return evs
	}
	f.ended.Add(t.ID)
	ev.Type, ev.Status, ev.Output, ev.Diff, ev.Detail = evToolEnd, string(t.Status), t.Output, t.Diff, t.Detail
	if t.Status == hostapi.CallErr || t.Status == hostapi.CallStopped {
		ev.Error = failureLine(t.Name, t.Output)
	}
	f.done.Tools = append(f.done.Tools, doneTool{ID: t.ID, Tool: t.Name, Input: t.Input, Status: ev.Status, Error: ev.Error})
	return append(evs, ev)
}

// failureLine is why a failed call failed: the shell tool is the one whose
// result ends in its reason (the exit status, signal or "stopped:" line under
// the output, which the tool itself reads); the others lead with it.
func failureLine(tool, output string) string {
	if tool == tools.ToolShell {
		return tools.ShellFailure(output)
	}
	return strings.Split(strings.TrimSpace(output), "\n")[0]
}

// runFailed reports a zemi run that failed before its turn: a script
// reading --json gets the done line it parses.
func runFailed(err error, jsonOut bool, stdout, stderr io.Writer) int {
	if jsonOut {
		writeJSON(stdout, doneEvent{Type: evDone, IsError: true, Error: err.Error()})
		return 1
	}
	fmt.Fprintln(stderr, "zemi:", hostapi.Headline(err.Error()))
	return 1
}

// writeJSON writes v as one JSON Lines line, what every --json prints. A
// byte that is not UTF-8 becomes U+FFFD, so no line is dropped; the fixed
// struct types leave Marshal nothing else to refuse. Every string the line
// carries, a map key or a nested row's included, passes sanitize here, and
// the caller's value is left alone, so no field needs a per-call list.
func writeJSON(w io.Writer, v any) {
	b, _ := json.Marshal(sanitizeWire(reflect.ValueOf(v)).Interface(), jsontext.AllowInvalidUTF8(true), json.Deterministic(true))
	_, _ = w.Write(append(b, '\n'))
}

// sanitizeWire returns a copy of v whose strings pass sanitize.String,
// descending into its structs, slices, arrays, maps and pointers; unexported
// fields ride along whole, reflect cannot write them one by one.
func sanitizeWire(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		nv := reflect.New(v.Type().Elem())
		nv.Elem().Set(sanitizeWire(v.Elem()))
		return nv
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		nv := reflect.New(v.Type()).Elem()
		nv.Set(sanitizeWire(v.Elem()))
		return nv
	case reflect.Struct:
		nv := reflect.New(v.Type()).Elem()
		nv.Set(v)
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				nv.Field(i).Set(sanitizeWire(v.Field(i)))
			}
		}
		return nv
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		nv := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			nv.Index(i).Set(sanitizeWire(v.Index(i)))
		}
		return nv
	case reflect.Array:
		nv := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			nv.Index(i).Set(sanitizeWire(v.Index(i)))
		}
		return nv
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		nv := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			nv.SetMapIndex(sanitizeWire(iter.Key()), sanitizeWire(iter.Value()))
		}
		return nv
	case reflect.String:
		nv := reflect.New(v.Type()).Elem()
		nv.SetString(sanitize.String(v.String()))
		return nv
	default: // a number, a bool or a flag: nothing on the line to sanitize
		return v
	}
}

// runHeadless runs o.prompt with no TUI: the run view on stderr, and on
// stdout the last message or, with --json, the events. It returns the exit
// status: 1 when the send or the turn failed, 128+n on a signal.
func runHeadless(h *hostapi.Host, view *ui.RunView, o options, stdout io.Writer) int {
	// the first signal stops the turn, the second or 5s later leaves
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, stopSignals()...)
	defer signal.Stop(sig)
	view.Start(h, o.reasoning)
	defer view.Close()

	h.TakeSwap() // a swap before this line is the boot's (-c, -r), not the run's
	next := len(h.Session().Transcript)
	began, before, asked := time.Now(), h.AgentStream(), len(h.HeadlessDenials())
	f, emit := newFollower(), func(ev any) {}
	if o.json {
		emit = func(ev any) { writeJSON(stdout, ev) }
	}
	sendCtx, cancelSend := context.WithCancel(context.Background())
	defer cancelSend()
	sent := make(chan error, 1)
	h.GoSafe("zemi run", func() {
		err := errors.New("the send panicked; the log has the stack")
		defer func() { sent <- err }() // a panic still ends the wait
		err = sendPrompt(sendCtx, h, o.prompt, o.text)
	})

	sending, interrupted, started := true, false, false
	gone := false // left without waiting for the turn to stop
	var sendErr error
	var stopped syscall.Signal
	var leave <-chan time.Time
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for over := false; !over; {
		select {
		case s := <-sig:
			if interrupted {
				gone = true
				break
			}
			interrupted = true
			if n, ok := s.(syscall.Signal); ok {
				stopped = n
			}
			cancelSend()
			h.AgentCancel()
			h.CancelBusy()
			leave = time.After(5 * time.Second)
		case <-leave:
			gone = true
		case sendErr = <-sent:
			sending = false
			// a command that swapped the session (/new, /resume) ran no turn,
			// and none of the rows or usage it brought are this run's.
			if h.TakeSwap() {
				next, before = len(h.Session().Transcript), h.AgentStream()
			}
		case <-tick.C:
		}
		over = gone || !sending && !h.AgentActive()
		fr := h.RunFrame(next)
		if sending { // rows a command swaps in are not this run's
			fr.Settled, fr.Pending, fr.Next = nil, nil, next
		}
		if id := h.AgentSessionID(); !started && id != "" && !sending {
			started = true
			emit(event{Type: evStart, Session: id, Model: fr.Stream.Model, Profile: fr.Stream.Profile, Trusted: new(h.ProjectTrusted()), CWD: h.Session().CWD})
		}
		evs := f.step(fr, h.HeadlessDenials()[asked:])
		// the tail's working rule shows a retry itself
		if !view.Tails() && slices.ContainsFunc(evs, func(ev event) bool { return ev.Type == evRetry }) {
			when := "retrying now"
			if in := fr.Stream.RetryIn.Round(time.Second); in > 0 {
				when = "next try in " + in.String()
			}
			fmt.Fprintf(view, "zemi: %s: %s\n", when, sanitize.String(fr.Stream.RetryWhy))
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
	case sendErr != nil:
		h.Log.Error(sendErr.Error(), "origin", "run") // stderr gets the headline, the log the traceback
		if !o.json && errors.Is(sendErr, hostapi.ErrNoModel) && h.InitError() != "" {
			return 1 // the init failure on stderr is the cause
		}
		d.IsError, d.Error = true, sendErr.Error()
	}
	// a goal the chain left set was blocked or paused; a met one is cleared
	if g, ok := h.Goal(); ok && !d.IsError && g.Reason != "" {
		d.IsError, d.Error = true, "goal "+g.Reason
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
		headline(view, "zemi: ", d.Error)
	}
	// where both streams are terminals the view drew the reply already
	if out, ok := stdout.(*os.File); !o.json && d.Result != "" && !(onTerminal && ok && term.IsTerminal(out.Fd())) {
		fmt.Fprintln(stdout, sanitize.String(d.Result))
	}
	printDenied(view, view.Width(), d.Denied, h.ProjectTrusted())
	if d.IsError {
		if interrupted {
			return 128 + int(stopped) // 130 INT, 143 TERM, 129 HUP
		}
		return 1
	}
	return 0
}

// printDenied lists the asks nobody could answer, subagents' included, which
// no row shows, and the one change that would let them through, in width
// columns.
func printDenied(w io.Writer, width int, denied []hostapi.Denial, trusted bool) {
	if len(denied) == 0 {
		return
	}
	fmt.Fprintf(w, "zemi: %d %s nobody could answer in a headless run:\n", len(denied), hostapi.Plural(len(denied), "ask", "asks"))
	shown := denied[:min(len(denied), 5)]
	clip := func(s string, n int) string { return ansi.Truncate(strings.Join(strings.Fields(s), " "), n, "…") }
	toolW := 0
	for _, d := range shown {
		toolW = max(toolW, ansi.StringWidth(d.Tool))
	}
	// the reason's 10-cell floor has to fit: the arg column gives way first
	argCap := min(40, max(0, width-toolW-16))
	args, argW := make([]string, len(shown)), 0
	for i, d := range shown {
		args[i] = clip(d.Arg, argCap)
		argW = max(argW, ansi.StringWidth(args[i]))
	}
	for i, d := range shown {
		fmt.Fprintf(w, "  %-*s  %s%s  %s\n", toolW, d.Tool, args[i], strings.Repeat(" ", argW-ansi.StringWidth(args[i])), clip(d.Reason, max(10, width-toolW-argW-6)))
	}
	if n := len(denied) - len(shown); n > 0 {
		fmt.Fprintf(w, "  … %d more\n", n)
	}
	fmt.Fprintf(w, "  %s\n", deniedHint(trusted))
}

// deniedHint says what would let zemi run's denied calls through.
func deniedHint(trusted bool) string {
	if !trusted {
		return "--trust trusts this project for the run"
	}
	return "a plugin's permission.request handler returning { decision = \"ask\" } could gate the calls here"
}
