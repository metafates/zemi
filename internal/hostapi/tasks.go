// tasks.go tracks subagent runs (the agent tool, background agents,
// workflows, zemi.agent.run). Each run keeps a live transcript: /agents lists
// the live session's runs and the chat can show one in place of the session.
// A background run's report joins the conversation as a message when it
// finishes (steering a running turn, or starting one).

package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// NOTE: a fixed cap keeps a model from fanning out without bound; make it
// a config knob if people run bigger batches.
const maxBackgroundAgents = 4

// NOTE: finished runs past maxKeptRuns are forgotten oldest first; /agents
// still reads their transcripts from the store.
const maxKeptRuns = 50

// RunStatus is where an agent run stands.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunDone      RunStatus = "done"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

var runStatuses = []RunStatus{RunRunning, RunDone, RunFailed, RunCancelled}

// RunMarks are the Marks a run's or an agent's status word draws, as the
// chat marks a call: queued and paused wait, running spins.
var RunMarks = map[string]string{
	"queued": "wait", "paused": "wait", string(RunRunning): "spin", string(RunDone): "ok",
	string(RunFailed): "err", string(RunCancelled): "stop",
}

// agentRun is one subagent run. The fields above mu never change once the
// run is registered, but cancel, which a continuation renews.
type agentRun struct {
	id, agent, description string
	parent                 string         // the session that started the run
	workflow, phase        string         // the flow that started it, "" for none
	schema                 map[string]any // the report must be a JSON object matching it
	// the model reference it started on
	// NOTE: "" for a run loadRun rebuilt, which continues on its definition's
	// model: sessions keep no model column.
	model      string
	background bool
	cancel     context.CancelFunc

	mu        sync.Mutex
	started   time.Time     // the last start: a continuation counts its own time
	sessionID string        // the run's child session, "" when not recorded
	runner    *agent.Runner // kept after the run ends: send_to continues it with its history
	status    RunStatus
	finished  time.Time
	calls     int
	tokens    int64          // billed input and output of its steps
	cost      float64        // 0 when the model has no prices
	tr        []ChatMessage  // the prompt, tool rows and replies so far
	text      string         // reply text streamed since the last row
	rows      map[string]int // tool call id -> tr index
	queue     []string       // messages waiting for the next step boundary
	closed    bool           // past its last step boundary: a message would be lost
	dropped   int            // queued messages the run ended before reading
}

type taskReg struct {
	mu       sync.Mutex
	runs     map[string]*agentRun
	flows    map[string]*flowRun
	watching bool // a watchFlows ticker runs
}

func newTaskReg() *taskReg {
	return &taskReg{runs: map[string]*agentRun{}, flows: map[string]*flowRun{}}
}

// detached refuses one more detached run past maxBackgroundAgents, agents
// and workflows alike (r.mu held).
func (r *taskReg) detached() error {
	n := 0
	for _, x := range r.runs {
		if x.background && x.isRunning() {
			n++
		}
	}
	for _, f := range r.flows {
		if f.background && f.isRunning() {
			n++
		}
	}
	if n >= maxBackgroundAgents {
		return fmt.Errorf("%d background agents and workflows are already running; wait for one or run this one in the foreground", maxBackgroundAgents)
	}
	return nil
}

// canDetach is detached for a check before the start. NOTE: a run started
// in between makes the start refuse after the caller acted on the check.
func (r *taskReg) canDetach() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.detached()
}

// addFlow registers a workflow run, as add does an agent run.
func (r *taskReg) addFlow(f *flowRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.background {
		if err := r.detached(); err != nil {
			return err
		}
	}
	r.flows[f.id] = f
	if len(r.flows) > maxKeptRuns {
		var oldest *flowRun
		for _, x := range r.flows {
			if !x.isRunning() && (oldest == nil || x.started.Before(oldest.started)) {
				oldest = x
			}
		}
		if oldest != nil {
			delete(r.flows, oldest.id)
		}
	}
	return nil
}

// watch claims the one ticker of the workflow views; false when it runs.
func (r *taskReg) watch() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.watching
	r.watching = true
	return !was
}

// unwatch releases the ticker once no workflow runs; a run registered
// after it claims a new one with watch.
func (r *taskReg) unwatch() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for f := range maps.Values(r.flows) {
		if f.isRunning() {
			return false
		}
	}
	r.watching = false
	return true
}

