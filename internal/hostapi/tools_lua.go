// tools_lua.go bridges Lua-registered tools and overrides into the tool
// pipeline (DESIGN §6.4): the zemi.tool namespace, the tool.pre hook
// with the full decision contract, and the model-facing tool set.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// luaTool is one zemi.tool.add entry: interpreter-owned callback,
// gen-scoped so a reload drops it (DESIGN §4).
type luaTool struct {
	Name     string
	Desc     string
	Schema   map[string]any
	Required []string
	Prompt   string
	Parallel bool
	Timeout  time.Duration
	Fn       *glua.LFunction
	Origin   lua.Origin
	Path     string
	Gen      Gen
}

// overrideRec is one zemi.tool.override of a tool: its pre and post
// handlers, either nil.
type overrideRec struct {
	Tool      string
	Pre, Post *glua.LFunction
	Origin    lua.Origin
	Path      string
	Gen       Gen
}

type toolReg struct {
	mu        sync.Mutex
	byName    map[string]*luaTool
	overrides map[string][]*overrideRec
	owner     func(path string) string // Host.PluginOf, see mayReplace
}

func newToolReg() *toolReg {
	return &toolReg{byName: map[string]*luaTool{}, overrides: map[string][]*overrideRec{}}
}

func (r *toolReg) add(t *luaTool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byName[t.Name]; ok {
		if err := mayReplace(r.owner, fmt.Sprintf("tool %q", t.Name), old.Origin, old.Path, t.Origin, t.Path); err != nil {
			return err
		}
	}
	r.byName[t.Name] = t
	return nil
}

func (r *toolReg) get(name string) *luaTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byName[name]
}

func (r *toolReg) list() []*luaTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.SortedFunc(maps.Values(r.byName), func(a, b *luaTool) int { return cmp.Compare(a.Name, b.Name) })
}

// toolSections renders the prompt of each Lua tool in toolset, so a profile
// that drops the tool drops its section. Name order and fixed text keep the
// provider's prompt cache; project prompts stay out, like their
// agent.assemble sections.
func (h *Host) toolSections(toolset []fantasy.AgentTool) string {
	var out strings.Builder
	for _, t := range h.luaTools.list() {
		if t.Prompt == "" || t.Origin.Rank() <= lua.OriginProject.Rank() || !hasTool(toolset, t.Name) {
			continue
		}
		out.WriteString("\n\n## Tool " + t.Name + " (from " + string(t.Origin) + " config)\n" + t.Prompt[:min(len(t.Prompt), assembleSectionMax)])
	}
	return out.String()
}

func (r *toolReg) addOverride(o *overrideRec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrides[o.Tool] = append(r.overrides[o.Tool], o)
}

func (r *toolReg) overridesFor(tool string) []*overrideRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.overrides[tool])
}

func (r *toolReg) overridden() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.overrides))
}

func (r *toolReg) purge(gen Gen) {
	r.mu.Lock()
	defer r.mu.Unlock()
	maps.DeleteFunc(r.byName, func(_ string, t *luaTool) bool { return t.Gen == gen })
	for tool, list := range r.overrides {
		r.overrides[tool] = slices.DeleteFunc(list, func(o *overrideRec) bool { return o.Gen == gen })
	}
	maps.DeleteFunc(r.overrides, func(_ string, list []*overrideRec) bool { return len(list) == 0 })
}

// builtinToolNames are the names Lua tools may not take: the provider would
// get two tools of one name and fantasy runs the last one. tool_search joins
// the set only past ten MCP tools, so it is reserved here by name.
func (h *Host) builtinToolNames() []string {
	names := []string{tools.ToolSearch}
	for _, t := range h.builtinTools(nil) {
		names = append(names, t.Info().Name)
	}
	return names
}

