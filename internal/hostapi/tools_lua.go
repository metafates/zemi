// tools_lua.go bridges Lua-registered tools and overrides into the tool
// pipeline (DESIGN §6.4): the zemi.tool namespace, the tool.pre hook
// with the full decision contract, and the model-facing tool set.
package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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
	return slices.SortedFunc(maps.Values(r.byName), func(a, b *luaTool) int { return cmp.Compare(a.Name, b.Name) })
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

// builtinToolNames are the names Lua tools may not take.
var builtinToolNames = []string{"bash", "read", "write", "edit", "multiedit", "glob", "grep"}

func (h *Host) toolNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.tool.register")
		if h.RT.CurrentOrigin() == lua.OriginLearned {
			L.RaiseError("zemi.tool.register: learned code may not register tools (DESIGN §10.4, §10.5)")
		}
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.tool.register: spec must be a table")
		}
		fn, ok := spec.RawGetString("execute").(*glua.LFunction)
		if !ok {
			L.RaiseError("zemi.tool.register: execute callback is required")
		}
		name := rawString(spec, "name")
		if name == "" || !isToolName(name) {
			L.RaiseError("zemi.tool.register: name must be an identifier")
		}
		if slices.Contains(builtinToolNames, name) {
			L.RaiseError("zemi.tool.register: %q is a builtin tool name", name)
		}
		schema := map[string]any{}
		var required []string
		if in, ok := spec.RawGetString("input").(*glua.LTable); ok {
			v, err := lua.ToGo(in)
			if err != nil {
				L.RaiseError("zemi.tool.register: input: %v", err)
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
		}
		return 0
	}))

	tb.RawSetString("override", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.tool.override")
		if o := h.RT.CurrentOrigin(); o == lua.OriginLearned || o == lua.OriginProject {
			L.RaiseError("zemi.tool.override: %s origin may not register rewrites (DESIGN §5)", o)
		}
		name := L.CheckString(1)
		spec, ok := L.Get(2).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.tool.override: spec must be a table")
		}
		fn, ok := spec.RawGetString("pre").(*glua.LFunction)
		if !ok {
			L.RaiseError("zemi.tool.override: pre callback is required")
		}
		h.luaTools.addOverride(&overrideRec{
			Tool: name, Fn: fn,
			Origin: h.RT.CurrentOrigin(), Path: h.RT.CurrentPath(),
			Gen: h.currentGen(),
		})
		return 0
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, name := range builtinToolNames {
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

func (a *luaToolAdapter) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var input map[string]any
	if err := json.Unmarshal([]byte(call.Input), &input); err != nil {
		return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
	}
	if resp, stop, err := a.h.preCheck(a.t.Name, call.Input); stop {
		return resp, err
	}
	req := tools.Request{Subject: "tool." + a.t.Name, Arg: a.t.Name, Reason: "Lua tool " + a.t.Name}
	if v := a.pipe.Check(req); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse("permission " + string(v.Decision) + " (" + v.DecidedBy + "): " + req.Subject + ": " + v.Reason), nil
	}
	out, err := a.h.runLuaTool(ctx, a.t, input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(out), nil
}

// runLuaTool executes the Lua callback on the interpreter (the fantasy
// tool goroutine blocks here; one interpreter serializes Lua tools), so an
// interrupt preempts it. A string result or a table's output field is the
// output; anything else is sent as JSON.
func (h *Host) runLuaTool(ctx context.Context, t *luaTool, input map[string]any) (string, error) {
	res, err := h.RT.Do(ctx, lua.ClassAgent, t.Timeout, 5*time.Second,
		func(L *glua.LState) ([]any, error) {
			defer h.RT.PushFrame(t.Origin, t.Path)()
			base := L.GetTop()
			defer L.SetTop(base)
			L.Push(t.Fn)
			L.Push(lua.FromGo(L, input))
			L.Push(lua.FromGo(L, map[string]any{"name": t.Name, "origin": string(t.Origin)}))
			if err := L.PCall(2, 1, nil); err != nil {
				return nil, err
			}
			v, err := lua.ToGo(L.Get(-1))
			return []any{v}, err
		})
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

// toolPreHook adapts the tool.pre events plus zemi.tool.override pre
// handlers into the pipeline PreHook, with the full decision contract and
// shallow rewrite merging (DESIGN §6.2, §6.4). A rewrite by any non-user
// handler marks the result loosened.
func (h *Host) toolPreHook() tools.PreHook {
	return func(name string, input map[string]any) (string, string, map[string]any, bool) {
		h.recordEvent("tool.pre", "host", map[string]any{"tool": name, "input": input})
		handlers := h.Events.Handlers("tool.pre")
		for _, o := range h.luaTools.overridesFor(name) {
			handlers = append(handlers, &EventHandler{Event: "tool.pre", Fn: o.Fn, Origin: o.Origin, Path: o.Path})
		}
		if len(handlers) == 0 {
			return "", "", nil, false
		}
		var decision, reason string
		var rewritten map[string]any
		loosened := false
		ceiling := time.Duration(len(handlers))*lua.VetoDeadline + 2*time.Second
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
				case d == "rewrite" && rw != nil:
					maps.Copy(merged, rw)
					decision, rewritten = "rewrite", merged
					loosened = loosened || hd.Origin != lua.OriginUser
				}
			}
			return nil, nil
		})
		if err != nil {
			h.Log.Warn("tools", "tool.pre run failed", map[string]any{"tool": name, "error": err.Error()})
			return "", "", nil, false
		}
		return decision, reason, rewritten, loosened
	}
}

// callToolPre runs one pre handler and parses the decision table.
func (h *Host) callToolPre(L *glua.LState, hd *EventHandler, tool string, input map[string]any) (decision, reason string, rewritten map[string]any) {
	ev := map[string]any{"name": "tool.pre", "from": "host", "tool": tool, "input": input}
	ret, _, err := h.callEvent(L, hd, ev, lua.VetoDeadline)
	if err != nil {
		h.Log.Error("tools", "tool.pre handler failed", map[string]any{"tool": tool, "error": err.Error()})
		return "", "", nil
	}
	tb, ok := ret.(*glua.LTable)
	if !ok {
		return "", "", nil
	}
	switch d := rawString(tb, "decision"); d {
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
