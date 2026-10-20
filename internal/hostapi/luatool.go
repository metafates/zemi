// luatool.go is the model's Lua tool: it runs a chunk on the live
// interpreter as user origin, so the agent can inspect and change zemi's own
// state (this session, agent runs, memories, settings, skills, errors)
// through the API every Lua caller uses. Without the user's approval a run
// may only call read-only members (luaReadOnly, enforced by guardLua);
// a script that names anything else asks once up front, and so does every
// run in an untrusted project or by a read-only agent. The tool.pre events always run
// so user hooks can deny or halt a script.
//
// A script also drives the run's tools: zemi.tool.call reaches one through the
// gate a direct call takes and its wait suspends the script, so a script that
// only drives tools needs no approval of its own.

package hostapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/tools"
)

// luaToolCeiling bounds each slice of a Lua tool run on the interpreter; a
// var so a test can hit it without waiting.
var luaToolCeiling = 10 * time.Second

// luaToolChunk names the tool's chunks; every run appends "#<n>".
const luaToolChunk = "lua-tool"

// luaToolCallsMax is how many of one script's calls run at once, the rest
// waiting for a slot; a script that maps a listing to a command per file would
// otherwise start them all together.
const luaToolCallsMax = 8

// luaToolCallWait bounds one call's wait; the tool's own timeout, a canceled
// turn or a reload usually ends it first.
const luaToolCallWait = time.Hour

// luaReadOnly is what the model's Lua tool may call without the user's
// approval: members that only read zemi's state, and pure helpers. Every
// other Go function exposed to Lua, handle methods included ("job:kill"),
// raises in an unapproved run, so a new member asks until it is added here.
var luaReadOnly = set.Of(
	"agent.current", "agent.list", "agent.prompt", "agent.usage",
	"command.list", "composer.get",
	"config.get", "config.schema",
	"errors.list", "event.log", "event.stats",
	"health.list",
	"fs.data_dir", "fs.exists", "fs.glob", "fs.grep", "fs.list", "fs.read", "fs.stat",
	"json.decode", "json.encode",
	"keymap.get", "keymap.list", "learn.list",
	"log", "log.list", "log.debug", "log.info", "log.warn", "log.error",
	"lsp.list", "mcp.list", "memory.list",
	"model.list", "notify.list",
	"project.root", "project.trusted", "provider.list",
	"session.id", "session.list", "session.messages", "session.title", "session.todos",
	"skills.list", "state.get", "tool.describe", "tool.list",
	"ui.focus.get", "ui.hl.get", "ui.layout.get", "ui.layout.rects", "ui.markdown", "ui.highlight", "ui.diff", "ui.wrap", "ui.width", "ui.screenshot", "ui.theme.get", "ui.theme.list", "ui.widget.list",
	"workflow.list", "job:wait", "http:wait", "tool:wait",
)

// luaDelegated is what a script may call without the user's approval of the
// script itself: zemi.tool.call decides for itself, one gate per call, and
// reaches nothing else of zemi.
var luaDelegated = set.Of("tool.call")

// luaEvalTool is the model's lua tool. build is profileTools' builder, which a
// script's callable set is built with under its own pipeline copy.
type luaEvalTool struct {
	tools.NoProviderOptions
	h     *Host
	pipe  *tools.Pipeline
	build func(*tools.Pipeline) ([]fantasy.AgentTool, []string)
}

func (h *Host) luaEvalTool(pipe *tools.Pipeline) fantasy.AgentTool {
	return &luaEvalTool{h: h, pipe: pipe}
}

// luaArgs is the lua tool's input and its schema source.
type luaArgs struct {
	About  string `json:"about" desc:"what the script does in 3-6 words, shown to the user"`
	Script string `json:"script,omitempty" desc:"Lua source"`
	Path   string `json:"path,omitempty" desc:"a .lua file to run instead of script"`
}

func (t *luaEvalTool) Info() fantasy.ToolInfo {
	approval := " A script that only reads (zemi.config.get, zemi.session.messages, zemi.errors.list, zemi.ui.screenshot, the zemi.fs readers, ...) or drives tools runs at once; one that changes anything needs the user's approval once, so write every call out by name."
	props, required := tools.ArgsOf(luaArgs{})
	return fantasy.ToolInfo{
		Name: tools.ToolLua,
		Description: "Run Lua 5.1 inside zemi to read or change its live state: this session, agent runs, settings, keymaps, widgets, skills, errors." +
			" It runs on the interpreter the UI draws with, 10s at most between waits: no busy loops." +
			" The standard io and os libraries are not available: files go through zemi.fs, commands through zemi.tool.call(\"shell\")." +
			" A script drives this run's tools too: `zemi.tool.call(name, args)` returns a handle whose `:wait()` gives its output; start independent calls together and wait on each; only the script's prints and returns reach you. `zemi.tool.list()` names this run's tools, deferred MCP ones too; `zemi.tool.describe(name)` gives its parameters." +
			" File calls, paths inside the project: `zemi.fs.read(path)` -> `content, err`; `write(path, content)`; `list(path)` -> entries `{name, type, size}`; `glob(pattern)` -> paths (one string pattern, `**` spans directories); `grep(pattern)` -> hits `{path, line, text}`; plus `stat`, `exists`, `mkdir`, `remove`, `data_dir(name)`. `glob` and `grep` raise on a bad pattern; the rest return `nil, err`." +
			" Work from these signatures on the first attempt: never a script that just probes the API." +
			approval +
			" For calls not named here read " + filepath.Join(t.h.builtinSkillsDir(), "zemi", "lua.md") + "; a member a namespace lacks raises with the list it has.",
		Parameters: props,
		Required:   required,
	}
}

