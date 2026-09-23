package hostapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestTaskRegEviction: a full registry forgets its oldest finished run, never
// the one being added, however old that one is.
func TestTaskRegEviction(t *testing.T) {
	reg := newTaskReg()
	for i := range maxKeptRuns {
		reg.add(&agentRun{id: fmt.Sprint(i), status: "done", started: time.Now()})
	}
	old := &agentRun{id: "old", status: "done", started: time.Now().Add(-time.Hour)}
	reg.add(old)
	if reg.find("old") != old || len(reg.runs) != maxKeptRuns {
		t.Fatalf("added run evicted, or nothing was: %d runs", len(reg.runs))
	}
}

// TestBackgroundAgent: the parent turn launches a slow background agent and
// finishes; the report then arrives as a message that starts a new turn,
// shown in the transcript as a one-line notice.
func TestBackgroundAgent(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{}) // a detached run needs the TUI: headless it runs inline
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reportMsg string
	release := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				<-release
				return textParts("found the thing")
			case toolResult:
				return textParts("started it")
			case strings.Contains(last, "<agent-report "):
				reportMsg = last
				return textParts("the agent reported: " + last[strings.Index(last, "found"):strings.Index(last, "found")+15])
			}
			return toolCallParts("c1", "agent", `{"description":"slow @main.go search","prompt":"find it","agent":"explore","background":true}`)
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if n := h.tasks.running(); n != 1 {
		t.Fatalf("background agents running = %d, want 1", n)
	}
	close(release)
	waitFor(t, func() error {
		if tr := h.Session().Transcript; tr[len(tr)-1].Text != "the agent reported: found the thing" || h.AgentActive() {
			return fmt.Errorf("report never arrived: %+v", tr)
		}
		return nil
	})
	if !strings.Contains(reportMsg, "slow @main.go search") || strings.Contains(reportMsg, "<file path=") {
		t.Fatalf("the model-written description must reach the model but attach nothing: %q", reportMsg)
	}
	// the head is zemi's notice, not a message the user typed, live and on resume
	notices := func() int {
		n := 0
		for _, m := range h.Session().Transcript {
			if m.Role == "user" && strings.HasPrefix(m.Text, "Background agent ") {
				t.Fatalf("report head drawn as a user row: %+v", m)
			}
			if m.Kind == "notice" && strings.HasPrefix(m.Text, "Background agent ") && strings.HasSuffix(m.Text, "(explore) finished.") {
				n++
			}
		}
		return n
	}
	if notices() != 1 {
		t.Fatalf("transcript should show one notice row: %+v", h.Session().Transcript)
	}
	sid := h.AgentSessionID()
	if err := h.ResumeSession(sid); err != nil {
		t.Fatal(err)
	}
	if notices() != 1 {
		t.Fatalf("resume should rebuild the notice row: %+v", h.Session().Transcript)
	}
}

// TestReportNoticeWhereRead: a report that reaches a running turn at a step
// boundary draws its notice under the reply streamed so far, where the
// model reads it, never above the turn's rows.
func TestReportNoticeWhereRead(t *testing.T) {
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	h.agents.mu.Lock()
	h.agents.active = true
	h.agents.textBuf = "so far"
	h.agents.steering = []string{attachMarker + "\n<agent-report run=\"r1\" agent=\"explore\" status=\"finished\" description=\"x\">\nfound\n</agent-report>\n</attached-files>"}
	h.agents.mu.Unlock()
	if msgs := h.deliverSteering(); len(msgs) != 1 {
		t.Fatalf("delivered %d messages", len(msgs))
	}
	tr := h.Session().Transcript
	if n := len(tr); n < 2 || tr[n-2].Text != "so far" || tr[n-1].Kind != "notice" || tr[n-1].Text != reportHead("r1", "explore", "finished") {
		t.Fatalf("transcript tail: %+v", tr)
	}
}

