// workflow.go runs workflows: Lua scripts that coordinate subagents. The
// model writes them through the workflow tool, users save them under
// workflows/ and run them with /workflows run or zemi.workflow.run.
//
// A script never runs on the interpreter goroutine (DESIGN §4): it waits on
// agents for minutes and is model-authored, so it gets its own small Lua
// state with only the base, string, table and math libraries plus the
// workflow API. Each agent() call yields the script's coroutine; the
// scheduler runs agents concurrently and resumes coroutines as results
// arrive, so parallel() branches progress independently.

package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	zemiruntime "github.com/metafates/zemi/runtime"
)

const (
	workflowConcurrency    = 5
	workflowMaxConcurrency = 16
	// NOTE: a hard cap on agents per run keeps a runaway loop from spending
	// without bound; make it a config knob if real workflows outgrow it.
	workflowMaxAgents = 100
	// workflowLogCap bounds a run's log the same way: a script logging in a
	// loop keeps only its last lines.
	workflowLogCap = 200
	// workflowDrainGrace bounds the wait for in-flight agents after a
	// cancel, so one that ignores its context cannot hold the run open.
	workflowDrainGrace = 2 * time.Second
)

// Workflow is one saved workflow file.
type Workflow struct {
	Name        string
	Description string // the file's leading "--" comment lines
	Path        string
	Source      lua.Origin
}

// workflows lists saved workflows: user files win over builtin ones, then
// plugin ones. A project's .agents/workflows is dead: project Lua never runs.
func (h *Host) workflows() []Workflow {
	var out []Workflow
	builtin, _ := fs.ReadDir(zemiruntime.Workflows, "workflows")
	for _, e := range builtin {
		if name, ok := strings.CutSuffix(e.Name(), ".lua"); ok {
			src, _ := fs.ReadFile(zemiruntime.Workflows, "workflows/"+e.Name())
			out = append(out, Workflow{Name: name, Description: leadingComment(string(src)), Path: "builtin:" + e.Name(), Source: lua.OriginBuiltin})
		}
	}
	h.layerFiles("workflows", ".lua", func(f layerFile) bool {
		src, err := readRegularFile(f.path)
		if err != nil {
			h.RecordErrorOnce("workflows", "", err.Error())
			return false
		}
		out = append(out, Workflow{Name: f.name, Description: leadingComment(src), Path: f.path, Source: f.origin})
		return true
	})
	sortBySource(out, func(w Workflow) lua.Origin { return w.Source })
	return dedupeByName(out, func(w Workflow) string { return w.Name }, nil)
}