func (t *luaEvalTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	h := t.h
	var in luaArgs
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := h.preCheck("lua", call.Input); stop {
		return resp, nil
	}
	defer h.postNotify(ctx, t.pipe.Pad, "lua", call.Input, &reply, time.Now())
	src := in.Script
	if in.Path != "" {
		// the file is read like the read tool reads it: a Lua error echoes
		// tokens of what it ran, so a secret or a file outside the project asks
		if filepath.Ext(in.Path) != ".lua" {
			return fantasy.NewTextErrorResponse(in.Path + ": not a .lua file"), nil
		}
		path, err := perm.Resolve(h.projectAbs(in.Path))
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		pipe := *t.pipe
		pipe.Path = path // judged where it is opened, not resolved again
		if v := pipe.CheckDenied(tools.Request{Tool: "lua", Subject: perm.SubjectRead, Arg: path.String()}); !v.Allowed {
			return fantasy.NewTextErrorResponse(tools.DeniedText(v, perm.SubjectRead+" "+in.Path)), nil
		}
		f, fi, err := tools.OpenRegular(path)
		var b []byte
		if err == nil {
			if fi.Size() > maxAgentFileBytes {
				err = fmt.Errorf("%s: exceeds %d bytes", in.Path, maxAgentFileBytes)
			} else {
				b, err = io.ReadAll(f)
			}
			f.Close()
		}
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			return fantasy.NewTextErrorResponse(in.Path + ": " + pe.Err.Error()), nil
		} else if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		src = string(b)
	}
	if strings.TrimSpace(src) == "" {
		return fantasy.NewTextErrorResponse("pass script or path"), nil
	}
	// every run gets its own chunk name: guardLua tags an approved one, and a
	// script's calls are keyed by it (h.scripts)
	rel := fmt.Sprintf("%s#%d", luaToolChunk, h.luaRuns.Add(1))
	// the script runs as user origin, so it asks like one: the whole script
	// is the ask's preview, and once it is approved its own calls ride that
	arg := "script"
	if in.Path != "" {
		arg = in.Path
	}
	if v := t.pipe.CheckDenied(tools.Request{Tool: "lua", Subject: perm.SubjectLua, Arg: arg, Description: in.About, Preview: src}); !v.Allowed {
		return fantasy.NewTextErrorResponse(tools.DeniedText(v, "Lua "+arg)), nil
	}
	h.luaApproved.Store(rel, true)
	defer h.luaApproved.Delete(rel)
	// what zemi.tool.call reaches while this runs: dropped with the run, so a
	// callback the script registered cannot drive tools, and so a call still
	// running when the script ends stops with it
	if t.build != nil {
		pipe := *t.pipe
		pipe.Script = rel
		// a call's progress (a command's last line) shows on this tool's row
		pipe.Progress = func(_, line string) { h.setToolDetail(call.ID, sanitize.String(line)) }
		set, _ := t.build(&pipe)
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		script := &luaScript{ctx: sctx, id: call.ID, tools: map[string]fantasy.AgentTool{}, calls: newLuaCalls(luaToolCallsMax)}
		for _, tool := range set {
			// a script cannot start another script
			if name := tool.Info().Name; name != tools.ToolLua {
				script.tools[name] = tool
			}
		}
		h.scripts.Store(rel, script)
		defer h.scripts.Delete(rel)
	}
	// the script sees what is on disk, and the result says whether it loaded
	note := h.ReloadIfChanged()
	out, err := h.runModelLua(ctx, src, rel)
	switch {
	case err != nil:
		msg := luaNilHint(err.Error())
		if in.Path != "" {
			// the chunk keeps its tool name, which the permission ask reads;
			// the error names the file the model can edit
			msg = strings.ReplaceAll(msg, rel+":", in.Path+":")
		}
		out = strings.TrimSpace(out + "\nerror: " + msg)
	case out == "":
		out = "(no output)"
	}
	reply = fantasy.NewTextResponse(h.truncOutput(t.pipe.Pad, "lua", strings.TrimSpace(note+"\n"+out)))
	reply.IsError = err != nil
	return reply, nil
}