func (h *Host) toolNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.tool.add: spec must be a table")
		}
		fn, ok := opt[*glua.LFunction](L, spec, "execute")
		if !ok {
			L.RaiseError("zemi.tool.add: execute callback is required")
		}
		name := rawString(L, spec, "name")
		if !toolName.MatchString(name) {
			L.RaiseError("zemi.tool.add: name must be an identifier")
		}
		if slices.Contains(h.builtinToolNames(), name) {
			L.RaiseError("zemi.tool.add: %q is a builtin tool name", name)
		}
		desc := rawString(L, spec, "description")
		if desc == "" {
			L.RaiseError("zemi.tool.add: description is required; the model picks the tool by it")
		}
		// input is the parameters themselves, flat; what reaches the provider
		// is only ever this map (tools.SchemaProperties)
		schema := map[string]any{"type": "object"}
		required := rawStrings(L, spec, "required")
		if v := spec.RawGetString("input"); v != glua.LNil {
			props, err := lua.ToGo(v)
			if err != nil {
				L.RaiseError("zemi.tool.add: input: %v", err)
			}
			m, ok := props.(map[string]any)
			if !ok {
				L.RaiseError("zemi.tool.add: input must map a parameter name to its schema, got %s", luaShape(props))
			}
			if _, wrapped := m["properties"]; wrapped {
				L.RaiseError(`zemi.tool.add: input holds "properties"; write the parameters flat: input = { path = { type = "string" } }`)
			}
			if _, inside := m["required"]; inside {
				L.RaiseError("zemi.tool.add: required is a field of the spec, beside input, not inside it")
			}
			for _, r := range required {
				if _, ok := m[r]; !ok {
					L.RaiseError("zemi.tool.add: required names %q, which input does not define", r)
				}
			}
			schema["properties"] = m
		} else if len(required) > 0 {
			L.RaiseError("zemi.tool.add: required names parameters but there is no input")
		}
		timeout := 60 * time.Second
		if ms, ok := opt[glua.LNumber](L, spec, "timeout_ms"); ok && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
		lt := &luaTool{
			Name: name, Desc: desc,
			Schema: schema, Required: required,
			Prompt:   strings.TrimSpace(rawString(L, spec, "prompt")),
			Parallel: rawBool(L, spec, "parallel"),
			Timeout:  timeout, Fn: fn,
			Origin: h.RT.CurrentOrigin(L), Path: h.RT.CurrentPath(L),
			Gen: h.currentGen(),
		}
		if err := h.luaTools.add(lt); err != nil {
			L.RaiseError("zemi.tool.add: %v", err)
		}
		return 0
	}))

	tb.RawSetString("override", L.NewFunction(func(L *glua.LState) int {
		if o := h.RT.CurrentOrigin(L); o.Rank() <= lua.OriginProject.Rank() {
			L.RaiseError("zemi.tool.override: %s origin may not register rewrites (DESIGN §5)", o)
		}
		name := L.CheckString(1)
		spec, ok := L.Get(2).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.tool.override: spec must be a table")
		}
		pre, _ := opt[*glua.LFunction](L, spec, "pre")
		post, _ := opt[*glua.LFunction](L, spec, "post")
		if pre == nil && post == nil {
			L.RaiseError("zemi.tool.override: a pre or post callback is required")
		}
		h.luaTools.addOverride(&overrideRec{
			Tool: name, Pre: pre, Post: post,
			Origin: h.RT.CurrentOrigin(L), Path: h.RT.CurrentPath(L),
			Gen: h.currentGen(),
		})
		return 0
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, name := range h.builtinToolNames() {
			rows = append(rows, map[string]any{"name": name, "builtin": true})
		}
		for _, t := range h.luaTools.list() {
			rows = append(rows, map[string]any{"name": t.Name, "builtin": false, "origin": string(t.Origin)})
		}
		// the MCP tools too: their schemas may be deferred from the model's own
		// list, so this is where a script learns the name to call
		for _, o := range h.mcpOffered() {
			rows = append(rows, map[string]any{"name": o.name, "builtin": false, "mcp": o.srv.name})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	// the lua tool's own running script drives the run's tools with these
	tb.RawSetString("call", L.NewFunction(h.luaToolCall))
	tb.RawSetString("describe", L.NewFunction(h.luaToolDescribe))

	return tb
}

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// luaFantasyTools wraps every registered Lua tool as a fantasy agent tool,
// checked through pipe.
func (h *Host) luaFantasyTools(pipe *tools.Pipeline) []fantasy.AgentTool {
	var out []fantasy.AgentTool
	for _, t := range h.luaTools.list() {
		out = append(out, &luaToolAdapter{h: h, t: t, pipe: pipe})
	}
	return out
}

// luaToolAdapter runs a Lua tool through tool.pre, the permission gate
// (tool.<name>) and the interpreter (DESIGN §6.4).
type luaToolAdapter struct {
	tools.NoProviderOptions
	h    *Host
	t    *luaTool
	pipe *tools.Pipeline
}

func (a *luaToolAdapter) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        a.t.Name,
		Description: a.t.Desc,
		Parameters:  tools.SchemaProperties(a.t.Schema),
		Required:    append([]string{}, a.t.Required...),
		Parallel:    a.t.Parallel,
	}
}

