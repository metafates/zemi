package ui

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/status"
	"github.com/metafates/zemi/internal/tools"
)

// statusApp is a bare zemi for the program status report's tests.
func statusApp(t *testing.T) (*App, *hostapi.Host) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	host := hostapi.NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{})
	t.Cleanup(host.Close)
	return NewApp(host, log), host
}

// statusFrame sends one frame and returns the OSC 7501 it carried, "" for none.
func statusFrame(app *App) string {
	_, cmd := app.Update(frameTickMsg{})
	app.wakeTick()
	for _, m := range runCmd(cmd) {
		if raw, ok := m.(tea.RawMsg); ok {
			if s := fmt.Sprint(raw.Msg); strings.Contains(s, "\x1b]7501;") {
				return s
			}
		}
	}
	return ""
}

// b64 encodes text as the protocol carries free-text values.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// report is the exact sequence the app sends for a root-record state.
func report(state status.State, kind status.Kind, msg string) string {
	s := "\x1b]7501;state=" + state.String()
	if kind != 0 {
		s += ":kind=" + kind.String()
	}
	s += ":app=zemi:title=" + b64("zemi")
	if msg != "" {
		s += ":msg=" + b64(msg)
	}
	return s + "\x1b\\"
}

// TestProgramStatus: the record the terminal holds follows the state the
// frame drew — working on the turn's own words, blocked with a kind while an
// ask waits, done or error as the turn ends, idle once the user cancelled —
// and an unchanged state is never resent.
func TestProgramStatus(t *testing.T) {
	t.Parallel()
	app, host := statusApp(t)
	const task = "migrate the config"
	idle := report(status.Idle, 0, "")
	if got := statusFrame(app); got != idle {
		t.Fatalf("boot: sent %q", got)
	}
	if got := statusFrame(app); got != "" {
		t.Fatalf("an unchanged state resent %q", got)
	}

	// a turn runs on the user's words
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Task: task})
	working := report(status.Working, 0, task)
	if got := statusFrame(app); got != working {
		t.Fatalf("working: sent %q", got)
	}

	// a permission ask blocks with what it wants, then the answer returns
	// to the still-running turn
	app.testQueue = make(chan tea.Msg, 1)
	answered := make(chan struct{}, 1)
	go func() {
		app.AskPermission(hostapi.PermRequest{Tool: "shell", Subject: perm.SubjectShell, Arg: "make check", Description: "run the check"})
		answered <- struct{}{}
	}()
	app.Update(<-app.testQueue)
	if got := statusFrame(app); got != report(status.Blocked, status.KindPermission, "Run this command? run the check") {
		t.Fatalf("ask: sent %q", got)
	}
	app.answerPerm(true, "user:once")
	<-answered
	if got := statusFrame(app); got != working {
		t.Fatalf("answered: sent %q", got)
	}

	// the trust ask names the root the way the window spells it
	app.trust = &trustPrompt{root: "/proj"}
	if got := statusFrame(app); got != report(status.Blocked, status.KindPermission, "Trust this project? /proj") {
		t.Fatalf("trust: sent %q", got)
	}
	app.trust = nil

	// a plan review blocks as its own kind: it answers a/s/e/esc, not y|n
	app.planAsk = &planAskPrompt{plan: "1. do it"}
	if got := statusFrame(app); got != report(status.Blocked, status.KindPlan, "A plan to review 1. do it") {
		t.Fatalf("plan: sent %q", got)
	}
	app.planAsk = nil

	// a question blocks with its first line, and a batch says how many wait
	app.question = &questionPrompt{qs: []tools.Question{{Question: "which database?"}}, answer: make(chan qAnswer, 1)}
	if got := statusFrame(app); got != report(status.Blocked, status.KindQuestion, "which database?") {
		t.Fatalf("question: sent %q", got)
	}
	app.question = &questionPrompt{qs: []tools.Question{{Question: "which database?"}, {Question: "which driver?"}}, answer: make(chan qAnswer, 1)}
	if got := statusFrame(app); got != report(status.Blocked, status.KindQuestion, "which database? (1 of 2)") {
		t.Fatalf("question batch: sent %q", got)
	}
	app.finishQuestion(false)

	// the turn ends: done carries the reply it left, error its one-line
	// cause, and a user's interrupt reports idle
	host.AppendChat(hostapi.ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "migrated, tests green"})
	host.SeedStream(hostapi.StreamSnapshot{Task: task})
	app.Update(turnDoneMsg{})
	if got := statusFrame(app); got != report(status.Done, 0, "migrated, tests green") {
		t.Fatalf("done: sent %q", got)
	}
	// a turn that only ran tools says nothing of itself
	host.AppendChat(hostapi.ChatMessage{Role: fantasy.MessageRoleUser, Text: task})
	host.SeedStream(hostapi.StreamSnapshot{Task: task})
	app.Update(turnDoneMsg{})
	if got := statusFrame(app); got != report(status.Done, 0, "") {
		t.Fatalf("a wordless turn: sent %q", got)
	}
	host.SeedStream(hostapi.StreamSnapshot{Task: task})
	app.Update(turnDoneMsg{err: "provider refused\n\tand again"})
	if got := statusFrame(app); got != report(status.Error, 0, "provider refused and again") {
		t.Fatalf("error: sent %q", got)
	}
	host.SeedStream(hostapi.StreamSnapshot{})
	app.Update(turnDoneMsg{stopped: true})
	if got := statusFrame(app); got != idle {
		t.Fatalf("interrupted: sent %q", got)
	}
}

// TestPlanStatus: the record and the alert name the plan's opening line,
// its heading marker off, and fall back to the bare label without one.
func TestPlanStatus(t *testing.T) {
	for _, c := range []struct{ plan, want string }{
		{"add retry to the deploy step", "A plan to review  add retry to the deploy step"},
		{"# Migration plan\nthen move the config", "A plan to review  Migration plan"},
		{"```go\npackage main", "A plan to review"},
		{"  \nsecond line first", "A plan to review"},
		{strings.Repeat("x", 300), "A plan to review  " + strings.Repeat("x", 199) + "…"},
	} {
		if got := planStatus(c.plan); got != c.want {
			t.Errorf("planStatus(%q) = %q, want %q", c.plan, got, c.want)
		}
	}
}