func (r *taskReg) flow(id string) *flowRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flows[id]
}

// flowList returns every workflow run this process holds, newest first.
func (r *taskReg) flowList() []*flowRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Collect(maps.Values(r.flows))
	slices.SortFunc(out, func(a, b *flowRun) int { return b.started.Compare(a.started) })
	return out
}

// flowsOf returns the workflows parent started, newest first.
func (r *taskReg) flowsOf(parent string) []*flowRun {
	return slices.DeleteFunc(r.flowList(), func(f *flowRun) bool { return f.parent != parent })
}

// add registers a run; a background run past maxBackgroundAgents is
// refused. Counting and inserting share one lock: parallel tool calls race here.
func (r *taskReg) add(run *agentRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run.background {
		if err := r.detached(); err != nil {
			return err
		}
	}
	r.runs[run.id] = run
	if len(r.runs) > maxKeptRuns {
		var oldest *agentRun
		for _, x := range r.runs {
			// never the run just added: loadRun adds old finished runs to view them
			if x != run && !x.isRunning() && (oldest == nil || x.startedAt().Before(oldest.startedAt())) {
				oldest = x
			}
		}
		if oldest != nil {
			delete(r.runs, oldest.id)
		}
	}
	return nil
}

// runList returns every agent run this process holds, newest first.
func (r *taskReg) runList() []*agentRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Collect(maps.Values(r.runs))
	slices.SortFunc(out, func(a, b *agentRun) int { return b.startedAt().Compare(a.startedAt()) })
	return out
}

// runsOf returns the runs parent started, newest first.
func (r *taskReg) runsOf(parent string) []*agentRun {
	return slices.DeleteFunc(r.runList(), func(x *agentRun) bool { return x.parent != parent })
}

// runningFor counts the live runs parent started.
func (r *taskReg) runningFor(parent string) int {
	a, f := r.live(func(p string) bool { return p == parent })
	return a + f
}

// live counts the running agents and workflows of the parents keep takes.
// A workflow is live until its script ends, paused or between agents too,
// and its agents count as it.
func (r *taskReg) live(keep func(parent string) bool) (agents, flows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.runs {
		if x.workflow == "" && keep(x.parent) && x.isRunning() {
			agents++
		}
	}
	for _, f := range r.flows {
		if keep(f.parent) && f.isRunning() {
			flows++
		}
	}
	return agents, flows
}

// stillRunning refuses what live work would undo, naming where to stop it.
func (r *taskReg) stillRunning(keep func(parent string) bool) error {
	a, f := r.live(keep)
	var what, where []string
	if a > 0 {
		what, where = append(what, fmt.Sprintf("%d background %s", a, plural(a, "agent", "agents"))), append(where, "/agents")
	}
	if f > 0 {
		what, where = append(what, fmt.Sprintf("%d %s", f, plural(f, "workflow", "workflows"))), append(where, "/workflows")
	}
	if what == nil {
		return nil
	}
	return Usagef("%s still running; stop %s from %s first", strings.Join(what, " and "), plural(a+f, "it", "them"), strings.Join(where, " or "))
}

// find looks a run up by its id, which is its child session's id too.
func (r *taskReg) find(id string) *agentRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[id]
}

// newRun starts tracking a run of def; the returned context is cancelled
// by ctx and by /agents stop.
func (h *Host) newRun(ctx context.Context, def AgentDef, description, prompt string, background bool) (*agentRun, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &agentRun{
		id: store.ShortID(), agent: def.Name, description: description, parent: h.AgentSessionID(), model: def.Model,
		background: background, started: time.Now(), cancel: cancel, status: RunRunning,
		tr: []ChatMessage{{Role: fantasy.MessageRoleUser, Text: prompt}}, rows: map[string]int{},
	}, ctx
}

func (run *agentRun) startedAt() time.Time {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.started
}

func (run *agentRun) isRunning() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.status == RunRunning
}

// prompt is the task the run was given, "" for a stored run with none.
func (run *agentRun) prompt() string {
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.tr) == 0 || run.tr[0].Role != fantasy.MessageRoleUser {
		return ""
	}
	return run.tr[0].Text
}

func (run *agentRun) usage() (int64, float64) {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.tokens, run.cost
}

// state is the run's list row data.
func (run *agentRun) state() (status RunStatus, calls int, took time.Duration, sessionID string) {
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
		tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: run.text})
	}
	return tr
}