func (a *luaToolAdapter) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	input, err := tools.ParseInput(a.Info(), call.Input, tools.SchemaOpen(a.t.Schema))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := a.h.preCheck(a.t.Name, call.Input); stop {
		return resp, nil
	}
	defer a.h.postNotify(ctx, a.pipe.Pad, a.t.Name, call.Input, &reply, time.Now())
	req := tools.Request{Tool: a.t.Name, Subject: "tool." + a.t.Name, Arg: a.t.Name, Preview: call.Input, Owner: a.h.OriginBadge(a.t.Origin, a.t.Path)}
	if v := a.pipe.CheckDenied(req); !v.Allowed {
		return fantasy.NewTextErrorResponse(tools.DeniedText(v, req.Subject)), nil
	}
	out, err := a.h.runLuaTool(ctx, a.t, call.ID, input)
	if err != nil {
		out = err.Error()
	}
	reply = fantasy.NewTextResponse(a.h.truncOutput(a.pipe.Pad, a.t.Name, out))
	reply.IsError = err != nil
	return reply, nil
}

// runLuaTool runs execute as a task, bounded by the tool's timeout in wall
// time; a wait in it frees the interpreter, and an interrupt ends it. A
// string result or a table's output field is the output; anything else is
// sent as JSON. nil, err fails the call with err alone (DESIGN §6.0). A
// raised error reaches the model without its traceback, which names the
// user's config paths; /errors keeps it.
func (h *Host) runLuaTool(ctx context.Context, t *luaTool, callID string, input map[string]any) (string, error) {
	// a turn's runner keeps the tools of the generation it started in: run
	// what a reload since registered under the name
	if cur := h.luaTools.get(t.Name); cur != nil {
		t = cur
	}
	ctx, cancel := context.WithTimeoutCause(ctx, t.Timeout, fmt.Errorf("timed out after %s", t.Timeout))
	defer cancel()
	res, err := h.RT.RunTask(ctx, lua.Task{
		Class: lua.ClassAgent, Ceiling: t.Timeout, Origin: t.Origin, Path: t.Path,
		Start: func(L *glua.LState) (*glua.LFunction, []glua.LValue, error) {
			// never call into a dropped generation
			if h.currentGen() != t.Gen {
				return nil, nil, errors.New("the tool is gone after a config reload")
			}
			return t.Fn, []glua.LValue{lua.FromGo(L, input), h.toolRun(L, t, callID)}, nil
		},
		Done: func(L *glua.LState, rets []glua.LValue) ([]any, error) {
			if len(rets) > 1 && rets[0] == glua.LNil && rets[1] != glua.LNil {
				return nil, errors.New(rets[1].String())
			}
			v, err := lua.ToGo(rets[0])
			return []any{v}, err
		},
	})
	switch _, raised := errors.AsType[*glua.ApiError](err); {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = context.Cause(ctx)
	case raised:
		h.RecordError("tool "+t.Name, err.Error())
		err = errText(err)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.Name, err)
	}
	if len(res) == 0 || res[0] == nil {
		return "", nil
	}
	if s, ok := res[0].(string); ok {
		return s, nil
	}
	if m, ok := res[0].(map[string]any); ok {
		if out, ok := m["output"].(string); ok {
			return out, nil
		}
	}
	b, err := json.Marshal(res[0], json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("%s: the result is not JSON: %w", t.Name, err)
	}
	return string(b), nil
}

// toolRun is the run handle execute gets: progress shows one line on the
// call's row, as a command's last output line does.
func (h *Host) toolRun(L *glua.LState, t *luaTool, callID string) *glua.LTable {
	run := L.NewTable()
	run.RawSetString("name", glua.LString(t.Name))
	run.RawSetString("origin", glua.LString(t.Origin))
	run.RawSetString("progress", L.NewFunction(func(L *glua.LState) int {
		line := sanitize.Line(L.CheckString(2))
		h.setToolDetail(callID, clipRunes(line, 120))
		return 0
	}))
	lua.GuardFuncs(L, run, "run:", h.guardLua)
	return run
}

