// tasks.go tracks subagent runs (the agent tool, background agents,
// workflows, zemi.agent.run). Each run keeps a live transcript: /agents lists
// the live session's runs and the chat can show one in place of the session.
// A background run's report joins the conversation as a message when it
// finishes (steering a running turn, or starting one).

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
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
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// NOTE: finished runs past maxKeptRuns are forgotten oldest first; /agents
// still reads their transcripts from the store.
const maxKeptRuns = 50

// RunStatus is where an agent run stands.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunQueued    RunStatus = "queued"
	RunDone      RunStatus = "done"
	RunWarn      RunStatus = "warn"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

var runStatuses = []RunStatus{RunRunning, RunQueued, RunDone, RunWarn, RunFailed, RunCancelled}

// RunMarks are the Marks a run's or an agent's status word draws, as the
// chat marks a call: queued and paused wait, running spins.
var RunMarks = map[string]MarkKind{
	string(RunQueued): tools.MarkWait, "paused": tools.MarkWait, string(RunRunning): tools.MarkSpin, string(RunDone): tools.MarkOK,
	string(RunWarn): tools.MarkWarn, string(RunFailed): tools.MarkErr, string(RunCancelled): tools.MarkStop,
}

// runState is the lifecycle agentRun and flowRun share: both are registered,
// counted against agent.max_background, handed off and asked whether they
// still run, and the two must not drift.
type runState struct {
	mu       sync.Mutex
	status   RunStatus
	started  time.Time // an agent run's last start: a continuation counts its own time
	finished time.Time
	// background is a start that asked not to be waited on.
	background bool
	// detached is a foreground call's report going to the session as a message
	// instead of back to the caller, set by the call's handoff and read by the
	// run's end: a run finishing in the same instant as the user's message must
	// be one or the other, never both.
	detached bool
}

func (s *runState) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status == RunRunning
}

// counts reports whether the run takes one of agent.max_background's slots: a
// detached report owes the session a message either way, so a run the user's
// message handed off counts as one.
func (s *runState) counts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return (s.background || s.detached) && s.status == RunRunning
}

// detach hands a foreground run to its session, so its end delivers the report
// as a message instead of answering the call that started it. False when the
// run already ended: the caller reports its result inline, so the report is
// never lost and never delivered twice.
func (s *runState) detach() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != RunRunning {
		return false
	}
	s.detached = true
	return true
}

// handedOff reports whether the run's report goes to the session as a message:
// a background start, or a foreground call the user's message took.
func (s *runState) handedOff() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.background || s.detached
}

// agentRun is one subagent run. The fields above runState never change once
// the run is registered, but cancel, which a continuation renews.
type agentRun struct {
	id, agent, description string
	parent                 string         // the session that started the run
	workflow, phase        string         // the flow that started it, "" for none
	schema                 map[string]any // the report must be a JSON object matching it
	// the model reference it started on
	// NOTE: "" for a run loadRun rebuilt, which continues on its definition's
	// model: sessions keep no model column.
	model  string
	cancel context.CancelFunc
	// forkBase is where a fork's own messages start in its history: the
	// conversation it opened with is not its report. 0 for any other run.
	forkBase int

	runState

	sessionID string        // the run's child session, "" when not recorded
	runner    *agent.Runner // kept after the run ends: send_to continues it with its history
	calls     int
	tokens    int64           // billed input and output of its steps
	cost      float64         // 0 when the model has no prices
	tr        []ChatMessage   // the prompt, tool rows and replies so far
	text      string          // reply text streamed since the last row
	reason    string          // thinking streamed since the last row, drawn live until it settles
	thinkAt   time.Time       // when the open reasoning block began, zero when none is
	thought   time.Duration   // closed reasoning blocks since the last row
	rows      map[string]int  // tool call id -> tr index
	rev       uint64          // bumped on every tr write (run.mu held): the chat caches the shown run's rows by it
	queue     []string        // messages waiting for the next step boundary
	closed    bool            // past its last step boundary: a message would be lost
	dropped   int             // queued messages the run ended before reading
	end       chan struct{}   // closed when the run's current attempt ends, fresh per attempt
	updates   []chan struct{} // update waiters, each closed once its send succeeds
	// the attempt in flight after a provider retry, as the session turn's
	// stream keeps it: Retrying sets it, the next streamed part clears it
	retries  int
	retryAt  time.Time
	retryWhy string
}

