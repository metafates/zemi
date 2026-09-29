// workflow.go runs workflows: Lua scripts that coordinate subagents. The
// model writes them through the workflow tool, users save them under
// workflows/ and run them with /workflow or zemi.workflow.run.
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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
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
// plugin ones, then project files; project files load only once the project
// tree is trusted.
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
			h.RecordErrorOnce("workflows", err.Error())
			return false
		}
		out = append(out, Workflow{Name: f.name, Description: leadingComment(src), Path: f.path, Source: f.origin})
		return true
	})
	sortBySource(out, func(w Workflow) lua.Origin { return w.Source })
	seen := map[string]bool{}
	return slices.DeleteFunc(out, func(w Workflow) bool {
		dup := seen[w.Name]
		seen[w.Name] = true
		return dup
	})
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
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && !h.ProjectTrusted() {
		if _, err := os.Stat(filepath.Join(pdir, "workflows", name+".lua")); err == nil {
			return "", Usagef("workflow %q is in %s, and this project is not trusted: the user runs /trust to load it (or it can be saved under %s)", name, pdir, filepath.Join(h.Dirs.ConfigHome, "workflows"))
		}
	}
	if len(names) == 0 {
		return "", Usagef("no saved workflow %q (save one as workflows/%s.lua)", name, name)
	}
	return "", Usagef("no saved workflow %q (have %s)", name, strings.Join(names, ", "))
}

// flowRun is one workflow run as the rest of zemi sees it. The scheduler
// (wfRun) owns the script on its goroutine and writes the fields below mu
// under it; the screens, the tool and flowControl read and steer them.
type flowRun struct {
	id, name, description string
	parent                string // the session that started it, its agents' parent too
	background            bool
	started               time.Time
	cancel                context.CancelFunc
	wake                  chan struct{}    // buffered: a control that lets more agents start
	progress              func(f *flowRun) // a foreground run's row, nil for none
	script                string           // the script's file in the parent's pad, "" with no session
	// journal is the held fd the scheduler appends to, never the path: a
	// sandboxed command can plant a link at the name. nil with no session.
	journal *os.File

	mu       sync.Mutex
	status   RunStatus
	paused   bool
	limit    int
	finished time.Time
	phase    string
	log      []string
	agents   []*flowAgent    // in call order
	restart  map[string]bool // agent run ids flowControl cancelled to run again
	result   string          // the script's value: strings as is, anything else JSON
	err      error
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
	progress               func(*flowRun)
	done                   func(*flowRun) // a background run's end

	cache map[string][]journalLine // from's finished agents by agentKey, in file order
}