// toolPreHook adapts the tool.pre events plus zemi.tool.override pre
// handlers into the pipeline PreHook, with the full decision contract and
// shallow rewrite merging (DESIGN §6.2).
func (h *Host) toolPreHook() tools.PreHook {
	return func(name string, input map[string]any) (decision tools.PreVerdict, reason string, rewritten map[string]any) {
		h.outsideReload(func() {
			decision, reason, rewritten = h.toolPre(name, input, h.hookHandlers(EventToolPre, name))
		})
		return decision, reason, rewritten
	}
}

// hookHandlers is event's handlers, then the pre or post of each
// zemi.tool.override of tool.
func (h *Host) hookHandlers(event, tool string) []*EventHandler {
	handlers := h.Events.Handlers(event)
	for _, o := range h.luaTools.overridesFor(tool) {
		fn := o.Post
		if event == EventToolPre {
			fn = o.Pre
		}
		if fn != nil {
			handlers = append(handlers, &EventHandler{Event: event, Fn: fn, Origin: o.Origin, Path: o.Path, Gen: o.Gen})
		}
	}
	return handlers
}

// toolPre runs handlers as tool.pre for one call of name.
func (h *Host) toolPre(name string, input map[string]any, handlers []*EventHandler) (tools.PreVerdict, string, map[string]any) {
	h.recordEvent(EventToolPre, "host", map[string]any{"tool": name, "input": input})
	if len(handlers) == 0 {
		return "", "", nil
	}
	var decision tools.PreVerdict
	var reason string
	var rewritten map[string]any
	// twice: the deny-only re-run after a rewrite calls every handler again
	ceiling := lua.VetoCeiling(len(handlers), 2)
	_, err := h.RT.Do(context.Background(), lua.ClassAgent, ceiling, 0, func(L *glua.LState) ([]any, error) {
		merged := maps.Clone(input)
		if merged == nil {
			merged = map[string]any{}
		}
		for _, hd := range handlers {
			d, r, rw := h.callToolPre(L, hd, name, merged)
			switch {
			case d == tools.PreDeny || d == tools.PreHalt:
				decision, reason, rewritten = d, r, nil
				return nil, nil
			case d == tools.PreRewrite && rw != nil && hd.Origin.Rank() <= lua.OriginProject.Rank():
				// same denial as zemi.tool.override (DESIGN §5)
				h.Log.Warn("tool.pre rewrite ignored", "origin", "tools", "tool", name, "from", string(hd.Origin))
			case d == tools.PreRewrite && rw != nil:
				maps.Copy(merged, rw)
				decision, rewritten = tools.PreRewrite, merged
			}
		}
		// deny-only re-run over the rewritten input, so a later rewrite
		// cannot slip past an earlier deny hook (DESIGN §6.2)
		if decision == tools.PreRewrite {
			for _, hd := range handlers {
				if d, r, _ := h.callToolPre(L, hd, name, merged); d == tools.PreDeny {
					decision, reason, rewritten = d, r, nil
					return nil, nil
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		h.Log.Warn("tool.pre run failed", "origin", "tools", "tool", name, "error", err)
		if !errors.Is(err, lua.ErrAbandoned) {
			// a hot runtime cannot run the deny hooks: fail closed
			return tools.PreDeny, "tool.pre hooks could not run (lua runtime unavailable, /reload): " + err.Error(), nil
		}
		return "", "", nil
	}
	return decision, reason, rewritten
}

// callToolPre runs one pre handler and parses the decision table.
func (h *Host) callToolPre(L *glua.LState, hd *EventHandler, tool string, input map[string]any) (decision tools.PreVerdict, reason string, rewritten map[string]any) {
	ev := map[string]any{"name": EventToolPre, "from": "host", "tool": tool, "input": input}
	ret, _, err := h.callEvent(L, hd, ev)
	if err != nil {
		h.logHandlerError(hd, err) // proceed is the tool.pre default (DESIGN §4)
		return "", "", nil
	}
	tb, d, bad := vetoDecision(ret, tools.PreVerdicts...)
	if bad != "" {
		// a misspelled deny must not read as consent (DESIGN §6.2)
		h.logHandlerError(hd, errors.New(bad))
		return tools.PreDeny, "tool.pre: " + bad, nil
	}
	switch d {
	case tools.PreDeny, tools.PreHalt:
		return d, glua.LVAsString(tb.RawGetString("reason")), nil
	case tools.PreRewrite:
		rw, err := lua.ToGo(tb.RawGetString("input"))
		if err != nil {
			return "", "", nil
		}
		m, _ := rw.(map[string]any)
		return tools.PreRewrite, "", m
	}
	return "", "", nil
}

// toolPostWait bounds one call's tool.post handlers in wall time, their
// waits included; a slice of one gets the veto deadline on the interpreter.
const toolPostWait = 30 * time.Second

// toolPost runs tool.post after a tool call and returns the output the
// model gets (DESIGN §6.2): the handlers, then the tool's override posts, in
// turn as tasks, each seeing the output the one before left. A return of
// { output = "..." } rewrites it, except from project code; a handler that
// fails or runs late leaves it as it was.
func (h *Host) toolPost(ctx context.Context, pad, name string, input map[string]any, ok bool, output string, changed []string, took time.Duration) string {
	return h.postChain(ctx, h.hookHandlers(EventToolPost, name), pad, name, input, ok, output, changed, took)
}

// postChain runs handlers as tool.post for one call of name.
func (h *Host) postChain(ctx context.Context, handlers []*EventHandler, pad, name string, input map[string]any, ok bool, output string, changed []string, took time.Duration) string {
	if !lspQuietTool[name] {
		h.lspDirty()
	}
	payload := map[string]any{"tool": name, "input": input, "ok": ok, "output": output, "changed": changed, "ms": took.Milliseconds()}
	logged := maps.Clone(payload) // the log keeps its map, which the chain below would change
	logged["output"] = truncRunes(output, 4096)
	h.recordEvent(EventToolPost, "host", logged)
	h.consultHooks("tool.post", map[string]any{"tool_name": name, "tool_input": input})
	if len(handlers) == 0 {
		return output
	}
	defer h.refresh() // what the handlers changed, like any event handler
	postCtx, cancel := context.WithTimeoutCause(ctx, toolPostWait, fmt.Errorf("tool.post handlers ran past %s", toolPostWait))
	defer cancel()
	out := output
	for _, hd := range handlers {
		if hd.Once && !h.Events.drop(hd) {
			continue
		}
		payload["output"] = out
		var rewrite *string
		t := h.luaTask(lua.ClassAgent, luaCaller{gen: hd.Gen, origin: hd.Origin, path: hd.Path}, hd.Fn, eventTable(EventToolPost, "host", payload))
		t.Ceiling = lua.VetoDeadline
		t.Done = func(L *glua.LState, rets []glua.LValue) ([]any, error) {
			if len(rets) > 0 {
				if tb, ok := rets[0].(*glua.LTable); ok {
					if s, ok := tb.RawGetString("output").(glua.LString); ok {
						rewrite = new(string(s))
					}
				}
			}
			return nil, nil
		}
		_, err := h.RT.RunTask(postCtx, t)
		switch {
		case postCtx.Err() != nil:
			// interrupted, or out of time: the output stands as it is now
			if ctx.Err() == nil {
				h.logHandlerError(hd, context.Cause(postCtx))
			}
			return h.postOutput(pad, name, output, out)
		case err != nil:
			if !lua.Dropped(err) {
				h.logHandlerError(hd, err)
			}
		case rewrite == nil:
		case hd.Origin.Rank() <= lua.OriginProject.Rank():
			h.Log.Warn("tool.post rewrite ignored", "origin", "tools", "tool", name, "from", string(hd.Origin))
		default:
			out = *rewrite
			if hd.Origin == lua.OriginPlugin {
				sid, ok := ctx.Value(runSessionKey{}).(string)
				if !ok {
					sid = h.AgentSessionID()
				}
				h.markNotLocal(sid, perm.PluginPrincipal(h.PluginOf(hd.Path)))
			}
		}
	}
	return h.postOutput(pad, name, output, out)
}

// postOutput cuts a rewritten output like any other.
func (h *Host) postOutput(pad, name, output, out string) string {
	if out == output {
		return out
	}
	return h.truncOutput(pad, name, out)
}
