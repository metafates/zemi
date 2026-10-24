package hostapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/tools"
)

// TestTaskRegEviction: a full registry forgets its oldest finished run, never
// the one being added, however old that one is.
func TestTaskRegEviction(t *testing.T) {
	reg := newTaskReg(config.NewStore())
	for i := range maxKeptRuns {
		reg.add(&agentRun{id: strconv.Itoa(i), status: "done", started: time.Now()})
	}
	old := &agentRun{id: "old", status: "done", started: time.Now().Add(-time.Hour)}
	reg.add(old)
	if reg.find("old") != old || len(reg.runs) != maxKeptRuns {
		t.Fatalf("added run evicted, or nothing was: %d runs", len(reg.runs))
	}
}

// TestViewedRunOutlivesTheRegistry: the view holds the run it shows, so the
// host and the ctx go on reporting it after the registry forgets it past
// maxKeptRuns.
func TestViewedRunOutlivesTheRegistry(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.SetUIHooks(&fakeUI{}) // ViewAgent opens a screen
	shown := &agentRun{id: "shown", agent: "explore", status: "done", started: time.Now()}
	if err := h.tasks.add(shown); err != nil {
		t.Fatal(err)
	}
	if err := h.ViewAgent(shown.id); err != nil {
		t.Fatal(err)
	}
	for i := range maxKeptRuns {
		h.tasks.add(&agentRun{id: "later" + strconv.Itoa(i), status: "done", started: time.Now()})
	}
	if h.tasks.find(shown.id) != nil {
		t.Fatal("the registry kept the viewed run, so nothing was evicted")
	}
	if got := h.viewedRun(); got != shown || h.ViewedAgent() != shown.id {
		t.Fatalf("the view reads %v, %q", got, h.ViewedAgent())
	}
}

// TestBackgroundCap: agent.max_background is the cap on detached runs, read
// at the check, and counted across background agents and workflows alike.
func TestBackgroundCap(t *testing.T) {
	settings := config.NewStore()
	if _, err := settings.SetQuiet("agent.max_background", 2.0); err != nil {
		t.Fatal(err)
	}
	run := func(id string) *agentRun { return &agentRun{id: id, background: true, status: RunRunning} }

	// a workflow and a background agent count toward one cap
	reg := newTaskReg(settings)
	if err := reg.addFlow(&flowRun{id: "f1", background: true, status: RunRunning}, foregroundAdvice); err != nil {
		t.Fatalf("background workflow refused: %v", err)
	}
	if err := reg.add(run("r1")); err != nil {
		t.Fatalf("background run refused: %v", err)
	}
	err := reg.add(run("r2"))
	if err == nil || !strings.Contains(err.Error(), "2 background agents and workflows are already running") {
		t.Fatalf("a run past the cap = %v", err)
	}
	// the cap is read at the check, not copied at boot
	if _, err := settings.SetQuiet("agent.max_background", 3.0); err != nil {
		t.Fatal(err)
	}
	if err := reg.add(run("r2")); err != nil {
		t.Fatalf("a raised cap still refused: %v", err)
	}
	// below one still leaves the one that fits, and what is refused is the
	// count, not the cap
	if _, err := settings.SetQuiet("agent.max_background", 0.0); err != nil {
		t.Fatal(err)
	}
	err = reg.add(run("r4"))
	if err == nil || !strings.Contains(err.Error(), "3 background agents and workflows are already running") {
		t.Fatalf("a run under a lowered cap = %v", err)
	}
	one := newTaskReg(settings)
	if err := one.add(run("r5")); err != nil {
		t.Fatalf("a cap of 0 refused the first run: %v", err)
	}
	if err := one.add(run("r6")); err == nil || !strings.Contains(err.Error(), "1 background agent or workflow is already running") {
		t.Fatalf("a second run under a cap of 0 = %v", err)
	}
	// the refusal names what the caller can still do: the model can run its
	// call in the foreground, a user's or a Lua caller's run has no such
	// version of it
	if err := one.canDetach(); err == nil || !strings.Contains(err.Error(), waitAdvice) {
		t.Fatalf("a check past the cap = %v, want the wait advice", err)
	}
	if err := one.add(run("r7")); err == nil || !strings.Contains(err.Error(), foregroundAdvice) {
		t.Fatalf("a background run past the cap = %v, want the foreground advice", err)
	}
	if err := one.addFlow(&flowRun{id: "f2", background: true, status: RunRunning}, waitAdvice); err == nil || !strings.Contains(err.Error(), waitAdvice) {
		t.Fatalf("a user's background flow past the cap = %v, want the wait advice", err)
	}
}

