// luatool.go is the model's Lua tool: it runs a chunk on the live
// interpreter as user origin, so the agent can inspect and change zemi's own
// state (this session, agent runs, memories, settings, skills, errors)
// through the API every Lua caller uses. Without the user's approval a run
// may only call read-only members (luaReadOnly, enforced by guardModelLua);
// a script that names anything else asks once up front, and so does every
// run in an untrusted project or plan mode. The tool.pre events always run
// so user hooks can deny or halt a script.
package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// luaToolCeiling bounds one run: it holds the interpreter the UI renders
// through. Cancelling stops the script at its next opcode.
const luaToolCeiling = 10 * time.Second

// luaToolChunk names the tool's chunks; an approved run appends "#<n>".
const luaToolChunk = "lua-tool"

// luaReadOnly is what the model's Lua tool may call without the user's
// approval: members that only read zemi's state, and pure helpers. Every
// other Go function exposed to Lua, handle methods included ("win:send"),
// raises in an unapproved run, so a new member asks until it is added here.
var luaReadOnly = map[string]bool{
	"agent.current": true, "agent.list": true, "agent.usage": true,
	"command.list": true, "composer.get": true,
	"config.get": true, "config.schema": true,
	"errors.list": true, "event.log": true, "event.stats": true,
	"json.decode": true, "json.encode": true,
	"keymap.get": true, "keymap.list": true, "learn.proposals": true,
	"log": true, "log.debug": true, "log.info": true, "log.warn": true, "log.error": true,
	"lsp.list": true, "mcp.list": true, "memory.list": true, "memory.search": true,
	"model.list": true, "notify.list": true,
	"permission.mode.list": true, "permission.rules": true, "provider.list": true,
	"session.id": true, "session.list": true, "session.messages": true, "session.title": true, "session.todos": true,
	"skills.list": true, "state.get": true, "tool.list": true,
	"ui.focus.get": true, "ui.hl_get": true, "ui.layout.get": true, "ui.layout.rects": true, "ui.markdown": true, "ui.screenshot": true, "ui.theme.get": true, "ui.widget.list": true,
	"win.get": true, "win.list": true, "win:text": true, "win:wait_text": true, "win:info": true,
	"workflow.list": true, "job:wait": true, "http:wait": true,
}

// luaReadMethods are the method calls the text scan lets through: string
// methods and the read-only handle methods.
var luaReadMethods = map[string]bool{
	"byte": true, "find": true, "format": true, "gmatch": true, "gsub": true, "len": true,
	"lower": true, "match": true, "rep": true, "reverse": true, "sub": true, "upper": true,
	"text": true, "wait_text": true, "info": true, "wait": true,
}

// luaDynamic names what can reach zemi without spelling a member out.
var luaDynamic = map[string]bool{
	"_G": true, "getfenv": true, "setfenv": true, "rawget": true, "load": true,
	"loadstring": true, "dofile": true, "loadfile": true, "require": true, "debug": true,
}

// luaUnlisted lists what src may call beyond luaReadOnly: zemi paths off the
// list (a bare namespace too), other method calls, and dynamic access. A
// script that does not tokenize lists nothing, as it will not compile.
func luaUnlisted(src string) []string {
	sc := parse.NewScanner(strings.NewReader(src), luaToolChunk)
	lx := &parse.Lexer{}
	var toks []ast.Token
	for {
		tok, err := sc.Scan(lx)
		if err != nil {
			return nil
		}
		if tok.Type == parse.EOF {
			break
		}
		lx.PrevTokenType = tok.Type
		toks = append(toks, tok)
	}
	var out []string
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for i := 0; i < len(toks); i++ {
		t, prev := toks[i], 0
		if i > 0 {
			prev = toks[i-1].Type
		}
		switch {
		case t.Type == ':' && i+1 < len(toks) && toks[i+1].Type == parse.TIdent:
			if !luaReadMethods[toks[i+1].Str] {
				add(":" + toks[i+1].Str)
			}
		case t.Type != parse.TIdent || prev == '.' || prev == ':':
		case luaDynamic[t.Str]:
			add(t.Str)
		case t.Str == "zemi":
			var path []string
			for i+2 < len(toks) && toks[i+1].Type == '.' && toks[i+2].Type == parse.TIdent {
				path = append(path, toks[i+2].Str)
				i += 2
			}
			if name := strings.Join(path, "."); !luaReadOnly[name] {
				add(strings.TrimSuffix("zemi."+name, "."))
			}
		}
	}
	return out
}

