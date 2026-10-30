// status.go reports zemi's state to its terminal through the Program Status
// Protocol (OSC 7501): working while a turn runs, blocked with a kind while
// an ask waits on the user, done or error as a turn ends, idle at rest. The
// terminal keeps the record and decides how to show it (a tab badge, a
// session picker); nothing here reads replies, which the protocol allows a
// program to skip.

package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/status"
)

// statusTick sends the state report when it changed; a report a terminal
// already holds is never resent. Reports are cheap to derive (a struct
// compare), and a state that cannot encode sends nothing.
func (a *App) statusTick() tea.Cmd {
	r := a.statusReport()
	if r == a.statusSent {
		return nil
	}
	a.statusSent = r
	if seq := r.Sequence(); seq != "" {
		return tea.Raw(seq)
	}
	return nil
}

// statusReport reads the state the frame already drew: an ask blocks before
// the running turn does, and the last turn's end outlives it — done or error
// until the next turn replaces it, idle once the user cancelled.
func (a *App) statusReport() status.Report {
	busy, _ := a.host.BusyNow()
	r := status.Report{App: "zemi", Title: a.host.TerminalTitle()}
	switch {
	case a.bootErr != nil:
		r.State, r.Msg = status.Error, a.bootErr.Error()
	case a.trust != nil:
		what := "project"
		if hostapi.TrustUnsaved(a.trust.root) {
			what = "directory"
		}
		r.State, r.Kind, r.Msg = status.Blocked, status.KindPermission, "Trust this "+what+"?"
	case a.permAsk != nil:
		r.State, r.Kind, r.Msg = status.Blocked, status.KindPermission, askStatus(a.permAsk.req)
	case a.planAsk != nil:
		r.State, r.Kind, r.Msg = status.Blocked, status.KindPermission, "a plan to review"
	case a.question != nil:
		r.State, r.Kind, r.Msg = status.Blocked, status.KindQuestion, a.question.qs[0].Question
	case a.turnActive:
		r.State, r.Msg = status.Working, a.host.AgentStream().Task
	case a.turnEnded == hostapi.TurnOK:
		r.State, r.Msg = status.Done, a.host.AgentStream().Task
	case a.turnEnded == hostapi.TurnError:
		r.State, r.Msg = status.Error, a.turnErr
	case a.turnEnded == hostapi.TurnCancelled:
		r.State = status.Idle
	case busy != "":
		// a job the user waits on outside a turn (a recap, an MCP sign-in)
		// is work all the same, and it must not stomp a fresh done
		r.State, r.Msg = status.Working, busy
	default:
		r.State = status.Idle
	}
	return r
}

// askStatus is the one line a permission ask reports: the ask's title, then
// the command or file it is about.
func askStatus(req hostapi.PermRequest) string {
	title := permTitle(req)
	if req.Description != "" {
		return title + ": " + req.Description
	}
	if arg, _, _ := strings.Cut(req.Arg, "\n"); arg != "" {
		return title + ": " + arg
	}
	return title
}