// TestBackgroundAgent: the parent turn launches a slow background agent and
// finishes; the report then arrives as a message that starts a new turn,
// shown in the transcript as a one-line notice.
func TestBackgroundAgent(t *testing.T) {
	t.Parallel()
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
	if n := h.tasks.runningFor(h.AgentSessionID()); n != 1 {
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
			if m.Kind == "notice" && strings.HasPrefix(m.Text, "Background agent ") && strings.HasSuffix(m.Text, "(slow @main.go search) finished") {
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
	t.Parallel()
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	h.agents.mu.Lock()
	h.agents.active = true
	h.agents.textBuf = "so far"
	h.agents.steering = []string{agent.AttachMarker + "\n<agent-report run=\"r1\" agent=\"explore\" status=\"finished\" description=\"x\">\nfound\n</agent-report>\n</attached-files>"}
	h.agents.mu.Unlock()
	if msgs := h.deliverSteering(); len(msgs) != 1 {
		t.Fatalf("delivered %d messages", len(msgs))
	}
	tr := h.Session().Transcript
	if n := len(tr); n < 2 || tr[n-2].Text != "so far" || tr[n-1].Kind != "notice" || tr[n-1].Text != reportHead(report{id: "r1", agent: "explore", status: "finished", description: "x"}) {
		t.Fatalf("transcript tail: %+v", tr)
	}
}

// TestRunRowFollowsItsRun: a call that handed off keeps its row, and the run's
// own end revises that row and stores how it ended, so neither the chat nor
// /agents reads a finished run as still going or a failed one as done.
func TestRunRowFollowsItsRun(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	if err := h.DB.CreateSession("r1", sid, "look around", "explore"); err != nil {
		t.Fatal(err)
	}
	h.setTranscript("t", []ChatMessage{
		{Kind: RowTool, Tool: tools.ToolAgent, Run: "r1", Status: CallBG},
		{Kind: RowTool, Tool: tools.ToolAgent, Run: "r2", Status: CallBG},
	})
	run := &agentRun{id: "r1", agent: "explore", parent: sid, sessionID: "r1", cancel: func() {}}
	run.finish(h, errors.New("boom"))
	if tr := h.Session().Transcript; tr[0].Status != CallErr || tr[1].Status != CallBG {
		t.Fatalf("rows after the run ended: %s, %s", tr[0].Status, tr[1].Status)
	}
	if sess, err := h.DB.GetSession("r1"); err != nil || sess.RunStatus != string(RunFailed) {
		t.Fatalf("stored run status = %q, %v", sess.RunStatus, err)
	}
	if old, err := h.loadRun("r1"); err != nil || old.status != RunFailed {
		t.Fatalf("loadRun = %+v, %v", old, err)
	}
}

// TestReportHeadsNameTheLine: a plugin send, an update or a loop note is named
// by its first line that has something in it, and one that has none is not
// named at all, so no row is ever drawn with nothing after its tag.
func TestReportHeadsNameTheLine(t *testing.T) {
	for _, c := range []struct {
		name, tail string
		want       []notice
	}{
		{
			name: "a report", tail: agent.Attached(`agent-report run="r1" agent="explore" status="finished" description="look around"`, "found it"),
			want: []notice{{Text: "Background agent r1 (look around) finished", Status: CallOK, Run: "r1"}},
		},
		{
			name: "a failed report", tail: agent.Attached(`agent-report run="r1" agent="explore" status="failed" description="look around"`, "it broke"),
			want: []notice{{Text: "Background agent r1 (look around) failed", Status: CallErr, Run: "r1"}},
		},
		{
			name: "a workflow that finished with a failed agent", tail: agent.Attached(`agent-report run="w1" agent="workflow" status="warn" description="audit"`, "3 of 4 angles came back"),
			want: []notice{{Text: "Workflow w1 (audit) warn", Status: CallWarn}},
		},
		// an element quoted in a body is still the user's text: only one
		// right behind the marker is a report
		{name: "a command's attachment is the user's", tail: agent.Attached("command", `see this: <agent-report run="r9" agent="explore" status="finished" description="d">
not a report
</agent-report>`), want: nil},
		{
			name: "a plugin send after blank lines", tail: agent.Attached("plugin:demo", "\n\nreal line"),
			want: []notice{{Text: "plugin:demo: real line"}},
		},
		{name: "a plugin send with nothing in it", tail: agent.Attached("plugin:demo", ""), want: nil},
		{
			name: "an update after a blank line", tail: agent.Attached(`agent-update run="r1" agent="explore"`, "\nkept going"),
			want: []notice{{Text: "Agent r1 (explore): kept going", Run: "r1"}},
		},
		{name: "a loop note", tail: agent.Attached("loop", "Run 1 of: check a"), want: []notice{{Text: "Loop run 1: check a"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := reportHeads(c.tail); !slices.Equal(got, c.want) {
				t.Fatalf("heads = %q, want %q", got, c.want)
			}
		})
	}
}

// TestMixedMessageRows: a message carrying the user's own words and an agent's
// markup draws a row of each, and taking the queue back returns the typed line
// while the markup stays queued for the model.
func TestMixedMessageRows(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	markup := agent.Attached(`agent-report run="r1" agent="explore" status="finished" description="look around"`, "found")
	h.agents.mu.Lock()
	h.agents.active = true
	h.agents.steering = []string{"look at this" + markup}
	h.agents.mu.Unlock()

	want := []Queued{{Text: "look at this"}, {Text: "Background agent r1 (look around) finished", Notice: true, Run: "r1"}}
	if st := h.AgentStream(); !slices.Equal(st.Steering, want) {
		t.Fatalf("rows = %+v, want %+v", st.Steering, want)
	}
	if q := h.AgentTakeQueue(); len(q) != 1 || q[0] != "look at this" {
		t.Fatalf("handed back = %q", q)
	}
	h.agents.mu.Lock()
	left := slices.Clone(h.agents.steering)
	h.agents.mu.Unlock()
	if len(left) != 1 || !strings.Contains(left[0], "<agent-report ") {
		t.Fatalf("queue after taking it back = %q", left)
	}
}

// TestViewedRunTakesItsQueueBack: up-arrow while the chat shows a run hands
// back what waits for that run, leaving the session's own queue alone, and a
// report riding it stays queued for the model.
func TestViewedRunTakesItsQueueBack(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	run, _ := h.newRun(t.Context(), AgentDef{Name: "explore"}, "look around", "look", true)
	if err := h.tasks.add(run); err != nil {
		t.Fatal(err)
	}
	markup := agent.Attached(`agent-report run="r1" agent="explore" status="finished" description="look around"`, "found")
	h.agents.mu.Lock()
	h.agents.steering = []string{"the session's own"}
	h.agents.mu.Unlock()
	if err := h.MessageAgent(run.id, "look at this"+markup); err != nil {
		t.Fatal(err)
	}
	if err := h.ViewAgent(run.id); err != nil {
		t.Fatal(err)
	}
	if q := h.AgentTakeQueue(); len(q) != 1 || q[0] != "look at this" {
		t.Fatalf("handed back = %q", q)
	}
	if q := run.queued(); len(q) != 1 || !strings.Contains(q[0], "<agent-report ") {
		t.Fatalf("the run's queue after taking it back = %q", q)
	}
	h.agents.mu.Lock()
	left := slices.Clone(h.agents.steering)
	h.agents.mu.Unlock()
	if len(left) != 1 || left[0] != "the session's own" {
		t.Fatalf("the session's queue = %q", left)
	}
	if err := h.ViewAgent(""); err != nil {
		t.Fatal(err)
	}
	if q := h.AgentTakeQueue(); len(q) != 1 || q[0] != "the session's own" {
		t.Fatalf("the session's queue after leaving the view = %q", q)
	}
}

// TestMessageAgent: a message to a running background agent reaches it at its
// next step boundary, or as one more turn when its last step already started,
// and is stored in its child session; a finished run refuses one.
func TestMessageAgent(t *testing.T) {
	viaTool := func(h *Host, id, text string) (string, error) {
		resp, err := h.agentTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "c2", Input: fmt.Sprintf(`{"description":"d","agent":"explore","prompt":%q,"send_to":%q}`, text, id)})
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
			sid := seedSession(t, h)
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
			resp, err := h.agentTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "c1", Input: `{"description":"d","agent":"explore","prompt":"look","background":true}`})
			if err != nil || resp.IsError {
				t.Fatalf("start: %v %s", err, resp.Content)
			}
			<-started
			id := h.tasks.runsOf(sid)[0].id
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
			_, _, _, rsid := r.state()
			// a run the process forgot is read back by the id the model saw
			forgetRun(h, id)
			stored, err := h.loadRun(id)
			if err != nil || stored.sessionID != rsid || !strings.Contains(fmt.Sprint(stored.transcript()), "{user  also this") {
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
				stored, _ := h.loadRun(rsid)
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

// TestFinishNeverDeadlocksNextTurn: finish marks the run's rows with run.mu
// released, because nextTurn holds ag.mu across runningFor's run.mu - the old
// order (run.mu under the mark) deadlocked the pair when a run ended as a
// turn did. One pass of the two races is enough to lock up on the old order
// (the deadlocked pair keeps ag.mu forever, so the next pass fails at once).
func TestFinishNeverDeadlocksNextTurn(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	sid := seedSession(t, h)
	for i := range 200 {
		run := &agentRun{id: strconv.Itoa(i), agent: "explore", parent: sid, status: RunRunning, started: time.Now()}
		if err := h.tasks.add(run); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { defer close(done); run.finish(h, nil) }()
		nested := make(chan struct{})
		go func() {
			// nextTurn's shape: ag.mu held across runningFor's run.mu
			h.agents.mu.Lock()
			h.tasks.runningFor(sid)
			h.agents.mu.Unlock()
			close(nested)
		}()
		select {
		case <-nested:
		case <-time.After(2 * time.Second):
			t.Fatalf("finish deadlocked against nextTurn's lock order (pass %d)", i)
		}
		<-done
	}
}

// TestToolCallEndNeverDeadlocksNextTurn: ToolCallEnd takes the call's
// outcome before run.mu, because nextTurn holds ag.mu across runningFor's
// run.mu - the old order (the outcome under run.mu) deadlocked the pair when
// a subagent's tool ended as the parent's turn did. One pass is enough to
// lock up on the old order (the deadlocked pair keeps ag.mu forever, so the
// next pass fails at once).
func TestToolCallEndNeverDeadlocksNextTurn(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	sid := seedSession(t, h)
	for i := range 200 {
		run := &agentRun{
			id: strconv.Itoa(i), agent: "explore", parent: sid, status: RunRunning, started: time.Now(),
			rows: map[string]int{"c1": 0}, tr: []ChatMessage{{Tool: "read", CallID: "c1"}},
		}
		if err := h.tasks.add(run); err != nil {
			t.Fatal(err)
		}
		h.setToolOutcome("c1", tools.Outcome{Job: "j1"})
		s := &subagentHooks{h: h, run: run, sid: sid}
		done := make(chan struct{})
		go func() { defer close(done); s.ToolCallEnd("c1", "read", fantasy.ToolResultOutputContentText{}, "") }()
		nested := make(chan struct{})
		go func() {
			// nextTurn's shape: ag.mu held across runningFor's run.mu
			h.agents.mu.Lock()
			h.tasks.runningFor(sid)
			h.agents.mu.Unlock()
			close(nested)
		}()
		select {
		case <-nested:
		case <-time.After(2 * time.Second):
			t.Fatalf("ToolCallEnd deadlocked against nextTurn's lock order (pass %d)", i)
		}
		<-done
		if run.tr[0].Job != "j1" {
			t.Fatalf("call row lost its outcome job (pass %d): %q", i, run.tr[0].Job)
		}
	}
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
	t.Parallel()
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	if err := h.DB.CreateSession("other", "", "Other work", ""); err != nil {
		t.Fatal(err)
	}
	run := &agentRun{id: "agent-9", agent: "explore", description: "look", parent: "other", status: "running", started: time.Now(), cancel: func() {}}
	if err := h.tasks.add(run); err != nil {
		t.Fatal(err)
	}
	// a paused workflow of that session is one row, its agent none
	if err := h.tasks.addFlow(&flowRun{id: "0000000f", description: "slow scan", parent: "other", status: RunRunning, paused: true, limit: 1, started: time.Now()}, foregroundAdvice); err != nil {
		t.Fatal(err)
	}
	if err := h.tasks.add(&agentRun{id: "agent-8", agent: "general", description: "scan part1", parent: "other", workflow: "0000000f", status: "running", started: time.Now(), cancel: func() {}}); err != nil {
		t.Fatal(err)
	}
	rows := dumpItems(h.agentRunItems())
	// the row is named by the run's label and its desc keeps the definition;
	// enter opens its preview (the facts and the task), v its transcript
	if !strings.Contains(rows, "section:Running") || !strings.Contains(rows, "text:look") || !strings.Contains(rows, "explore · from Other work") || !strings.Contains(rows, "keys:[desc:view its transcript key:v run:agents view agent-9 danger:true desc:stop key:s run:agents stop agent-9]") || !strings.Contains(rows, "fields:[status running]") || !strings.Contains(rows, "0 tool calls") {
		t.Fatalf("run of another session not listed:\n%s", rows)
	}
	if strings.Contains(rows, "scan part1") || !strings.Contains(rows, "· from Other work id:0000000f mark:wait run:workflows view 0000000f section:Workflows") {
		t.Fatalf("another session's workflow is not one row:\n%s", rows)
	}
	// the running run, under New agent, is selected
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openAgents(); err != nil || ui.opened[0].Verb != "open" || ui.opened[0].Selected != 1 {
		t.Fatalf("agents verb %q, selected %d, %v", ui.opened[0].Verb, ui.opened[0].Selected, err)
	}
}

// TestStoredRunningReadsCancelled: a run_status left 'running' in the store
// is one this process does not hold, so it reads as cancelled - listed under
// Earlier, not Running, and viewed it does not tick the frame.
func TestStoredRunningReadsCancelled(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	child := sid + "c"
	if err := h.DB.CreateSession(child, sid, "stalerun1", "explore"); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.SetSessionRunStatus(child, "running"); err != nil {
		t.Fatal(err)
	}
	rows := dumpItems(h.agentRunItems())
	if strings.Contains(rows, "section:Running") || !strings.Contains(rows, "fields:[status cancelled") {
		t.Fatalf("the stored 'running' row reads as running:\n%s", rows)
	}
	h.SetUIHooks(&fakeUI{}) // ViewAgent opens a screen
	if err := h.ViewAgent(child); err != nil {
		t.Fatal(err)
	}
	if h.ViewedRunRunning() {
		t.Fatal("a stored 'running' row viewed keeps the frame ticking")
	}
}

func TestAgentSendsWhileBackground(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	run, _ := h.newRun(t.Context(), AgentDef{Name: "explore"}, "d", "look", true)
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
	if !strings.Contains(got, "user msg") || h.tasks.runningFor(run.parent) != 1 {
		t.Fatalf("message waited on the background agent: sent %q, running %d", got, h.tasks.runningFor(run.parent))
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

// TestReportStoredWhenSessionMoved: a background report that finishes after
// the user moved on is stored in the session that started the run, so a
// resume shows it and the next send carries it.
func TestReportStoredWhenSessionMoved(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	release := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, _ bool) []fantasy.StreamPart {
			if strings.Contains(system, "a subagent of zemi") {
				<-release
				return textParts("found it")
			}
			return textParts("ok: " + last)
		}}, nil
	}
	parent := seedSession(t, h)
	def, _ := h.agentDef("explore")
	id, err := h.startBackgroundAgent(t.Context(), def, "left behind", "look")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() error {
		rows, _ := h.DB.Messages(parent)
		if len(rows) == 0 || !strings.Contains(rows[len(rows)-1].PartsJSON, "agent-report") {
			return errors.New("report never stored")
		}
		return nil
	})
	if err := h.ResumeSession(parent); err != nil {
		t.Fatal(err)
	}
	if tr := h.Session().Transcript; len(tr) == 0 || tr[len(tr)-1].Text != reportHead(report{id: id, agent: "explore", status: "finished", description: "left behind"}) {
		t.Fatalf("resume lacks the report: %+v", tr)
	}
	if err := h.AgentSend("next"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if tr := h.Session().Transcript; tr[len(tr)-1].Text != "ok: next" {
		t.Fatalf("send after the stored report: %+v", tr)
	}
}

// TestSendTo: a continuation keeps the run's model and its row counts only
// its own calls; another session's run is refused.
func TestSendTo(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{}) // send_to is a TUI feature
	runUser(t, r, `zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "https://llm.internal/v1", api_key = "k", models = { { id = "a", max_output = 1000 }, { id = "b", max_output = 2000 } } })`)
	h.Settings.SetQuiet("models.chat", "p/a")
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, toolResult bool) []fantasy.StreamPart {
			if (last == "work" || last == "more") && !toolResult {
				return toolCallParts("g1", "glob", `{"pattern":"*.none"}`)
			}
			return textParts("done: " + last)
		}}, nil
	}
	seedSession(t, h)
	def, _ := h.agentDef("explore")
	def.Model = "p/b"
	id, _, err := h.runSubagent(t.Context(), def, "d", "work")
	if err != nil {
		t.Fatal(err)
	}
	run := h.tasks.find(id)
	var lines []string
	if _, err := h.continueRun(t.Context(), run, "more", func(l string) { lines = append(lines, l) }); err != nil || len(lines) != 2 ||
		!strings.HasPrefix(lines[0], "explore · 1 tool call · glob") || lines[1] != "explore · 1 tool call" || run.runner.MaxOutputTokens != 2000 {
		t.Fatalf("continuation: %v, rows %q, max output %d", err, lines, run.runner.MaxOutputTokens)
	}
	h.agents.mu.Lock()
	h.agents.sessionID = "other"
	h.agents.mu.Unlock()
	resp, err := h.agentTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "c1", Input: fmt.Sprintf(`{"prompt":"again","send_to":%q}`, id)})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "belongs to another session") {
		t.Fatalf("another session's run: %v %q", err, resp.Content)
	}
}

// TestStopWording: a stopped run reads as stopped, whoever stopped it,
// and /agents stop with nothing to stop says how to name one.
func TestStopWording(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	resp, _ := (&agentTool{h: h}).foreground(t.Context(), "c1", &agentRun{id: "r1", agent: "explore"}, func(func(string)) (string, error) {
		return "", context.Canceled
	})
	if !strings.Contains(resp.Content, "this agent was stopped") {
		t.Errorf("foreground stop: %q", resp.Content)
	}
	if err := h.stopRun(""); err == nil || !strings.Contains(err.Error(), "name the run to stop") {
		t.Errorf("stop without an id: %v", err)
	}
}

// queued copies the messages still waiting, oldest first.
func (run *agentRun) queued() []string {
	run.mu.Lock()
	defer run.mu.Unlock()
	return slices.Clone(run.queue)
}