type luaEvalTool struct{ h *Host }

func (h *Host) luaEvalTool() fantasy.AgentTool { return &luaEvalTool{h: h} }

func (t *luaEvalTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: "Lua",
		Description: "Run Lua 5.1 inside zemi to read or change zemi's live state: this session, agent runs, settings, keymaps, widgets, skills, errors. Load the zemi-lua skill for the API first." +
			" What the chunk prints and returns (as JSON) comes back. It runs on the interpreter the UI draws with, for at most 10s: no waiting loops; start slow work with zemi.job or zemi.timer." +
			" A script that only reads (zemi.config.get, zemi.session.messages, zemi.errors.list, zemi.ui.screenshot, ...) runs at once; one that changes anything asks the user once, so write every call out by name." +
			" Keep scripts you reuse in the scratchpad and pass path instead of script.",
		Parameters: map[string]any{
			"description": map[string]any{"type": "string", "description": "what the script does in 3-6 words, shown to the user"},
			"script":      map[string]any{"type": "string", "description": "Lua source"},
			"path":        map[string]any{"type": "string", "description": "a .lua file to run instead of script"},
		},
		Required: []string{"description"},
	}
}

func (t *luaEvalTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *luaEvalTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *luaEvalTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	h := t.h
	var in struct {
		Description string `json:"description"`
		Script      string `json:"script"`
		Path        string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
		return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
	}
	if resp, stop, err := h.preCheck("Lua", call.Input); stop {
		return resp, err
	}
	src := in.Script
	if in.Path != "" {
		path := in.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(h.Dirs.ProjectDir, path)
		}
		s, err := readRegularFile(path)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		src = s
	}
	if strings.TrimSpace(src) == "" {
		return fantasy.NewTextErrorResponse("pass script or path"), nil
	}
	rel := luaToolChunk
	// the script can do anything user config can, so it asks where the
	// model's text is less trusted or the user wants nothing changed, and
	// before any call that is not a read: no mode may settle that, since
	// what it changes or starts runs unconfined
	unlisted := luaUnlisted(src)
	if len(unlisted) > 0 || !h.ProjectTrusted() || h.permModeDef().Base == "plan" {
		arg := "script"
		if in.Path != "" {
			arg = in.Path
		}
		preview := "+ " + strings.ReplaceAll(strings.TrimRight(src, "\n"), "\n", "\n+ ")
		req := tools.Request{Tool: "Lua", Subject: "tool.lua", Arg: arg, Reason: in.Description, Preview: preview}
		if len(unlisted) > 0 {
			req.Unsandboxed = true
			req.Reason = strings.TrimSuffix("uses "+strings.Join(unlisted, ", ")+", which can change zemi or start programs outside the sandbox: "+in.Description, ": ")
		}
		if v := h.PipelineFor().Check(req); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): Lua " + arg + ": " + v.Reason), nil
		}
		if len(unlisted) > 0 {
			rel = fmt.Sprintf("%s#%d", luaToolChunk, h.luaRuns.Add(1))
			h.luaApproved.Store(rel, true)
			defer h.luaApproved.Delete(rel)
		}
	}
	out, err := h.runModelLua(ctx, src, rel)
	if err != nil {
		return fantasy.NewTextErrorResponse(truncOutput(strings.TrimSpace(out + "\nerror: " + err.Error()))), nil
	}
	if out == "" {
		out = "(no output)"
	}
	return fantasy.NewTextResponse(truncOutput(out)), nil
}

// runModelLua runs src as a user-origin chunk named rel (the model can do
// what a user init.lua can once the user approves the run, see
// guardModelLua) and returns what it printed, then its return values as JSON.
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
		data, jerr := json.Marshal(ret)
		if jerr != nil {
			data = []byte(fmt.Sprint(ret))
		}
		b.WriteString("return: " + string(data) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), err
}

// guardModelLua is the sandbox guard every Go function exposed to Lua runs
// through: code from an unapproved Lua tool run may call only luaReadOnly.
func (h *Host) guardModelLua(L *glua.LState, member string) {
	if luaReadOnly[member] {
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
	isApproved := func(name string) bool {
		_, ok := h.luaApproved.Load(name)
		return ok
	}
	path := h.RT.CurrentPath(L)
	if strings.HasPrefix(path, luaToolChunk) && !isApproved(path) {
		return true, false
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
			return true, isApproved(name)
		}
	}
	return strings.HasPrefix(path, luaToolChunk), isApproved(path)
}