// startFlow registers a run of spec and runs it to its end on the caller's
// goroutine, or detached from ctx when spec.background, calling spec.done
// once it ends. The error is a start's; the run's own is f.err.
func (h *Host) startFlow(ctx context.Context, spec flowSpec) (*flowRun, error) {
	parent := h.AgentSessionID()
	pad := h.scratchFor(parent)
	var cache map[string][]journalLine
	if spec.from != "" {
		old, err := readJournal(pad, spec.from)
		if err != nil {
			return nil, err
		}
		if spec.src == "" {
			b, err := readPadFile(pad, flowFile(spec.from, ".lua"))
			if err != nil {
				return nil, err
			}
			spec.src = string(b)
		}
		if spec.args == nil {
			spec.args = old.head.Args
		}
		spec.name, spec.description = cmp.Or(spec.name, old.head.Name), cmp.Or(spec.description, old.head.Description)
		cache = old.cache()
	}
	limit := cmp.Or(spec.concurrency, workflowConcurrency)
	if limit < 1 || limit > workflowMaxConcurrency {
		return nil, Usagef("concurrency must be 1-%d, not %d", workflowMaxConcurrency, limit)
	}
	if spec.background {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	f := &flowRun{
		id: store.NewID()[:runIDLen], name: spec.name, description: cmp.Or(spec.description, spec.name, "workflow"),
		parent: parent, background: spec.background, cancel: cancel, wake: make(chan struct{}, 1), progress: spec.progress,
		status: RunRunning, limit: limit, started: time.Now(), restart: map[string]bool{},
	}
	if pad != "" {
		var err error
		head := journalLine{Workflow: f.id, Name: f.name, Description: f.description, Args: spec.args, Concurrency: limit, Started: f.started.UnixMilli()}
		if f.journal, err = openJournal(pad, f.id, spec.src, head); err != nil {
			cancel()
			return nil, err
		}
		f.script = filepath.Join(pad, flowFile(f.id, ".lua"))
	}
	if err := h.tasks.addFlow(f); err != nil {
		cancel()
		if f.journal != nil {
			f.journal.Close()
		}
		return nil, err
	}
	spec.cache = cache
	if !spec.background {
		h.runFlow(ctx, f, spec)
		return f, nil
	}
	h.GoSafe("workflow", func() {
		h.runFlow(ctx, f, spec)
		if spec.done != nil {
			spec.done(f)
		}
	})
	return f, nil
}

// wfTask is one coroutine: the script body or a parallel() branch.
type wfTask struct {
	co      *glua.LState
	fn      *glua.LFunction // set until the first resume
	parent  *wfTask
	slot    int
	kids    []*glua.LFunction // set by parallel() before it yields
	waiting int
	vals    *glua.LTable
	errs    *glua.LTable
	req     *wfAgentReq   // set by agent() before it yields
	args    []glua.LValue // resume values while waiting in ready
}

type wfAgentReq struct {
	def         AgentDef
	prompt, key string
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

// runFlow runs f's script to its end and records how it ended.
func (h *Host) runFlow(ctx context.Context, f *flowRun, spec flowSpec) {
	defer f.cancel()
	val, err := h.runScript(ctx, f, spec)
	f.mu.Lock()
	f.result, f.err, f.finished = val, err, time.Now()
	switch {
	case err == nil:
		f.status = RunDone
	case errors.Is(err, context.Canceled):
		f.status = RunCancelled
	default:
		f.status = RunFailed
	}
	end := journalLine{Status: f.status, MS: f.finished.Sub(f.started).Milliseconds(), Result: jsonString(val), Err: errString(err)}
	f.mu.Unlock()
	f.note(end)
	if f.journal != nil {
		f.journal.Close()
	}
	f.changed()
}

// runScript is the scheduler loop: it resumes the script's coroutines as
// agents report and control arrives, and returns the script's value.
func (h *Host) runScript(ctx context.Context, f *flowRun, spec flowSpec) (string, error) {
	r := &wfRun{h: h, f: f, ctx: ctx, tasks: map[*glua.LState]*wfTask{}, done: make(chan wfDone, workflowMaxAgents), cache: spec.cache}
	L := r.open(spec.args)
	defer L.Close()
	L.SetContext(ctx)

	fn, err := L.LoadString(spec.src)
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
		return "", r.resultErr
	}
	return luaResultString(r.result), nil
}

// agentDone hands an agent's report to the script, or queues the call
// again when flowControl cancelled it to restart it.
func (r *wfRun) agentDone(d wfDone) {
	f := r.f
	f.mu.Lock()
	again := f.restart[d.run.id] && r.ctx.Err() == nil
	delete(f.restart, d.run.id)
	f.mu.Unlock()
	stopped := errors.Is(d.err, context.Canceled) && r.ctx.Err() == nil
	if again && stopped && r.started < workflowMaxAgents {
		r.started++
		d.task.req = d.req
		r.queue = append([]*wfTask{d.task}, r.queue...)
		r.dispatch()
		return
	}
	r.dispatch()
	status, calls, took, _ := d.run.state()
	tokens, cost := d.run.usage()
	line := journalLine{
		Agent: d.run.id, Key: d.req.key, Def: d.req.def.Name, Description: d.run.description, Phase: d.run.phase,
		Status: status, Calls: calls, Tokens: tokens, Cost: cost, MS: took.Milliseconds(),
	}
	switch {
	case stopped:
		line.Err = "stopped by the user"
	case d.err != nil:
		line.Err = r.h.friendlyError(d.err)
	default:
		line.Result = jsonString(d.text)
	}
	f.note(line)
	if line.Err != "" {
		r.resume(d.task, glua.LNil, glua.LString(line.Err))
	} else {
		r.resume(d.task, glua.LString(d.text))
	}
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
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return v.String()
	}
	return string(b)
}