func leadingComment(src string) string {
	var lines []string
	for l := range strings.SplitSeq(src, "\n") {
		c, ok := strings.CutPrefix(strings.TrimSpace(l), "--")
		if !ok {
			break
		}
		lines = append(lines, strings.TrimSpace(c))
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

// flowLabel names a run: its description, else the saved workflow's name,
// else the script's leading comment; the chat row names it the same way.
func flowLabel(description, name, src string) string {
	return cmp.Or(description, name, clipRunes(leadingComment(src), 60))
}

// unknownFlowError marks workflowSource's no-such-workflow error: the caller
// that named it raises (zemi.workflow.run), one that only reports the miss
// keeps the nil, err shape.
type unknownFlowError struct{ error }

func (e unknownFlowError) Unwrap() error { return e.error }

func (h *Host) workflowSource(name string) (string, error) {
	var names []string
	for _, w := range h.workflows() {
		if w.Name == name && w.Source == lua.OriginBuiltin {
			b, err := fs.ReadFile(zemiruntime.Workflows, "workflows/"+name+".lua")
			return string(b), err
		}
		if w.Name == name {
			return readRegularFile(w.Path)
		}
		names = append(names, w.Name)
	}
	if len(names) == 0 {
		return "", unknownFlowError{Usagef("no saved workflow %q (save one as workflows/%s.lua)", name, name)}
	}
	return "", unknownFlowError{Usagef("no saved workflow %q (have %s)", name, strings.Join(names, ", "))}
}

// flowRun is one workflow run as the rest of zemi sees it. The scheduler
// (wfRun) owns the script on its goroutine and writes the fields below mu
// under it; the screens, the tool and flowControl read and steer them.
type flowRun struct {
	id, name, description string
	about                 string // a saved workflow's leading comment, "" for an inline script
	parent                string // the session that started it, its agents' parent too
	cancel                context.CancelFunc
	wake                  chan struct{}    // buffered: a control that lets more agents start
	progress              func(f *flowRun) // a foreground run's row, nil for none
	done                  chan struct{}    // closed when the run has ended and reported
	script                string           // the script's file in the parent's pad, "" with no session
	src                   string           // the script as approved and run: the pad's copy is the model's to rewrite
	// journal is the held fd the scheduler appends to, never the path: a
	// stale file can plant a link at the name. nil with no session.
	journal *os.File

	runState

	paused  bool
	limit   int
	phase   string
	log     []string
	agents  []*flowAgent    // in call order
	restart set.Set[string] // agent run ids flowControl cancelled to run again
	result  string          // the script's value: strings as is, anything else JSON
	err     error
	// fails are the parallel branches that failed on their own, as the report
	// shows them: the script gets them in errs and may drop them
	fails []string
}

// flowAgent is one zemi.workflow.agent call: queued until run is set, or
// ended as its row says when a journal answered it.
type flowAgent struct {
	agentRow
	run *agentRun
}

// flowSpec is what startFlow runs.
type flowSpec struct {
	name, description, src string
	args                   any
	concurrency            int // 0 is workflowConcurrency
	background             bool
	from                   string // a run of this session whose journal answers identical calls
	advice                 string // a refusal past the cap: the caller's own, "" the model's foreground one
	progress               func(*flowRun)
	done                   func(*flowRun) // the end of a run whose report goes to the session

	cache map[string][]journalLine // from's finished agents by agentKey, in file order
}

// startFlow registers a run of spec and runs it to its end on its own
// goroutine, closing f.done where it ends. A caller that means to wait does so
// on f.done: the run outlives the call that started it, since a foreground one
// hands off to the user's message (f.detach) and spec.done then delivers its
// report as a message. The error is a start's; the run's own is f.err.
func (h *Host) startFlow(ctx context.Context, spec flowSpec) (*flowRun, error) {
	parent := h.AgentSessionID()
	pad := h.scratchFor(parent)
	var cache map[string][]journalLine
	if spec.from != "" {
		old, err := readJournal(pad, spec.from)
		if err != nil {
			return nil, err
		}
		if spec.args == nil {
			spec.args = old.head.Args
		}
		spec.name, spec.description = cmp.Or(spec.name, old.head.Name), cmp.Or(spec.description, old.head.Description)
		spec.concurrency = cmp.Or(spec.concurrency, old.head.Concurrency)
		cache = old.cache()
	}
	limit := cmp.Or(spec.concurrency, workflowConcurrency)
	if limit < 1 || limit > workflowMaxConcurrency {
		return nil, Usagef("concurrency must be 1-%d, not %d", workflowMaxConcurrency, limit)
	}
	// the flow outlives the turn it was started in, a background one from the
	// start and a foreground one from its handoff, but a run it starts must
	// know where it came from
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f := &flowRun{
		id: store.ShortID(), name: spec.name, description: cmp.Or(flowLabel(spec.description, spec.name, spec.src), "workflow"),
		parent: parent, background: spec.background, cancel: cancel, wake: make(chan struct{}, 1), progress: spec.progress, src: spec.src,
		done: make(chan struct{}), status: RunRunning, limit: limit, started: time.Now(), restart: set.Set[string]{},
	}
	if spec.name != "" {
		f.about = leadingComment(spec.src)
	}
	if pad != "" {
		var err error
		head := journalLine{Workflow: f.id, Name: f.name, Description: f.description, About: f.about, Args: spec.args, Concurrency: limit, Started: f.started.UnixMilli()}
		if f.journal, err = openJournal(pad, f.id, spec.src, head); err != nil {
			cancel()
			return nil, err
		}
		f.script = filepath.Join(pad, flowFile(f.id, ".lua"))
	}
	if err := h.tasks.addFlow(f, cmp.Or(spec.advice, foregroundAdvice)); err != nil {
		cancel()
		if f.journal != nil { // a run that never ran leaves no files for /workflows to list
			f.journal.Close()
			os.Remove(f.script)
			os.Remove(filepath.Join(pad, flowFile(f.id, ".jsonl")))
		}
		return nil, err
	}
	spec.cache = cache
	h.watchFlows()
	h.refresh() // the run is listed and counted from here on
	h.GoSafe("workflow", func() {
		defer h.drainQueue() // this run no longer holds a queued report back
		// closed even when the run or its report panics, so nothing waiting
		// on the run is left hanging; the bump tells the registry's waiters,
		// which see only the change channel, that this run ended
		defer func() {
			close(f.done)
			h.tasks.bump()
		}()
		h.runFlow(ctx, f, spec)
		if spec.done != nil && f.handedOff() {
			spec.done(f)
		}
	})
	return f, nil
}

// wfTask is one coroutine: the script body or a parallel() branch.
type wfTask struct {
	co       *glua.LState
	fn       *glua.LFunction // set until the first resume
	parent   *wfTask
	slot     int
	kids     []*glua.LFunction // set by parallel() before it yields
	waiting  int
	vals     *glua.LTable
	errs     *glua.LTable
	req      *wfAgentReq   // set by agent() before it yields
	agentErr bool          // resumed with an agent's failure last, which failedAgent warns
	args     []glua.LValue // resume values while waiting in ready
}

type wfAgentReq struct {
	def         AgentDef
	prompt, key string
	schema      map[string]any // the report must be a JSON object matching it
	agent       *flowAgent
}

type wfDone struct {
	task *wfTask
	req  *wfAgentReq
	run  *agentRun
	text string
	err  error
}

// wfRun is the scheduler: every field is its goroutine's alone.
type wfRun struct {
	h       *Host
	f       *flowRun
	ctx     context.Context
	L       *glua.LState
	tasks   map[*glua.LState]*wfTask
	queue   []*wfTask
	running int
	started int // agents started, restarts included: done holds one report each
	done    chan wfDone
	cache   map[string][]journalLine // an earlier run's answers, taken in order

	ready []*wfTask // parents whose parallel() branches all finished

	result    glua.LValue
	resultErr error
	over      bool
}

// runFlow runs f's script to its end and records how it ended, a panic's
// end too: a flow left running would block /reset until zemi exits.
func (h *Host) runFlow(ctx context.Context, f *flowRun, spec flowSpec) {
	val, err := "", errors.New("the workflow crashed")
	defer func() { h.markRunRow(f.id, callMark(f.end(val, err))) }()
	val, err = h.runScript(ctx, f, spec)
}

// end records how the run ended and returns its status. A script that went on
// past a failed agent or a branch that failed on its own finished by design
// (the skill says an angle that fails is a wf.log, not a lost run), so the run
// warns rather than failing.
func (f *flowRun) end(val string, err error) RunStatus {
	f.cancel()
	f.mu.Lock()
	f.result, f.err, f.finished = val, err, time.Now()
	switch {
	case err == nil && (f.failedAgent() || len(f.fails) > 0):
		f.status = RunWarn
	case err == nil:
		f.status = RunDone
	case errors.Is(err, context.Canceled):
		f.status = RunCancelled
	default:
		f.status = RunFailed
	}
	status := f.status
	end := journalLine{Status: status, MS: f.finished.Sub(f.started).Milliseconds(), Result: jsonString(val), Err: errString(err)}
	f.mu.Unlock()
	f.note(end)
	if f.journal != nil {
		f.journal.Close()
	}
	f.changed()
	return status
}

// agentStatus is a's status: a live run's state, else its settled one
// (f.mu held).
func (f *flowRun) agentStatus(a *flowAgent) RunStatus {
	if a.run == nil {
		return a.status
	}
	status, _, _, _ := a.run.state()
	return status
}

// failedAgent is whether an agent this run started failed (f.mu held).
func (f *flowRun) failedAgent() bool {
	return slices.ContainsFunc(f.agents, func(a *flowAgent) bool { return f.agentStatus(a) == RunFailed })
}

// finishedAgents counts the agents a rerun would answer at once: the same
// predicate journal.cache replays.
func (f *flowRun) finishedAgents() (done, total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	total = len(f.agents)
	for _, a := range f.agents {
		if f.agentStatus(a) == RunDone {
			done++
		}
	}
	return done, total
}

// runScript is the scheduler loop: it resumes the script's coroutines as
// agents report and control arrives, and returns the script's value.
func (h *Host) runScript(ctx context.Context, f *flowRun, spec flowSpec) (string, error) {
	r := &wfRun{h: h, f: f, ctx: ctx, tasks: map[*glua.LState]*wfTask{}, done: make(chan wfDone, workflowMaxAgents), cache: spec.cache}
	L := r.open(spec.args)
	defer L.Close()
	L.SetContext(ctx)

	// the script's name is what its errors carry: a model edits this file to rerun
	name := "workflow.lua"
	if f.script != "" {
		name = filepath.Base(f.script)
	}
	fn, err := L.Load(strings.NewReader(spec.src), name)
	if err != nil {
		return "", fmt.Errorf("workflow script: %w", err)
	}
	r.start(fn, nil, 0)
	for {
		// parents resume here, not from inside finish: instant branches
		// would otherwise recurse without bound
		for len(r.ready) > 0 && !r.over {
			t := r.ready[0]
			r.ready = r.ready[1:]
			r.resume(t, t.args...)
		}
		if r.over {
			break
		}
		if r.running == 0 && len(r.queue) == 0 {
			return "", errors.New("workflow stalled: every branch is waiting (was zemi.workflow.agent called inside pcall?)")
		}
		select {
		case d := <-r.done:
			r.running--
			r.agentDone(d)
		case <-f.wake:
			r.dispatch()
		case <-ctx.Done():
			r.drainAgents()
			return "", ctx.Err()
		}
		f.changed()
	}
	if r.running > 0 { // a script that returned early leaves agents behind
		f.cancel()
		r.drainAgents()
	}
	if r.resultErr != nil {
		// the VM reports a context check it hit as a Lua error of its own, which
		// would read as a failure: a run whose context is done was stopped
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", r.resultErr
	}
	return luaResultString(r.result), nil
}

// agentDone hands an agent's report to the script, or queues the call
// again when flowControl cancelled it to restart it.
func (r *wfRun) agentDone(d wfDone) {
	f := r.f
	f.mu.Lock()
	again := f.restart.Has(d.run.id) && r.ctx.Err() == nil
	f.restart.Remove(d.run.id)
	f.mu.Unlock()
	stopped := errors.Is(d.err, context.Canceled) && r.ctx.Err() == nil
	if again && stopped && r.started < workflowMaxAgents {
		r.started++
		spent, cost := d.run.usage() // billed already, so the row and the journal keep it
		f.mu.Lock()
		d.req.agent.run = nil // queued again, which a paused run holds
		d.req.agent.tokens += spent
		d.req.agent.cost += cost
		f.mu.Unlock()
		d.task.req = d.req
		r.queue = append([]*wfTask{d.task}, r.queue...)
		r.dispatch()
		return
	}
	r.dispatch()
	status, calls, took, _ := d.run.state()
	tokens, cost := d.run.usage()
	f.mu.Lock()
	tokens, cost = tokens+d.req.agent.tokens, cost+d.req.agent.cost
	f.mu.Unlock()
	line := journalLine{
		Agent: d.run.id, Key: d.req.key, Def: d.req.def.Name, Description: d.run.description, Phase: d.run.phase,
		Status: status, Calls: calls, Tokens: tokens, Cost: cost, MS: took.Milliseconds(),
	}
	switch {
	case stopped:
		line.Err = "stopped by the user"
	case d.err != nil:
		line.Err = r.h.friendlyError(d.err)
	case d.req.schema != nil: // runAgent checked it is the object asked for
		line.Result = jsontext.Value(d.text)
	default:
		line.Result = jsonString(d.text)
	}
	f.note(line)
	d.task.agentErr = line.Err != ""
	if line.Err != "" {
		r.resume(d.task, glua.LNil, glua.LString(line.Err))
	} else {
		r.resume(d.task, answer(r.L, line.Result))
	}
}

// answer is an agent's journaled result as the script gets it: the report,
// or a table for a schema call. A line zemi did not write answers nil.
func answer(L *glua.LState, result jsontext.Value) glua.LValue {
	var v any
	_ = json.Unmarshal(result, &v)
	return lua.FromGo(L, v)
}

// addAgent records a call in the run and tells its row: the count moves now,
// since the scheduler's next report waits on an event that may be far off.
func (f *flowRun) addAgent(a *flowAgent) {
	f.mu.Lock()
	f.agents = append(f.agents, a)
	f.mu.Unlock()
	f.changed()
}

// changed tells the foreground row what moved.
func (f *flowRun) changed() {
	if f.progress != nil {
		f.progress(f)
	}
}

// drainAgents waits for in-flight agents to report after a cancel. They
// share the run's context and normally return at once; one that ignores it
// must not hold the run open, and done is buffered at workflowMaxAgents so
// giving up early leaks nothing.
func (r *wfRun) drainAgents() {
	t := time.NewTimer(workflowDrainGrace)
	defer t.Stop()
	for r.running > 0 {
		select {
		case <-r.done:
			r.running--
		case <-t.C:
			return
		}
	}
}

func luaResultString(v glua.LValue) string {
	switch v := v.(type) {
	case nil, *glua.LNilType:
		return ""
	case glua.LString:
		return string(v)
	}
	g, err := lua.ToGo(v)
	if err != nil {
		return v.String()
	}
	b, err := json.Marshal(g, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return v.String()
	}
	return string(b)
}

// workflowPrelude is the part of zemi.workflow written in Lua. The shape
// errors raise bare: naming the call is the point, and the chunk's own
// position ("<string>") would read as the engine's.
const workflowPrelude = `
local parallel = zemi.workflow.parallel
function zemi.workflow.pipeline(items, ...)
	local stages = { ... }
	if type(items) ~= "table" then
		error("pipeline: pass the items as one table first: pipeline(items, stage, ...)", 0)
	end
	if #stages == 1 and type(stages[1]) == "table" then
		error("pipeline: pass the stages as arguments, not one table: pipeline(items, stage, ...)", 0)
	end
	for i, stage in ipairs(stages) do
		if type(stage) ~= "function" then
			error("pipeline: stage " .. i .. " is not a function", 0)
		end
	end
	local fns = {}
	for i, item in ipairs(items) do
		fns[i] = function()
			local v, err = item, nil
			for _, stage in ipairs(stages) do
				v, err = stage(v, item, i)
				if v == nil then return nil, err end
			end
			return v
		end
	end
	return parallel(fns)
end`

// misnamed are the bare names a model used to other harnesses reaches for:
// a pointed error beats "attempt to call a non-function object".
var misnamed = map[string]string{
	"agent": "zemi.workflow.agent", "parallel": "zemi.workflow.parallel", "pipeline": "zemi.workflow.pipeline",
	"phase": "zemi.workflow.phase", "log": "zemi.workflow.log", "print": "zemi.workflow.log",
	"args": "zemi.workflow.args", "json": "zemi.json", "wf": "zemi.workflow", "workflow": "zemi.workflow",
	"coroutine": "zemi.workflow.agent or zemi.workflow.parallel, which suspend on their own",
}

// open builds the script's own state (DESIGN §4): base, string, table and
// math, and a zemi table holding only workflow and json.
func (r *wfRun) open(args any) *glua.LState {
	L := glua.NewState(glua.Options{SkipOpenLibs: true, CallStackSize: 256})
	r.L = L
	for _, lib := range []struct {
		name string
		fn   glua.LGFunction
	}{{"", glua.OpenBase}, {glua.TabLibName, glua.OpenTable}, {glua.StringLibName, glua.OpenString}, {glua.MathLibName, glua.OpenMath}} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(glua.LString(lib.name))
		L.Call(1, 0)
	}
	for _, name := range []string{"dofile", "loadfile", "module", "require", "_printregs", "xpcall", "newproxy", "print"} {
		L.SetGlobal(name, glua.LNil)
	}
	lua.FixStringLib(L)
	mt := L.NewTable()
	mt.RawSetString("__index", L.NewFunction(func(L *glua.LState) int {
		if to, ok := misnamed[L.CheckString(2)]; ok {
			L.RaiseError("a workflow script has no %s, write %s", L.CheckString(2), to)
		}
		return 0
	}))
	L.SetMetatable(L.G.Global, mt)

	wf := L.NewTable()
	wf.RawSetString("args", lua.FromGo(L, args))
	wf.RawSetString("agent", lua.Suspending(L, L.NewFunction(r.luaAgent)))
	wf.RawSetString("parallel", lua.Suspending(L, L.NewFunction(r.luaParallel)))
	wf.RawSetString("phase", L.NewFunction(func(L *glua.LState) int {
		if _, ok := L.Get(1).(glua.LString); !ok {
			lua.RaiseScript(L, "phase: pass the phase name as a string")
		}
		phase := string(L.Get(1).(glua.LString))
		r.f.mu.Lock()
		r.f.phase = phase
		r.f.mu.Unlock()
		r.f.changed()
		return 0
	}))
	wf.RawSetString("log", L.NewFunction(func(L *glua.LState) int {
		parts := make([]string, 0, L.GetTop())
		for i := 1; i <= L.GetTop(); i++ {
			parts = append(parts, L.ToStringMeta(L.Get(i)).String())
		}
		r.f.mu.Lock()
		r.f.log = append(r.f.log, strings.Join(parts, " "))
		if over := len(r.f.log) - workflowLogCap; over > 0 {
			r.f.log = append(r.f.log[:0], r.f.log[over:]...)
		}
		r.f.mu.Unlock()
		r.f.changed()
		return 0
	}))
	// a member the table lacks is a script bug: the known misspellings point
	// at the right spelling, the rest name what the table has
	wmt := L.NewTable()
	wmt.RawSetString("__index", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(2)
		if name == "args" {
			// RawSetString drops a nil value, so args is absent exactly when
			// the run had none: the read falls through to nil, which the
			// builtins' `type(wf.args) == "string"` guard expects
			return 0
		}
		if to, ok := misnamed[name]; ok {
			lua.RaiseScript(L, "a workflow script has no %s, write %s", name, to)
		}
		lua.RaiseScript(L, "a workflow script has no %s; zemi.workflow has %s", name, strings.Join(wfMembers, ", "))
		return 0
	}))
	L.SetMetatable(wf, wmt)
	zemi := L.NewTable()
	zemi.RawSetString("workflow", wf)
	zemi.RawSetString("json", r.h.jsonNamespace(L))
	zemi.RawSetString("csv", r.h.csvNamespace(L))
	zemi.RawSetString("yaml", r.h.yamlNamespace(L))
	L.SetGlobal("zemi", zemi)
	if err := L.DoString(workflowPrelude); err != nil {
		panic(err) // a constant: every workflow test fails first
	}
	return L
}

