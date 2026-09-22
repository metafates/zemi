// tasks.go tracks subagent runs (the agent tool, background agents,
// workflows, zemi.agent.run). Each run keeps a live transcript: /agents lists
// the live session's runs and the chat can show one in place of the session.
// A background run's report joins the conversation as a message when it
// finishes (steering a running turn, or starting one).
package hostapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// NOTE: a fixed cap keeps a model from fanning out without bound; make it
// a config knob if people run bigger batches.
const maxBackgroundAgents = 4

// NOTE: finished runs past maxKeptRuns are forgotten oldest first; /agents
// still reads their transcripts from the store.
const maxKeptRuns = 50

// agentRun is one subagent run. The fields above mu never change once the
// run is registered.
type agentRun struct {
	id, agent, description string
	parent                 string // the session that started the run
	background             bool
	started                time.Time
	cancel                 context.CancelFunc

	mu        sync.Mutex
	sessionID string // the run's child session, "" when not recorded
	status    string // running, done, failed, cancelled
	finished  time.Time
	calls     int
	tr        []ChatMessage  // the prompt, tool rows and replies so far
	text      string         // reply text streamed since the last row
	rows      map[string]int // tool call id -> tr index
	queue     []string       // messages waiting for the next step boundary
	closed    bool           // past its last step boundary: a message would be lost
	dropped   int            // queued messages the run ended before reading
}

type taskReg struct {
	mu   sync.Mutex
	runs map[string]*agentRun
}

func newTaskReg() *taskReg { return &taskReg{runs: map[string]*agentRun{}} }

// add registers a run; a background run past maxBackgroundAgents is
// refused. Counting and inserting share one lock: parallel tool calls race here.
func (r *taskReg) add(run *agentRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run.background {
		n := 0
		for _, x := range r.runs {
			if x.background && x.isRunning() {
				n++
			}
		}
		if n >= maxBackgroundAgents {
			return fmt.Errorf("%d background agents are already running; wait for one or run this one in the foreground", maxBackgroundAgents)
		}
	}
	r.runs[run.id] = run
	if len(r.runs) > maxKeptRuns {
		var oldest *agentRun
		for _, x := range r.runs {
			// never the run just added: loadRun adds old finished runs to view them
			if x != run && !x.isRunning() && (oldest == nil || x.started.Before(oldest.started)) {
				oldest = x
			}
		}
		if oldest != nil {
			delete(r.runs, oldest.id)
		}
	}
	return nil
}

// list returns the runs parent started, newest first.
func (r *taskReg) list(parent string) []*agentRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*agentRun
	for _, x := range r.runs {
		if x.parent == parent {
			out = append(out, x)
		}
	}
	slices.SortFunc(out, func(a, b *agentRun) int { return b.started.Compare(a.started) })
	return out
}

// running counts every live run; runningFor only those parent started.
func (r *taskReg) running() int {
	return r.count(func(*agentRun) bool { return true })
}

func (r *taskReg) runningFor(parent string) int {
	return r.count(func(x *agentRun) bool { return x.parent == parent })
}

func (r *taskReg) count(keep func(*agentRun) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.runs {
		if keep(x) && x.isRunning() {
			n++
		}
	}
	return n
}

// find looks a run up by its id or its child session id.
func (r *taskReg) find(id string) *agentRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	if x := r.runs[id]; x != nil || id == "" {
		return x
	}
	for _, x := range r.runs {
		if _, _, _, sid := x.state(); sid == id {
			return x
		}
	}
	return nil
}

// newRun starts tracking a run of def; the returned context is cancelled
// by ctx and by /agents stop.
func (h *Host) newRun(ctx context.Context, def AgentDef, description, prompt string, background bool) (*agentRun, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &agentRun{
		id: store.NewID()[:8], agent: def.Name, description: description, parent: h.AgentSessionID(),
		background: background, started: time.Now(), cancel: cancel, status: "running",
		tr: []ChatMessage{{Role: "user", Text: prompt}}, rows: map[string]int{},
	}, ctx
}

func (run *agentRun) isRunning() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.status == "running"
}

// state is the run's list row data.
func (run *agentRun) state() (status string, calls int, took time.Duration, sessionID string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	took = time.Since(run.started)
	if !run.finished.IsZero() {
		took = run.finished.Sub(run.started)
	}
	return run.status, run.calls, took, run.sessionID
}