// flush moves the streamed reply into a row (run.mu held).
func (run *agentRun) flush() {
	if strings.TrimSpace(run.text) != "" {
		run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: run.text})
	}
	run.text = ""
}

// message queues text for the run's next step boundary (Steer), or for one
// more turn when it arrives after the last one.
func (run *agentRun) message(text string) error {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.status != RunRunning || run.closed {
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
		run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleUser, Text: m})
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

// continueRun sends one more message to a finished run: it runs again on
// its own history (the runner it kept, or its child session's rows once
// the process forgot it) and answers as a foreground run does. A running
// run takes messages through MessageAgent instead.
func (h *Host) continueRun(ctx context.Context, run *agentRun, prompt string, progress func(string)) (string, error) {
	def, err := h.agentDef(run.agent)
	if err != nil {
		return "", err
	}
	def.Model = cmp.Or(run.model, def.Model)
	run.mu.Lock()
	if run.status == RunRunning {
		run.mu.Unlock()
		return "", fmt.Errorf("agent %s is running: its queue takes the message", run.id)
	}
	run.status, run.closed, run.started, run.finished, run.dropped = RunRunning, false, time.Now(), time.Time{}, 0
	run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleUser, Text: prompt})
	ctx, run.cancel = context.WithCancel(ctx)
	run.mu.Unlock()
	return h.runAgent(ctx, run, def, prompt, progress)
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
		h.scroll(ScrollBottom, "")
		h.refresh()
	}
	return nil
}

func (run *agentRun) finish(h *Host, err error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.flush()
	cut := CallErr
	if errors.Is(err, context.Canceled) {
		cut = CallStopped
	}
	for _, i := range run.rows {
		if run.tr[i].Status == CallRun || run.tr[i].Status == CallWait {
			run.tr[i].Status, run.tr[i].Diff = cut, ""
		}
	}
	run.finished = time.Now()
	switch {
	case errors.Is(err, context.Canceled):
		run.status = RunCancelled
		run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowError, Status: CallCancelled, Text: "Stopped"})
	case err != nil:
		run.status = RunFailed
		run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowError, Text: clipRunes(sanitize.String(h.friendlyError(err)), 400)})
	default:
		run.status = RunDone
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
			report = strings.TrimSpace(reportFailed + h.friendlyError(err) + "\n\n" + text)
		}
		h.deliverReport(run.parent, run.id, def.Name, map[bool]string{true: "finished", false: "failed"}[err == nil], description, report, "")
	})
	return run.id, nil
}

// deliverReport hands a detached run's report to the session that started
// it: to the model, attached behind it, when that session is live, else
// stored there and toasted.
func (h *Host) deliverReport(parent, id, agentName, status, description, report, attached string) {
	if h.AgentSessionID() != parent {
		// the user moved on; do not drop the report into another conversation
		h.notifyReport(id, agentName, status, description, report)
		return
	}
	if err := h.AgentSend(h.reportMessage(h.scratchFor(parent), id, agentName, status, description, report) + attached); err != nil {
		h.Notify(slog.LevelError, "Could not send the report of "+id, h.friendlyError(err))
	}
}

// reportHead is the one line the user sees of a background report; a
// workflow's agent attribute is "workflow" or "workflow:<saved name>", and
// its description names it, since runs of one script differ by no name.
func reportHead(id, agent, status, description string) string {
	if agent == "workflow" || strings.HasPrefix(agent, "workflow:") {
		return fmt.Sprintf("Workflow %s (%s) %s", id, clipRunes(sanitize.Line(description), 60), status)
	}
	return fmt.Sprintf("Background agent %s (%s) %s", id, agent, status)
}

// reportMessage is the user message a report reaches the model as. The whole
// message is for the model, behind the marker where @ refs do not attach (the
// description is model text); the user gets the head as a notice row where the
// model reads it (appendUserRows), so it never lands above the rows of the
// turn it arrives in.
func (h *Host) reportMessage(pad, id, agentName, status, description, report string) string {
	return agent.AttachMarker + fmt.Sprintf("\n<agent-report run=%q agent=%q status=%q description=%q>\n", id, agentName, status, description) + h.truncOutput(pad, "agent", report) + "\n</agent-report>\n</attached-files>"
}