// agentOpts are the option keys zemi.workflow.agent reads; any other raises.
var agentOpts = []string{"agent", "description", "model", "phase", "schema"}

// wfMembers are zemi.workflow's members, named when a script reads one the
// table lacks.
var wfMembers = []string{"agent", "parallel", "pipeline", "phase", "log", "args"}

// luaAgent is zemi.workflow.agent(prompt [, opts]): the agent's report, or
// nil and an error.
func (r *wfRun) luaAgent(L *glua.LState) int {
	t := r.tasks[L]
	switch {
	case t == nil:
		lua.RaiseScript(L, "agent() must run inside the workflow")
	case lua.InGoCall(L):
		lua.RaiseScript(L, "agent() cannot wait inside pcall or a library callback such as table.sort; it returns nil, err on failure instead")
	}
	prompt, ok := arg[glua.LString](L, 1, "agent")
	if !ok || strings.TrimSpace(string(prompt)) == "" {
		lua.RaiseScript(L, "agent: prompt is required")
	}
	opts, ok := arg[*glua.LTable](L, 2, "agent")
	if !ok {
		opts = L.NewTable()
	}
	opts.ForEach(func(k, v glua.LValue) {
		if key, ok := k.(glua.LString); !ok || !slices.Contains(agentOpts, string(key)) {
			lua.RaiseScript(L, "agent: unknown option %q; have %s", k, strings.Join(agentOpts, ", "))
		}
	})
	name, desc, model, phase := rawString(L, opts, "agent"), rawString(L, opts, "description"), rawString(L, opts, "model"), rawString(L, opts, "phase")
	def, err := r.h.agentDef(name)
	if err != nil {
		lua.RaiseScript(L, "agent: %v", err)
	}
	if err := r.h.checkModelID(model); err != nil {
		lua.RaiseScript(L, "agent: %v", err)
	}
	if model != "" {
		def.Model = model
	}
	if desc == "" {
		first, _, _ := strings.Cut(string(prompt), "\n")
		desc = clipRunes(first, 40)
	}
	schema := callSchema(L, opts)
	a := &flowAgent{def: def.Name, description: desc, phase: cmp.Or(phase, r.f.phase)}
	key := agentKey(def.Name, model, schema, string(prompt))
	// an identical call an earlier run finished answers at once and starts
	// nothing, so it does not count toward the cap
	if q := r.cache[key]; len(q) > 0 {
		hit := q[0]
		r.cache[key] = q[1:]
		a.id, a.status, a.reused, a.calls, a.tokens, a.cost, a.took = hit.Agent, RunDone, true, hit.Calls, hit.Tokens, hit.Cost, time.Duration(hit.MS)*time.Millisecond
		r.f.addAgent(a)
		hit.Description, hit.Phase, hit.Reused = a.description, a.phase, true
		r.f.note(hit)
		L.Push(answer(L, hit.Result))
		return 1
	}
	if r.started >= workflowMaxAgents {
		lua.RaiseScript(L, "agent: this workflow already started %d agents (the limit)", workflowMaxAgents)
	}
	r.started++
	r.f.addAgent(a)
	reqPrompt := string(prompt)
	if schema != nil { // in the task, never the system prompt, which runs share
		b, _ := json.Marshal(schema, json.Deterministic(true))
		reqPrompt += "\n\nEnd with only a JSON object matching this JSON Schema, no prose and no code fences:\n" + string(b)
	}
	t.req = &wfAgentReq{def: def, prompt: reqPrompt, key: key, schema: schema, agent: a}
	return L.Yield()
}

// callSchema reads agent's schema option: nil for none, and a raise for one
// that is no JSON Schema object whose required fields it declares.
func callSchema(L *glua.LState, opts *glua.LTable) map[string]any {
	tb, ok := opt[*glua.LTable](L, opts, "schema")
	if !ok {
		return nil
	}
	v, _ := lua.ToGo(tb)
	s, _ := v.(map[string]any)
	props, _ := s["properties"].(map[string]any)
	req, _ := s["required"].([]any)
	stray := slices.ContainsFunc(req, func(r any) bool {
		name, _ := r.(string)
		_, ok := props[name]
		return !ok
	})
	if s["type"] != "object" || props == nil || stray {
		lua.RaiseScript(L, "agent: schema must be a JSON Schema object with type \"object\" and properties, and required naming only those; it holds a %s", tb.Type())
	}
	return s
}

// parallel({fn, ...}) runs each function as its own branch and returns the
// results in order plus a table of errors (nil when every branch finished).
func (r *wfRun) luaParallel(L *glua.LState) int {
	t := r.tasks[L]
	if t == nil || lua.InGoCall(L) {
		lua.RaiseScript(L, "parallel() cannot wait inside pcall or a library callback such as table.sort")
	}
	list, ok := L.Get(1).(*glua.LTable)
	if !ok {
		lua.RaiseScript(L, "parallel: pass the branches as one table: parallel({fn, ...})")
	}
	n := list.Len()
	// Every entry becomes a coroutine before the concurrency cap applies,
	// so refuse a batch larger than the run's whole agent budget.
	if n > workflowMaxAgents {
		lua.RaiseScript(L, "parallel: %d branches is more than the %d agents a workflow may start", n, workflowMaxAgents)
	}
	for i := 1; i <= n; i++ {
		fn, ok := list.RawGetInt(i).(*glua.LFunction)
		if !ok {
			lua.RaiseScript(L, "parallel: item %d is not a function", i)
		}
		t.kids = append(t.kids, fn)
	}
	if n == 0 {
		L.Push(L.NewTable())
		return 1
	}
	return L.Yield()
}

func (r *wfRun) start(fn *glua.LFunction, parent *wfTask, slot int) {
	co, _ := r.L.NewThread()
	t := &wfTask{co: co, fn: fn, parent: parent, slot: slot}
	r.tasks[co] = t
	r.resume(t)
}