type taskReg struct {
	mu       sync.Mutex
	settings *config.Store // reads agent.max_background
	runs     map[string]*agentRun
	flows    map[string]*flowRun
	watching bool          // a watchFlows ticker runs
	change   chan struct{} // closed by bump, fresh for the next: waitAll re-snapshots on it
}

func newTaskReg(settings *config.Store) *taskReg {
	return &taskReg{settings: settings, runs: map[string]*agentRun{}, flows: map[string]*flowRun{}}
}

// maxBackground is agent.max_background: how many detached runs may live at
// once. A value below one would refuse every one of them.
func (r *taskReg) maxBackground() int {
	return max(r.settings.Int(config.KeyAgentMaxBackground), 1)
}

// The two answers a refused detach can give. The model can run the same call
// in the foreground; a user asking for a workflow, or a Lua caller, has no
// foreground version of it to fall back on.
const (
	foregroundAdvice = "wait for one, run this one in the foreground, or raise agent.max_background"
	waitAdvice       = "wait for one or raise agent.max_background"
)

// detached refuses one more detached run past maxBackground, agents
// and workflows alike (r.mu held) with the advice its caller can act on.
func (r *taskReg) detached(advice string) error {
	limit := r.maxBackground()
	n := 0
	for _, x := range r.runs {
		if x.counts() {
			n++
		}
	}
	for _, f := range r.flows {
		if f.counts() {
			n++
		}
	}
	if n >= limit {
		return fmt.Errorf("%d %s; %s", n,
			Plural(n, "background agent or workflow is already running", "background agents and workflows are already running"), advice)
	}
	return nil
}

// canDetach is detached for a check before the start; the run it refuses is
// the user's own, with no foreground version of it to fall back on. NOTE: a
// run started in between makes the start refuse after the caller acted on
// the check.
func (r *taskReg) canDetach() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.detached(waitAdvice)
}

