// tools_lua.go bridges Lua-registered tools and overrides into the tool
// pipeline (DESIGN §6.4): the zemi.tool namespace, the tool.pre hook
// with the full decision contract, and the model-facing tool set.
package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
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
	owner     func(path string) string // Host.PluginOf, see pluginClash
}

func newToolReg() *toolReg {
	return &toolReg{byName: map[string]*luaTool{}, overrides: map[string][]*overrideRec{}}
}

func (r *toolReg) add(t *luaTool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byName[t.Name]; ok && old.Origin.Rank() > t.Origin.Rank() {
		return fmt.Errorf("tool %q is already registered by a %s registration", t.Name, old.Origin)
	} else if ok {
		if err := pluginClash(r.owner, fmt.Sprintf("tool %q", t.Name), old.Origin, old.Path, t.Origin, t.Path); err != nil {
			return err
		}
	}
	r.byName[t.Name] = t
	return nil
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
	var out string
	for _, t := range h.luaTools.list() {
		if t.Prompt == "" || t.Origin.Rank() <= lua.OriginProject.Rank() || !hasTool(toolset, t.Name) {
			continue
		}
		out += "\n\n## Tool " + t.Name + " (from " + string(t.Origin) + " config)\n" + t.Prompt[:min(len(t.Prompt), assembleSectionMax)]
	}
	return out
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

func (r *toolReg) purge(gen Gen) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, t := range r.byName {
		if t.Gen == gen {
			delete(r.byName, name)
		}
	}
	for tool, list := range r.overrides {
		var keep []*overrideRec
		for _, o := range list {
			if o.Gen != gen {
				keep = append(keep, o)
			}
		}
		if len(keep) == 0 {
			delete(r.overrides, tool)
		} else {
			r.overrides[tool] = keep
		}
	}
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
		fn, ok := spec.RawGetString("execute").(*glua.LFunction)
		if !ok {
			L.RaiseError("zemi.tool.add: execute callback is required")
		}
		name := rawString(spec, "name")
		if name == "" || !isToolName(name) {
			L.RaiseError("zemi.tool.add: name must be an identifier")
		}
		if slices.Contains(h.builtinToolNames(), name) {
			L.RaiseError("zemi.tool.add: %q is a builtin tool name", name)
		}
		desc := rawString(spec, "description")
		if desc == "" {
			L.RaiseError("zemi.tool.add: description is required; the model picks the tool by it")
		}
		// input is the parameters themselves, flat; what reaches the provider
		// is only ever this map (tools.SchemaProperties)
		schema := map[string]any{"type": "object"}
		required := rawStrings(spec, "required")
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
		if ms, ok := spec.RawGetString("timeout_ms").(glua.LNumber); ok && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
		lt := &luaTool{
			Name: name, Desc: desc,
			Schema: schema, Required: required,
			Prompt:   strings.TrimSpace(rawString(spec, "prompt")),
			Parallel: rawBool(spec, "parallel"),
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
		pre, _ := spec.RawGetString("pre").(*glua.LFunction)
		post, _ := spec.RawGetString("post").(*glua.LFunction)
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
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	return tb
}

func isToolName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return len(s) > 0 && s[0] >= 'a' && s[0] <= 'z'
}

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

func (a *luaToolAdapter) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (a *luaToolAdapter) SetProviderOptions(fantasy.ProviderOptions) {}

func (a *luaToolAdapter) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	input, err := tools.ParseInput(a.Info(), call.Input, tools.SchemaOpen(a.t.Schema))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop, err := a.h.preCheck(a.t.Name, call.Input); stop {
		return resp, err
	}
	defer a.h.postNotify(ctx, a.t.Name, call.Input, &reply, time.Now())
	req := tools.Request{Tool: a.t.Name, Subject: "tool." + a.t.Name, Arg: a.t.Name, Reason: "Lua tool " + a.t.Name, Preview: call.Input}
	if v := a.pipe.CheckDenied(req); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse("permission " + string(v.Decision) + " (" + v.DecidedBy + "): " + req.Subject + ": " + v.Reason), nil
	}
	out, err := a.h.runLuaTool(ctx, a.t, call.ID, input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(out), nil
}