// resume runs t until it yields or ends and routes what it asked for.
func (r *wfRun) resume(t *wfTask, args ...glua.LValue) {
	fn := t.fn
	t.fn = nil
	st, vals, err := lua.Resume(r.L, t.co, fn, args...)
	if _, raised := errors.AsType[*glua.ApiError](err); err != nil && !raised {
		// a VM panic: the state is not usable afterwards
		r.result, r.resultErr, r.over = glua.LNil, err, true
		return
	}
	switch st {
	case glua.ResumeError:
		r.finish(t, glua.LNil, err)
	case glua.ResumeOK:
		// `return agent(...)` hands back agent's nil, err: a failure, not a nil result
		val, err := glua.LNil, error(nil)
		if len(vals) > 0 {
			val = vals[0]
		}
		if val == glua.LNil && len(vals) > 1 && vals[1] != glua.LNil {
			err = errors.New(vals[1].String())
		}
		r.finish(t, val, err)
	default:
		switch {
		case t.req != nil:
			r.queue = append(r.queue, t)
			r.dispatch()
		case len(t.kids) > 0:
			kids := t.kids
			t.kids, t.waiting = nil, len(kids)
			t.vals, t.errs = r.L.NewTable(), r.L.NewTable()
			for i, k := range kids {
				r.start(k, t, i+1)
			}
		default:
			r.finish(t, glua.LNil, errors.New("coroutine.yield is not available in workflows"))
		}
	}
}

// dispatch starts queued calls while the run is not paused and under its
// limit. Each is a run of the flow's parent session, which /new may have
// left since the flow started.
func (r *wfRun) dispatch() {
	f := r.f
	for len(r.queue) > 0 {
		f.mu.Lock()
		hold := f.paused || r.running >= f.limit
		f.mu.Unlock()
		if hold {
			return
		}
		t := r.queue[0]
		r.queue = r.queue[1:]
		req := t.req
		t.req = nil
		run, ctx := r.h.newRun(r.ctx, req.def, req.agent.description, req.prompt, false)
		run.parent, run.workflow, run.phase, run.schema = f.parent, f.id, req.agent.phase, req.schema
		_ = r.h.tasks.add(run) // only background runs are capped
		f.mu.Lock()
		req.agent.run = run
		f.mu.Unlock()
		r.running++
		r.h.GoSafe("workflow agent", func() {
			// deferred so a panicking agent still reports: the run waits on done
			d := wfDone{task: t, req: req, run: run, err: errors.New("the agent crashed")}
			defer func() { r.done <- d }()
			d.text, d.err = r.h.runAgent(ctx, run, req.def, req.prompt, nil)
		})
	}
}

func (r *wfRun) finish(t *wfTask, val glua.LValue, err error) {
	delete(r.tasks, t.co)
	p := t.parent
	if p == nil {
		r.result, r.resultErr, r.over = val, err, true
		return
	}
	if err != nil {
		p.errs.RawSetInt(t.slot, glua.LString(err.Error()))
		// an agent's failure warns through failedAgent; one the branch raised
		// itself lives only in errs, which the script may drop
		if !t.agentErr {
			f := r.f
			f.mu.Lock()
			f.fails = append(f.fails, fmt.Sprintf("branch %d failed: %s", t.slot, err.Error()))
			f.mu.Unlock()
		}
	} else {
		p.vals.RawSetInt(t.slot, val)
	}
	if p.waiting--; p.waiting == 0 {
		errs := glua.LNil
		if p.errs.MaxN() > 0 {
			errs = p.errs
		}
		p.args = []glua.LValue{p.vals, errs}
		r.ready = append(r.ready, p)
	}
}

// agentRow is a flowAgent as the views read it; status "queued" before it
// starts, cancelled once its run ended first.
type agentRow struct {
	call                        int // its index in the run's agents
	id, def, description, phase string
	status                      RunStatus
	reused                      bool // answered from an earlier run's journal
	calls                       int
	tokens                      int64
	cost                        float64
	took                        time.Duration
}

// rows snapshots the agents in call order (f.mu held).
func (f *flowRun) rows() []agentRow {
	out := make([]agentRow, len(f.agents))
	for i, a := range f.agents {
		r := a.agentRow
		r.call = i
		switch {
		case a.run != nil:
			r.id = a.run.id
			r.status, r.calls, r.took, _ = a.run.state()
			tokens, cost := a.run.usage()
			r.tokens, r.cost = r.tokens+tokens, r.cost+cost // a restart's first attempts are in the row
		case r.status == "" && f.status == RunRunning:
			r.status = RunQueued
		case r.status == "": // the run ended before it started, so it never will
			r.status = RunCancelled
		}
		out[i] = r
	}
	return out
}

// flowView is a run as the views read it.
type flowView struct {
	status         RunStatus
	paused         bool
	limit          int
	phase, lastLog string
	took           time.Duration
	agents         []agentRow
	done, running  int
	tokens         int64
	cost           float64
}

func (f *flowRun) view() flowView {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := flowView{status: f.status, paused: f.paused, limit: f.limit, phase: f.phase, agents: f.rows()}
	if n := len(f.log); n > 0 {
		v.lastLog = f.log[n-1]
	}
	v.took = time.Since(f.started)
	if !f.finished.IsZero() {
		v.took = f.finished.Sub(f.started)
	}
	v.done = ended(v.agents)
	for _, a := range v.agents {
		if a.status == RunRunning {
			v.running++
		}
		if !a.reused { // an earlier run spent it
			v.tokens += a.tokens
			v.cost += a.cost
		}
	}
	return v
}

// state is the run's status word, under the same lock as every other read.
func (f *flowRun) state() RunStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// ended counts the rows neither running nor queued.
func ended(rows []agentRow) int {
	n := 0
	for _, a := range rows {
		if a.status != RunRunning && a.status != RunQueued {
			n++
		}
	}
	return n
}

// summary is the row's detail: the phase, the agents and the last log line
// while it runs; the agents, cost and time once it ended.
func (f *flowRun) summary() string {
	v := f.view()
	var parts []string
	if v.status == RunRunning {
		if v.phase != "" {
			parts = append(parts, v.phase)
		}
		if len(v.agents) > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d agents", v.done, len(v.agents)))
		}
		if v.paused {
			parts = append(parts, "paused")
		}
		if v.lastLog != "" {
			parts = append(parts, clipRunes(v.lastLog, 60))
		}
		return strings.Join(parts, " · ")
	}
	parts = append(parts, fmt.Sprintf("%d %s", len(v.agents), Plural(len(v.agents), "agent", "agents")))
	if v.cost > 0 {
		parts = append(parts, fmtCost(v.cost))
	}
	return strings.Join(append(parts, fmtDuration(v.took)), " · ")
}

// phases groups rows by phase in the order reached; a row with none goes
// under the run's description.
func (f *flowRun) phases(rows []agentRow) (names []string, by map[string][]agentRow) {
	by = map[string][]agentRow{}
	for _, a := range rows {
		p := cmp.Or(a.phase, f.description)
		if by[p] == nil {
			names = append(names, p)
		}
		by[p] = append(by[p], a)
	}
	return names, by
}

// statusText is what the tool's status action answers.
func (f *flowRun) statusText() string {
	v := f.view()
	var b strings.Builder
	fmt.Fprintf(&b, "workflow %s %s: %s · %d/%d agents · %d running · %d at once", f.id, f.description, flowWord(v), v.done, len(v.agents), v.running, v.limit)
	names, by := f.phases(v.agents)
	for _, p := range names {
		fmt.Fprintf(&b, "\n%s %d/%d", p, ended(by[p]), len(by[p]))
		for _, a := range by[p] {
			fmt.Fprintf(&b, "\n  %s %s %s: %s", cmp.Or(a.id, "-"), a.def, a.description, a.status)
			if a.status != RunQueued {
				fmt.Fprintf(&b, ", %d %s, %s", a.calls, Plural(a.calls, "call", "calls"), fmtDuration(a.took))
			}
		}
	}
	return b.String()
}

// flowAction is what flowControl does to a live run.
type flowAction string

const (
	actStatus      flowAction = "status"
	actWait        flowAction = "wait"
	actPause       flowAction = "pause"
	actResume      flowAction = "resume"
	actStop        flowAction = "stop"
	actConcurrency flowAction = "concurrency" // n is the new limit
	actRestart     flowAction = "restart"     // agent is the agent run id
)