// workflowPrelude is the part of zemi.workflow written in Lua.
const workflowPrelude = `
local parallel = zemi.workflow.parallel
function zemi.workflow.pipeline(items, ...)
	local stages, fns = { ... }, {}
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
	"args": "zemi.workflow.args", "json": "zemi.json",
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
			L.RaiseError("%s is %s in a workflow script", L.CheckString(2), to)
		}
		return 0
	}))
	L.SetMetatable(L.G.Global, mt)

	wf := L.NewTable()
	wf.RawSetString("args", lua.FromGo(L, args))
	wf.RawSetString("agent", lua.Suspending(L, L.NewFunction(r.luaAgent)))
	wf.RawSetString("parallel", lua.Suspending(L, L.NewFunction(r.luaParallel)))
	wf.RawSetString("phase", L.NewFunction(func(L *glua.LState) int {
		phase := L.CheckString(1)
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
		r.f.mu.Unlock()
		r.f.changed()
		return 0
	}))
	zemi := L.NewTable()
	zemi.RawSetString("workflow", wf)
	zemi.RawSetString("json", r.h.jsonNamespace(L))
	L.SetGlobal("zemi", zemi)
	if err := L.DoString(workflowPrelude); err != nil {
		panic(err) // a constant: every workflow test fails first
	}
	return L
}

// luaAgent is zemi.workflow.agent(prompt [, opts]): the agent's report, or
// nil and an error.
func (r *wfRun) luaAgent(L *glua.LState) int {
	t := r.tasks[L]
	switch {
	case t == nil:
		L.RaiseError("agent() must run inside the workflow")
	case lua.InGoCall(L):
		L.RaiseError("agent() cannot wait inside pcall or a library callback such as table.sort; it returns nil, err on failure instead")
	}
	prompt := L.CheckString(1)
	opts := L.OptTable(2, L.NewTable())
	name, desc, model, phase := rawString(L, opts, "agent"), rawString(L, opts, "description"), rawString(L, opts, "model"), rawString(L, opts, "phase")
	if strings.TrimSpace(prompt) == "" {
		L.RaiseError("agent: prompt is required")
	}
	def, err := r.h.agentDef(name)
	if err != nil {
		L.RaiseError("agent: %v", err)
	}
	if err := r.h.checkModelID(model); err != nil {
		L.RaiseError("agent: %v", err)
	}
	if model != "" {
		def.Model = model
	}
	if desc == "" {
		first, _, _ := strings.Cut(prompt, "\n")
		desc = clipRunes(first, 40)
	}
	a := &flowAgent{def: def.Name, description: desc, phase: cmp.Or(phase, r.f.phase)}
	key := agentKey(def.Name, model, prompt)
	// an identical call an earlier run finished answers at once and starts
	// nothing, so it does not count toward the cap
	if q := r.cache[key]; len(q) > 0 {
		hit := q[0]
		r.cache[key] = q[1:]
		a.id, a.status, a.reused, a.calls, a.tokens, a.cost, a.took = hit.Agent, RunDone, true, hit.Calls, hit.Tokens, hit.Cost, time.Duration(hit.MS)*time.Millisecond
		r.f.mu.Lock()
		r.f.agents = append(r.f.agents, a)
		r.f.mu.Unlock()
		hit.Description, hit.Phase, hit.Reused = a.description, a.phase, true
		r.f.note(hit)
		var v any
		_ = json.Unmarshal(hit.Result, &v) // a line zemi did not write answers nil
		L.Push(lua.FromGo(L, v))
		return 1
	}
	if r.started >= workflowMaxAgents {
		L.RaiseError("agent: this workflow already started %d agents (the limit)", workflowMaxAgents)
	}
	r.started++
	r.f.mu.Lock()
	r.f.agents = append(r.f.agents, a)
	r.f.mu.Unlock()
	t.req = &wfAgentReq{def: def, prompt: prompt, key: key, agent: a}
	return L.Yield()
}

// parallel({fn, ...}) runs each function as its own branch and returns the
// results in order plus a table of errors (nil when every branch finished).
func (r *wfRun) luaParallel(L *glua.LState) int {
	t := r.tasks[L]
	if t == nil || lua.InGoCall(L) {
		L.RaiseError("parallel() cannot wait inside pcall or a library callback such as table.sort")
	}
	list := L.CheckTable(1)
	n := list.Len()
	// Every entry becomes a coroutine before the concurrency cap applies,
	// so refuse a batch larger than the run's whole agent budget.
	if n > workflowMaxAgents {
		L.RaiseError("parallel: %d branches is more than the %d agents a workflow may start", n, workflowMaxAgents)
	}
	for i := 1; i <= n; i++ {
		fn, ok := list.RawGetInt(i).(*glua.LFunction)
		if !ok {
			L.RaiseError("parallel: item %d is not a function", i)
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
		run.parent, run.workflow, run.phase = f.parent, f.id, req.agent.phase
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
// starts.
type agentRow struct {
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
		switch {
		case a.run != nil:
			r.id = a.run.id
			r.status, r.calls, r.took, _ = a.run.state()
			r.tokens, r.cost = a.run.usage()
		case r.status == "":
			r.status = "queued"
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
	for _, a := range v.agents {
		switch a.status {
		case RunRunning:
			v.running++
		case "queued":
		default:
			v.done++
		}
		v.tokens += a.tokens
		v.cost += a.cost
	}
	return v
}

func (f *flowRun) isRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status == RunRunning
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
	parts = append(parts, fmt.Sprintf("%d %s", len(v.agents), plural(len(v.agents), "agent", "agents")))
	if v.cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", v.cost))
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
	word := string(v.status)
	if v.paused && v.status == RunRunning {
		word = "paused"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "workflow %s %s: %s · %d/%d agents · %d running · %d at once", f.id, f.description, word, v.done, len(v.agents), v.running, v.limit)
	names, by := f.phases(v.agents)
	for _, p := range names {
		n := 0
		for _, a := range by[p] {
			if a.status != RunRunning && a.status != "queued" {
				n++
			}
		}
		fmt.Fprintf(&b, "\n%s %d/%d", p, n, len(by[p]))
		for _, a := range by[p] {
			fmt.Fprintf(&b, "\n  %s %s %s: %s", cmp.Or(a.id, "-"), a.def, a.description, a.status)
			if a.status != "queued" {
				fmt.Fprintf(&b, ", %d %s, %s", a.calls, plural(a.calls, "call", "calls"), fmtDuration(a.took))
			}
		}
	}
	return b.String()
}

// flowAction is what flowControl does to a live run.
type flowAction string

const (
	actStatus      flowAction = "status"
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
		f.restart[agent] = true
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

// ---------------------------------------------------------------------------
// the run's files: its script and journal in the parent session's pad

// journalLine is one line of a run's journal: the header (Workflow set),
// an agent that ended (Agent set), or the end.
type journalLine struct {
	Workflow    string          `json:"workflow,omitempty"`
	Name        string          `json:"name,omitempty"`
	Args        any             `json:"args,omitempty"`
	Concurrency int             `json:"concurrency,omitzero"`
	Started     int64           `json:"started,omitzero"`
	Agent       string          `json:"agent,omitempty"`
	Key         string          `json:"key,omitempty"`
	Def         string          `json:"def,omitempty"`
	Description string          `json:"description,omitempty"`
	Phase       string          `json:"phase,omitempty"`
	Status      RunStatus       `json:"status,omitempty"`
	Reused      bool            `json:"reused,omitzero"`
	Calls       int             `json:"calls,omitzero"`
	Tokens      int64           `json:"tokens,omitzero"`
	Cost        float64         `json:"cost,omitzero"`
	MS          int64           `json:"ms,omitzero"`
	Result      json.RawMessage `json:"result,omitempty"`
	Err         string          `json:"err,omitempty"`
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// agentKey is what makes two agent calls identical for a rerun.
func agentKey(def, model, prompt string) string {
	sum := sha256.Sum256([]byte(def + "\x00" + model + "\x00" + prompt))
	return hex.EncodeToString(sum[:])
}

var flowIDShape = regexp.MustCompile(`^[0-9a-f]{8}$`)

func flowFile(id, ext string) string { return "workflow-" + id + ext }

// padOpen opens name in pad as a sandboxed command would attack it: the pad
// is a write root, so a link planted at the name or a swapped parent fails
// the open instead of leading zemi, which runs unconfined, elsewhere.
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
	b, _ := json.Marshal(head)
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
	b, _ := json.Marshal(l)
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
// grade: a sandboxed command may have written it.
func readJournal(pad, id string) (journal, error) {
	if !flowIDShape.MatchString(id) {
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
		id: id, name: hd.Name, description: cmp.Or(hd.Description, "workflow"), parent: sid, started: time.UnixMilli(hd.Started),
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
		if id, ok := strings.CutSuffix(strings.TrimPrefix(e.Name(), "workflow-"), ".jsonl"); ok && flowIDShape.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// flowAgentIDs are the agent run ids session sid's journals name: /agents
// lists a workflow's agents as the workflow. A forged journal can only hide
// this session's own agents from that list.
func (h *Host) flowAgentIDs(sid string) map[string]bool {
	ids := map[string]bool{}
	for _, id := range h.padFlows(sid) {
		if j, err := readJournal(h.scratchFor(sid), id); err == nil {
			for _, a := range j.agents {
				ids[a.Agent] = true
			}
		}
	}
	return ids
}

var savedName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// saveFlow copies run id's script to workflows/<name>.lua in the config
// dir, headed by the run's description when it has no leading comment.
func (h *Host) saveFlow(id, name string) (string, error) {
	if !savedName.MatchString(name) {
		return "", Usagef("a workflow name is lowercase letters, digits and dashes, not %q", name)
	}
	sid := h.AgentSessionID()
	j, err := readJournal(h.scratchFor(sid), id)
	if err != nil {
		return "", err
	}
	src, err := readPadFile(j.pad, flowFile(id, ".lua"))
	if err != nil {
		return "", err
	}
	if desc := strings.Join(strings.Fields(j.head.Description), " "); leadingComment(string(src)) == "" && desc != "" {
		src = append([]byte("-- "+desc+"\n"), src...)
	}
	dir := filepath.Join(h.Dirs.ConfigHome, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".lua")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return "", Usagef("workflows/%s.lua exists: pick another name", name)
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

// ---------------------------------------------------------------------------
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

const workflowToolDesc = `Run a Lua script that coordinates subagents when code has to sit between them: stages that feed each other, loops, checking results. Independent tasks need no workflow. Pass a saved workflow's name, or a new script after reading the zemi skill's workflows.md for the API.`

func (t *workflowTool) Info() fantasy.ToolInfo {
	var desc strings.Builder
	desc.WriteString(workflowToolDesc)
	if len(t.saved) > 0 {
		desc.WriteString("\n\nSaved workflows (pass name instead of script):")
		for _, w := range t.saved {
			desc.WriteString("\n- " + w.Name + ": " + truncRunes(w.Description, 160))
		}
	}
	return fantasy.ToolInfo{
		Name:        tools.ToolWorkflow,
		Description: desc.String(),
		Parameters: map[string]any{
			"description": map[string]any{"type": "string", "description": "3-6 word label shown to the user"},
			"script":      map[string]any{"type": "string", "description": "Lua source"},
			"name":        map[string]any{"type": "string", "description": "saved workflow to run instead of a script"},
			"args":        map[string]any{"description": "value exposed to the script as args"},
		},
		Required: []string{"description"},
	}
}

func (t *workflowTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	h := t.h
	var in struct {
		Description string `json:"description"`
		Script      string `json:"script"`
		Name        string `json:"name"`
		Args        any    `json:"args"`
	}
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := h.preCheck("workflow", call.Input); stop {
		return resp, nil
	}
	defer h.postNotify(ctx, t.pipe.Pad, "workflow", call.Input, &reply, time.Now())
	src, arg := in.Script, "script"
	if in.Name != "" {
		s, err := h.workflowSource(in.Name)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		src, arg = s, in.Name
	}
	if strings.TrimSpace(src) == "" {
		return fantasy.NewTextErrorResponse("pass script or name"), nil
	}
	// The script decides how many agents run and what they are told, so the
	// user sees it before it starts (tools inside stay gated one by one).
	if v := t.pipe.CheckDenied(tools.Request{Tool: "workflow", Subject: "tool.workflow", Arg: arg, Description: in.Description, Preview: src}); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse(tools.DeniedText(v, "workflow "+arg)), nil
	}
	f, err := h.startFlow(ctx, flowSpec{name: in.Name, description: in.Description, src: src, args: in.Args, progress: func(f *flowRun) {
		h.setToolDetail(call.ID, f.summary())
	}})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	out := f.result
	if len(f.log) > 0 {
		out += "\n\nlog:\n" + strings.Join(f.log, "\n")
	}
	if f.err != nil {
		msg := "workflow failed: " + f.err.Error()
		if errors.Is(f.err, context.Canceled) {
			msg = "workflow cancelled"
		}
		return fantasy.NewTextErrorResponse(h.truncOutput(t.pipe.Pad, "workflow", strings.TrimSpace(msg+"\n"+out))), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(the workflow returned nothing)"
	}
	return fantasy.NewTextResponse(h.truncOutput(t.pipe.Pad, "workflow", out)), nil
}

// truncOutput cuts a host tool's output to what a result may carry and
// saves the whole text to pad, the scratchpad of the pipeline that made the
// call, so the rest is one read away instead of gone.
func (h *Host) truncOutput(pad, name, s string) string {
	cut := tools.TruncOut(s, 30000)
	if cut == s {
		return s
	}
	return cut + tools.Spill(pad, name, s)
}

// ---------------------------------------------------------------------------
// /workflow and zemi.workflow

// workflowCommands are /workflow and /review.
func (h *Host) workflowCommands() []*Command {
	return []*Command{{
		Name: "workflow", NArgs: NArgsAny, Desc: "run a saved workflow",
		GoFn: func(h *Host, args string) error {
			typed, attached := agent.CutAttached(args)
			typed = strings.TrimSpace(typed)
			name, rest, _ := strings.Cut(typed, " ")
			if name == "" {
				return h.openWorkflows()
			}
			return h.sendWorkflow("/workflow "+typed, name, rest, attached)
		},
		GoComplete: func(h *Host, _, _ string) []string {
			var names []string
			for _, w := range h.workflows() {
				names = append(names, w.Name)
			}
			return names
		},
	}, {
		Name: "review", NArgs: NArgsOptional, Desc: "review your changes, a base ref or a commit, with several agents",
		GoFn: func(h *Host, args string) error {
			typed, attached := agent.CutAttached(args)
			typed = strings.TrimSpace(typed)
			return h.sendWorkflow(strings.TrimSpace("/review "+typed), "review", typed, attached)
		},
	}}
}

// sendWorkflow sends line, what the user typed, with the ask to run a saved
// workflow through the workflow tool, so progress, permissions and the
// result land in the conversation.
func (h *Host) sendWorkflow(line, name, args, attached string) error {
	if _, err := h.workflowSource(name); err != nil {
		return err
	}
	msg := "Run the saved workflow `" + name + "` with the workflow tool"
	if args = strings.TrimSpace(args); args != "" {
		msg += " and args " + strconv.Quote(args)
	}
	return h.AgentSend(line + agent.Attached("command", msg+", then relay its result.") + attached)
}

func (h *Host) openWorkflows() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	var items []any
	list := h.workflows()
	sortBySource(list, func(w Workflow) lua.Origin { return w.Source })
	for _, w := range list {
		path := TildePath(w.Path)
		if w.Source == lua.OriginBuiltin {
			path = ""
		}
		items = append(items, map[string]any{
			"text":    w.Name,
			"desc":    listDesc(w.Description),
			"section": sourceSection(w.Source),
			"fields":  fieldList("source", h.sourceOf(w.Source, w.Path), "path", path),
			"detail":  w.Description,
			"actions": []any{
				map[string]any{"text": "Run", "hotkey": "r", "run": "workflow " + w.Name},
				map[string]any{"text": "Run with arguments", "hotkey": "a", "fill": "/workflow " + w.Name + " "},
			},
		})
	}
	ui.OpenOverlay(Overlay{ID: "workflows", Title: "Workflows", Origin: lua.OriginBuiltin, Items: items})
	return nil
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
		onDone, _ := L.Get(2).(*glua.LFunction)
		// agents act with the user's tools and permissions: only user config
		// (and shipped code) may start them from Lua
		h.refuseBelowUser(L, "workflow.run")
		src := rawString(L, spec, "script")
		if name := rawString(L, spec, "name"); name != "" {
			s, err := h.workflowSource(name)
			if err != nil {
				return luaResult(L, err)
			}
			src = s
		}
		if strings.TrimSpace(src) == "" {
			L.RaiseError("zemi.workflow.run: script or name is required")
		}
		args, err := lua.ToGo(spec.RawGetString("args"))
		if err != nil {
			L.RaiseError("zemi.workflow.run: args: %v", err)
		}
		n, _ := opt[glua.LNumber](L, spec, "concurrency")
		at := h.callerOf(L)
		f, err := h.startFlow(context.Background(), flowSpec{name: rawString(L, spec, "name"), src: src, args: args, concurrency: int(n), background: true, done: func(f *flowRun) {
			switch {
			case onDone == nil:
			case f.err != nil:
				h.callLuaAsync("zemi.workflow.run on_done", at, onDone, nil, f.err.Error(), f.log)
			default:
				h.callLuaAsync("zemi.workflow.run on_done", at, onDone, f.result, nil, f.log)
			}
		}})
		if err != nil {
			return luaResult(L, err)
		}
		handle := L.NewTable()
		handle.RawSetString("id", glua.LString(f.id))
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
