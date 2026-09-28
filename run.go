package main

import (
	"context"
	"encoding/json"
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

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// failPrint reports a zemi run failure. A script reading stdout for
// --json gets the same report shape it gets for a failed turn.
func failPrint(err error, jsonOut bool, stdout, stderr io.Writer) int {
	if !jsonOut {
		fmt.Fprintln(stderr, "zemi:", hostapi.Headline(err.Error()))
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report{IsError: true, Error: err.Error(), Tools: []reportTool{}})
	return 1
}

// printTurn runs one turn without the TUI: the reply goes to stdout (or a
// JSON report with --json), a failure to stderr with status 1, an
// interrupt ends with 130.
func printTurn(h *hostapi.Host, prompt string, jsonOut bool, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	h.WaitMCP(ctx)
	sid, start := h.AgentSessionID(), len(h.Session().Transcript)
	began, before, denied := time.Now(), h.AgentStream(), h.HeadlessDenials()
	interrupted := ctx.Err() != nil
	if !interrupted {
		if err := sendPrompt(h, prompt); err != nil {
			if errors.Is(err, context.Canceled) {
				return 130
			}
			if !jsonOut && errors.Is(err, hostapi.ErrNoModel) && h.InitError() != "" {
				return 1 // the init failure on stderr is the cause
			}
			h.Log.Error("print", err.Error(), nil) // stderr gets the headline, the log the traceback
			return failPrint(err, jsonOut, stdout, stderr)
		}
	}
	// a command that swapped the session (/new, /resume) ran no turn, and
	// none of the rows or usage it brought are this run's.
	// NOTE: a command whose turn starts a session and ends before this check
	// (a markdown command's quick reply) reads as a swap and prints nothing;
	// a swap the command reported itself would tell them apart.
	if strings.HasPrefix(prompt, "/") && h.AgentSessionID() != sid && !h.AgentActive() {
		start, before = len(h.Session().Transcript), h.AgentStream()
	}
wait:
	for h.AgentActive() {
		select {
		case <-ctx.Done():
			if interrupted {
				break wait
			}
			interrupted = true
			h.AgentCancel()
			// the turn gets a moment to stop, then zemi run leaves anyway
			ctx, stop = context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
		case <-time.After(20 * time.Millisecond):
		}
	}
	rows := h.Session().Transcript
	rep := turnReport(rows[min(start, len(rows)):])
	if interrupted {
		proc.KillAll()
		rep.IsError, rep.Error = true, "interrupted"
	}
	d := h.HeadlessDenials()
	rep.Denied, rep.unsandboxed, rep.tuiOnly = d.Asked-denied.Asked, d.Unsandboxed-denied.Unsandboxed, d.TUIOnly-denied.TUIOnly
	if jsonOut {
		after := h.AgentStream()
		rep.SessionID, rep.DurationMS = h.AgentSessionID(), time.Since(began).Milliseconds()
		rep.Usage.Input, rep.Usage.Output = after.SessionIn-before.SessionIn, after.SessionOut-before.SessionOut
		rep.Usage.CostUSD = after.SessionCost - before.SessionCost
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		if rep.IsError {
			head, detail, _ := strings.Cut(rep.Error, "\n")
			fmt.Fprintln(stderr, "zemi:", head)
			if detail != "" {
				fmt.Fprintln(stderr, "  "+detail)
			}
		}
		// The denied calls are in the JSON report but nowhere in the text
		// reply, so a run that did nothing looks like a lazy model.
		if n := rep.Denied; n > 0 {
			fmt.Fprintf(stderr, "zemi: %d tool %s denied; nobody can approve a call in zemi run, %s\n", n, plural(n, "call", "calls"), deniedHint(rep, h.ProjectTrusted()))
		}
		if n := rep.refused; n > 0 {
			fmt.Fprintf(stderr, "zemi: %d tool %s denied by a rule, the mode, the profile or a permission.request handler\n", n, plural(n, "call", "calls"))
		}
		if rep.Result != "" {
			fmt.Fprintln(stdout, sanitize.String(rep.Result))
		}
	}
	switch {
	case interrupted:
		return 130
	case rep.IsError:
		return 1
	}
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// deniedHint says what would let zemi run's denied calls through.
func deniedHint(rep report, trusted bool) string {
	const rule = "an allow rule (zemi.permission.rule.add in init.lua) lets it through"
	switch {
	case rep.unsandboxed > 0:
		return "and running outside the sandbox needs the TUI; " + rule
	case rep.tuiOnly > 0:
		return "and only the TUI can approve a Lua script or a memory edit; " + rule
	case !trusted:
		return "try --trust (untrusted, auto mode asks for almost everything)"
	}
	return "and the mode left it to the user; " + rule
}

// report is the --json document for one zemi run turn.
type report struct {
	Result     string       `json:"result"`
	IsError    bool         `json:"is_error"`
	Error      string       `json:"error,omitempty"`
	SessionID  string       `json:"session_id"`
	DurationMS int64        `json:"duration_ms"`
	Tools      []reportTool `json:"tools"`
	// asks nobody could answer, subagents' and workflows' included
	Denied int `json:"denied"`
	// times the turn compacted or cleared old tool output to fit the window
	Compactions int `json:"compactions"`
	Usage       struct {
		Input   int64   `json:"input_tokens"`
		Output  int64   `json:"output_tokens"`
		CostUSD float64 `json:"cost_usd"`
	} `json:"usage"`

	unsandboxed int // of Denied, asks to run outside the sandbox
	tuiOnly     int // of Denied, asks no permission mode ever settles
	refused     int // main-agent calls a rule, mode, profile or handler denied
}

type reportTool struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Error is the first line of a failed call's output, so a script driving
	// zemi run reads why; Denied marks the ones the permission gate refused
	Error  string `json:"error,omitempty"`
	Denied bool   `json:"denied,omitempty"`
}

// failureLine is why a failed call failed: a bash result ends in its
// exit status, signal or "stopped:" line under the command's output, so the last such
// line (the output may hold one too), else the last line; other tools
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

// turnReport summarizes the transcript rows one turn added.
func turnReport(rows []hostapi.ChatMessage) report {
	rep := report{Tools: []reportTool{}}
	var replies []string
	for _, m := range rows {
		switch {
		case m.Role != fantasy.MessageRoleAssistant:
		case m.Kind == hostapi.RowMessage:
			replies = append(replies, strings.TrimSpace(m.Text))
		case m.Kind == hostapi.RowTool:
			rt := reportTool{Name: m.Tool, Input: m.Text, OK: m.Status == hostapi.CallOK, Detail: m.Detail}
			if !rt.OK {
				rt.Error = failureLine(m.Tool, m.Output)
				// the gate's DeniedText; a bare "permission denied" is also EACCES
				rt.Denied = strings.HasPrefix(strings.TrimSpace(m.Output), "permission denied (")
			}
			// an unanswered ask is counted at the gate (Host.HeadlessDenials)
			if by, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(m.Output), "permission denied ("), ")"); rt.Denied && by != tools.ByDefault && by != "unsandboxed" && by != "network" {
				rep.refused++
			}
			rep.Tools = append(rep.Tools, rt)
		case m.Kind == hostapi.RowCompact:
			rep.Compactions++
		case m.Kind == hostapi.RowError:
			rep.IsError, rep.Error = true, m.Text
		}
	}
	rep.Result = strings.Join(replies, "\n\n")
	return rep
}