// flowControl acts on run id and says what it did: the one control path
// of the tool, /workflows and its screen. Pause lets running agents finish
// and starts no more; stop cancels the run; restart cancels one running
// agent and queues the same call again.
func (h *Host) flowControl(id string, act flowAction, n int, agent string) (string, error) {
	f := h.tasks.flow(id)
	if f == nil {
		return "", Usagef("no workflow %q", id)
	}
	if act == actStatus {
		return f.statusText(), nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != RunRunning {
		return "", Usagef("workflow %s is not running", id)
	}
	switch act {
	case actPause:
		f.paused = true
		return "Paused workflow " + id + ": running agents finish, no new ones start", nil
	case actResume:
		f.paused = false
	case actStop:
		f.cancel()
		return "Stopped workflow " + id, nil
	case actConcurrency:
		if n < 1 || n > workflowMaxConcurrency {
			return "", Usagef("concurrency must be 1-%d, not %d", workflowMaxConcurrency, n)
		}
		f.limit = n
	case actRestart:
		i := slices.IndexFunc(f.agents, func(a *flowAgent) bool { return a.run != nil && a.run.id == agent })
		if i < 0 || !f.agents[i].run.isRunning() {
			return "", Usagef("no running agent %q in workflow %s", agent, id)
		}
		f.restart.Add(agent)
		f.agents[i].run.cancel()
		return "Restarting agent " + agent, nil
	default:
		return "", Usagef("unknown workflow action %q", act)
	}
	select { // more agents may start now
	case f.wake <- struct{}{}:
	default:
	}
	if act == actResume {
		return "Resumed workflow " + id, nil
	}
	return fmt.Sprintf("Workflow %s runs %d agents at once", id, n), nil
}

// the run's files: its script and journal in the parent session's pad

// journalLine is one line of a run's journal: the header (Workflow set),
// an agent that ended (Agent set), or the end.
type journalLine struct {
	Workflow    string         `json:"workflow,omitempty"`
	Name        string         `json:"name,omitempty"`
	About       string         `json:"about,omitempty"`
	Args        any            `json:"args,omitempty"`
	Concurrency int            `json:"concurrency,omitzero"`
	Started     int64          `json:"started,omitzero"`
	Agent       string         `json:"agent,omitempty"`
	Key         string         `json:"key,omitempty"`
	Def         string         `json:"def,omitempty"`
	Description string         `json:"description,omitempty"`
	Phase       string         `json:"phase,omitempty"`
	Status      RunStatus      `json:"status,omitempty"`
	Reused      bool           `json:"reused,omitzero"`
	Calls       int            `json:"calls,omitzero"`
	Tokens      int64          `json:"tokens,omitzero"`
	Cost        float64        `json:"cost,omitzero"`
	MS          int64          `json:"ms,omitzero"`
	Result      jsontext.Value `json:"result,omitempty"`
	Err         string         `json:"err,omitempty"`
}

func jsonString(s string) jsontext.Value {
	b, _ := json.Marshal(s, json.Deterministic(true))
	return b
}

// agentKey is what makes two agent calls identical for a rerun.
func agentKey(def, model string, schema map[string]any, prompt string) string {
	s, _ := json.Marshal(schema, json.Deterministic(true)) // map keys sort: one schema, one text
	sum := sha256.Sum256([]byte(def + "\x00" + model + "\x00" + string(s) + "\x00" + prompt))
	return hex.EncodeToString(sum[:])
}

func flowFile(id, ext string) string { return "workflow-" + id + ext }

// padOpen opens name in pad: a create (O_EXCL) fails on anything already
// at the name, a read follows what is there.
func padOpen(pad, name string, flag int) (*os.File, error) {
	p, err := perm.Lresolve(filepath.Join(pad, name))
	if err != nil {
		return nil, err
	}
	return p.Open(flag, 0o600)
}

// openJournal creates the run's script and journal, neither of which may
// exist yet, and returns the journal with its header written.
func openJournal(pad, id, src string, head journalLine) (*os.File, error) {
	if err := os.MkdirAll(pad, 0o700); err != nil {
		return nil, err
	}
	create := os.O_CREATE | os.O_EXCL | os.O_WRONLY
	s, err := padOpen(pad, flowFile(id, ".lua"), create)
	if err != nil {
		return nil, err
	}
	_, err = s.WriteString(src)
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	j, err := padOpen(pad, flowFile(id, ".jsonl"), create|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(head, json.Deterministic(true))
	if _, err := j.Write(append(b, '\n')); err != nil {
		j.Close()
		return nil, err
	}
	return j, nil
}

// note appends a line to the journal; only the scheduler's goroutine
// writes it. A lost line costs a rerun one reused answer, nothing more.
func (f *flowRun) note(l journalLine) {
	if f.journal == nil {
		return
	}
	b, _ := json.Marshal(l, json.Deterministic(true))
	_, _ = f.journal.Write(append(b, '\n'))
}

// readPadFile reads a regular file of at most 1 MiB from pad; O_NONBLOCK
// keeps a planted fifo from holding the open.
func readPadFile(pad, name string) ([]byte, error) {
	f, err := padOpen(pad, name, os.O_RDONLY|syscall.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxAgentFileBytes {
		return nil, fmt.Errorf("%s: not a regular file of at most %d bytes", f.Name(), maxAgentFileBytes)
	}
	return io.ReadAll(io.LimitReader(f, maxAgentFileBytes))
}

// journal is a run's journal read back; end is zero for a run that never
// ended.
type journal struct {
	head        journalLine
	agents      []journalLine
	end         journalLine
	pad, script string
}

// readJournal reads run id's journal from pad. Everything in it is model
// grade: the run may have written it.
func readJournal(pad, id string) (journal, error) {
	if !store.IDShape.MatchString(id) {
		return journal{}, Usagef("%q is not a workflow run id", id)
	}
	if pad == "" {
		return journal{}, Usagef("no session: workflow runs keep their files in its scratchpad")
	}
	b, err := readPadFile(pad, flowFile(id, ".jsonl"))
	if err != nil {
		return journal{}, err
	}
	j := journal{pad: pad, script: filepath.Join(pad, flowFile(id, ".lua"))}
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		var l journalLine
		switch {
		case json.Unmarshal(line, &l) != nil: // a torn last line
		case l.Workflow != "":
			j.head = l
		case l.Agent != "":
			j.agents = append(j.agents, l)
		default:
			j.end = l
		}
	}
	if j.head.Workflow != id {
		return journal{}, fmt.Errorf("%s: no workflow header", flowFile(id, ".jsonl"))
	}
	return j, nil
}

// cache is the answers of the agents that finished, by agentKey in file
// order: a repeated identical call takes them in turn, then runs live.
func (j journal) cache() map[string][]journalLine {
	c := map[string][]journalLine{}
	for _, a := range j.agents {
		if a.Status == RunDone && a.Key != "" {
			c[a.Key] = append(c[a.Key], a)
		}
	}
	return c
}

// loadFlow rebuilds a finished run of session sid from its journal, for
// the screens; one with no end line reads as stopped.
func (h *Host) loadFlow(sid, id string) (*flowRun, error) {
	j, err := readJournal(h.scratchFor(sid), id)
	if err != nil {
		return nil, err
	}
	hd := j.head
	f := &flowRun{
		id: id, name: hd.Name, description: cmp.Or(hd.Description, "workflow"), about: hd.About, parent: sid, started: time.UnixMilli(hd.Started),
		cancel: func() {}, script: j.script, status: cmp.Or(j.end.Status, RunCancelled), limit: hd.Concurrency,
	}
	f.finished = f.started.Add(time.Duration(j.end.MS) * time.Millisecond)
	_ = json.Unmarshal(j.end.Result, &f.result)
	if j.end.Err != "" {
		f.err = errors.New(j.end.Err)
	}
	for _, a := range j.agents {
		f.agents = append(f.agents, &flowAgent{
			id: a.Agent, def: a.Def, description: a.Description, phase: a.Phase, status: a.Status, reused: a.Reused,
			calls: a.Calls, tokens: a.Tokens, cost: a.Cost, took: time.Duration(a.MS) * time.Millisecond,
		})
	}
	return f, nil
}

// padFlows lists the run ids whose journals are in session sid's pad.
func (h *Host) padFlows(sid string) []string {
	ents, _ := os.ReadDir(h.scratchFor(sid))
	var ids []string
	for _, e := range ents {
		if id, ok := strings.CutSuffix(strings.TrimPrefix(e.Name(), "workflow-"), ".jsonl"); ok && store.IDShape.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// flowAgentIDs are the agent run ids session sid's journals name: /agents
// lists a workflow's agents as the workflow. A forged journal can only hide
// this session's own agents from that list.
func (h *Host) flowAgentIDs(sid string) set.Set[string] {
	ids := set.Set[string]{}
	for _, id := range h.padFlows(sid) {
		if j, err := readJournal(h.scratchFor(sid), id); err == nil {
			for _, a := range j.agents {
				ids.Add(a.Agent)
			}
		}
	}
	return ids
}

// saveFlow copies run id's script as it ran to workflows/<name>.lua in the
// config dir, headed by the run's description when it has no leading
// comment; no name names it after the description. Only a run this process
// holds has that script: the pad's copy is the model's to rewrite, and a
// saved one runs by name in every project.
func (h *Host) saveFlow(id, name string) (string, error) {
	f := h.tasks.flow(id)
	if f == nil || f.parent != h.AgentSessionID() {
		return "", Usagef("zemi no longer holds run %s of this session: rerun it, then save it", id)
	}
	if name == "" {
		// a default name never shadows a builtin or plugin workflow unseen
		name = slug(f.description)
	}
	// the loader's own rule, so a saved name is one workflows() takes back
	if !skill.ValidName(name) {
		return "", Usagef("a workflow name is %s, not %q", skill.NameRule, name)
	}
	// an explicit name too: a saved one shadows a builtin in every project
	if slices.ContainsFunc(h.workflows(), func(w Workflow) bool { return w.Name == name }) {
		return "", Usagef("a workflow %s exists: /workflows save %s <name> saves it under another", name, id)
	}
	src := []byte(f.src)
	if desc := strings.Join(strings.Fields(f.description), " "); leadingComment(f.src) == "" && desc != "" {
		src = append([]byte("-- "+desc+"\n"), src...)
	}
	dir := filepath.Join(h.Dirs.ConfigHome, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".lua")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return "", Usagef("workflows/%s.lua exists: /workflows save %s <name> saves it under another", name, id)
	}
	if err != nil {
		return "", err
	}
	_, err = out.Write(src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return path, err
}

// the workflow tool

type workflowTool struct {
	tools.NoProviderOptions
	h     *Host
	pipe  *tools.Pipeline
	saved []Workflow
}

func (h *Host) workflowTool(pipe *tools.Pipeline) fantasy.AgentTool {
	return &workflowTool{h: h, pipe: pipe, saved: h.workflows()}
}

// workflowToolDesc carries the whole script API: a model that must load a
// skill first writes a file instead of passing the script.
const workflowToolDesc = `Run a Lua script that coordinates subagents with code between them: stages that feed each other, fan-out over a list, loops, checks. For independent tasks send several agent calls at once instead.

The script gets Lua's string, table and math, zemi.json and zemi.workflow, and no bare copy of them: write local wf = zemi.workflow first, and every call as wf.<name>.
wf.agent(prompt, {agent, description, phase, model, schema}) -> report, or nil, err; agent defaults to general; the prompt must stand alone; with schema (a JSON Schema object) the report is a table
wf.parallel({fn, ...}) -> results, errs: runs the functions at once
wf.pipeline(items, stage, ...) -> results, errs: each item through the stages, stage(prev, item, i), items independent
wf.phase(name) groups the agents after it; wf.log(...) tells the user; wf.args is the input's args
The script's return value is the result.

local wf = zemi.workflow
local notes = wf.pipeline(wf.args, function(dir) return wf.agent("Summarize " .. dir .. ", citing file:line.", { agent = "explore" }) end)
return wf.agent("Write a design note from:\n" .. table.concat(notes, "\n"))`

// bgWorkflowStarted opens the result of a workflow call that went on in the
// background, whose row reads as still running (toolStatus).
const bgWorkflowStarted = "Started workflow "

// warnWorkflowFinished opens the answer of a foreground workflow call whose
// run finished with a failed agent or branch inside: the model hears the
// result may be partial, and the call's own row warns rather than reading a
// check (toolStatus). The run's report follows it.
const warnWorkflowFinished = "Finished workflow "

// bgFlowText is what a call whose run reports as a message answers: a
// background start, or a foreground call the user's message handed off.
func bgFlowText(f *flowRun) string {
	where := ""
	if f.script != "" {
		where = " (" + f.script + ")"
	}
	return bgWorkflowStarted + f.id + where + ". Wait for it with run=" + f.id + " and action=wait, or read its report when it arrives as a message."
}

// list names the runs of this session this process holds, newest first: a run
// the user started in /workflows has no call of the model's behind it, so this
// is where the model learns it is there, and the id it acts on.
func (t *workflowTool) list() string {
	flows := t.h.tasks.flowsOf(t.h.AgentSessionID())
	if len(flows) == 0 {
		return "(no workflow run to act on in this session)"
	}
	lines, live := make([]string, 0, len(flows)), 0
	for _, f := range flows {
		v := f.view()
		if v.status == RunRunning {
			live++
		}
		line := fmt.Sprintf("%s %s: %s", f.id, clipRunes(f.description, 60), flowWord(v))
		if s := f.summary(); s != "" {
			line += " · " + s
		}
		lines = append(lines, line)
	}
	return fmt.Sprintf("%d %s to act on in this session, %d running, newest first:\n%s",
		len(flows), Plural(len(flows), "run", "runs"), live, strings.Join(lines, "\n"))
}

// workflowArgs is the workflow tool's input and its schema source. Every
// field is optional in the schema: the run code names what a call missed.
type workflowArgs struct {
	Script      string `json:"script,omitempty" desc:"Lua source"`
	Name        string `json:"name,omitempty" desc:"a saved workflow to run"`
	Args        any    `json:"args,omitempty" desc:"the script's zemi.workflow.args"`
	Description string `json:"description,omitempty" desc:"3-6 word label shown to the user"`
	Concurrency int    `json:"concurrency,omitempty"`
	Background  bool   `json:"background,omitempty" desc:"run detached; the result arrives later as a message"`
	Run         string `json:"run,omitempty" desc:"id of a workflow run in this session to act on; rerun reruns its edited script file, and finished agent calls answer at once"`
	Action      string `json:"action,omitempty" desc:"what to do to run"`
}

func (t *workflowTool) Info() fantasy.ToolInfo {
	var desc strings.Builder
	desc.WriteString(workflowToolDesc)
	actions := []string{string(actStatus), "list", string(actWait), string(actPause), string(actResume), string(actStop), "rerun"}
	props, required := tools.ArgsOf(workflowArgs{})
	desc.WriteString(" Set background for runs that take minutes, and act on one with run and action; action list names this session's runs, the user's own from /workflows among them, and status lists the run ids of its agents." +
		" wait blocks until that run ends, so its result is in the conversation by the time this call returns; a report the same run sends arrives as a message.")
	props["action"].(map[string]any)["enum"] = actions
	props["concurrency"].(map[string]any)["description"] = fmt.Sprintf("agents running at once, 1-%d (default %d); with run, changes a running workflow's", workflowMaxConcurrency, workflowConcurrency)
	if len(t.saved) > 0 {
		desc.WriteString("\n\nSaved workflows (pass name instead of script):")
		for _, w := range t.saved {
			desc.WriteString("\n- " + w.Name + ": " + truncRunes(w.Description, 160))
		}
		desc.WriteString("\n\nA saved workflow is <name>.lua under the workflows/ folder of the config directory or of the repository's .agents/: the same script shape, `zemi.workflow.args` its args string. Run it by name, list it with action list.")
	}
	return fantasy.ToolInfo{
		Name:        string(tools.ToolWorkflow),
		Description: desc.String(),
		Parameters:  props,
		Required:    required,
	}
}

func (t *workflowTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	h := t.h
	var in workflowArgs
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := h.preCheck(string(tools.ToolWorkflow), call.Input); stop {
		return resp, nil
	}
	defer h.postNotify(ctx, t.pipe.Pad, string(tools.ToolWorkflow), call.Input, &reply, time.Now())
	// a run started in /workflows has no call of the model's behind it: list is
	// how the model learns it is there, and the id to act on
	if in.Action == "list" && in.Run == "" && in.Name == "" && in.Script == "" {
		return fantasy.NewTextResponse(t.list()), nil
	}
	rerun := in.Run != "" && in.Action == "rerun"
	switch n := len(slices.DeleteFunc([]string{in.Script, in.Name, in.Run}, func(s string) bool { return s == "" })); {
	case n != 1 && !(n == 2 && rerun && in.Name == ""):
		return fantasy.NewTextErrorResponse("pass exactly one of script, name or run; script goes with run only for action rerun"), nil
	case in.Action != "" && in.Run == "":
		return fantasy.NewTextErrorResponse("action acts on a run: pass its id as run"), nil
	case in.Run != "" && !rerun:
		if flowAction(in.Action) == actWait {
			return t.wait(ctx, call.ID, in.Run), nil
		}
		act := flowAction(in.Action)
		switch {
		case act == "" && in.Concurrency > 0:
			act = actConcurrency
		case act == "":
			act = actStatus
		}
		out, err := t.control(in.Run, act, in.Concurrency)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return fantasy.NewTextResponse(out), nil
	}
	spec := flowSpec{name: in.Name, description: in.Description, src: in.Script, args: in.Args, concurrency: in.Concurrency, background: in.Background && h.hooks() != nil}
	arg := "script"
	switch {
	case in.Name != "":
		src, err := h.workflowSource(in.Name)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		spec.src, arg = src, in.Name
	case rerun:
		spec.from = in.Run
		if spec.src == "" {
			src, err := flowSource(t.pipe.Pad, in.Run)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			spec.src = src
		}
	}
	if strings.TrimSpace(spec.src) == "" {
		return fantasy.NewTextErrorResponse("the script is empty"), nil
	}
	// a script that cannot parse never runs: the ask would only spend the
	// user's approval on a syntax error
	if _, err := parse.Parse(strings.NewReader(spec.src), arg); err != nil {
		return fantasy.NewTextErrorResponse("workflow script: " + err.Error()), nil
	}
	// The script decides how many agents run and what they are told, so the
	// user sees it before it starts (tools inside stay gated one by one).
	if v := t.pipe.CheckDenied(tools.Request{Tool: string(tools.ToolWorkflow), Subject: perm.SubjectWorkflow, Arg: arg, Description: in.Description, Preview: spec.src}); !v.Allowed {
		return fantasy.NewTextErrorResponse(tools.DeniedText(v, string(tools.ToolWorkflow)+" "+arg)), nil
	}
	spec.done = h.deliverFlow("")
	if spec.background {
		f, err := h.startFlow(ctx, spec)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		h.setTool(call.ID, func(st *StreamTool) { st.Detail, st.Run = "background "+f.id+" (/workflows)", f.id })
		h.setToolOutcome(call.ID, tools.Outcome{Run: f.id})
		return fantasy.NewTextResponse(bgFlowText(f)), nil
	}
	spec.progress = func(f *flowRun) { h.setTool(call.ID, func(st *StreamTool) { st.Detail, st.Run = f.summary(), f.id }) }
	f, err := h.startFlow(ctx, spec)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	// The run outlives this call: the user's own message hands it off, so the
	// turn reads that message instead of waiting for the run (its report then
	// arrives as one). Only the run's end and esc keep the call attached, and a
	// handoff that lost the race to the end reads the result inline.
	select {
	case <-f.done:
	case <-ctx.Done():
		f.cancel()
		<-f.done
	case <-h.userSpoke():
		if f.detach() {
			h.setToolOutcome(call.ID, tools.Outcome{Run: f.id})
			return fantasy.NewTextResponse(bgFlowText(f)), nil
		}
		<-f.done
	}
	report, ok := f.report()
	if !ok {
		return fantasy.NewTextErrorResponse(h.truncOutput(t.pipe.Pad, "workflow", report)), nil
	}
	if f.status == RunWarn {
		h.setToolOutcome(call.ID, tools.Outcome{Warn: true})
	}
	return fantasy.NewTextResponse(h.truncOutput(t.pipe.Pad, "workflow", f.asCall(report))), nil
}

// flowSource reads run id's script from pad once: the run or the
// model's file tools may rewrite the file, so what is approved is what runs.
func flowSource(pad, id string) (string, error) {
	if _, err := readJournal(pad, id); err != nil {
		return "", err
	}
	src, err := readPadFile(pad, flowFile(id, ".lua"))
	return string(src), err
}

// approveRerun is the script of run id the user reruns, asked about as the
// tool's script is: the model may have rewritten it since the user saw it.
func (h *Host) approveRerun(id string) (string, error) {
	pipe := h.PipelineFor()
	src, err := flowSource(pipe.Pad, id)
	if err != nil {
		return "", err
	}
	if v := pipe.CheckDenied(tools.Request{Tool: string(tools.ToolWorkflow), Subject: perm.SubjectWorkflow, Arg: id, Preview: src}); !v.Allowed {
		return "", errors.New(tools.DeniedText(v, string(tools.ToolWorkflow)+" "+id))
	}
	return src, nil
}

// wait blocks until run id ends, the user types, or the call is stopped: the
// join a detached run otherwise has none of. A sibling - an agent run or
// another workflow of this session - that ends or sends an update wakes the
// wait too: the answer names the sibling and the workflow that still runs, so
// the two are never confused. It answers with the run's status and never with
// its report, which the same instant sends as a message.
func (t *workflowTool) wait(ctx context.Context, callID, id string) fantasy.ToolResponse {
	h := t.h
	f := h.tasks.flow(id)
	if f == nil {
		// a run this process no longer holds: its journal has how it ended,
		// and a run with no end line died with the previous process, whose
		// report was never delivered
		j, err := readJournal(h.scratchFor(h.AgentSessionID()), id)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error())
		}
		if j.end.Status == "" {
			return fantasy.NewTextResponse("workflow " + id + " died with the previous zemi process; its report was lost. Rerun it (workflow run=" + id + " with action rerun) if you still need it.")
		}
		return fantasy.NewTextResponse("workflow " + id + " already ended (" + string(j.end.Status) + "); its report is in the conversation.")
	}
	if f.parent != h.AgentSessionID() {
		// deliverReport would refuse this session and toast the report there
		return fantasy.NewTextErrorResponse("workflow " + id + " belongs to another session: its report arrives there")
	}
	if !f.isRunning() {
		return fantasy.NewTextResponse(f.statusText() + "\nIt has ended; its report is in the conversation.")
	}
	h.setTool(callID, func(st *StreamTool) { st.Run = id })
	sibs := h.watchSiblings(h.AgentSessionID(), id)
	defer sibs.stop()
	end := func() fantasy.ToolResponse {
		return fantasy.NewTextResponse(f.statusText() + "\nIts report arrives as a message of its own; do not wait for it again.")
	}
	for {
		// before any state read: a bump between the two is a wake this
		// iteration re-checks, never a missed one
		change := h.tasks.onChange()
		select {
		case <-f.done:
			return end()
		case <-h.userSpoke():
			// the user's own message takes the run: its report goes to the session
			if f.detach() {
				h.setToolOutcome(callID, tools.Outcome{Run: id})
				return fantasy.NewTextResponse(handoffText("workflow", f.id, f.description,
					"wait it again (workflow run="+id+" with action wait) if you still need it"))
			}
			<-f.done
			return end()
		case <-ctx.Done():
			// esc stops the wait, not the run
			h.setToolOutcome(callID, tools.Outcome{Run: id})
			return fantasy.NewTextResponse("Stopped waiting; workflow " + id + " goes on. Call it again with run=" + id + " and action wait, or read its report when it arrives.")
		case <-change:
			if !f.isRunning() {
				return end()
			}
			if lines := h.sibMoves(sibs); len(lines) > 0 {
				h.setToolOutcome(callID, tools.Outcome{Run: id})
				return fantasy.NewTextResponse(sibText(lines,
					fmt.Sprintf("workflow %s (%s)", f.id, f.description),
					fmt.Sprintf("wait it again (workflow run=%s with action wait)", f.id)))
			}
			// a run this wait does not name moved (a new sibling, this
			// workflow's own agent): keep waiting
		}
	}
}

// control runs a control action on a run of this session.
func (t *workflowTool) control(id string, act flowAction, n int) (string, error) {
	if f := t.h.tasks.flow(id); f != nil && f.parent != t.h.AgentSessionID() {
		return "", Usagef("workflow %s belongs to another session", id)
	}
	return t.h.flowControl(id, act, n, "")
}

// report is what the model reads of an ended run: its value and log,
// headed by the run and its script, or the failure and how to rerun.
func (f *flowRun) report() (string, bool) {
	var log string
	if len(f.log) > 0 {
		log = "\n\nlog:\n" + strings.Join(f.log, "\n")
	}
	fails := f.failLines()
	where := "workflow " + f.id
	if f.err != nil {
		msg := where + " failed: " + f.err.Error()
		if errors.Is(f.err, context.Canceled) {
			msg = where + ": the user stopped it; do not start it again unless they ask"
		}
		if f.script != "" {
			log += "\n\nEdit the script if needed and pass run=" + f.id + " with action rerun"
			if k, n := f.finishedAgents(); n > 0 {
				log += fmt.Sprintf(": %d of %d agents finished, and a rerun answers those at once", k, n)
			} else {
				log += "."
			}
		}
		return msg + fails + log, false
	}
	if f.script != "" {
		where += " · " + f.script
	}
	return "[" + where + "]\n\n" + cmp.Or(strings.TrimSpace(f.result), "(the workflow returned nothing)") + fails + log, true
}

// failLines names the branches that failed on their own, the ones the script
// may have dropped from parallel's errs, and the agents that failed.
func (f *flowRun) failLines() string {
	var b strings.Builder
	for _, l := range f.fails {
		b.WriteString("\n\n" + l)
	}
	for _, a := range f.agents {
		if a.run == nil || f.agentStatus(a) != RunFailed {
			continue
		}
		fmt.Fprintf(&b, "\n\nagent %q failed: %s", a.description, a.run.errText())
	}
	return b.String()
}

// deliverFlow hands a background run's report to the session that started
// it, as a background agent's, with attached (a loop's note) behind it; a
// stopped one has none.
func (h *Host) deliverFlow(attached string) func(*flowRun) {
	return func(f *flowRun) {
		if errors.Is(f.err, context.Canceled) {
			return
		}
		attr, status, body := f.reportAs()
		h.deliverReport(f.parent, report{id: f.id, agent: attr, status: status, description: f.description, body: body}, attached)
	}
}

// reportAs is an ended run's report with the agent attribute and status it
// goes to the model under.
func (f *flowRun) reportAs() (attr string, status reportStatus, report string) {
	attr, status = "workflow", reportFinished
	if f.name != "" {
		attr += ":" + f.name
	}
	report, ok := f.report()
	if !ok {
		status = reportFailure
	} else if f.status == RunWarn {
		status = reportWarn
	}
	return attr, status, report
}

// asCall is a finished run's report as the foreground call answers it: a run
// that went on past a failed agent or branch says so first.
func (f *flowRun) asCall(report string) string {
	if f.status != RunWarn {
		return report
	}
	return warnWorkflowFinished + f.id + " with an agent or branch that failed inside: the result below may be partial.\n\n" + report
}

// /workflows and zemi.workflow

var workflowVerbs = []string{"run", "view", "pause", "resume", "stop", "rerun", "concurrency", "restart", "save", "open", "new"}

// newWorkflowPrompt rides behind /workflows new and takes zemiSkillDir, where
// the procedure lives.
const newWorkflowPrompt = "I want a new zemi workflow that does what the command above says; ask me when it says nothing. Read SKILL.md in %s, the zemi skill's directory, then follow the New workflow steps in workflows.md there."

// workflowCommands are /workflows and /review.
func (h *Host) workflowCommands() []*Command {
	return []*Command{{
		Name: "workflows", NArgs: NArgsAny, Desc: "see workflow runs and saved workflows, act on them, or make one",
		GoFn: func(h *Host, args string, confirm bool) error {
			typed, attached := agent.CutAttached(args)
			f := strings.Fields(typed)
			if len(f) == 0 {
				return h.openWorkflows()
			}
			arg := func(i int) string {
				if i < len(f) {
					return f[i]
				}
				return ""
			}
			verb := f[0]
			if verb == "new" {
				// the line as typed, newlines included, with the prompt behind it
				return h.sendCommandPrompt("workflows", strings.TrimSpace(typed), agent.Attached("command", fmt.Sprintf(newWorkflowPrompt, h.zemiSkillDir())), attached)
			}
			id := arg(1)
			if id == "" {
				return Usagef("/workflows %s needs a run id or a name: /workflows lists them", verb)
			}
			// a run's view's keys run these too, and the open screens rebuild at
			// once, keys included
			say := func(out string, err error) error {
				if err == nil {
					h.refreshFlows()
					h.Notify(slog.LevelInfo, out, "")
				}
				return err
			}
			switch verb {
			case "run":
				return h.runSaved(attached, id, strings.Join(f[2:], " "))
			case "view":
				if arg(2) != "" {
					if err := h.ViewAgentFrom(arg(2), "workflows view "+id, "workflows"); err != nil {
						return err
					}
					// the transcript takes the menu's place whole: the screens
					// close, and esc restores them - the menu under the run
					// view the back command reopens
					for _, o := range []string{"workflow:" + id, "workflows"} {
						h.hooks().CloseOverlay(o, lua.OriginBuiltin, "")
					}
					return nil
				}
				return h.openFlow(id)
			case "pause", "resume", "stop":
				return say(h.flowControl(id, flowAction(verb), 0, ""))
			case "concurrency":
				n, err := strconv.Atoi(arg(2))
				if err != nil {
					return Usagef("/workflows concurrency <id> <1-%d, or +n or -n>", workflowMaxConcurrency)
				}
				// relative, so a key pressed twice before its view rebuilds acts twice
				if f := h.tasks.flow(id); f != nil && strings.ContainsAny(arg(2)[:1], "+-") {
					n += f.view().limit
				}
				return say(h.flowControl(id, actConcurrency, n, ""))
			case "restart":
				return say(h.flowControl(id, actRestart, 0, arg(2)))
			case "rerun":
				rerun := func() (string, error) {
					src, err := h.approveRerun(id)
					if err != nil {
						return "", err
					}
					return h.startUserFlow(flowSpec{from: id, src: src}, attached)
				}
				// its ask blocks, and a Go command runs on the key loop
				h.GoSafe("workflows rerun", func() {
					next, err := rerun()
					if err == nil {
						// the new run's view in the old one's place, where its keys act on it
						h.hooks().CloseOverlay("workflow:"+id, lua.OriginBuiltin, "")
						err = h.openFlow(next)
					}
					if err != nil {
						h.CommandFailed("workflows", err)
					}
				})
				return nil
			case "save":
				path, err := h.saveFlow(id, arg(2))
				return say("Saved as "+perm.TildePath(path), err)
			case "open":
				return h.openFlowScript(id)
			}
			return Usagef("unknown /workflows subcommand %q (%s)", verb, strings.Join(workflowVerbs, ", "))
		},
		GoComplete: func(h *Host, _, rest string) []string {
			f := strings.Fields(rest)
			if len(f) == 0 || len(f) == 1 && !strings.HasSuffix(rest, " ") {
				return workflowVerbs
			}
			if f[0] == "new" { // what it should do is the user's own text
				return nil
			}
			var out []string
			if f[0] == "run" {
				for _, w := range h.workflows() {
					out = append(out, w.Name)
				}
				return out
			}
			for _, f := range h.tasks.flowsOf(h.AgentSessionID()) {
				out = append(out, f.id)
			}
			return out
		},
	}, {
		Name: "review", NArgs: NArgsOptional, Desc: "review your changes, a base ref or a commit, with several agents",
		GoFn: func(h *Host, args string, confirm bool) error {
			typed, attached := agent.CutAttached(args)
			return h.runSaved(attached, "review", strings.TrimSpace(typed))
		},
	}}
}

// runSaved starts a saved workflow for the user, args its text ("" none).
func (h *Host) runSaved(attached, name, args string) error {
	src, err := h.workflowSource(name)
	if err != nil {
		return err
	}
	spec := flowSpec{name: name, src: src}
	if args != "" {
		spec.args = args
	}
	_, err = h.startUserFlow(spec, attached)
	return err
}

// startUserFlow starts a run the user asked for, with no model call before
// the run's own first one.
// the work. It runs in the background and reports to the model like a
// background agent, and returns the run's id.
func (h *Host) startUserFlow(spec flowSpec, attached string) (string, error) {
	// the refusals before the run, so a start that cannot run leaves nothing
	if _, err := h.resolveModel(h.roleModel(defaultRole)); err != nil {
		return "", err
	}
	// the user has no foreground version of this run to fall back on,
	// here and at the late refusal in addFlow
	spec.advice = waitAdvice
	if err := h.tasks.canDetach(); err != nil {
		return "", err
	}
	h.captureSessionStart()
	spec.background, spec.done = true, h.deliverFlow(attached)
	f, err := h.startFlow(context.Background(), spec)
	if err != nil {
		return "", err
	}
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: fmt.Sprintf("Started workflow %s (%s) · /workflows", f.id, clipLine(f.description, 60))})
	return f.id, nil
}

