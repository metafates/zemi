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
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
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
	about                 string // a saved workflow's leading comment, "" for an inline script
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
		spec.concurrency = cmp.Or(spec.concurrency, old.head.Concurrency)
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
		id: store.NewID()[:runIDLen], name: spec.name, description: cmp.Or(spec.description, spec.name, clipRunes(leadingComment(spec.src), 60), "workflow"),
		parent: parent, background: spec.background, cancel: cancel, wake: make(chan struct{}, 1), progress: spec.progress,
		status: RunRunning, limit: limit, started: time.Now(), restart: map[string]bool{},
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
	if err := h.tasks.addFlow(f); err != nil {
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
	if !spec.background {
		h.runFlow(ctx, f, spec)
		return f, nil
	}
	h.GoSafe("workflow", func() {
		defer h.drainQueue() // this run no longer holds a queued report back
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
// end too: a flow left running would block rewind until zemi exits.
func (h *Host) runFlow(ctx context.Context, f *flowRun, spec flowSpec) {
	val, err := "", errors.New("the workflow crashed")
	defer func() { f.end(val, err) }()
	val, err = h.runScript(ctx, f, spec)
}

func (f *flowRun) end(val string, err error) {
	f.cancel()
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
		f.mu.Lock()
		d.req.agent.run = nil // queued again, which a paused run holds
		f.mu.Unlock()
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
	case d.req.schema != nil: // runAgent checked it is the object asked for
		line.Result = json.RawMessage(d.text)
	default:
		line.Result = jsonString(d.text)
	}
	f.note(line)
	if line.Err != "" {
		r.resume(d.task, glua.LNil, glua.LString(line.Err))
	} else {
		r.resume(d.task, answer(r.L, line.Result))
	}
}

// answer is an agent's journaled result as the script gets it: the report,
// or a table for a schema call. A line zemi did not write answers nil.
func answer(L *glua.LState, result json.RawMessage) glua.LValue {
	var v any
	_ = json.Unmarshal(result, &v)
	return lua.FromGo(L, v)
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
	schema := callSchema(L, opts)
	a := &flowAgent{def: def.Name, description: desc, phase: cmp.Or(phase, r.f.phase)}
	key := agentKey(def.Name, model, schema, prompt)
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
		L.Push(answer(L, hit.Result))
		return 1
	}
	if r.started >= workflowMaxAgents {
		L.RaiseError("agent: this workflow already started %d agents (the limit)", workflowMaxAgents)
	}
	r.started++
	r.f.mu.Lock()
	r.f.agents = append(r.f.agents, a)
	r.f.mu.Unlock()
	if schema != nil { // in the task, never the system prompt, which runs share
		b, _ := json.Marshal(schema)
		prompt += "\n\nEnd with only a JSON object matching this JSON Schema, no prose and no code fences:\n" + string(b)
	}
	t.req = &wfAgentReq{def: def, prompt: prompt, key: key, schema: schema, agent: a}
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
		L.RaiseError(`agent: schema must be a JSON Schema object with type "object" and properties, and required naming only those`)
	}
	return s
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
		if !a.reused { // an earlier run spent it
			v.tokens += a.tokens
			v.cost += a.cost
		}
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
	About       string          `json:"about,omitempty"`
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
func agentKey(def, model string, schema map[string]any, prompt string) string {
	s, _ := json.Marshal(schema) // map keys sort: one schema, one text
	sum := sha256.Sum256([]byte(def + "\x00" + model + "\x00" + string(s) + "\x00" + prompt))
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

// workflowToolDesc carries the whole script API: a model that must load a
// skill first writes a file instead of passing the script.
const workflowToolDesc = `Run a Lua script that coordinates subagents with code between them: stages that feed each other, fan-out over a list, loops, checks. For independent tasks send several agent calls at once instead.

The script gets Lua's string, table and math, zemi.json and zemi.workflow:
agent(prompt, {agent, description, phase, model, schema}) -> report, or nil, err; the prompt must stand alone; with schema (a JSON Schema object) the report is a table
parallel({fn, ...}) -> results, errs: runs the functions at once
pipeline(items, stage, ...) -> results, errs: each item through the stages, stage(prev, item, i), items independent
phase(name) groups the agents after it; log(...) tells the user; args is the input's args
The script's return value is the result.

local wf = zemi.workflow
local notes = wf.pipeline(wf.args, function(dir) return wf.agent("Summarize " .. dir .. ", citing file:line.", { agent = "explore" }) end)
return wf.agent("Write a design note from:\n" .. table.concat(notes, "\n"))

The result names the run's script file. To change a run, edit that file and pass run with action rerun: agent calls identical to its finished ones answer at once.`

// bgWorkflowStarted opens the result of a workflow call that went on in the
// background, whose row reads as still running (toolStatus).
const bgWorkflowStarted = "Started workflow "

func (t *workflowTool) Info() fantasy.ToolInfo {
	tui := t.h.hooks() != nil
	var desc strings.Builder
	desc.WriteString(workflowToolDesc)
	actions := []string{string(actStatus), "rerun"}
	params := map[string]any{
		"script":      map[string]any{"type": "string", "description": "Lua source"},
		"name":        map[string]any{"type": "string", "description": "a saved workflow to run"},
		"args":        map[string]any{"description": "the script's zemi.workflow.args"},
		"description": map[string]any{"type": "string", "description": "3-6 word label shown to the user"},
		"concurrency": map[string]any{"type": "integer", "description": fmt.Sprintf("agents running at once, 1-%d (default %d); with run, changes a running workflow's", workflowMaxConcurrency, workflowConcurrency)},
		"run":         map[string]any{"type": "string", "description": "id of a workflow run in this session to act on"},
	}
	// a detached run outlives a headless turn, which would throw its result away
	if tui {
		desc.WriteString(" Set background for runs that take minutes, and act on one with run and action; send_to on the agent tool messages one of its agents (status lists them).")
		actions = []string{string(actStatus), string(actPause), string(actResume), string(actStop), "rerun"}
		params["background"] = map[string]any{"type": "boolean", "description": "run detached; the result arrives later as a message"}
	}
	params["action"] = map[string]any{"type": "string", "enum": actions, "description": "what to do to run"}
	if len(t.saved) > 0 {
		desc.WriteString("\n\nSaved workflows (pass name instead of script):")
		for _, w := range t.saved {
			desc.WriteString("\n- " + w.Name + ": " + truncRunes(w.Description, 160))
		}
	}
	return fantasy.ToolInfo{
		Name:        tools.ToolWorkflow,
		Description: desc.String(),
		Parameters:  params,
		Required:    []string{},
	}
}

func (t *workflowTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	h := t.h
	var in struct {
		Script      string `json:"script"`
		Name        string `json:"name"`
		Args        any    `json:"args"`
		Description string `json:"description"`
		Concurrency int    `json:"concurrency"`
		Background  bool   `json:"background"`
		Run         string `json:"run"`
		Action      string `json:"action"`
	}
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := h.preCheck("workflow", call.Input); stop {
		return resp, nil
	}
	defer h.postNotify(ctx, t.pipe.Pad, "workflow", call.Input, &reply, time.Now())
	rerun := in.Run != "" && in.Action == "rerun"
	switch n := len(slices.DeleteFunc([]string{in.Script, in.Name, in.Run}, func(s string) bool { return s == "" })); {
	case n != 1 && !(n == 2 && rerun && in.Name == ""):
		return fantasy.NewTextErrorResponse("pass exactly one of script, name or run; script goes with run only for action rerun"), nil
	case in.Action != "" && in.Run == "":
		return fantasy.NewTextErrorResponse("action acts on a run: pass its id as run"), nil
	case in.Run != "" && !rerun:
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
	// The script decides how many agents run and what they are told, so the
	// user sees it before it starts (tools inside stay gated one by one).
	if v := t.pipe.CheckDenied(tools.Request{Tool: "workflow", Subject: "tool.workflow", Arg: arg, Description: in.Description, Preview: spec.src}); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse(tools.DeniedText(v, "workflow "+arg)), nil
	}
	if spec.background {
		spec.done = h.deliverFlow
		f, err := h.startFlow(ctx, spec)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		h.setTool(call.ID, func(st *StreamTool) { st.Detail, st.Run = "background "+f.id+" (/workflows)", f.id })
		where := ""
		if f.script != "" {
			where = " (" + f.script + ")"
		}
		return fantasy.NewTextResponse(bgWorkflowStarted + f.id + where + ". Its result arrives as a message when it finishes; do not wait for it."), nil
	}
	spec.progress = func(f *flowRun) { h.setTool(call.ID, func(st *StreamTool) { st.Detail, st.Run = f.summary(), f.id }) }
	f, err := h.startFlow(ctx, spec)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	report, ok := f.report()
	if !ok {
		return fantasy.NewTextErrorResponse(h.truncOutput(t.pipe.Pad, "workflow", report)), nil
	}
	return fantasy.NewTextResponse(h.truncOutput(t.pipe.Pad, "workflow", report)), nil
}

// flowSource reads run id's script from pad once: a sandboxed command or the
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
	if v := pipe.CheckDenied(tools.Request{Tool: "workflow", Subject: "tool.workflow", Arg: id, Description: "rerun " + id, Preview: src}); v.Decision != perm.Allow {
		return "", errors.New(tools.DeniedText(v, "workflow "+id))
	}
	return src, nil
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
	where := "workflow " + f.id
	if f.err != nil {
		msg := where + " failed: " + f.err.Error()
		if errors.Is(f.err, context.Canceled) {
			msg = where + ": the user stopped it; do not start it again unless they ask"
		}
		if f.script != "" {
			log += "\n\nEdit the script if needed and pass run=" + f.id + " with action rerun: the agents that finished answer at once."
		}
		return msg + log, false
	}
	if f.script != "" {
		where += " · " + f.script
	}
	return "[" + where + "]\n\n" + cmp.Or(strings.TrimSpace(f.result), "(the workflow returned nothing)") + log, true
}

// deliverFlow hands a background run's report to the session that started
// it, as a background agent's; a stopped one has none.
func (h *Host) deliverFlow(f *flowRun) {
	if errors.Is(f.err, context.Canceled) {
		return
	}
	attr, status, report := f.reportAs()
	h.deliverReport(f.parent, f.id, attr, status, f.description, report)
}

// reportAs is an ended run's report with the agent attribute and status it
// goes to the model under.
func (f *flowRun) reportAs() (attr, status, report string) {
	attr, status = "workflow", "finished"
	if f.name != "" {
		attr += ":" + f.name
	}
	report, ok := f.report()
	if !ok {
		status = "failed"
	}
	return attr, status, report
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
// /workflows and zemi.workflow

var workflowVerbs = []string{"run", "view", "pause", "resume", "stop", "rerun", "concurrency", "restart", "save", "open"}

// workflowCommands are /workflows and /review.
func (h *Host) workflowCommands() []*Command {
	return []*Command{{
		Name: "workflows", NArgs: NArgsAny, Desc: "see workflow runs and saved workflows, and act on them",
		GoFn: func(h *Host, args string) error {
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
			verb, id := f[0], arg(1)
			if id == "" {
				return Usagef("/workflows %s needs a run id or a name: /workflows lists them", verb)
			}
			// a run's view's keys run these too, and the open screens rebuild at
			// once, keys included
			say := func(out string, err error) error {
				if err == nil {
					h.refreshFlows()
					h.Notify(logging.Info, out, "")
				}
				return err
			}
			switch verb {
			case "run":
				return h.runSaved("/workflows "+strings.Join(f, " "), attached, id, strings.Join(f[2:], " "))
			case "view":
				if arg(2) != "" {
					if err := h.ViewAgentFrom(arg(2), "workflows view "+id); err != nil {
						return err
					}
					// the chat shows the transcript; esc reopens the view
					for _, o := range []string{"workflow:" + id, "workflows"} {
						h.hooks().CloseOverlay(o, lua.OriginBuiltin)
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
				rerun := func() error {
					src, err := h.approveRerun(id)
					if err != nil {
						return err
					}
					return h.startUserFlow(flowSpec{from: id, src: src}, "/workflows rerun "+id, attached)
				}
				if h.hooks() == nil {
					return rerun()
				}
				// its ask blocks, and a Go command runs on the key loop
				h.GoSafe("workflows rerun", func() {
					if err := rerun(); err != nil {
						h.CommandFailed("workflows", err)
					}
				})
				return nil
			case "save":
				path, err := h.saveFlow(id, arg(2))
				return say("Saved as "+TildePath(path), err)
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
		GoFn: func(h *Host, args string) error {
			typed, attached := agent.CutAttached(args)
			return h.runSaved(strings.TrimSpace("/review "+typed), attached, "review", strings.TrimSpace(typed))
		},
	}}
}

// runSaved starts a saved workflow for the user, args its text ("" none).
func (h *Host) runSaved(line, attached, name, args string) error {
	src, err := h.workflowSource(name)
	if err != nil {
		return err
	}
	spec := flowSpec{name: name, src: src}
	if args != "" {
		spec.args = args
	}
	return h.startUserFlow(spec, line, attached)
}

// startUserFlow starts a run the user asked for, with no model call before
// the work. In the TUI it runs in the background and reports to the model
// like a background agent. Headless it claims the agent as /btw does, so
// zemi run waits on it and ctrl+c stops it, and its report starts the turn
// under line, what the user typed, with attached (piped stdin) behind it.
func (h *Host) startUserFlow(spec flowSpec, line, attached string) error {
	if h.hooks() != nil {
		spec.background, spec.done = true, h.deliverFlow
		f, err := h.startFlow(context.Background(), spec)
		if err != nil {
			return err
		}
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "Started workflow " + f.description + " " + f.id + " · /workflows"})
		return nil
	}
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.mu.Unlock()
		return errors.New("a turn is running; start the workflow after it")
	}
	ag.active = true
	scope := h.beginScope()
	ag.mu.Unlock()
	h.GoSafe("workflow", func() {
		defer h.endTurnAfterPanic()
		f, err := h.startFlow(scope.ctx, spec)
		if err == nil && errors.Is(f.err, context.Canceled) {
			err = f.err
		}
		if err != nil {
			h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowError, Text: sanitize.String(h.errorRowText(err))})
			h.handOff(scope)
			return
		}
		attr, status, report := f.reportAs()
		msg := h.reportMessage(h.scratchFor(f.parent), f.id, attr, status, f.description, report)
		if err := h.sendClaimed(scope, line+msg+attached); err != nil {
			h.Log.Warn("workflows", "report not sent", map[string]any{"error": err.Error()})
		}
	})
	return nil
}

// openFlowScript opens a run's script, or a saved workflow's file, in the
// user's editor.
func (h *Host) openFlowScript(id string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	path := ""
	if flowIDShape.MatchString(id) {
		pad := h.ScratchDir()
		if _, err := readPadFile(pad, flowFile(id, ".lua")); err != nil { // no link the editor would follow
			return err
		}
		path = filepath.Join(pad, flowFile(id, ".lua"))
	} else {
		i := slices.IndexFunc(h.workflows(), func(w Workflow) bool { return w.Name == id && w.Source != lua.OriginBuiltin })
		if i < 0 {
			return Usagef("no workflow run or saved file %q", id)
		}
		path = h.workflows()[i].Path
	}
	// the editor blocks and a Go command runs on the key loop
	h.GoSafe("workflows open", func() {
		if err := ui.EditFile(context.Background(), EditRequest{Path: path, Title: "the workflow"}); err != nil {
			h.Notify(logging.Error, "Could not open the workflow", err.Error())
		}
	})
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
// session's too: they block /rewind), then this session's earlier ones,
// then the saved workflows by source.
func (h *Host) workflowsOverlay() Overlay {
	sid := h.AgentSessionID()
	var running, earlier []any
	held := map[string]bool{}
	for _, f := range h.tasks.flowList() {
		held[f.id] = true
		switch live := f.isRunning(); {
		case live && f.parent != sid:
			running = append(running, h.flowItem(f, "Running", " (from "+h.sessionName(f.parent)+")"))
		case live:
			running = append(running, h.flowItem(f, "Running", ""))
		case f.parent == sid:
			earlier = append(earlier, h.flowItem(f, "Earlier in this session", ""))
		}
	}
	var stored []*flowRun
	for _, id := range h.padFlows(sid) {
		if f, err := h.loadFlow(sid, id); err == nil && !held[id] {
			stored = append(stored, f)
		}
	}
	slices.SortFunc(stored, func(a, b *flowRun) int { return b.started.Compare(a.started) })
	for _, f := range stored {
		earlier = append(earlier, h.flowItem(f, "Earlier in this session", ""))
	}
	items := slices.Concat(running, earlier)
	// NOTE: while a run lives, an open list reads the saved files and the
	// pad's journals each second; cache them if a large pad shows in a profile.
	for _, w := range h.workflows() {
		path := ""
		actions := []any{
			map[string]any{"text": "Run", "hotkey": "r", "run": "workflows run " + w.Name},
			map[string]any{"text": "Run with arguments", "hotkey": "a", "fill": "/workflows run " + w.Name + " "},
		}
		if w.Source != lua.OriginBuiltin {
			path = TildePath(w.Path)
			actions = append(actions, map[string]any{"text": "Open in the editor", "hotkey": "o", "run": "workflows open " + w.Name})
		}
		items = append(items, map[string]any{
			"text":    w.Name,
			"desc":    listDesc(w.Description),
			"section": sourceSection(w.Source),
			"fields":  fieldList("source", h.sourceOf(w.Source, w.Path), "path", path),
			"detail":  w.Description,
			"actions": actions,
		})
	}
	return Overlay{
		ID: "workflows", Title: "Workflows", Origin: lua.OriginBuiltin, Items: items,
		Reload: func() ([]any, [][]Segment) { return h.workflowsOverlay().Items, nil },
	}
}

// flowWord is a run's status word, paused while it holds its queue.
func flowWord(v flowView) string {
	if v.paused && v.status == RunRunning {
		return "paused"
	}
	return string(v.status)
}

// flowFields are the name, value pairs a run's detail and view open with,
// each there from the start, so a run that moves adds no row.
func flowFields(f *flowRun, v flowView) []string {
	usage := fmtTokens(v.tokens) + " tok"
	if v.cost > 0 {
		usage += fmt.Sprintf(" · $%.2f", v.cost)
	}
	script := ""
	if f.script != "" {
		script = TildePath(f.script)
	}
	return []string{
		"status", flowWord(v), "agents", fmt.Sprintf("%d/%d done · %d running · %d at once", v.done, len(v.agents), v.running, v.limit),
		"usage", usage, "time", fmtDuration(v.took), "run", f.id, "script", script,
	}
}

// flowDesc is a run's row desc in /workflows and /agents: the padded status
// and the id, then what grows, the cost last since it appears late.
func flowDesc(f *flowRun, v flowView) string {
	desc := fmt.Sprintf("%s · %s · %d/%d agents · %s", statusCol(flowWord(v)), f.id, v.done, len(v.agents), fmtDuration(v.took))
	if v.cost > 0 {
		desc += fmt.Sprintf(" · $%.2f", v.cost)
	}
	return desc
}

// flowItem is a run's row in /workflows: its detail acts on it, and v
// opens its agents.
func (h *Host) flowItem(f *flowRun, section, from string) map[string]any {
	v := f.view()
	actions := []any{map[string]any{"text": "View its agents", "hotkey": "v", "run": "workflows view " + f.id}}
	switch {
	case v.status == RunRunning:
		pause := map[string]any{"text": "Pause", "hotkey": "p", "run": "workflows pause " + f.id}
		if v.paused {
			pause = map[string]any{"text": "Resume", "hotkey": "p", "run": "workflows resume " + f.id}
		}
		actions = append(actions, pause, map[string]any{"text": "Stop", "hotkey": "x", "run": "workflows stop " + f.id, "danger": true})
	case f.script != "" && f.parent == h.AgentSessionID():
		actions = append(actions, map[string]any{"text": "Rerun", "hotkey": "r", "run": "workflows rerun " + f.id})
	}
	if f.script != "" && f.parent == h.AgentSessionID() {
		actions = append(actions,
			map[string]any{"text": "Open the script", "hotkey": "o", "run": "workflows open " + f.id},
			map[string]any{"text": "Save", "hotkey": "s", "fill": "/workflows save " + f.id + " " + slug(f.description)})
	}
	return map[string]any{
		"id": f.id, "text": f.description + from, "desc": flowDesc(f, v), "section": section,
		"fields": fieldList(flowFields(f, v)...), "detail": f.about, "actions": actions,
	}
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug is a saved workflow name made from a description.
func slug(s string) string {
	s = notSlug.ReplaceAllString(strings.ToLower(s), "-") // ascii from here
	return cmp.Or(strings.Trim(s[:min(len(s), 40)], "-"), "workflow")
}

// openFlow opens a run's view: its agents by phase.
func (h *Host) openFlow(id string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	f := h.tasks.flow(id)
	if f == nil {
		if f, err = h.loadFlow(h.AgentSessionID(), id); err != nil {
			return err
		}
	}
	ui.OpenOverlay(h.flowOverlay(f))
	return nil
}

// flowOverlay is a run's view: its fields, then its agents under their
// phases; enter on one opens its detail, whose actions view, restart or
// stop it. p pauses or resumes, + and - change how many run at once.
func (h *Host) flowOverlay(f *flowRun) Overlay {
	v := f.view()
	var items []any
	names, by := f.phases(v.agents)
	for _, p := range names {
		n := 0
		for _, a := range by[p] {
			if a.status != RunRunning && a.status != "queued" {
				n++
			}
		}
		for _, a := range by[p] {
			items = append(items, h.flowAgentItem(f, a, fmt.Sprintf("%s  %d/%d", p, n, len(by[p]))))
		}
	}
	o := Overlay{
		ID: "workflow:" + f.id, Title: "Workflows / " + f.description, Origin: lua.OriginBuiltin, Items: items,
		// one row, so a long one never pushes the list down
		Body: clipRunes(strings.Join(strings.Fields(f.about), " "), 76), Empty: "No agent has started yet.",
		Reload: func() ([]any, [][]Segment) { return h.flowOverlay(f).Items, nil },
	}
	for kv := flowFields(f, v); len(kv) > 0; kv = kv[2:] {
		o.Fields = append(o.Fields, Field{kv[0], kv[1]})
	}
	if v.status == RunRunning {
		// pause is padded to resume's width, so pausing shifts no footer text
		pause := OverlayKey{Key: "p", Desc: "pause ", Run: "workflows pause " + f.id}
		if v.paused {
			pause = OverlayKey{Key: "p", Desc: "resume", Run: "workflows resume " + f.id}
		}
		o.Keys = []OverlayKey{
			pause,
			{Key: "+", Desc: "at once", Run: "workflows concurrency " + f.id + " +1"},
			{Key: "-", Desc: "at once", Run: "workflows concurrency " + f.id + " -1"},
		}
	}
	return o
}

// flowAgentItem is an agent's row in a run's view, keyed by its call's
// index, which a call made later in an earlier phase does not move.
func (h *Host) flowAgentItem(f *flowRun, a agentRow, section string) map[string]any {
	word := statusCol(string(a.status))
	desc := word + " · " + a.def
	if a.reused {
		desc = word + " · reused"
	}
	if a.status != "queued" {
		desc += fmt.Sprintf(" · %d %s · %s tok · %s", a.calls, plural(a.calls, "call", "calls"), fmtTokens(a.tokens), fmtDuration(a.took))
	}
	item := map[string]any{"id": strconv.Itoa(a.call), "text": a.description, "desc": desc, "section": section}
	if a.id == "" {
		return item
	}
	run := h.tasks.find(a.id)
	def := a.def
	if run != nil && run.model != "" {
		def += " · " + run.model
	}
	usage, detail := fmt.Sprintf("%d %s · %s tok", a.calls, plural(a.calls, "call", "calls"), fmtTokens(a.tokens)), ""
	if a.cost > 0 {
		usage += fmt.Sprintf(" · $%.2f", a.cost)
	}
	actions := []any{map[string]any{"text": "View its transcript", "hotkey": "v", "run": "workflows view " + f.id + " " + a.id}}
	if run != nil {
		detail = run.prompt()
		if a.status == RunRunning {
			actions = append(actions,
				map[string]any{"text": "Restart", "hotkey": "r", "run": "workflows restart " + f.id + " " + a.id},
				map[string]any{"text": "Stop", "hotkey": "x", "run": "agents stop " + a.id, "danger": true})
		}
	}
	item["fields"] = fieldList("status", string(a.status), "time", fmtDuration(a.took), "agent", def, "usage", usage, "run", a.id)
	item["detail"], item["actions"] = detail, actions
	return item
}

// watchFlows keeps the open /workflows and run views current once a second
// while any run lives, one ticker for every run; an idle zemi runs none.
func (h *Host) watchFlows() {
	if h.hooks() == nil || !h.tasks.watch() {
		return
	}
	h.GoSafe("workflow views", func() {
		for range time.Tick(time.Second) {
			last := h.tasks.unwatch() // read first, so the last refresh shows the end
			h.refreshFlows()
			if last {
				return
			}
		}
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