// addFlow registers a workflow run, as add does an agent run. advice is
// what a refusal past the cap tells its caller it can still do.
func (r *taskReg) addFlow(f *flowRun, advice string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.background {
		if err := r.detached(advice); err != nil {
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

// running reports whether any agent or workflow run is live.
func (r *taskReg) running() bool {
	a, f := r.live(func(string) bool { return true })
	return a+f > 0
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

// add registers a run; a background run past the cap is refused (detached).
// Counting and inserting share one lock: parallel tool calls race here.
func (r *taskReg) add(run *agentRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run.background {
		if err := r.detached(foregroundAdvice); err != nil {
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

// runningOf returns the agent runs parent is running right now, newest first.
func (r *taskReg) runningOf(parent string) []*agentRun {
	return slices.DeleteFunc(r.runList(), func(x *agentRun) bool {
		return x.parent != parent || !x.isRunning()
	})
}

// runningFlows returns the workflow runs parent is running right now, newest
// first.
func (r *taskReg) runningFlows(parent string) []*flowRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Collect(maps.Values(r.flows))
	out = slices.DeleteFunc(out, func(f *flowRun) bool { return f.parent != parent || !f.isRunning() })
	slices.SortFunc(out, func(a, b *flowRun) int { return b.started.Compare(a.started) })
	return out
}

// bump wakes every waitAll selecting on the registry: a run ended or sent an
// update, so what it re-snapshots may have changed.
func (r *taskReg) bump() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.change != nil {
		close(r.change)
		r.change = nil
	}
}

// onChange is the channel the next bump closes.
func (r *taskReg) onChange() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.change == nil {
		r.change = make(chan struct{})
	}
	return r.change
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
		what, where = append(what, fmt.Sprintf("%d background %s", a, Plural(a, "agent", "agents"))), append(where, "/agents")
	}
	if f > 0 {
		what, where = append(what, fmt.Sprintf("%d %s", f, Plural(f, "workflow", "workflows"))), append(where, "/workflows")
	}
	if what == nil {
		return nil
	}
	return Usagef("%s still running; stop %s from %s first", strings.Join(what, " and "), Plural(a+f, "it", "them"), strings.Join(where, " or "))
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
		id: store.ShortID(), agent: def.Name, description: description, parent: store.MainSessionID, model: def.Model,
		background: background, started: time.Now(), cancel: cancel, status: RunRunning,
		tr: []ChatMessage{{Role: fantasy.MessageRoleUser, Text: prompt}}, rows: map[string]int{},
	}, ctx
}

func (run *agentRun) startedAt() time.Time {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.started
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

// answer is the run's last plain reply row, "" while it has none.
func (run *agentRun) answer() string {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.lastText("")
}

// pending is the one line a running run's preview shows while its answer is
// still pending: "thinking" while a reasoning block is open, else the last
// transcript row's label (its tool, when the model wrote none), else what a
// run that has not reached its first step says.
func (run *agentRun) pending() string {
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.thinkAt.IsZero() {
		return "thinking"
	}
	if n := len(run.tr); n > 0 {
		if s := cmp.Or(run.tr[n-1].Label, string(run.tr[n-1].Tool)); s != "" {
			return clipLine(s, 80)
		}
	}
	return "(no answer yet)"
}

// errText is what a failed or stopped run ended with, "" for any other.
func (run *agentRun) errText() string {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.lastText(RowError)
}

// lastText is the text of the last transcript row of a kind, skipping blanks
// and empty rows (run.mu held).
func (run *agentRun) lastText(kind RowKind) string {
	for _, m := range slices.Backward(run.tr) {
		if m.Kind == kind && strings.TrimSpace(m.Text) != "" {
			return m.Text
		}
	}
	return ""
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
	return run.status, run.calls, run.took(), run.sessionID
}

// label names the run for the user: what the model said it does, or its
// definition's name when it wrote none.
func (run *agentRun) label() string { return cmp.Or(run.description, run.agent) }

// took is how long the run has run: to now while it runs (run.mu held).
func (run *agentRun) took() time.Duration {
	if !run.finished.IsZero() {
		return run.finished.Sub(run.started)
	}
	return time.Since(run.started)
}

// transcript copies the rows, with the reply still streaming last.
func (run *agentRun) transcript() []ChatMessage {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.transcriptLocked()
}

// transcriptLocked is transcript with run.mu held.
func (run *agentRun) transcriptLocked() []ChatMessage {
	tr := slices.Clone(run.tr)
	if strings.TrimSpace(run.text) != "" {
		tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: run.text})
	}
	return tr
}

// view is what the chat draws while this run is shown: its transcript and the
// live stream of its own turn, read under one lock. base is the session's
// snapshot, for what belongs to the process rather than to a turn: the model
// and the usage tally. rev is the transcript's revision, so the
// chat can cache what it rendered of rows this copy has.
func (run *agentRun) view(base StreamSnapshot) (RunStatus, []ChatMessage, StreamSnapshot, uint64) {
	run.mu.Lock()
	defer run.mu.Unlock()
	base.Active, base.Elapsed = run.status == RunRunning, run.took()
	base.Reasoning, base.Thinking, base.Thought = run.reason, !run.thinkAt.IsZero(), run.thinkTime()
	base.Steering = queuedRows(run.queue)
	// the run's calls and its reply are rows of its transcript, and a run
	// compacts on its own never: what it does retry for the provider, it says
	base.Text, base.Tools = "", nil
	base.Retries = run.retries
	if !run.retryAt.IsZero() {
		base.Retrying, base.RetryIn, base.RetryWhy = true, max(time.Until(run.retryAt), 0), run.retryWhy
	}
	base.CheckingGoal = false
	return run.status, run.transcriptLocked(), base, run.rev
}

// touch marks the transcript as written (run.mu held): every tr write goes
// through it, or the chat caches rows the run has since rewritten.
func (run *agentRun) touch() { run.rev++ }

// flush moves the streamed thinking and reply into rows, in the order they
// streamed: the run's flushOutput (run.mu held).
func (run *agentRun) flush() {
	run.endThinking()
	rows := len(run.tr)
	if strings.TrimSpace(run.reason) != "" {
		run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: run.reason, Stat: &TurnStat{ThoughtMS: run.thought.Milliseconds()}})
	}
	if strings.TrimSpace(run.text) != "" {
		run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: run.text})
	}
	run.reason, run.text, run.thought = "", "", 0
	if len(run.tr) != rows {
		run.touch()
	}
}

// beginThinking opens a reasoning block (run.mu held).
func (run *agentRun) beginThinking() {
	if run.thinkAt.IsZero() {
		run.thinkAt = time.Now()
	}
}

