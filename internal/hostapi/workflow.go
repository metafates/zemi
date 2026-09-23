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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/tools"
	zemiruntime "github.com/metafates/zemi/runtime"
)

const (
	workflowConcurrency = 5
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
	Source      string // user, builtin or project
}

// workflows lists saved workflows: user files win over builtin ones, which
// win over project files; project files load only once the project tree is
// trusted.
func (h *Host) workflows() []Workflow {
	var out []Workflow
	seen := map[string]bool{}
	scan := func(dir, source string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), ".lua")
			if !ok || e.IsDir() || seen[name] {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if !skills.ValidName(name) {
				h.RecordErrorOnce("workflows", path+": name must match [a-z0-9][a-z0-9-]*")
				continue
			}
			src, err := readRegularFile(path)
			if err != nil {
				h.RecordErrorOnce("workflows", err.Error())
				continue
			}
			seen[name] = true
			out = append(out, Workflow{Name: name, Description: leadingComment(src), Path: path, Source: source})
		}
	}
	if h.Dirs.ConfigHome != "" {
		scan(filepath.Join(h.Dirs.ConfigHome, "workflows"), "user")
	}
	builtin, _ := fs.ReadDir(zemiruntime.Workflows, "workflows")
	for _, e := range builtin {
		name, ok := strings.CutSuffix(e.Name(), ".lua")
		if !ok || seen[name] {
			continue
		}
		src, _ := fs.ReadFile(zemiruntime.Workflows, "workflows/"+e.Name())
		seen[name] = true
		out = append(out, Workflow{Name: name, Description: leadingComment(string(src)), Path: "builtin:" + e.Name(), Source: "builtin"})
	}
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && h.ProjectTrusted() {
		scan(filepath.Join(pdir, "workflows"), "project")
	}
	return out
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
		if w.Name == name && w.Source == "builtin" {
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
			return "", fmt.Errorf("workflow %q is in %s, and this project is not trusted: the user runs /trust to load it (or it can be saved under %s)", name, pdir, filepath.Join(h.Dirs.ConfigHome, "workflows"))
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no saved workflow %q (save one as workflows/%s.lua)", name, name)
	}
	return "", fmt.Errorf("no saved workflow %q (have %s)", name, strings.Join(names, ", "))
}

// WorkflowResult is a finished run: the script's return value (strings as
// is, anything else JSON), its log lines, and one "<run id> <agent>
// <status>: <description>" line per agent that finished.
type WorkflowResult struct {
	Value  string
	Log    []string
	Agents []string
}

// wfTask is one coroutine: the script body or a parallel() branch.
type wfTask struct {
	co      *glua.LState
	fn      *glua.LFunction // set until the first resume
	parent  *wfTask
	slot    int
	pdepth  int               // pcall nesting; yielding inside pcall would break the coroutine
	kids    []*glua.LFunction // set by parallel() before it yields
	waiting int
	vals    *glua.LTable
	errs    *glua.LTable
	req     *wfAgentReq   // set by agent() before it yields
	args    []glua.LValue // resume values while waiting in ready
}

type wfAgentReq struct {
	def         AgentDef
	description string
	prompt      string
}

type wfDone struct {
	task *wfTask
	req  *wfAgentReq
	id   string
	text string
	err  error
}

type wfRun struct {
	h        *Host
	ctx      context.Context
	L        *glua.LState
	tasks    map[*glua.LState]*wfTask
	queue    []*wfTask
	running  int
	started  int
	finished int
	done     chan wfDone
	phase    string
	log      []string
	agents   []string // WorkflowResult.Agents
	progress func(string)

	ready []*wfTask // parents whose parallel() branches all finished

	result    glua.LValue
	resultErr error
	over      bool
}