// TestMessageAgent: a message to a running background agent reaches it at its
// next step boundary, or as one more turn when its last step already started,
// and is stored in its child session; a finished run refuses one.
func TestMessageAgent(t *testing.T) {
	viaTool := func(h *Host, id, text string) (string, error) {
		resp, err := h.agentTool().Run(t.Context(), fantasy.ToolCall{ID: "c2", Input: fmt.Sprintf(`{"description":"d","agent":"explore","prompt":%q,"send_to":%q}`, text, id)})
		if err == nil && resp.IsError {
			err = errors.New(resp.Content)
		}
		return resp.Content, err
	}
	viaView := func(h *Host, id, text string) (string, error) {
		if err := h.ViewAgent(id); err != nil {
			return "", err
		}
		return "", h.MessageAgent(id, text)
	}
	run := func(name string, firstCallsTool, continues bool, send func(*Host, string, string) (string, error)) {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.SetUIHooks(&fakeUI{}) // background agents are a TUI feature
			started, release := make(chan struct{}), make(chan struct{})
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
					switch {
					case !strings.Contains(system, "a subagent of zemi"):
						return textParts("ok")
					case last == "look":
						close(started)
						<-release
						if firstCallsTool {
							return toolCallParts("t1", "glob", `{"pattern":"*.none"}`)
						}
						return textParts("first")
					case last == "also this" && !toolResult:
						return textParts("got it")
					}
					return textParts("unexpected: " + last)
				}}, nil
			}
			if _, err := viaTool(h, "", ""); err == nil {
				t.Fatal("an empty prompt must be refused")
			}
			resp, err := h.agentTool().Run(t.Context(), fantasy.ToolCall{ID: "c1", Input: `{"description":"d","agent":"explore","prompt":"look","background":true}`})
			if err != nil || resp.IsError {
				t.Fatalf("start: %v %s", err, resp.Content)
			}
			<-started
			id := h.tasks.list("")[0].id
			out, err := send(h, id, "also this")
			if err != nil || (out != "" && !strings.Contains(out, "Queued")) {
				t.Fatalf("send: %q %v", out, err)
			}
			close(release)
			var r *agentRun
			waitFor(t, func() error {
				if r = h.tasks.find(id); r.isRunning() {
					return errors.New("run never finished")
				}
				return nil
			})
			tr := r.transcript()
			if len(tr) < 3 || tr[len(tr)-2].Text != "also this" || tr[len(tr)-1].Text != "got it" {
				t.Fatalf("transcript: %+v", tr)
			}
			_, _, _, sid := r.state()
			// a run the process forgot is read back by the id the model saw
			forgetRun(h, id)
			stored, err := h.loadRun(id)
			if err != nil || stored.sessionID != sid || !strings.Contains(fmt.Sprint(stored.transcript()), "{user  also this") {
				t.Fatalf("stored run: %v %+v", err, stored)
			}
			// the tool continues a finished run on its history; the view only
			// talks to a running one
			out, err = send(h, id, "too late")
			r = h.tasks.find(id)
			switch {
			case !continues && (err == nil || !strings.Contains(err.Error(), "not running")):
				t.Fatalf("finished run took a message: %v", err)
			case continues && (err != nil || !strings.Contains(out, "unexpected: too late") || !strings.HasPrefix(out, "[run "+id+"]")):
				t.Fatalf("continuation: %q %v", out, err)
			}
			if continues {
				if tr := r.transcript(); r.isRunning() || tr[len(tr)-1].Text != "unexpected: too late" || tr[len(tr)-2].Text != "too late" {
					t.Fatalf("continued transcript: %+v", tr)
				}
				forgetRun(h, id)
				stored, _ := h.loadRun(sid)
				if !strings.Contains(fmt.Sprint(stored.transcript()), "{user  too late") {
					t.Fatalf("continuation not stored: %+v", stored.transcript())
				}
			}
			waitIdle(t, h)
		})
	}
	run("model at a step boundary", true, true, viaTool)
	run("user after the last step", false, false, viaView)
}

// forgetRun drops a run from the registry, as eviction or a restart would.
func forgetRun(h *Host, id string) {
	h.tasks.mu.Lock()
	defer h.tasks.mu.Unlock()
	delete(h.tasks.runs, id)
}

// TestQueueDroppedOnFailure: the run's queue reaches a turn that follows,
// and a failure counts what is left instead of losing it silently.
func TestQueueDroppedOnFailure(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		wantNext string
		wantDrop int
	}{
		{"a finished turn takes the queue", nil, "also this", 0},
		{"a failed turn drops it", errors.New("boom"), "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := &agentRun{status: "running", rows: map[string]int{}}
			if err := run.message("also this"); err != nil {
				t.Fatal(err)
			}
			if next := run.followUp(c.err); next != c.wantNext || run.droppedCount() != c.wantDrop {
				t.Fatalf("next %q, dropped %d", next, run.droppedCount())
			}
		})
	}
}

// TestAgentQueuesWhileBackground: a message sent while a background agent runs
// is queued and starts no turn; once the run finishes the send that follows
// (the report) starts a turn and the queued text reaches the model.
// TestAgentItemsListRunsOfOtherSessions: a run left behind by /new still
// blocks /rewind, so /agents lists it under Running, named by its session.
func TestAgentItemsListRunsOfOtherSessions(t *testing.T) {
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	if err := h.DB.CreateSession("other", "", "Other work", ""); err != nil {
		t.Fatal(err)
	}
	run := &agentRun{id: "agent-9", agent: "explore", description: "look", parent: "other", status: "running", started: time.Now(), cancel: func() {}}
	if err := h.tasks.add(run); err != nil {
		t.Fatal(err)
	}
	rows := fmt.Sprint(h.agentRunItems())
	if !strings.Contains(rows, "section:Running") || !strings.Contains(rows, "look (from Other work)") || !strings.Contains(rows, "agents view agent-9") {
		t.Fatalf("run of another session not listed:\n%s", rows)
	}
}

func TestAgentSendsWhileBackground(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	run, _ := h.newRun(context.Background(), AgentDef{Name: "explore"}, "d", "look", true)
	if err := h.tasks.add(run); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			mu.Lock()
			sent = append(sent, last)
			mu.Unlock()
			return textParts("ok")
		}}, nil
	}
	// a background agent does not hold the conversation: the message runs now
	if err := h.AgentSend("user msg"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	mu.Lock()
	got := strings.Join(sent, "\n")
	mu.Unlock()
	if !strings.Contains(got, "user msg") || h.tasks.running() != 1 {
		t.Fatalf("message waited on the background agent: sent %q, running %d", got, h.tasks.running())
	}
	run.finish(h, nil)
	if err := h.AgentSend("Background agent finished: report"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	var texts []string
	for _, m := range h.Session().Transcript {
		texts = append(texts, m.Text)
	}
	if all := strings.Join(texts, "\n"); !strings.Contains(all, "user msg") || !strings.Contains(all, "Background agent finished: report") {
		t.Fatalf("transcript missing the msg or the report: %q", all)
	}
}
