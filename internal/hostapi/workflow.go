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
			if !ok || e.IsDir() || seen[name] || !skills.ValidName(name) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			src, err := readWorkflowFile(path)
			if err != nil {
				h.Log.Warn("workflows", err.Error(), nil)
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
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && h.projectTrusted.Load() {
		scan(filepath.Join(pdir, "workflows"), "project")
	}
	return out
}

func readWorkflowFile(path string) (string, error) {
	_, body, err := readFrontmatterFile(path)
	return body, err
}

func leadingComment(src string) string {
	var lines []string
	for _, l := range strings.Split(src, "\n") {
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
			return readWorkflowFile(w.Path)
		}
		names = append(names, w.Name)
	}
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && !h.projectTrusted.Load() {
		if _, err := os.Stat(filepath.Join(pdir, "workflows", name+".lua")); err == nil {
			return "", fmt.Errorf("workflow %q is in %s, which is not trusted yet: the user runs /Reload and trusts it (or it can be saved under %s)", name, pdir, filepath.Join(h.Dirs.ConfigHome, "workflows"))
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no saved workflow %q (save one as workflows/%s.lua)", name, name)
	}
	return "", fmt.Errorf("no saved workflow %q (have %s)", name, strings.Join(names, ", "))
}

// WorkflowResult is a finished run: the script's return value (strings as
// is, anything else JSON) and its log lines.
type WorkflowResult struct {
	Value  string
	Log    []string
	Agents int
}