// runWorkflow executes src to completion. args is exposed to the script as
// the global args (a string or a decoded JSON value).
func (h *Host) runWorkflow(ctx context.Context, src string, args any, progress func(string)) (WorkflowResult, error) {
	L := glua.NewState(glua.Options{SkipOpenLibs: true, CallStackSize: 256})
	defer L.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	L.SetContext(ctx)
	for _, lib := range []struct {
		name string
		fn   glua.LGFunction
	}{{"", glua.OpenBase}, {glua.TabLibName, glua.OpenTable}, {glua.StringLibName, glua.OpenString}, {glua.MathLibName, glua.OpenMath}} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(glua.LString(lib.name))
		L.Call(1, 0)
	}
	for _, name := range []string{"dofile", "loadfile", "module", "require", "_printregs", "xpcall", "newproxy"} {
		L.SetGlobal(name, glua.LNil)
	}
	lua.CapStringRep(L)
	run := &wfRun{h: h, ctx: ctx, L: L, tasks: map[*glua.LState]*wfTask{}, done: make(chan wfDone, workflowMaxAgents), progress: progress}
	run.install(args)

	fn, err := L.LoadString(src)
	if err != nil {
		return WorkflowResult{}, fmt.Errorf("workflow script: %w", err)
	}
	run.start(fn, nil, 0)
	for {
		// parents resume here, not from inside finish: instant branches
		// would otherwise recurse without bound
		for len(run.ready) > 0 && !run.over {
			t := run.ready[0]
			run.ready = run.ready[1:]
			run.resume(t, t.args...)
		}
		if run.over {
			break
		}
		if run.running == 0 && len(run.queue) == 0 {
			return WorkflowResult{Log: run.log, Agents: run.agents}, errors.New("workflow stalled: every branch is waiting (was agent() called inside pcall?)")
		}
		select {
		case d := <-run.done:
			run.running--
			run.finished++
			run.dispatch()
			if d.id != "" { // a crashed agent returned none
				run.agents = append(run.agents, fmt.Sprintf("%s %s %s: %s", d.id, d.req.def.Name, map[bool]string{true: "done", false: "failed"}[d.err == nil], d.req.description))
			}
			if d.err != nil {
				run.resume(d.task, glua.LNil, glua.LString(h.friendlyError(d.err)))
			} else {
				run.resume(d.task, glua.LString(d.text))
			}
		case <-ctx.Done():
			run.drainAgents()
			return WorkflowResult{Log: run.log, Agents: run.agents}, ctx.Err()
		}
		run.report()
	}
	if run.running > 0 { // a script that returned early leaves agents behind
		cancel()
		run.drainAgents()
	}
	res := WorkflowResult{Log: run.log, Agents: run.agents}
	if run.resultErr != nil {
		return res, run.resultErr
	}
	res.Value = luaResultString(run.result)
	return res, nil
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

func (r *wfRun) install(args any) {
	L := r.L
	L.SetGlobal("args", lua.FromGo(L, args))
	// gopher-lua drops the resume values of a yielding Go function reached
	// by a tail call (`return agent(...)`), so scripts see Lua wrappers that
	// call the Go functions in non-tail position.
	wrap, err := L.LoadString(`local a, p = ...
		return function(...) local r, e = a(...) return r, e end,
			function(...) local r, e = p(...) return r, e end`)
	if err != nil {
		panic(err)
	}
	L.Push(wrap)
	L.Push(L.NewFunction(r.luaAgent))
	L.Push(L.NewFunction(r.luaParallel))
	L.Call(2, 2)
	L.SetGlobal("agent", L.Get(-2))
	L.SetGlobal("parallel", L.Get(-1))
	L.Pop(2)
	L.SetGlobal("phase", L.NewFunction(func(L *glua.LState) int {
		r.phase = L.CheckString(1)
		r.report()
		return 0
	}))
	logFn := L.NewFunction(func(L *glua.LState) int {
		parts := make([]string, 0, L.GetTop())
		for i := 1; i <= L.GetTop(); i++ {
			parts = append(parts, L.ToStringMeta(L.Get(i)).String())
		}
		r.log = append(r.log, strings.Join(parts, " "))
		r.report()
		return 0
	})
	L.SetGlobal("log", logFn)
	L.SetGlobal("print", logFn)
	L.SetGlobal("pcall", L.NewFunction(func(L *glua.LState) int {
		L.CheckAny(1) // PCall with nothing to call dereferences nil
		t := r.tasks[L]
		if t != nil {
			t.pdepth++
		}
		err := L.PCall(L.GetTop()-1, glua.MultRet, nil)
		if t != nil {
			t.pdepth--
		}
		if err != nil {
			L.SetTop(0)
			L.Push(glua.LFalse)
			if ae, ok := errors.AsType[*glua.ApiError](err); ok {
				L.Push(ae.Object)
			} else {
				L.Push(glua.LString(err.Error()))
			}
			return 2
		}
		L.Insert(glua.LTrue, 1)
		return L.GetTop()
	}))
	js := L.NewTable()
	js.RawSetString("encode", L.NewFunction(jsonEncode("json.encode")))
	js.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		var v any
		// Model replies often wrap JSON in prose or fences: take the
		// outermost object or array.
		s := L.CheckString(1)
		if i := strings.IndexAny(s, "{["); i >= 0 {
			if j := strings.LastIndexAny(s, "}]"); j > i {
				s = s[i : j+1]
			}
		}
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(lua.FromGo(L, v))
		return 1
	}))
	L.SetGlobal("json", js)
}