// endThinking closes an open reasoning block into the accumulated time.
func (run *agentRun) endThinking() {
	if run.thinkAt.IsZero() {
		return
	}
	run.thought += time.Since(run.thinkAt)
	run.thinkAt = time.Time{}
}

// thinkTime is how long the run has spent reasoning: the closed blocks plus
// the one open now (run.mu held).
func (run *agentRun) thinkTime() time.Duration {
	if run.thinkAt.IsZero() {
		return run.thought
	}
	return run.thought + time.Since(run.thinkAt)
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
	if len(q) > 0 {
		run.touch()
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
	run.touch()
	// a continuation outlives the call like any run: the call that started it
	// hands it off when the user speaks, and cancels it through run.cancel
	ctx, run.cancel = context.WithCancel(context.WithoutCancel(ctx))
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
	run.flush()
	run.retries, run.retryAt, run.retryWhy = 0, time.Time{}, ""
	if run.end != nil {
		close(run.end)
	}
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
	run.touch()
	status := run.status
	if h.DB != nil && run.sessionID != "" {
		// a restart reads this back instead of calling every run done
		if err := h.DB.SetSessionRunStatus(run.sessionID, string(status)); err != nil {
			h.Log.Warn("run status not stored", "origin", "agents", "run", run.id, "error", err)
		}
	}
	run.mu.Unlock()
	h.tasks.bump()
	// markRunRow takes ag.mu, which nextTurn holds across runningFor's
	// run.mu: under run.mu here the pair deadlocks
	h.markRunRow(run.id, callMark(status))
}

// waitEnd is the channel a waiter selects on: closed when the run's current
// attempt ends, nil before it started, so a wait never blocks on nothing.
func (run *agentRun) waitEnd() chan struct{} {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.end
}

// waitUpdate registers a waiter for the run's next update and returns its
// channel with the cancel that deregisters it: wakeUpdates closes every
// waiter's channel once, so a wait never misses a wake and never wakes twice
// for one update.
func (run *agentRun) waitUpdate() (chan struct{}, func()) {
	ch := make(chan struct{})
	run.mu.Lock()
	run.updates = append(run.updates, ch)
	run.mu.Unlock()
	return ch, func() {
		run.mu.Lock()
		defer run.mu.Unlock()
		if i := slices.Index(run.updates, ch); i >= 0 {
			run.updates = slices.Delete(run.updates, i, i+1)
		}
	}
}

// wakeUpdates closes and drops every update waiter, after the update's send
// succeeded.
func (run *agentRun) wakeUpdates() {
	run.mu.Lock()
	waiters := run.updates
	run.updates = nil
	run.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

// callMark is a run's outcome as the chat marks the call that handed off to
// it: the row was written as CallBG and would otherwise read as running.
func callMark(s RunStatus) CallStatus {
	switch s {
	case RunDone:
		return CallOK
	case RunWarn:
		return CallWarn
	case RunCancelled:
		return CallStopped
	default:
		return CallErr
	}
}

// markRunRow revises the row of a run that ended: a handed-off call's row and
// the transcript row it settled as both carry the run's id, and the run's end
// is the only other writer either of them has.
func (h *Host) markRunRow(id string, status CallStatus) {
	if id == "" {
		return
	}
	ag := h.agents
	ag.mu.Lock()
	for i := range ag.tools {
		if ag.tools[i].Run == id && ag.tools[i].Status == CallBG {
			ag.tools[i].Status = status
		}
	}
	ag.mu.Unlock()
	h.tMu.Lock()
	if i := slices.IndexFunc(h.session.Transcript, func(m ChatMessage) bool {
		return m.Kind == RowTool && m.Run == id && m.Status == CallBG
	}); i >= 0 {
		tr := slices.Clone(h.session.Transcript)
		tr[i].Status = status
		// replaced whole, like setTranscript: readers share the old array and
		// the chat widget caches rows by index
		h.session.Transcript = tr
		h.trEdit.Add(1)
	}
	h.tMu.Unlock()
	h.refresh()
}

// startBackgroundAgent launches def on prompt detached from the turn and
// returns the run id.
func (h *Host) startBackgroundAgent(ctx context.Context, def AgentDef, description, prompt string) (string, error) {
	// values survive the detach, so a run knows where it came from; cancellation
	// does not, so it outlives the turn
	run, ctx := h.newRun(context.WithoutCancel(ctx), def, description, prompt, true)
	if err := h.tasks.add(run); err != nil {
		run.cancel()
		return "", err
	}
	h.refresh()     // the run is listed and counted from here on
	h.watchAgents() // and an open /agents follows it
	h.GoSafe("background agent", func() {
		// whatever queued behind this run starts once nothing else holds it,
		// report or not (a cancelled run has none, a finished one's joins the turn)
		defer h.drainQueue()
		text, err := h.runAgent(ctx, run, def, prompt, nil)
		h.deliverAgent(run, text, err)
	})
	return run.id, nil
}

// deliverAgent hands a run's result to the session that started it, as its
// report: a run the user stopped has none.
func (h *Host) deliverAgent(run *agentRun, text string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	body := text
	if err != nil {
		body = strings.TrimSpace(reportFailed + h.friendlyError(err) + "\n\n" + text)
	}
	status := reportFailure
	if err == nil {
		status = reportFinished
	}
	h.deliverReport(run.parent, report{
		id: run.id, agent: run.agent,
		status:      status,
		description: run.description,
		body:        body,
	}, "")
}

// reportStatus is the status word an agent-report element carries: what
// reportMark marks the user's row and what the model reads back.
type reportStatus string

const (
	reportFinished reportStatus = "finished"
	reportFailure  reportStatus = "failed"
	// reportWarn is RunWarn as a report carries it: a workflow that ended
	// with a failed agent or branch inside
	reportWarn reportStatus = reportStatus(RunWarn)
)

// report is a run's report identity and text: the tag a report message names
// them by, and what reportAttrs and reportBody read back.
type report struct {
	id, agent   string
	status      reportStatus
	description string
	body        string
}

// deliverReport hands a detached run's report to the session that started
// it: to the model, attached behind it, when that session is live, else
// stored there and toasted.
func (h *Host) deliverReport(parent string, r report, attached string) {
	// the run is over: every count-derived indicator and list moves now, not
	// at the next frame the user happens to cause
	h.refresh()
	if h.AgentSessionID() != parent {
		// the user moved on; do not drop the report into another conversation
		h.notifyReport(r)
		return
	}
	pad := h.scratchFor(parent)
	h.work(r.id, r.agent, string(r.status), h.truncOutput(pad, "agent", r.body))
	if err := h.AgentSend(h.reportMessage(pad, r) + attached); err != nil {
		h.Notify(slog.LevelError, "Could not send the report of "+r.id, h.friendlyError(err))
	}
}

// updateStatus is the status an update reaches the user under: it names the
// run's line and keeps its toast off the warning level.
const updateStatus = reportStatus("update")

// deliverUpdate hands a running agent's own line to the session that started
// it, as deliverReport hands a finished run's report: onto the turn in
// progress, which reads it at its next step boundary, else stored there and
// toasted.
func (h *Host) deliverUpdate(run *agentRun, text string) error {
	defer h.tasks.bump()
	if h.AgentSessionID() != run.parent {
		h.notifyReport(report{id: run.id, agent: run.agent, status: updateStatus, description: run.description, body: text})
		return nil
	}
	h.work(run.id, run.agent, string(updateStatus), text)
	if err := h.AgentSend(h.updateMessage(h.scratchFor(run.parent), run.id, run.agent, text)); err != nil {
		h.Notify(slog.LevelError, "Could not send the update of "+run.id, h.friendlyError(err))
		return fmt.Errorf("the update did not reach the caller: %s", h.friendlyError(err))
	}
	run.wakeUpdates()
	return nil
}

// work writes one work row into the eternal log and appends the row the chat
// draws for it, so a live window and a rebuilt one draw the same line: the
// draw is firstLine of the logged text, never a second spelling of it.
func (h *Host) work(run, name, status, body string) {
	text := "[" + name + "] " + strings.TrimSpace(body)
	m := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: firstLine(text), Status: workStatus(status), Run: run}
	if h.chat != nil {
		first, err := h.chat.Work(run, name, status, text)
		if err != nil {
			h.logAppendFailed(err)
		} else {
			m.Seq = first
		}
	}
	h.AppendChat(m)
}

// updateMessage is the user message an update reaches the model as, live and
// stored: behind the marker where @ refs do not attach, under the tag the
// chat reads the update's line from.
func (h *Host) updateMessage(pad, id, agentName, text string) string {
	return agent.Attached(fmt.Sprintf("agent-update run=%q agent=%q", id, agentName), h.truncOutput(pad, "agent", text))
}

// reportHead is the one line the user sees of a background report, named by
// the run's label, as the chat names it. A workflow's agent attribute is
// "workflow" or "workflow:<saved name>", and its description names it, since
// runs of one script differ by no name.
func reportHead(r report) string {
	if workflowAgent(r.agent) {
		return fmt.Sprintf("Workflow %s (%s) %s", r.id, clipLine(r.description, 60), string(r.status))
	}
	return fmt.Sprintf("Background agent %s (%s) %s", r.id, clipLine(cmp.Or(r.description, r.agent), 60), string(r.status))
}

// workflowAgent reports whether a report's agent attribute names a workflow,
// whose id is a flow's, not a run the agents view can open.
func workflowAgent(agent string) bool {
	return agent == "workflow" || strings.HasPrefix(agent, "workflow:")
}

// reportMessage is the user message a report reaches the model as. The whole
// message is for the model, behind the marker where @ refs do not attach (the
// description is model text); the user gets the head as a notice row where the
// model reads it (appendUserRows), so it never lands above the rows of the
// turn it arrives in.
func (h *Host) reportMessage(pad string, r report) string {
	return agent.Attached(fmt.Sprintf("agent-report run=%q agent=%q status=%q description=%q", r.id, r.agent, string(r.status), r.description), h.truncOutput(pad, "agent", r.body))
}

// reportFailed heads a failed run's report for the model; the toast of one
// leaves it out, since the title says failed.
const reportFailed = "the agent failed: "

// notifyReport tells the user of a report the live conversation will not
// read, and stores it in the session that started the run, where a resume
// hands it to the model.
func (h *Host) notifyReport(r report) {
	parent := ""
	if run := h.tasks.find(r.id); run != nil {
		parent = run.parent
	} else if f := h.tasks.flow(r.id); f != nil {
		parent = f.parent
	}
	if parent != "" && h.DB != nil {
		pad := h.scratchFor(parent)
		msg := h.reportMessage(pad, r)
		if r.status == updateStatus {
			msg = h.updateMessage(pad, r.id, r.agent, r.body)
		}
		parts, err := json.Marshal(agent.UserMessage(msg), json.Deterministic(true))
		if err == nil {
			err = h.DB.AddMessage(&store.Message{SessionID: parent, Role: string(fantasy.MessageRoleUser), PartsJSON: string(parts)})
		}
		if err != nil {
			h.Log.Warn("report not stored", "origin", "agents", "run", r.id, "error", err)
		}
	}
	level := slog.LevelWarn
	if r.status != reportFailure {
		level = slog.LevelInfo
	}
	h.Notify(level, reportHead(r), r.description+"\n"+clipRunes(strings.TrimPrefix(r.body, reportFailed), 300))
}

// reportTag is a report's opening tag, its element name up to its closing
// bracket: the patterns below open it with attachedTag.
const reportTag = `agent-report run="([^"]*)" agent="([^"]*)" status="([^"]*)" description=("(?:[^"\\]|\\.)*")`

// attachedTag opens an element right behind agent.AttachMarker (the tail
// starts past the first one), so a stray line in a file or a send draws no row;
// no body carries the marker, agent.AttachElement defuses it.
var attachedTag = `(?:\A|` + regexp.QuoteMeta(agent.AttachMarker) + `)\n<`

var (
	reportAttrs = regexp.MustCompile(attachedTag + reportTag)
	// a zemi.agent.send from a plugin, with its first line that has something in
	// it: an element whose text opens with blank lines has no line to name it by
	pluginSent = regexp.MustCompile(attachedTag + `(plugin:[^>\s]+)>\n(?s:(.*?))\n</`)
	// a running agent's own line, with its first
	updateSent = regexp.MustCompile(attachedTag + `agent-update run="([^"]*)" agent="([^"]*)">\n(?s:(.*?))\n</`)
)

// notice is a notice row built from the message behind agent.AttachMarker:
// the line the user reads, the status of the run it names, so a failed
// report's row carries its failure, and that run's id, so the chat's row
// opens its transcript. The Status is empty for a plugin's send, a loop's run
// or a running agent's line, which draw the plain note mark.
type notice struct {
	Text   string
	Status CallStatus
	Run    string
}

// reportMark is a report's status word as its row's mark: a failed run
// failed, a run that finished with a failed agent inside warns, and
// anything else finished.
// workStatus is the mark a work row draws: a final report's outcome, and
// nothing for an update or a plugin send, which no outcome names.
func workStatus(status string) CallStatus {
	switch reportStatus(status) {
	case reportFailure:
		return CallErr
	case reportWarn:
		return CallWarn
	case reportFinished:
		return CallOK
	}
	return ""
}

// reportHeads rebuilds the notice rows from the reports behind a message's
// agent.AttachMarker (a joined queue may carry several), the plugins' sends
// and the loop's runs, so a reply that follows no user row says why; none for
// none of them.
func reportHeads(tail string) []notice {
	var heads []notice
	for _, m := range reportAttrs.FindAllStringSubmatch(tail, -1) {
		desc, _ := strconv.Unquote(m[4])
		run := ""
		if !workflowAgent(m[2]) {
			run = m[1]
		}
		r := report{id: m[1], agent: m[2], status: reportStatus(m[3]), description: desc}
		heads = append(heads, notice{Text: reportHead(r), Status: workStatus(m[3]), Run: run})
	}
	for _, m := range pluginSent.FindAllStringSubmatch(tail, -1) {
		if line := skill.FirstBodyLine(m[2]); line != "" {
			heads = append(heads, notice{Text: m[1] + ": " + clipRunes(sanitize.String(line), 200)})
		}
	}
	for _, m := range updateSent.FindAllStringSubmatch(tail, -1) {
		if line := skill.FirstBodyLine(m[3]); line != "" {
			heads = append(heads, notice{Text: "Agent " + m[1] + " (" + m[2] + "): " + clipRunes(sanitize.String(line), 200), Run: m[1]})
		}
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
	h.tMu.RLock()
	defer h.tMu.RUnlock()
	if h.view == nil {
		return ""
	}
	return h.view.id
}

// ViewedRunRunning reports whether the run the chat shows is still going:
// its clock ticks on the frame while it is, like a turn's.
func (h *Host) ViewedRunRunning() bool {
	run := h.viewedRun()
	return run != nil && run.isRunning()
}

// ViewAgent shows a run's transcript in the chat in place of the session's;
// "" shows the session again. A run this process no longer holds (an
// earlier zemi, or evicted) is read from its child session.
func (h *Host) ViewAgent(id string) error { return h.ViewAgentFrom(id, "", "") }

// ViewAgentFrom is ViewAgent from a screen: back is the command that
// reopens it, which LeaveAgentView hands the esc that leaves the run, and
// menu the overlay left open under the view, whose esc goes to the view
// before it closes the menu.
func (h *Host) ViewAgentFrom(id, back, menu string) error {
	if _, err := h.Screen(); err != nil {
		return err
	}
	var run *agentRun
	if id != "" {
		run = h.tasks.find(id)
		if run == nil {
			var err error
			if run, err = h.loadRun(id); err != nil {
				return err
			}
		}
	}
	h.tMu.Lock()
	h.view, h.viewBack, h.viewMenu = run, back, menu
	h.tMu.Unlock()
	// no scroll either way: a transcript the chat draws opens at its tail and
	// the session keeps a scroll state per view, so esc lands where it was left
	h.refresh()
	return nil
}

// ViewedAgentMenu is the overlay the shown run was raised from, "" for none:
// the one overlay whose esc goes to the view instead of closing it.
func (h *Host) ViewedAgentMenu() string {
	h.tMu.RLock()
	defer h.tMu.RUnlock()
	return h.viewMenu
}

// LeaveAgentView shows the session again and returns the command that
// reopens the screen the run was opened from, "" for none.
func (h *Host) LeaveAgentView() string {
	h.tMu.RLock()
	back := h.viewBack
	h.tMu.RUnlock()
	_ = h.ViewAgent("")
	return back
}

// viewedRun is the run the chat shows, nil for the session. The host holds the
// run itself: the registry forgets a finished run past maxKeptRuns and the chat
// goes on showing it.
func (h *Host) viewedRun() *agentRun {
	h.tMu.RLock()
	defer h.tMu.RUnlock()
	return h.view
}

// refreshViewed redraws only when the run is on screen: runs stream while
// nobody looks.
func (h *Host) refreshViewed(run *agentRun) {
	if h.ViewedAgent() == run.id {
		h.refresh()
	}
}

// storedStatus reads a run_status back from the store, which is written only
// where a run ends: none is done, and 'running' is one this process does not
// hold - a crash left it behind - so it reads as cancelled, as a journal with
// no end line does.
func storedStatus(s string) RunStatus {
	switch st := RunStatus(s); st {
	case "":
		return RunDone
	case RunRunning:
		return RunCancelled
	default:
		return st
	}
}

// loadRun rebuilds a finished run from its child session.
func (h *Host) loadRun(id string) (*agentRun, error) {
	if h.DB == nil {
		return nil, Usagef("no agent run %q: the store is unavailable", id)
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
		id: id, agent: sess.Agent, description: sess.Title, parent: sess.ParentID, cancel: func() {},
		started: time.UnixMilli(sess.CreatedAt), finished: time.UnixMilli(sess.UpdatedAt),
		sessionID: id, status: storedStatus(sess.RunStatus), tr: tr,
	}
	_ = h.tasks.add(run)
	return run, nil
}

// sectionRunning is the section a list puts its live rows in, and how the
// /agents screen knows its first row is one.
const sectionRunning = "Running"

// agentRunItems are the /agents rows for the live session's runs, the
// stored ones this process does not hold included; enter opens the run's
// preview, the same view a workflow's agent gets, and v its transcript.
// A row is named by the run's label and carries the definition it ran in its
// desc, next to the status, the clock and the calls.
func (h *Host) agentRunItems() []OverlayItem {
	sid := h.AgentSessionID()
	var running, flows, earlier []OverlayItem
	row := func(id string, status RunStatus, label string, took time.Duration, desc string, calls int, run *agentRun) {
		item := OverlayItem{
			ID:   id,
			Text: label,
			Mark: RunMarks[string(status)],
		}
		if run != nil {
			tokens, cost := run.usage()
			item.Desc = statusCol(string(status)) + " · " + fmtDuration(took) +
				fmt.Sprintf(" · %d tool %s", calls, Plural(calls, "call", "calls")) + " · " + desc
			item.Fields, item.Detail, item.Parts, item.Empty = h.agentView(agentRow{id: id, def: run.agent, status: status, calls: calls, tokens: tokens, cost: cost, took: took}, run)
		} else {
			// a stored row holds what its session row says; its transcript loads on demand
			item.Desc = statusCol(string(status)) + " · " + fmtDuration(took) + " · " + desc
			item.Fields = fieldList("status", string(status), "time", fmtDuration(took), "agent", desc, "run", id)
		}
		item.Keys = []ItemKey{{Key: "v", Desc: "view its transcript", Run: "agents view " + id + " list"}}
		item.Section = "Earlier in this session"
		if status == RunRunning {
			item.Keys = append(item.Keys, ItemKey{Key: "s", Desc: "stop", Run: "agents stop " + id, Danger: true})
			item.Section = sectionRunning
			running = append(running, item)
			return
		}
		earlier = append(earlier, item)
	}
	// a workflow's agents are one row, its own; another session's live one
	// is listed like its runs below
	held := set.Set[string]{}
	for _, f := range slices.DeleteFunc(h.tasks.flowList(), func(f *flowRun) bool { return f.parent != sid && !f.isRunning() }) {
		flows = append(flows, h.flowItem(f, "Workflows"))
	}
	for _, run := range h.tasks.runsOf(sid) {
		status, calls, took, runSID := run.state()
		held.Add(runSID)
		if run.workflow == "" {
			row(run.id, status, run.label(), took, run.agent, calls, run)
		}
	}
	// a run another session started (/new leaves a background run behind)
	// still writes files and blocks /reset, so it is listed, named by that session
	for _, run := range slices.DeleteFunc(h.tasks.runList(), func(x *agentRun) bool { return x.parent == sid || x.workflow != "" || !x.isRunning() }) {
		status, calls, took, _ := run.state()
		row(run.id, status, run.label(), took, run.agent+h.fromSession(run.parent), calls, run)
	}
	if h.DB != nil && sid != "" {
		children, _ := h.DB.ChildSessions(sid)
		inFlows := h.flowAgentIDs(sid)
		for _, c := range children {
			if !held.Has(c.ID) && !inFlows.Has(c.ID) {
				row(c.ID, storedStatus(c.RunStatus), cmp.Or(c.Title, c.Agent), time.Duration(c.UpdatedAt-c.CreatedAt)*time.Millisecond, c.Agent, 0, nil)
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
