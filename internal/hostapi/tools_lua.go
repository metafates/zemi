// tools_lua.go bridges Lua-registered tools and overrides into the tool
// pipeline (DESIGN §6.4): the zemi.tool namespace, the tool.pre/pre-event
// hook with the full decision contract, and the model-facing tool set.
package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// luaTool is one zemi.tool.register entry: interpreter-owned callback,
// gen-scoped so a reload drops it (DESIGN §4).
type luaTool struct {
	Name     string
	Desc     string
	Schema   map[string]any
	Required []string
	Parallel bool
	Timeout  time.Duration
	Fn       *glua.LFunction
	Origin   lua.Origin
	Path     string
	Gen      Gen
}

// overrideRec is one zemi.tool.override pre hook for a builtin tool.
type overrideRec struct {
	Tool   string
	Fn     *glua.LFunction
	Origin lua.Origin
	Path   string
	Gen    Gen
}

type toolReg struct {
	mu        sync.Mutex
	byName    map[string]*luaTool
	overrides map[string][]*overrideRec
}

func newToolReg() *toolReg {
	return &toolReg{byName: map[string]*luaTool{}, overrides: map[string][]*overrideRec{}}
}

func (r *toolReg) add(t *luaTool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[t.Name]; ok {
		return fmt.Errorf("tool %q already registered", t.Name)
	}
	r.byName[t.Name] = t
	return nil
}

func (r *toolReg) list() []*luaTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*luaTool, 0, len(r.byName))
	for _, t := range r.byName {
		out = append(out, t)
	}
	return out
}

func (r *toolReg) addOverride(o *overrideRec) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrides[o.Tool] = append(r.overrides[o.Tool], o)
	return true
}

func (r *toolReg) overridesFor(tool string) []*overrideRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*overrideRec
	for _, o := range r.overrides[tool] {
		out = append(out, o)
	}
	return out
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

// builtinToolNames returns the builtin tool names for collision checks.
var builtinToolNames = map[string]bool{
	"bash": true, "read": true, "write": true, "edit": true,
	"multiedit": true, "glob": true, "grep": true,
}

func (h *Host) toolNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.tool.register") {
			return 0
		}
		if h.RT.CurrentOrigin() == lua.OriginLearned {
			L.RaiseError("zemi.tool.register: learned code may not register tools (DESIGN §10.4, §10.5)")
			return 0
		}
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.tool.register: spec must be a table")
			return 0
		}
		fn, ok := spec.RawGetString("execute").(*glua.LFunction)
		if !ok {
			L.RaiseError("zemi.tool.register: execute callback is required")
			return 0
		}
		name := rawString(spec, "name")
		if name == "" || !isToolName(name) {
			L.RaiseError("zemi.tool.register: name must be an identifier")
			return 0
		}
		if builtinToolNames[name] {
			L.RaiseError("zemi.tool.register: %q is a builtin tool name", name)
			return 0
		}
		schema := map[string]any{}
		var required []string
		if in, ok := spec.RawGetString("input").(*glua.LTable); ok {
			v, err := lua.ToGo(in)
			if err != nil {
				L.RaiseError("zemi.tool.register: input: %v", err)
				return 0
			}
			if m, ok := v.(map[string]any); ok {
				schema = m
			}
			if req, ok := in.RawGetString("required").(*glua.LTable); ok {
				req.ForEach(func(_, v glua.LValue) {
					if s, ok := v.(glua.LString); ok {
						required = append(required, string(s))
					}
				})
			}
		}
		if schema["type"] == nil {
			schema["type"] = "object"
		}
		timeout := 30 * time.Second
		if ms, ok := spec.RawGetString("timeout_ms").(glua.LNumber); ok && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
		lt := &luaTool{
			Name: name, Desc: rawString(spec, "description"),
			Schema: schema, Required: required,
			Parallel: rawBool(spec, "parallel"),
			Timeout:  timeout, Fn: fn,
			Origin: h.RT.CurrentOrigin(), Path: h.RT.CurrentPath(),
			Gen: h.currentGen(),
		}
		if err := h.luaTools.add(lt); err != nil {
			L.RaiseError("zemi.tool.register: %v", err)
			return 0
		}
		return 0
	}))

	tb.RawSetString("override", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.tool.override") {
			return 0
		}
		if o := h.RT.CurrentOrigin(); o == lua.OriginLearned || o == lua.OriginProject {
			L.RaiseError("zemi.tool.override: %s origin may not register rewrites (DESIGN §5)", o)
			return 0
		}
		name := L.CheckString(1)
		spec, ok := L.Get(2).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.tool.override: spec must be a table")
			return 0
		}
		fn, ok := spec.RawGetString("pre").(*glua.LFunction)
		if !ok {
			L.RaiseError("zemi.tool.override: pre callback is required")
			return 0
		}
		h.luaTools.addOverride(&overrideRec{
			Tool: name, Fn: fn,
			Origin: h.RT.CurrentOrigin(), Path: h.RT.CurrentPath(),
			Gen: h.currentGen(),
		})
		return 0
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, name := range []string{"bash", "read", "write", "edit", "multiedit", "glob", "grep"} {
			row := L.NewTable()
			row.RawSetString("name", glua.LString(name))
			row.RawSetString("builtin", glua.LTrue)
			out.Append(row)
		}
		for _, t := range h.luaTools.list() {
			row := L.NewTable()
			row.RawSetString("name", glua.LString(t.Name))
			row.RawSetString("builtin", glua.LFalse)
			row.RawSetString("origin", glua.LString(t.Origin))
			out.Append(row)
		}
		L.Push(out)
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