// reportFailed heads a failed run's report for the model; the toast of one
// leaves it out, since the title says failed.
const reportFailed = "the agent failed: "

// notifyReport tells the user of a report the live conversation will not
// read, and stores it in the session that started the run, where a resume
// hands it to the model.
func (h *Host) notifyReport(id, agentName, status, description, report string) {
	parent := ""
	if run := h.tasks.find(id); run != nil {
		parent = run.parent
	} else if f := h.tasks.flow(id); f != nil {
		parent = f.parent
	}
	if parent != "" && h.DB != nil {
		parts, err := json.Marshal(agent.UserMessage(h.reportMessage(h.scratchFor(parent), id, agentName, status, description, report)))
		if err == nil {
			err = h.DB.AddMessage(&store.Message{SessionID: parent, Role: string(fantasy.MessageRoleUser), PartsJSON: string(parts)})
		}
		if err != nil {
			h.Log.Warn("report not stored", "origin", "agents", "run", id, "error", err)
		}
	}
	level := slog.LevelWarn
	if status == "finished" {
		level = slog.LevelInfo
	}
	h.Notify(level, reportHead(id, agentName, status, description), description+"\n"+clipRunes(strings.TrimPrefix(report, reportFailed), 300))
}

// reportTag is a report's opening tag up to its closing bracket.
const reportTag = `<agent-report run="([^"]*)" agent="([^"]*)" status="([^"]*)" description=("(?:[^"\\]|\\.)*")`

// attachedTag opens an element right behind agent.AttachMarker (the tail
// starts past the first one), so a stray line in a file or a send draws no row.
// NOTE: text that carries the marker itself still draws one.
var attachedTag = `(?:\A|` + regexp.QuoteMeta(agent.AttachMarker) + `)\n<`

var (
	reportAttrs = regexp.MustCompile(reportTag)
	reportBody  = regexp.MustCompile(`(?s)` + reportTag + `>\n(.*?)\n</agent-report>`)
	// a zemi.agent.send from a plugin, with its first line
	pluginSent = regexp.MustCompile(attachedTag + `(plugin:[^>\s]+)>\n([^\n]*)`)
	loopSent   = regexp.MustCompile(attachedTag + `loop>\nRun (\d+) of: ([^\n]*)`)
)

// notifyReports tells the user of the queued reports a closing session drops.
func (h *Host) notifyReports(queued []string) {
	for _, m := range queued {
		for _, r := range reportBody.FindAllStringSubmatch(m, -1) {
			desc, _ := strconv.Unquote(r[4])
			h.notifyReport(r[1], r[2], r[3], desc, r[5])
		}
	}
}

// reportHeads rebuilds the notice rows from the reports behind a message's
// agent.AttachMarker (a joined queue may carry several), the plugins' sends,
// the loop's runs and the planner's nudge, so a reply that follows no user
// row says why; none for none of them.
func reportHeads(tail string) []string {
	var heads []string
	for _, m := range reportAttrs.FindAllStringSubmatch(tail, -1) {
		desc, _ := strconv.Unquote(m[4])
		heads = append(heads, reportHead(m[1], m[2], m[3], desc))
	}
	for _, m := range pluginSent.FindAllStringSubmatch(tail, -1) {
		heads = append(heads, m[1]+": "+clipRunes(sanitize.String(m[2]), 200))
	}
	for _, m := range loopSent.FindAllStringSubmatch(tail, -1) {
		heads = append(heads, "loop run "+m[1]+": "+clipRunes(sanitize.String(m[2]), 200))
	}
	if strings.Contains(tail, planNudgeText) {
		heads = append(heads, "Asked the planner to end with the plan or question tool.")
	}
	return heads
}