// agent(prompt [, opts]) or agent({prompt=, agent=, description=, model=}):
// returns the agent's report, or nil and an error.
func (r *wfRun) luaAgent(L *glua.LState) int {
	t := r.tasks[L]
	if t == nil {
		L.RaiseError("agent() must run inside the workflow")
	}
	if t.pdepth > 0 {
		L.RaiseError("agent() cannot run inside pcall; it returns nil, err on failure instead")
	}
	var prompt, name, desc, model string
	switch v := L.Get(1).(type) {
	case glua.LString:
		prompt = string(v)
		if opts, ok := L.Get(2).(*glua.LTable); ok {
			name, desc, model = rawString(opts, "agent"), rawString(opts, "description"), rawString(opts, "model")
		}
	case *glua.LTable:
		prompt, name, desc, model = rawString(v, "prompt"), rawString(v, "agent"), rawString(v, "description"), rawString(v, "model")
	}
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
	if r.started >= workflowMaxAgents {
		L.RaiseError("agent: this workflow already started %d agents (the limit)", workflowMaxAgents)
	}
	if desc == "" {
		first, _, _ := strings.Cut(prompt, "\n")
		desc = truncRunes(first, 40)
	}
	r.started++
	t.req = &wfAgentReq{def: def, description: desc, prompt: prompt}
	return L.Yield()
}