// luaFantasyTools wraps every registered Lua tool as a fantasy agent tool.
func (h *Host) luaFantasyTools() []fantasy.AgentTool {
	pipe := h.PipelineFor()
	var out []fantasy.AgentTool
	for _, t := range h.luaTools.list() {
		out = append(out, &luaToolAdapter{h: h, t: t, pipe: pipe})
	}
	return out
}

// luaToolAdapter runs a Lua tool through the standard pipeline: schema
// validation, tool.pre, permission gate (tool.<name>), interpreter
// execution, tool.post (DESIGN §6.4).
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

func (a *luaToolAdapter) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var input map[string]any
	if err := json.Unmarshal([]byte(call.Input), &input); err != nil {
		return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
	}
	for _, req := range []tools.Request{{Subject: "tool." + a.t.Name, Arg: a.t.Name, Reason: "Lua tool " + a.t.Name}} {
		v := a.pipe.Check(req)
		if v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission " + string(v.Decision) + " (" + v.DecidedBy + "): " + req.Subject + ": " + v.Reason), nil
		}
	}
	out, err := a.h.runLuaTool(a.t, input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(out), nil
}

// runLuaTool executes the Lua callback on the interpreter (the fantasy
// tool goroutine blocks here; one interpreter serializes Lua tools).
func (h *Host) runLuaTool(t *luaTool, input map[string]any) (string, error) {
	res, err := h.RT.Do(context.Background(), lua.ClassAgent, t.Timeout, 5*time.Second,
		func(L *glua.LState) ([]any, error) {
			h.RT.PushFrame(t.Origin, t.Path)
			defer h.RT.PopFrame()
			base := L.GetTop()
			L.Push(t.Fn)
			L.Push(lua.FromGo(L, input))
			call := L.NewTable()
			call.RawSetString("name", glua.LString(t.Name))
			call.RawSetString("origin", glua.LString(t.Origin))
			L.Push(call)
			if err := L.PCall(2, 1, nil); err != nil {
				L.SetTop(base)
				return nil, err
			}
			v, err := lua.ToGo(L.Get(-1))
			L.SetTop(base)
			if err != nil {
				return nil, err
			}
			return []any{v}, nil
		})
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.Name, err)
	}
	if len(res) == 0 || res[0] == nil {
		return "", nil
	}
	switch v := res[0].(type) {
	case string:
		return v, nil
	case map[string]any:
		if out, ok := v["output"].(string); ok {
			return out, nil
		}
		b, _ := json.Marshal(v)
		return string(b), nil
	default:
		b, _ := json.Marshal(v)
		return string(b), nil
	}
}