// wfTask is one coroutine: the script body or a parallel() branch.
type wfTask struct {
	co      *glua.LState
	fn      *glua.LFunction // set until the first resume
	parent  *wfTask
	slot    int
	pdepth  int // pcall nesting; yielding inside pcall would break the coroutine
	pending []glua.LValue
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
			return WorkflowResult{}, errors.New("workflow stalled: every branch is waiting (was agent() called inside pcall?)")
		}
		select {
		case d := <-run.done:
			run.running--
			run.finished++
			run.dispatch()
			if d.err != nil {
				run.resume(d.task, glua.LNil, glua.LString(friendlyError(d.err)))
			} else {
				run.resume(d.task, glua.LString(d.text))
			}
		case <-ctx.Done():
			// in-flight agents share ctx and return promptly; drain them
			for run.running > 0 {
				<-run.done
				run.running--
			}
			return WorkflowResult{}, ctx.Err()
		}
		run.report()
	}
	for run.running > 0 { // a script that returned early leaves agents behind
		cancel()
		<-run.done
		run.running--
	}
	res := WorkflowResult{Log: run.log, Agents: run.started}
	if run.resultErr != nil {
		return res, run.resultErr
	}
	res.Value = luaResultString(run.result)
	return res, nil
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
			if ae, ok := err.(*glua.ApiError); ok {
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
	js.RawSetString("encode", L.NewFunction(func(L *glua.LState) int {
		g, err := lua.ToGo(L.CheckAny(1))
		if err != nil {
			L.RaiseError("json.encode: %v", err)
		}
		b, err := json.Marshal(g)
		if err != nil {
			L.RaiseError("json.encode: %v", err)
		}
		L.Push(glua.LString(b))
		return 1
	}))
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

// agent(prompt [, opts]) or agent({prompt=, agent=, description=}):
// returns the agent's report, or nil and an error.
func (r *wfRun) luaAgent(L *glua.LState) int {
	t := r.tasks[L]
	if t == nil {
		L.RaiseError("agent() must run inside the workflow")
		return 0
	}
	if t.pdepth > 0 {
		L.RaiseError("agent() cannot run inside pcall; it returns nil, err on failure instead")
		return 0
	}
	var prompt, name, desc string
	switch v := L.Get(1).(type) {
	case glua.LString:
		prompt = string(v)
		if opts, ok := L.Get(2).(*glua.LTable); ok {
			name, desc = rawString(opts, "agent"), rawString(opts, "description")
		}
	case *glua.LTable:
		prompt, name, desc = rawString(v, "prompt"), rawString(v, "agent"), rawString(v, "description")
	}
	if strings.TrimSpace(prompt) == "" {
		L.RaiseError("agent: prompt is required")
		return 0
	}
	def, err := r.h.agentDef(name)
	if err != nil {
		L.RaiseError("agent: %v", err)
		return 0
	}
	if r.started >= workflowMaxAgents {
		L.RaiseError("agent: this workflow already started %d agents (the limit)", workflowMaxAgents)
		return 0
	}
	if desc == "" {
		desc = truncRunes(strings.SplitN(prompt, "\n", 2)[0], 40)
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
		return 0
	}
	list := L.CheckTable(1)
	n := list.Len()
	for i := 1; i <= n; i++ {
		fn, ok := list.RawGetInt(i).(*glua.LFunction)
		if !ok {
			L.RaiseError("parallel: item %d is not a function", i)
			return 0
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
		r.finish(t, vals[0], nil)
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
			text, err := r.h.runSubagent(r.ctx, req.def, req.description, req.prompt, nil)
			r.done <- wfDone{task: t, text: text, err: err}
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
		if p.errs.Len() > 0 || p.errs.MaxN() > 0 {
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

const workflowToolDesc = `Run a Lua workflow that coordinates subagents: fan out independent work, chain stages, verify results. Prefer a single agent call for one task; use a workflow when the shape is several agents with logic between them.

The script runs in a sandbox with Lua's base, string, table and math libraries and:
- agent(prompt [, {agent = "explore", description = "..."}]) -> report or nil, err. Blocks this branch until the subagent finishes. Agents cannot see this conversation: prompts must be self-contained.
- parallel({fn, fn, ...}) -> results, errs. Runs each function as a concurrent branch (up to 5 agents at once); results[i] is fn i's return value, errs[i] its error (errs is nil when none failed).
- phase(name) labels progress for the user; log(...) records a line returned with the result.
- json.encode(v), json.decode(s) (decode tolerates prose around the JSON); args holds the tool's args.
The script's return value is the tool result (tables come back as JSON). agent() and parallel() cannot run inside pcall or library callbacks (table.sort, string.gsub). No file or network access: only agents act.
To keep a workflow the user wants again, write the script with your file tools to <user config>/workflows/<name>.lua (every project, usable at once) or <project config>/workflows/<name>.lua (this repository, usable after the user trusts the project config on /Reload), starting with -- comment lines that describe it; it then runs as /workflow <name> or with name instead of script. The system prompt names both directories.

Example:
  phase("review")
  local lenses = {"correctness", "security", "simplicity"}
  local jobs = {}
  for _, lens in ipairs(lenses) do
    jobs[#jobs + 1] = function()
      return agent("Review the diff from 'git diff main' for " .. lens .. " issues. Report each with file:line.", {agent = "explore"})
    end
  end
  local reports = parallel(jobs)
  phase("verify")
  return agent("Check each finding against the code and keep only real ones:\n" .. table.concat(reports, "\n\n"))`

func (t *workflowTool) Info() fantasy.ToolInfo {
	desc := workflowToolDesc
	if len(t.saved) > 0 {
		desc += "\n\nSaved workflows (pass name instead of script):"
		for _, w := range t.saved {
			desc += "\n- " + w.Name + ": " + truncRunes(w.Description, 160)
		}
	}
	return fantasy.ToolInfo{
		Name:        "workflow",
		Description: desc,
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

func (t *workflowTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	h := t.h
	var in struct {
		Description string `json:"description"`
		Script      string `json:"script"`
		Name        string `json:"name"`
		Args        any    `json:"args"`
	}
	if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
		return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
	}
	if resp, stop, err := h.preCheck("workflow", call.Input); stop {
		return resp, err
	}
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
	if v := h.PipelineFor().Check(tools.Request{Subject: "tool.workflow", Arg: arg, Reason: in.Description, Preview: preview}); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): workflow " + arg), nil
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
		if errors.Is(err, context.Canceled) {
			return fantasy.NewTextErrorResponse("workflow cancelled"), nil
		}
		return fantasy.NewTextErrorResponse(truncOutput("workflow failed: " + err.Error() + "\n" + out)), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(the workflow returned nothing)"
	}
	return fantasy.NewTextResponse(truncOutput(out)), nil
}

func truncOutput(s string) string {
	const max = 30000
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}

// ---------------------------------------------------------------------------
// /workflow and zemi.workflow

// WorkflowCommandsStep registers /workflow.
func (h *Host) WorkflowCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "workflow commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "workflow", NArgs: "*", Desc: "run a saved workflow: /workflow <name> [args]",
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
						msg += " and args " + strconvQuote(rest)
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
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			if err != nil {
				return err
			}
			_, err = h.Commands.Add(&Command{
				Name: "review", NArgs: "?", Desc: "review changes with parallel agents, then verify the findings: /review [git ref]",
				GoFn: func(h *Host, args string) error {
					return h.RunCommand(context.Background(), "workflow", strings.TrimSpace("review "+args))
				},
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			return err
		},
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (h *Host) openWorkflows() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
	}
	list := h.workflows()
	if len(list) == 0 {
		return errors.New("no saved workflows yet: add workflows/<name>.lua to ~/.config/zemi or .zemi")
	}
	items := make([]any, 0, len(list))
	for _, w := range list {
		items = append(items, map[string]any{
			"text": fmt.Sprintf("%-16s %-8s %s", w.Name, w.Source, truncRunes(w.Description, 70)),
			"run":  "workflow " + w.Name,
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
		out := L.NewTable()
		for _, w := range h.workflows() {
			row := L.NewTable()
			row.RawSetString("name", glua.LString(w.Name))
			row.RawSetString("description", glua.LString(w.Description))
			row.RawSetString("source", glua.LString(w.Source))
			row.RawSetString("path", glua.LString(w.Path))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("run", L.NewFunction(func(L *glua.LState) int {
		spec := L.CheckTable(1)
		onDone, _ := L.Get(2).(*glua.LFunction)
		// agents act with the user's tools and permissions: only user config
		// (and shipped code) may start them from Lua
		if o := h.RT.CurrentOrigin(); o != lua.OriginUser && o != lua.OriginBuiltin {
			L.RaiseError("zemi.workflow.run: only user config may run workflows")
			return 0
		}
		src := rawString(spec, "script")
		if name := rawString(spec, "name"); name != "" {
			s, err := h.workflowSource(name)
			if err != nil {
				L.RaiseError("zemi.workflow.run: %v", err)
				return 0
			}
			src = s
		}
		if strings.TrimSpace(src) == "" {
			L.RaiseError("zemi.workflow.run: script or name is required")
			return 0
		}
		args, err := lua.ToGo(spec.RawGetString("args"))
		if err != nil {
			L.RaiseError("zemi.workflow.run: args: %v", err)
			return 0
		}
		ctx, cancel := context.WithCancel(context.Background())
		gen := h.currentGen()
		h.bgAgents.Add(1)
		h.refresh()
		h.GoSafe("zemi.workflow.run", func() {
			defer cancel()
			defer func() { h.bgAgents.Add(-1); h.refresh() }()
			res, err := h.runWorkflow(ctx, src, args, nil)
			if onDone == nil {
				return
			}
			_, _ = h.RT.Do(context.Background(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
				if h.currentGen() != gen {
					return nil, nil
				}
				L.Push(onDone)
				if err != nil {
					L.Push(glua.LNil)
					L.Push(glua.LString(err.Error()))
				} else {
					L.Push(glua.LString(res.Value))
					L.Push(glua.LNil)
				}
				L.Push(lua.FromGo(L, res.Log))
				return nil, L.PCall(3, 0, nil)
			})
		})
		handle := L.NewTable()
		handle.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
			cancel()
			return 0
		}))
		L.Push(handle)
		return 1
	}))
	return tb
}