// openFlowScript opens a run's script, or a saved workflow's file, in the
// user's editor.
func (h *Host) openFlowScript(id string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if store.IDShape.MatchString(id) {
		pad := h.ScratchDir()
		if _, err := readPadFile(pad, flowFile(id, ".lua")); err != nil { // no link the editor would follow
			return err
		}
		// above every pad, where the model does not write, so the editor.s
		// language server knows zemi.workflow
		if err := writeEditorConfigs(filepath.Dir(pad), h.TypesDir()); err != nil {
			h.Log.Warn("editor config not written", "origin", "workflows", "dir", filepath.Dir(pad), "error", err)
		}
		h.openInEditor(ui, filepath.Join(pad, flowFile(id, ".lua")), "the workflow")
		return nil
	}
	saved := h.workflows()
	i := slices.IndexFunc(saved, func(w Workflow) bool { return w.Name == id && w.Source != lua.OriginBuiltin })
	if i < 0 {
		return Usagef("no workflow run or saved file %q", id)
	}
	h.openInEditor(ui, saved[i].Path, "the workflow")
	return nil
}

func (h *Host) openWorkflows() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(h.workflowsOverlay())
	return nil
}

// workflowsOverlay is /workflows: the runs, live ones first (another
// session's too: they block /reset), then this session's earlier ones,
// then the saved workflows by source.
func (h *Host) workflowsOverlay() Overlay {
	sid := h.AgentSessionID()
	var running, earlier []OverlayItem
	held := set.Set[string]{}
	for _, f := range h.tasks.flowList() {
		held.Add(f.id)
		switch {
		case f.isRunning():
			running = append(running, h.flowItem(f, "Running"))
		case f.parent == sid:
			earlier = append(earlier, h.flowItem(f, "Earlier in this session"))
		}
	}
	var stored []*flowRun
	for _, id := range h.padFlows(sid) {
		if f, err := h.loadFlow(sid, id); err == nil && !held.Has(id) {
			stored = append(stored, f)
		}
	}
	slices.SortFunc(stored, func(a, b *flowRun) int { return b.started.Compare(a.started) })
	for _, f := range stored {
		earlier = append(earlier, h.flowItem(f, "Earlier in this session"))
	}
	// making one heads the list, as it does in /agents and /plugins, and a run
	// already in flight is what its keys act on by default
	items := []OverlayItem{{Text: "New workflow", Verb: VerbFill, Fill: "/workflows new "}}
	sel := 0
	if len(running) > 0 {
		sel = 1
	}
	items = append(items, running...)
	items = append(items, earlier...)
	// NOTE: while a run lives, an open list reads the saved files and the
	// pad's journals each second; cache them if a large pad shows in a list.
	for _, w := range h.workflows() {
		path := ""
		keys := []ItemKey{
			{Key: "r", Desc: "run", Run: "workflows run " + w.Name},
			{Key: "a", Desc: "run with arguments", Fill: "/workflows run " + w.Name + " "},
		}
		if w.Source != lua.OriginBuiltin {
			path = perm.TildePath(w.Path)
			keys = append(keys, ItemKey{Key: "o", Desc: openInEditorLabel, Run: "workflows open " + w.Name})
		}
		items = append(items, OverlayItem{
			Text:    w.Name,
			Desc:    listDesc(w.Description),
			Section: sourceSection(w.Source),
			Fields:  fieldList("source", h.sourceOf(w.Source, w.Path), "path", path),
			Detail:  w.Description,
			Keys:    keys,
		})
	}
	return Overlay{
		ID: "workflows", Title: "Workflows", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Wide: true,
		Reload: func() []OverlayItem { return h.workflowsOverlay().Items },
	}
}