// transcript copies the rows, with the reply still streaming last.
func (run *agentRun) transcript() []ChatMessage {
	run.mu.Lock()
	defer run.mu.Unlock()
	tr := append([]ChatMessage(nil), run.tr...)
	if strings.TrimSpace(run.text) != "" {
		tr = append(tr, ChatMessage{Role: "assistant", Text: run.text})
	}
	return tr
}

// flush moves the streamed reply into a row (run.mu held).
func (run *agentRun) flush() {
	if strings.TrimSpace(run.text) != "" {
		run.tr = append(run.tr, ChatMessage{Role: "assistant", Text: run.text})
	}
	run.text = ""
}

// message queues text for the run's next step boundary (Steer), or for one
// more turn when it arrives after the last one.
func (run *agentRun) message(text string) error {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.status != "running" || run.closed {
		return fmt.Errorf("agent %s is not running", run.id)
	}
	run.queue = append(run.queue, text)
	return nil
}

// queued copies the messages still waiting, oldest first.
func (run *agentRun) queued() []string {
	run.mu.Lock()
	defer run.mu.Unlock()
	return slices.Clone(run.queue)
}

// take empties the queue into transcript rows, after the reply they
// interrupted (run.mu held).
func (run *agentRun) take() []string {
	q := run.queue
	run.queue = nil
	if len(q) > 0 {
		run.flush()
	}
	for _, m := range q {
		run.tr = append(run.tr, ChatMessage{Role: "user", Text: m})
	}
	return q
}

// takeQueue hands the queued messages to the runner at a step boundary.
func (run *agentRun) takeQueue() []string {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.take()
}

// followUp is what the run sends after a turn ended with err: the messages
// queued after its last step boundary, or "" once it is done, which closes
// the queue in the same lock so no message is accepted and then dropped.
// A failed run cannot read what is still queued; dropped counts it so the
// report says so instead of losing it silently.
func (run *agentRun) followUp(err error) string {
	run.mu.Lock()
	defer run.mu.Unlock()
	if err != nil || len(run.queue) == 0 {
		run.closed = true
		run.dropped += len(run.queue)
		run.queue = nil
		return ""
	}
	return strings.Join(run.take(), "\n\n")
}

// droppedCount is how many queued messages the run never read.
func (run *agentRun) droppedCount() int {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.dropped
}

// MessageAgent queues text for a running agent (the agent tool's send_to, or
// a submit while its transcript is shown); it joins the run at its next step
// boundary and is stored in the run's child session.
func (h *Host) MessageAgent(id, text string) error {
	run := h.tasks.find(id)
	if run == nil {
		return fmt.Errorf("no agent %q", id)
	}
	if err := run.message(text); err != nil {
		return err
	}
	if h.ViewedAgent() == run.id {
		h.scroll("bottom", "")
		h.refresh()
	}
	return nil
}

func (run *agentRun) finish(h *Host, err error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.flush()
	for _, i := range run.rows {
		if run.tr[i].Status == "run" {
			run.tr[i].Status = "err"
		}
	}
	run.finished = time.Now()
	switch {
	case errors.Is(err, context.Canceled):
		run.status = "cancelled"
		run.tr = append(run.tr, ChatMessage{Role: "assistant", Kind: "error", Status: "cancelled", Text: "stopped"})
	case err != nil:
		run.status = "failed"
		run.tr = append(run.tr, ChatMessage{Role: "assistant", Kind: "error", Text: truncRunes(sanitize.String(h.friendlyError(err)), 400)})
	default:
		run.status = "done"
	}
}

// startBackgroundAgent launches def on prompt detached from the turn and
// returns the run id.
func (h *Host) startBackgroundAgent(def AgentDef, description, prompt string) (string, error) {
	run, ctx := h.newRun(context.Background(), def, description, prompt, true)
	if err := h.tasks.add(run); err != nil {
		run.cancel()
		return "", err
	}
	h.GoSafe("background agent", func() {
		// whatever queued behind this run starts once nothing else holds it,
		// report or not (a cancelled run has none, a finished one's joins the turn)
		defer h.drainQueue()
		text, err := h.runAgent(ctx, run, def, prompt, nil)
		if errors.Is(err, context.Canceled) {
			return
		}
		report := text
		if err != nil {
			report = strings.TrimSpace("the agent failed: " + h.friendlyError(err) + "\n\n" + text)
		}
		head := fmt.Sprintf("Background agent %s (%s) %s.", run.id, def.Name, map[bool]string{true: "finished", false: "failed"}[err == nil])
		if h.AgentSessionID() != run.parent {
			// the user moved on; do not drop the report into another conversation
			h.Notify("info", "Background agent "+run.id, head+" "+description+"\n"+truncRunes(report, 300))
			return
		}
		// the description is model text, so it stays behind the marker where @ refs do not attach
		if err := h.AgentSend(head + attachMarker + fmt.Sprintf("\n<agent-report description=%q>\n", description) + h.truncOutput("agent", report) + "\n</agent-report>\n</attached-files>"); err != nil {
			h.Notify("error", "Background agent "+run.id+" failed", err.Error())
		}
	})
	return run.id, nil
}