// stopRun cancels a running agent; "" means the one the chat shows.
func (h *Host) stopRun(id string) error {
	if id == "" {
		id = h.ViewedAgent()
	}
	if id == "" {
		return errors.New("name the run to stop: /agents stop <id> (ids are in /agents)")
	}
	run := h.tasks.find(id)
	if run == nil || !run.isRunning() {
		return Usagef("no running agent %q", id)
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
func (h *Host) ViewAgent(id string) error { return h.ViewAgentFrom(id, "") }

// ViewAgentFrom is ViewAgent from a screen: back is the command that
// reopens it, which LeaveAgentView hands the esc that leaves the run.
func (h *Host) ViewAgentFrom(id, back string) error {
	if _, err := h.Screen(); err != nil {
		return err
	}
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
	h.view, h.viewBack = id, back
	h.tMu.Unlock()
	h.scroll(ScrollBottom, "")
	h.refresh()
	return nil
}

// LeaveAgentView shows the session again and returns the command that
// reopens the screen the run was opened from, "" for none.
func (h *Host) LeaveAgentView() string {
	h.tMu.Lock()
	back := h.viewBack
	h.tMu.Unlock()
	_ = h.ViewAgent("")
	return back
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

// loadRun rebuilds a finished run from its child session.
func (h *Host) loadRun(id string) (*agentRun, error) {
	if h.DB == nil {
		return nil, errNoStore
	}
	sess, err := h.DB.GetSession(id)
	if err != nil || sess.ParentID == "" {
		return nil, Usagef("no agent run %q", id)
	}
	rows, err := h.DB.Messages(id)
	if err != nil {
		return nil, err
	}
	_, tr := historyFrom(rows)
	run := &agentRun{
		id: id, agent: sess.Profile, description: sess.Title, parent: sess.ParentID, cancel: func() {},
		started: time.UnixMilli(sess.CreatedAt), finished: time.UnixMilli(sess.UpdatedAt),
		sessionID: id, status: RunDone, tr: tr,
	}
	_ = h.tasks.add(run)
	return run, nil
}

// agentRunItems are the /agents rows for the live session's runs, the
// stored ones this process does not hold included; enter shows a transcript.
func (h *Host) agentRunItems() []any {
	sid := h.AgentSessionID()
	var running, flows, earlier []any
	row := func(id string, status RunStatus, agent string, took time.Duration, desc, calls string) {
		item := map[string]any{
			"text":    agent,
			"desc":    statusCol(string(status)) + " · " + fmtDuration(took) + calls + " · " + desc,
			"run":     "agents view " + id,
			"section": "Earlier in this session",
			"mark":    RunMarks[string(status)],
		}
		if status == RunRunning {
			item["section"] = "Running"
			running = append(running, item)
			return
		}
		earlier = append(earlier, item)
	}
	// a workflow's agents are one row, its own; another session's live one
	// is listed like its runs below
	held := map[string]bool{}
	for _, f := range slices.DeleteFunc(h.tasks.flowList(), func(f *flowRun) bool { return f.parent != sid && !f.isRunning() }) {
		flows = append(flows, h.flowItem(f, "Workflows"))
	}
	for _, run := range h.tasks.runsOf(sid) {
		status, calls, took, runSID := run.state()
		held[runSID] = true
		if run.workflow == "" {
			row(run.id, status, run.agent, took, run.description, fmt.Sprintf(" · %d tool %s", calls, plural(calls, "call", "calls")))
		}
	}
	// a run another session started (/new leaves a background run behind)
	// still writes files and blocks /rewind, so it is listed, named by that session
	for _, run := range slices.DeleteFunc(h.tasks.runList(), func(x *agentRun) bool { return x.parent == sid || x.workflow != "" || !x.isRunning() }) {
		status, calls, took, _ := run.state()
		row(run.id, status, run.agent, took, run.description+h.fromSession(run.parent), fmt.Sprintf(" · %d tool %s", calls, plural(calls, "call", "calls")))
	}
	if h.DB != nil && sid != "" {
		children, _ := h.DB.ChildSessions(sid)
		inFlows := h.flowAgentIDs(sid)
		for _, c := range children {
			if !held[c.ID] && !inFlows[c.ID] {
				row(c.ID, RunDone, c.Profile, time.Duration(c.UpdatedAt-c.CreatedAt)*time.Millisecond, c.Title, "")
			}
		}
	}
	return slices.Concat(running, flows, earlier)
}

// fromSession ends the desc of a run another session started with that
// session's title, else "another session"; last, so a long title clips
// there and widens no label column.
func (h *Host) fromSession(parent string) string {
	if parent == h.AgentSessionID() {
		return ""
	}
	if h.DB != nil {
		if sess, err := h.DB.GetSession(parent); err == nil && sess.Title != "" {
			return " · from " + clipRunes(sess.Title, 60)
		}
	}
	return " · from another session"
}

// statusCol pads a run's status or a project's trust word to the widest, so
// a row whose word changes moves nothing after it.
func statusCol(s string) string { return fmt.Sprintf("%-9s", s) }