// luaNilHint names the fix for the error a script hits when it skips a
// zemi.fs check: the raw message says nothing was nil or why.
func luaNilHint(msg string) string {
	if !strings.Contains(msg, "non-table object(nil)") && !strings.Contains(msg, "non-function object") {
		return msg
	}
	return msg + "\n(a zemi.fs call answers nil, err when the path is a directory or gone; check the value before indexing or calling it)"
}

// runModelLua runs src as a user-origin chunk named rel (the model can do
// what a user init.lua can once the user approves the run, see
// guardLua) and returns what it printed, then its return values as JSON.
func (h *Host) runModelLua(ctx context.Context, src, rel string) (string, error) {
	var b strings.Builder
	vals, err := h.RT.Eval(ctx, src, rel, lua.OriginUser, luaToolCeiling, func(line string) {
		b.WriteString(line + "\n")
	})
	if len(vals) > 0 {
		var ret any = vals
		if len(vals) == 1 {
			ret = vals[0]
		}
		data, jerr := json.Marshal(ret, json.Deterministic(true))
		if jerr != nil {
			data = []byte(fmt.Sprint(ret))
		}
		b.WriteString("return: " + string(data) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), err
}

// guardLua is the guard every Go function exposed to Lua runs through: code
// from an unapproved Lua tool run may call only luaReadOnly and
// luaDelegated, and a widget render, which runs every frame, only the
// luaReadOnly members other than fs.* and a handle's wait.
func (h *Host) guardLua(L *glua.LState, member string) {
	if h.rendering.Load() && (!luaReadOnly.Has(member) || strings.HasPrefix(member, "fs.") || strings.HasSuffix(member, ":wait")) {
		L.RaiseError("zemi.%s is not allowed during widget render (render must be pure)", member)
	}
	if luaReadOnly.Has(member) || luaDelegated.Has(member) {
		return
	}
	if tool, approved := h.modelLuaRun(L); tool && !approved {
		L.RaiseError("the Lua tool asks before zemi.%s; write the call out by name in the script and the whole script runs after one approval", member)
	}
}

// modelLuaRun reports whether code from the model's Lua tool (its chunk, a
// function it defined, or a callback it registered) is calling, and whether
// the user approved that run. The innermost tool function decides: an
// approved run calling code an earlier run defined does not approve it.
func (h *Host) modelLuaRun(L *glua.LState) (tool, approved bool) {
	name := h.modelLuaScript(L)
	if name == "" {
		return false, false
	}
	_, approved = h.luaApproved.Load(name)
	return true, approved
}

// modelLuaScript names the model's Lua tool run that decides what a call on L
// may do: the innermost one, so a run calling code an earlier run defined is
// judged as that earlier run. "" when the caller is not a script's.
func (h *Host) modelLuaScript(L *glua.LState) string {
	path := h.RT.CurrentPath(L)
	if strings.HasPrefix(path, luaToolChunk) {
		if _, approved := h.luaApproved.Load(path); !approved {
			return path
		}
	}
	for lvl := 0; ; lvl++ {
		dbg, ok := L.GetStack(lvl)
		if !ok {
			break
		}
		fn, _ := L.GetInfo("f", dbg, glua.LNil)
		f, ok := fn.(*glua.LFunction)
		if !ok || f.IsG {
			continue
		}
		// load and loadstring tag chunks with the running origin, so only
		// user code can name one like the tool's
		if name, ok := strings.CutPrefix(f.Proto.SourceName, lua.OriginUser.ChunkName("")); ok && strings.HasPrefix(name, luaToolChunk) {
			return name
		}
	}
	if strings.HasPrefix(path, luaToolChunk) {
		return path
	}
	return ""
}

// luaScript is what one running lua-tool script drives: the tools its
// zemi.tool.call reaches, the cap their calls run under, and where their
// progress shows.
type luaScript struct {
	id    string // the model's tool call id, the row a call reports on
	ctx   context.Context
	tools map[string]fantasy.AgentTool
	calls *luaCalls
	n     int // calls started, for their ids
}

// luaCalls caps one script's calls: at most limit run at once, the rest wait
// for a slot. A waiting call holds nothing another call can wait on, so the cap
// cannot deadlock. NOTE: unlike the model's own calls, a script's calls do not
// enter tools.CallStart, so two it starts at once for one file land in either
// order; a script that edits a file twice waits on the first (the file tools'
// own lock keeps them from corrupting it, and the second reports the file
// changed since it was read).
type luaCalls struct{ slots chan struct{} }

func newLuaCalls(limit int) *luaCalls { return &luaCalls{slots: make(chan struct{}, limit)} }

// enter waits for a free slot and reports the release; false once ctx ends, so
// a script that ended leaves nothing of its own waiting.
func (c *luaCalls) enter(ctx context.Context) (func(), bool) {
	select {
	case c.slots <- struct{}{}:
		return func() { <-c.slots }, true
	case <-ctx.Done():
		return nil, false
	}
}

// luaToolCall is zemi.tool.call(name, args): it runs one of the run's tools
// through the pipeline a direct call takes, off the interpreter, and returns
// the handle whose wait() suspends the script until the call ends. Only the
// script the lua tool is running reaches it, and only while that run lives.
func (h *Host) luaToolCall(L *glua.LState) int {
	name := L.CheckString(1)
	v, ok := h.scripts.Load(h.modelLuaScript(L))
	if !ok {
		return luaResult(L, errors.New("zemi.tool.call is for the lua tool's own script, while it runs"))
	}
	s := v.(*luaScript)
	tool, ok := s.tools[name]
	if !ok {
		return luaResult(L, fmt.Errorf("no tool %q in this run; zemi.tool.list() names the tools, the prompt the rest", name))
	}
	args := L.Get(2)
	body := "{}"
	if tb, ok := args.(*glua.LTable); ok {
		in, err := lua.ToGo(tb)
		if err != nil {
			return luaResult(L, fmt.Errorf("zemi.tool.call: args: %w", err))
		}
		data, err := json.Marshal(in, json.Deterministic(true))
		if err != nil {
			return luaResult(L, fmt.Errorf("zemi.tool.call: args: %w", err))
		}
		if string(data) != "null" {
			body = string(data)
		}
	} else if args != glua.LNil {
		return luaResult(L, errors.New("zemi.tool.call: args must be a table"))
	}
	s.n++
	id := fmt.Sprintf("%s/%d", s.id, s.n)
	hnd := &luaToolHandle{done: make(chan struct{})}
	// the call's context ends with the script's run, so a call still queued or
	// running when the script ends stops with it
	callCtx, cancel := context.WithCancel(s.ctx)
	hnd.cancel = cancel
	stopID := h.stops.add(cancel)
	h.GoSafe("lua tool call", func() {
		defer close(hnd.done)
		defer h.stops.remove(stopID)
		defer cancel()
		leave, ok := s.calls.enter(callCtx)
		if !ok {
			hnd.err = "the script ended before this call ran"
			return
		}
		defer leave()
		h.setToolDetail(s.id, clipLine(name+": "+toolSummary(name, body), 120))
		resp, err := tool.Run(callCtx, fantasy.ToolCall{ID: id, Name: name, Input: body})
		switch {
		case err != nil:
			hnd.err = err.Error()
		case resp.IsError:
			// a failed call is nil, err like every other zemi call; the text
			// zemi's tools fail with carries their output
			hnd.err = resp.Content
		default:
			hnd.out = resp.Content
		}
	})
	handle := L.NewTable()
	handle.RawSetString("wait", lua.Suspending(L, L.NewFunction(func(L *glua.LState) int {
		lua.CheckWait(L, "zemi.tool.call")
		return waitHandle(L, "tool:wait", hnd.done, luaToolCallWait, hnd.cancel, func(L *glua.LState, err error) []glua.LValue {
			if err != nil {
				hnd.cancel()
				return []glua.LValue{glua.LNil, glua.LString(err.Error())}
			}
			if hnd.err != "" {
				return []glua.LValue{glua.LNil, glua.LString(hnd.err)}
			}
			return []glua.LValue{glua.LString(hnd.out)}
		})
	})))
	lua.GuardFuncs(L, handle, "tool:", h.guardLua)
	L.Push(handle)
	return 1
}

// luaToolHandle is one nested call's handle: wait() suspends the script until
// the call ends, and cancel stops the call when the wait ends first.
type luaToolHandle struct {
	done   chan struct{}
	out    string
	err    string
	cancel context.CancelFunc
}

// luaToolDescribe is zemi.tool.describe(name): the declaration of one of the
// run's tools, for one the prompt does not declare, nil when the run has none.
func (h *Host) luaToolDescribe(L *glua.LState) int {
	name := L.CheckString(1)
	v, ok := h.scripts.Load(h.modelLuaScript(L))
	if !ok {
		return luaResult(L, errors.New("zemi.tool.describe is for the lua tool's own script, while it runs"))
	}
	tool, ok := v.(*luaScript).tools[name]
	if !ok {
		L.Push(glua.LNil)
		return 1
	}
	info := tool.Info()
	L.Push(lua.FromGo(L, map[string]any{
		"name": info.Name, "description": info.Description,
		"input": info.Parameters, "required": info.Required,
	}))
	return 1
}
