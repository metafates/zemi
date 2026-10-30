package ui

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

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
	if got := statusFrame(app); got != report(status.Blocked, status.KindPermission, "Run this command?: run the check") {
		t.Fatalf("ask: sent %q", got)
	}
	app.answerPerm(true, "user:once")
	<-answered
	if got := statusFrame(app); got != working {
		t.Fatalf("answered: sent %q", got)
	}

	// a question blocks with its first line
	app.question = &questionPrompt{qs: []tools.Question{{Question: "which database?"}}, answer: make(chan qAnswer, 1)}
	if got := statusFrame(app); got != report(status.Blocked, status.KindQuestion, "which database?") {
		t.Fatalf("question: sent %q", got)
	}
	app.finishQuestion(false)

	// the turn ends: done names the task, error its one-line cause, and a
	// user's interrupt reports idle
	host.SeedStream(hostapi.StreamSnapshot{Task: task})
	app.Update(turnDoneMsg{})
	if got := statusFrame(app); got != report(status.Done, 0, task) {
		t.Fatalf("done: sent %q", got)
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