// runLuaTool runs execute as a task, bounded by the tool's timeout in wall
// time; a wait in it frees the interpreter, and an interrupt ends it. A
// string result or a table's output field is the output; anything else is
// sent as JSON. nil, err fails the call with err alone (DESIGN §6.0). A
// raised error reaches the model without its traceback, which names the
// user's config paths; /errors keeps it.
func (h *Host) runLuaTool(ctx context.Context, t *luaTool, callID string, input map[string]any) (string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.Timeout, fmt.Errorf("timed out after %s", t.Timeout))
	defer cancel()
	res, err := h.RT.RunTask(ctx, lua.Task{Class: lua.ClassAgent, Ceiling: t.Timeout, Origin: t.Origin, Path: t.Path,
		Start: func(L *glua.LState) (*glua.LFunction, []glua.LValue, error) {
			// a turn keeps the tools it started with: never call into a dropped generation
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
	switch ae, raised := errors.AsType[*glua.ApiError](err); {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = context.Cause(ctx)
	case raised:
		h.RecordError("tool "+t.Name, err.Error())
		err = errors.New(ae.Object.String())
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
	b, _ := json.Marshal(res[0])
	return string(b), nil
}

// toolRun is the run handle execute gets: progress shows one line on the
// call's row, as a command's last output line does.
func (h *Host) toolRun(L *glua.LState, t *luaTool, callID string) *glua.LTable {
	run := L.NewTable()
	run.RawSetString("name", glua.LString(t.Name))
	run.RawSetString("origin", glua.LString(t.Origin))
	run.RawSetString("progress", L.NewFunction(func(L *glua.LState) int {
		line := strings.Join(strings.Fields(sanitize.String(L.CheckString(2))), " ")
		h.setToolDetail(callID, truncRunes(line, 120))
		return 0
	}))
	lua.GuardFuncs(L, run, "run:", h.guardLua)
	return run
}

// toolPreHook adapts the tool.pre events plus zemi.tool.override pre
// handlers into the pipeline PreHook, with the full decision contract and
// shallow rewrite merging (DESIGN §6.2, §6.4). A rewrite by any non-user
// handler marks the result loosened.
func (h *Host) toolPreHook() tools.PreHook {
	pre := func(name string, input map[string]any) (string, string, map[string]any, bool) {
		h.recordEvent("tool.pre", "host", map[string]any{"tool": name, "input": input})
		handlers := h.Events.Handlers("tool.pre")
		for _, o := range h.luaTools.overridesFor(name) {
			if o.Pre != nil {
				handlers = append(handlers, &EventHandler{Event: "tool.pre", Fn: o.Pre, Origin: o.Origin, Path: o.Path})
			}
		}
		if len(handlers) == 0 {
			return "", "", nil, false
		}
		var decision, reason string
		var rewritten map[string]any
		loosened := false
		// twice: the deny-only re-run after a rewrite calls every handler again
		ceiling := time.Duration(2*len(handlers))*lua.VetoDeadline + 2*time.Second
		_, err := h.RT.Do(context.Background(), lua.ClassAgent, ceiling, 0, func(L *glua.LState) ([]any, error) {
			merged := maps.Clone(input)
			if merged == nil {
				merged = map[string]any{}
			}
			for _, hd := range handlers {
				d, r, rw := h.callToolPre(L, hd, name, merged)
				switch {
				case d == "deny" || d == "halt":
					decision, reason, rewritten, loosened = d, r, nil, false
					return nil, nil
				case d == "rewrite" && rw != nil && hd.Origin.Rank() <= lua.OriginProject.Rank():
					// same denial as zemi.tool.override (DESIGN §5)
					h.Log.Warn("tools", "tool.pre rewrite ignored", map[string]any{"tool": name, "origin": string(hd.Origin)})
				case d == "rewrite" && rw != nil:
					maps.Copy(merged, rw)
					decision, rewritten = "rewrite", merged
					loosened = loosened || hd.Origin != lua.OriginUser
				}
			}
			// deny-only re-run over the rewritten input, so a later rewrite
			// cannot slip past an earlier deny hook (DESIGN §6.2)
			if decision == "rewrite" {
				for _, hd := range handlers {
					if d, r, _ := h.callToolPre(L, hd, name, merged); d == "deny" {
						decision, reason, rewritten, loosened = d, r, nil, false
						return nil, nil
					}
				}
			}
			return nil, nil
		})
		if err != nil {
			h.Log.Warn("tools", "tool.pre run failed", map[string]any{"tool": name, "error": err.Error()})
			if !errors.Is(err, lua.ErrAbandoned) {
				// a hot runtime cannot run the deny hooks: fail closed
				return "deny", "tool.pre hooks could not run (lua runtime unavailable, /reload): " + err.Error(), nil, false
			}
			return "", "", nil, false
		}
		return decision, reason, rewritten, loosened
	}
	return func(name string, input map[string]any) (decision, reason string, rewritten map[string]any, loosened bool) {
		h.outsideReload(func() { decision, reason, rewritten, loosened = pre(name, input) })
		return
	}
}

// callToolPre runs one pre handler and parses the decision table.
func (h *Host) callToolPre(L *glua.LState, hd *EventHandler, tool string, input map[string]any) (decision, reason string, rewritten map[string]any) {
	ev := map[string]any{"name": "tool.pre", "from": "host", "tool": tool, "input": input}
	ret, _, err := h.callEvent(L, hd, ev, lua.VetoDeadline)
	if err != nil {
		h.logHandlerError(hd, err) // proceed is the tool.pre default (DESIGN §4)
		return "", "", nil
	}
	tb, d, bad := vetoDecision(ret, "deny", "halt", "rewrite")
	if bad != "" {
		// a misspelled deny must not read as consent (DESIGN §6.2)
		h.logHandlerError(hd, errors.New(bad))
		return "deny", "tool.pre: " + bad, nil
	}
	switch d {
	case "deny", "halt":
		return d, rawString(tb, "reason"), nil
	case "rewrite":
		rw, err := lua.ToGo(tb.RawGetString("input"))
		if err != nil {
			return "", "", nil
		}
		m, _ := rw.(map[string]any)
		return "rewrite", "", m
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
func (h *Host) toolPost(ctx context.Context, name string, input map[string]any, ok bool, output string, changed []string, took time.Duration) string {
	if !lspQuietTool[name] {
		h.lspDirty()
	}
	payload := map[string]any{"tool": name, "input": input, "ok": ok, "output": output, "changed": changed, "ms": took.Milliseconds()}
	logged := maps.Clone(payload) // the log keeps its map, which the chain below would change
	logged["output"] = truncRunes(output, 4096)
	h.recordEvent("tool.post", "host", logged)
	handlers := h.Events.Handlers("tool.post")
	for _, o := range h.luaTools.overridesFor(name) {
		if o.Post != nil {
			handlers = append(handlers, &EventHandler{Event: "tool.post", Fn: o.Post, Origin: o.Origin, Path: o.Path, Gen: o.Gen})
		}
	}
	if len(handlers) == 0 {
		return output
	}
	defer h.refresh() // what the handlers changed, like any event handler
	postCtx, cancel := context.WithTimeoutCause(ctx, toolPostWait, fmt.Errorf("tool.post handlers ran past %s", toolPostWait))
	defer cancel()
	out := output
	for _, hd := range handlers {
		if hd.Once {
			h.Events.drop(hd)
		}
		payload["output"] = out
		var rewrite *string
		t := h.luaTask(lua.ClassAgent, luaCaller{gen: hd.Gen, origin: hd.Origin, path: hd.Path}, hd.Fn, eventTable("tool.post", "host", payload))
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
			return h.postOutput(name, output, out)
		case err != nil:
			if !errors.Is(err, lua.ErrReloaded) && !errors.Is(err, lua.ErrHot) {
				h.logHandlerError(hd, err)
			}
		case rewrite == nil:
		case hd.Origin.Rank() <= lua.OriginProject.Rank():
			h.Log.Warn("tools", "tool.post rewrite ignored", map[string]any{"tool": name, "origin": string(hd.Origin)})
		default:
			out = *rewrite
			if hd.Origin == lua.OriginPlugin {
				sid, ok := ctx.Value(runSessionKey{}).(string)
				if !ok {
					sid = h.AgentSessionID()
				}
				h.markNotLocal(sid, "plugin:"+h.PluginOf(hd.Path))
			}
		}
	}
	return h.postOutput(name, output, out)
}

// postOutput cuts a rewritten output like any other.
func (h *Host) postOutput(name, output, out string) string {
	if out == output {
		return out
	}
	return h.truncOutput(name, out)
}