// parallel({fn, ...}) runs each function as its own branch and returns the
// results in order plus a table of errors (nil when every branch finished).
func (r *wfRun) luaParallel(L *glua.LState) int {
	t := r.tasks[L]
	if t == nil || t.pdepth > 0 {
		L.RaiseError("parallel() cannot run inside pcall")
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
	st, err, vals, broken := r.safeResume(t, fn, args)
	if broken {
		r.result, r.resultErr, r.over = glua.LNil, err, true
		return
	}
	switch st {
	case glua.ResumeError:
		r.finish(t, glua.LNil, err)
	case glua.ResumeOK:
		// `return agent(...)` hands back agent's nil, err: a failure, not a nil result
		val, err := glua.LValue(glua.LNil), error(nil)
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

// safeResume turns a panic out of the VM into a failed run: gopher-lua
// panics when agent() or parallel() yields from inside a Go library callback
// (table.sort, string.gsub), and the state is not usable afterwards.
func (r *wfRun) safeResume(t *wfTask, fn *glua.LFunction, args []glua.LValue) (st glua.ResumeState, err error, vals []glua.LValue, broken bool) {
	defer func() {
		if p := recover(); p != nil {
			st, broken = glua.ResumeError, true
			err = fmt.Errorf("agent() or parallel() ran where it cannot wait (inside pcall or a library callback such as table.sort): %v", p)
		}
	}()
	st, err, vals = r.L.Resume(t.co, fn, args...)
	return st, err, vals, false
}

func (r *wfRun) dispatch() {
	for r.running < workflowConcurrency && len(r.queue) > 0 {
		t := r.queue[0]
		r.queue = r.queue[1:]
		req := t.req
		t.req = nil
		r.running++
		r.h.GoSafe("workflow agent", func() {
			// deferred so a panicking agent still reports: the run waits on done
			d := wfDone{task: t, req: req, err: errors.New("the agent crashed")}
			defer func() { r.done <- d }()
			d.id, d.text, d.err = r.h.runSubagent(r.ctx, req.def, req.description, req.prompt, nil)
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
		var errs glua.LValue = glua.LNil
		if p.errs.MaxN() > 0 {
			errs = p.errs
		}
		p.args = []glua.LValue{p.vals, errs}
		r.ready = append(r.ready, p)
	}
}

func (r *wfRun) report() {
	if r.progress == nil {
		return
	}
	var parts []string
	if r.phase != "" {
		parts = append(parts, r.phase)
	}
	if r.started > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d agents done", r.finished, r.started))
	}
	if n := len(r.log); n > 0 {
		parts = append(parts, truncRunes(r.log[n-1], 60))
	}
	r.progress(strings.Join(parts, " · "))
}

// ---------------------------------------------------------------------------
// the workflow tool

type workflowTool struct {
	h     *Host
	saved []Workflow
}

func (h *Host) workflowTool() fantasy.AgentTool { return &workflowTool{h: h, saved: h.workflows()} }

const workflowToolDesc = `Run a Lua script that coordinates subagents when code has to sit between them: stages that feed each other, loops, checking results. Independent tasks need no workflow. Pass a saved workflow's name, or a new script after loading the zemi-customize skill for the API.`

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

func (t *workflowTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *workflowTool) SetProviderOptions(fantasy.ProviderOptions) {}

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
	if resp, stop, err := h.preCheck("workflow", call.Input); stop {
		return resp, err
	}
	defer h.postNotify("workflow", call.Input, &reply)
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
	preview := "+ " + strings.ReplaceAll(strings.TrimRight(src, "\n"), "\n", "\n+ ")
	if v := h.PipelineFor().CheckDenied(tools.Request{Tool: "workflow", Subject: "tool.workflow", Arg: arg, Description: in.Description, Preview: preview}); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse(tools.DeniedText(v, "workflow "+arg)), nil
	}
	start := time.Now()
	var last atomic.Value
	res, err := h.runWorkflow(ctx, src, in.Args, func(line string) {
		last.Store(line)
		h.setToolDetail(call.ID, line)
	})
	summary, _ := last.Load().(string)
	h.setToolDetail(call.ID, strings.TrimPrefix(summary+" · "+fmtDuration(time.Since(start)), " · "))
	out := res.Value
	if len(res.Log) > 0 {
		out += "\n\nlog:\n" + strings.Join(res.Log, "\n")
	}
	if err != nil {
		// the reports are lost with the script, the runs are not
		if len(res.Agents) > 0 && h.hooks() != nil { // headless has no send_to
			out += "\n\nagents that finished (continue one with the agent tool's send_to):\n" + strings.Join(res.Agents, "\n")
		}
		msg := "workflow failed: " + err.Error()
		if errors.Is(err, context.Canceled) {
			msg = "workflow cancelled"
		}
		return fantasy.NewTextErrorResponse(h.truncOutput("workflow", strings.TrimSpace(msg+"\n"+out))), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(the workflow returned nothing)"
	}
	return fantasy.NewTextResponse(h.truncOutput("workflow", out)), nil
}

// truncOutput cuts a host tool's output to what a result may carry and
// saves the whole text to the session scratchpad, so the rest is one read
// away instead of gone.
func (h *Host) truncOutput(name, s string) string {
	cut := tools.TruncOut(s, 30000)
	if cut == s {
		return s
	}
	return cut + tools.Spill(h.ScratchDir(), name, s)
}

// ---------------------------------------------------------------------------
// /workflow and zemi.workflow

// workflowCommands are /workflow and /review.
func (h *Host) workflowCommands() []*Command {
	return []*Command{{
		Name: "workflow", NArgs: "*", Desc: "run a saved workflow",
		GoFn: func(h *Host, args string) error {
			name, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
			if name == "" {
				return h.openWorkflows()
			}
			if _, err := h.workflowSource(name); err != nil {
				return err
			}
			// The agent runs it through the workflow tool, so progress,
			// permissions and the result land in the conversation.
			msg := "Run the saved workflow `" + name + "` with the workflow tool"
			if rest = strings.TrimSpace(rest); rest != "" {
				msg += " and args " + strconv.Quote(rest)
			}
			return h.AgentSend(msg + ", then relay its result.")
		},
		GoComplete: func(h *Host, _, _ string) []string {
			var names []string
			for _, w := range h.workflows() {
				names = append(names, w.Name)
			}
			return names
		},
	}, {
		Name: "review", NArgs: "?", Desc: "review your changes, a base ref or a commit, with several agents",
		GoFn: func(h *Host, args string) error {
			return h.RunCommand(context.Background(), "workflow", strings.TrimSpace("review "+args))
		},
	}}
}

func (h *Host) openWorkflows() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
	}
	var items []any
	for _, w := range h.workflows() {
		detail := strings.TrimLeft(w.Description+"\n\nsource: "+w.Source, "\n")
		if w.Source != "builtin" {
			detail += "\npath: " + TildePath(w.Path)
		}
		items = append(items, map[string]any{
			"text":    w.Name,
			"desc":    w.Description,
			"section": sourceSection(w.Source),
			"detail":  detail,
			"actions": []any{
				map[string]any{"text": "Run", "run": "workflow " + w.Name},
				map[string]any{"text": "Run with arguments", "fill": "/workflow " + w.Name + " "},
			},
		})
	}
	ui.OpenOverlay(Overlay{ID: "workflows", Title: "Workflows", Origin: lua.OriginBuiltin, Items: items,
		Empty: "No saved workflows. Add workflows/<name>.lua to " + TildePath(h.Dirs.ConfigHome) + " or .zemi."})
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
		src := rawString(spec, "script")
		if name := rawString(spec, "name"); name != "" {
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
		ctx, cancel := context.WithCancel(context.Background())
		at := h.callerOf(L)
		h.GoSafe("zemi.workflow.run", func() {
			defer cancel()
			res, err := h.runWorkflow(ctx, src, args, nil)
			switch {
			case onDone == nil:
			case err != nil:
				h.callLuaAsync(at, onDone, nil, err.Error(), res.Log)
			default:
				h.callLuaAsync(at, onDone, res.Value, nil, res.Log)
			}
		})
		handle := L.NewTable()
		handle.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
			cancel()
			return 0
		}))
		lua.GuardFuncs(L, handle, "workflow:", h.guardModelLua)
		L.Push(handle)
		return 1
	}))
	return tb
}