// watchFlows keeps the open /workflows and run views current once a second
// while any run lives, one ticker for every run; an idle zemi runs none.
func (h *Host) watchFlows() {
	h.overlayTicker("workflow views", h.tasks.watch, func() bool {
		last := h.tasks.unwatch() // read first, so the last refresh shows the end
		h.refreshFlows()
		return !last
	})
}

// refreshFlows rebuilds /workflows and the run views that are open.
func (h *Host) refreshFlows() {
	ui := h.hooks()
	if ui == nil {
		return
	}
	ui.RefreshOverlay("workflows", h.workflowsOverlay)
	for _, f := range h.tasks.flowList() {
		ui.RefreshOverlay("workflow:"+f.id, func() Overlay { return h.flowOverlay(f) })
	}
}

// workflowNamespace is zemi.workflow: list() and run(spec, on_done), where
// spec is {script = src} or {name = saved} plus optional args, and
// on_done(result, err, log) fires on the interpreter.
func (h *Host) workflowNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, w := range h.workflows() {
			rows = append(rows, map[string]any{"name": w.Name, "description": w.Description, "source": w.Source, "path": w.Path})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	tb.RawSetString("run", L.NewFunction(func(L *glua.LState) int {
		spec := L.CheckTable(1)
		onDone, _ := arg[*glua.LFunction](L, 2, "zemi.workflow.run")
		src := rawString(L, spec, "script")
		if name := rawString(L, spec, "name"); name != "" {
			s, err := h.workflowSource(name)
			if _, miss := errors.AsType[unknownFlowError](err); miss {
				// a mistyped name is misuse, like agent.run's unknown agent
				L.RaiseError("zemi.workflow.run: %v", err)
			}
			if err != nil {
				return luaResult(L, err)
			}
			src = s
		}
		if strings.TrimSpace(src) == "" {
			L.RaiseError("zemi.workflow.run: script or name is required")
		}
		h.luaGate(L, luaWorkflowRun, cmp.Or(rawString(L, spec, "name"), "(script)"), nil)
		args, err := lua.ToGo(spec.RawGetString("args"))
		if err != nil {
			L.RaiseError("zemi.workflow.run: args: %v", err)
		}
		n, _ := opt[glua.LNumber](L, spec, "concurrency")
		at := h.callerOf(L)
		// the Lua caller's own, refused past the cap with the advice it can
		// act on, and never the foreground one the agent tool's call gets
		if err := h.tasks.canDetach(); err != nil {
			return luaResult(L, err)
		}
		stopID := make(chan int, 1) // a reload cancels the run, like a job
		f, err := h.startFlow(context.Background(), flowSpec{name: rawString(L, spec, "name"), src: src, args: args, concurrency: int(n), background: true, advice: waitAdvice, done: func(f *flowRun) {
			h.stops.remove(<-stopID)
			switch {
			case onDone == nil:
			case f.err != nil:
				h.callLuaAsync("zemi.workflow.run on_done", at, onDone, nil, f.err.Error(), f.log)
			default:
				var res any = f.result
				if f.result == "" {
					res = nil // the script returned nothing, not ""
				}
				h.callLuaAsync("zemi.workflow.run on_done", at, onDone, res, nil, f.log)
			}
		}})
		if err != nil {
			return luaResult(L, err)
		}
		stopID <- h.stops.add(f.cancel)
		handle := L.NewTable()
		handle.RawSetString("id", glua.LString(f.id))
		handle.RawSetString("wait", lua.Suspending(L, L.NewFunction(func(L *glua.LState) int {
			return waitHandle(L, "workflow:wait", f.done, 0, nil, func(L *glua.LState, err error) []glua.LValue {
				if err != nil {
					return []glua.LValue{glua.LNil, glua.LString(err.Error())}
				}
				if f.err != nil {
					return []glua.LValue{glua.LNil, glua.LString(f.err.Error())}
				}
				if f.result == "" {
					return []glua.LValue{glua.LNil}
				}
				return []glua.LValue{glua.LString(f.result)}
			})
		})))
		handle.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
			f.cancel()
			return 0
		}))
		lua.GuardFuncs(L, handle, "workflow:", h.guardLua)
		L.Push(handle)
		return 1
	}))
	return tb
}