// stopRun cancels a running agent; "" means the one the chat shows.
func (h *Host) stopRun(id string) error {
	if id == "" {
		id = h.ViewedAgent()
	}
	run := h.tasks.find(id)
	if run == nil || !run.isRunning() {
		return fmt.Errorf("no running agent %q", id)
	}
	run.cancel()
	return nil
}

// ViewedAgent is the id of the run the chat shows, "" for the session.
func (h *Host) ViewedAgent() string {
	h.tMu.Lock()
	defer h.tMu.Unlock()
	return h.view
}

// ViewAgent shows a run's transcript in the chat in place of the session's;
// "" shows the session again. A run this process no longer holds (an
// earlier zemi, or evicted) is read from its child session.
func (h *Host) ViewAgent(id string) error {
	if id != "" {
		run := h.tasks.find(id)
		if run == nil {
			var err error
			if run, err = h.loadRun(id); err != nil {
				return err
			}
		}
		id = run.id
	}
	h.tMu.Lock()
	h.view = id
	h.tMu.Unlock()
	h.scroll("bottom", "")
	h.refresh()
	return nil
}

// viewedRun is the run the chat shows, nil for the session.
func (h *Host) viewedRun() *agentRun {
	if id := h.ViewedAgent(); id != "" {
		return h.tasks.find(id)
	}
	return nil
}

// refreshViewed redraws only when the run is on screen: runs stream while
// nobody looks.
func (h *Host) refreshViewed(run *agentRun) {
	if h.ViewedAgent() == run.id {
		h.refresh()
	}
}

func (h *Host) loadRun(sid string) (*agentRun, error) {
	if h.DB == nil {
		return nil, errors.New("session store unavailable")
	}
	sess, err := h.DB.GetSession(sid)
	if err != nil || sess.ParentID == "" {
		return nil, fmt.Errorf("no agent run %q", sid)
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return nil, err
	}
	_, tr := historyFrom(rows)
	run := &agentRun{
		id: sid, agent: sess.Profile, description: sess.Title, parent: sess.ParentID, cancel: func() {},
		started: time.UnixMilli(sess.CreatedAt), finished: time.UnixMilli(sess.UpdatedAt),
		sessionID: sid, status: "done", tr: tr,
	}
	_ = h.tasks.add(run)
	return run, nil
}

// agentRunItems are the /agents rows for the live session's runs, the
// stored ones this process does not hold included; enter shows a transcript.
func (h *Host) agentRunItems() []any {
	sid := h.AgentSessionID()
	var running, earlier []any
	row := func(id, status, agent string, took time.Duration, desc, calls string) {
		item := map[string]any{
			"text":    agent,
			"desc":    status + " · " + fmtDuration(took) + calls + " · " + desc,
			"run":     "agents view " + id,
			"section": "Earlier in this session",
		}
		if status == "running" {
			item["section"] = "Running"
			running = append(running, item)
			return
		}
		earlier = append(earlier, item)
	}
	held := map[string]bool{}
	for _, run := range h.tasks.list(sid) {
		status, calls, took, runSID := run.state()
		held[runSID] = true
		row(run.id, status, run.agent, took, run.description, fmt.Sprintf(" · %d %s", calls, plural(calls, "call", "calls")))
	}
	if h.DB != nil && sid != "" {
		children, _ := h.DB.ChildSessions(sid)
		for _, c := range children {
			if !held[c.ID] {
				row(c.ID, "done", c.Profile, time.Duration(c.UpdatedAt-c.CreatedAt)*time.Millisecond, c.Title, "")
			}
		}
	}
	return append(running, earlier...)
}