// toolPreHook adapts the tool.pre events plus zemi.tool.override pre
// handlers into the pipeline PreHook, with the full decision contract and
// shallow rewrite merging (DESIGN §6.2, §6.4).
func (h *Host) toolPreHook() tools.PreHook {
	return func(name string, input map[string]any) (string, string, map[string]any, bool) {
		handlers := h.Events.Handlers("tool.pre")
		overrides := h.luaTools.overridesFor(name)
		if len(handlers) == 0 && len(overrides) == 0 {
			return "", "", nil, false
		}
		type verdict struct {
			decision, reason string
			rewritten        map[string]any
			loosened         bool
		}
		var out verdict
		deadline := lua.VetoDeadline
		ceiling := time.Duration(len(handlers)+len(overrides))*deadline + 2*time.Second
		_, err := h.RT.Do(context.Background(), lua.ClassAgent, ceiling, 0, func(L *glua.LState) ([]any, error) {
			merged := mapsClone(input)
			for _, hd := range handlers {
				d, r, rw, loose := h.callToolPre(L, hd.Fn, hd.Origin, hd.Path, name, merged, deadline)
				switch d {
				case "deny":
					out = verdict{decision: "deny", reason: r}
					return nil, nil
				case "halt":
					out = verdict{decision: "halt", reason: r}
					return nil, nil
				case "rewrite":
					if rw != nil {
						mapsMerge(merged, rw)
						out = verdict{decision: "rewrite", rewritten: merged, loosened: loose || out.loosened}
					}
				}
			}
			for _, o := range overrides {
				d, r, rw, loose := h.callToolPre(L, o.Fn, o.Origin, o.Path, name, merged, deadline)
				switch d {
				case "deny":
					out = verdict{decision: "deny", reason: r}
					return []any{"deny"}, nil
				case "halt":
					out = verdict{decision: "halt", reason: r}
					return []any{"halt"}, nil
				case "rewrite":
					if rw != nil {
						mapsMerge(merged, rw)
						out = verdict{decision: "rewrite", rewritten: merged, loosened: loose || out.loosened}
					}
				}
			}
			if out.decision == "" {
				out = verdict{rewritten: nil}
			}
			return nil, nil
		})
		if err != nil {
			h.Log.Warn("tools", "tool.pre run failed", map[string]any{"tool": name, "error": err.Error()})
			return "", "", nil, false
		}
		return out.decision, out.reason, out.rewritten, out.loosened
	}
}

// callToolPre runs one pre handler and parses the decision table.
func (h *Host) callToolPre(L *glua.LState, fn *glua.LFunction, origin lua.Origin, path, tool string, input map[string]any, deadline time.Duration) (decision, reason string, rewritten map[string]any, loosened bool) {
	h.RT.PushFrame(origin, path)
	defer h.RT.PopFrame()
	ev := map[string]any{"name": "tool.pre", "from": "host", "tool": tool, "input": input}
	_, restore := setHandlerContext(L, deadline)
	defer restore()
	base := L.GetTop()
	L.Push(fn)
	L.Push(lua.FromGo(L, ev))
	if err := L.PCall(1, 1, nil); err != nil {
		L.SetTop(base)
		h.Log.Error("tools", "tool.pre handler failed", map[string]any{"tool": tool, "error": err.Error()})
		return "", "", nil, false
	}
	defer L.SetTop(base)
	dec := L.Get(-1)
	if dec == glua.LNil {
		return "", "", nil, false
	}
	tb, ok := dec.(*glua.LTable)
	if !ok {
		return "", "", nil, false
	}
	d, _ := tb.RawGetString("decision").(glua.LString)
	r, _ := tb.RawGetString("reason").(glua.LString)
	switch string(d) {
	case "deny", "halt":
		return string(d), string(r), nil, false
	case "rewrite":
		rw, err := lua.ToGo(tb.RawGetString("input"))
		if err != nil {
			return "", "", nil, false
		}
		m, _ := rw.(map[string]any)
		return "rewrite", string(r), m, string(origin) != string(lua.OriginUser)
	}
	return "", "", nil, false
}

func mapsClone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mapsMerge shallow-merges src over dst (rewrite semantics, §6.2).
func mapsMerge(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}
