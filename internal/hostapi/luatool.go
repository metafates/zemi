// luatool.go is the model's Lua tool: it runs a chunk on the live
// interpreter as user origin, so the agent can inspect and change zemi's own
// state (this session, agent runs, memories, settings, skills, errors)
// through the API every Lua caller uses. Without the user's approval a run
// may only call read-only members (luaReadOnly, enforced by guardModelLua);
// a script that names anything else asks once up front, and so does every
// run in an untrusted project or by a read-only agent. The tool.pre events always run
// so user hooks can deny or halt a script.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// luaToolCeiling bounds one Lua tool run on the interpreter; a var so a
// test can hit it without waiting.
var luaToolCeiling = 10 * time.Second

// luaToolChunk names the tool's chunks; an approved run appends "#<n>".
const luaToolChunk = "lua-tool"

// luaReadOnly is what the model's Lua tool may call without the user's
// approval: members that only read zemi's state, and pure helpers. Every
// other Go function exposed to Lua, handle methods included ("job:kill"),
// raises in an unapproved run, so a new member asks until it is added here.
var luaReadOnly = map[string]bool{
	"agent.current": true, "agent.list": true, "agent.prompt": true, "agent.usage": true,
	"command.list": true, "composer.get": true,
	"config.get": true, "config.schema": true,
	"errors.list": true, "event.log": true, "event.stats": true,
	"health.list": true,
	"fs.datadir":  true, "fs.exists": true, "fs.glob": true, "fs.grep": true, "fs.list": true, "fs.read": true, "fs.stat": true,
	"json.decode": true, "json.encode": true,
	"keymap.get": true, "keymap.list": true, "learn.proposals": true,
	"log": true, "log.list": true, "log.debug": true, "log.info": true, "log.warn": true, "log.error": true,
	"lsp.list": true, "mcp.list": true, "memory.list": true,
	"model.list": true, "notify.list": true,
	"permission.mode.list": true, "permission.rule.list": true, "project.root": true, "project.trusted": true, "provider.list": true,
	"session.id": true, "session.list": true, "session.messages": true, "session.title": true, "session.todos": true,
	"skills.list": true, "state.get": true, "tool.list": true,
	"ui.focus.get": true, "ui.hl.get": true, "ui.layout.get": true, "ui.layout.rects": true, "ui.markdown": true, "ui.wrap": true, "ui.width": true, "ui.screenshot": true, "ui.theme.get": true, "ui.theme.list": true, "ui.widget.list": true,
	"workflow.list": true, "job:wait": true, "http:wait": true,
}

// luaReadMethods are the method calls the text scan lets through: string
// methods and the read-only handle methods.
var luaReadMethods = map[string]bool{
	"byte": true, "find": true, "format": true, "gmatch": true, "gsub": true, "len": true,
	"lower": true, "match": true, "rep": true, "reverse": true, "sub": true, "upper": true,
	"wait": true,
}

// luaBuiltinRequire reports whether the require at i names a zemi.*
// module: those are shipped code the requirer gets read-only, so loading
// one changes nothing.
func luaBuiltinRequire(toks []ast.Token, i int) bool {
	j := i + 1
	if j < len(toks) && toks[j].Type == '(' {
		j++
	}
	return j < len(toks) && toks[j].Type == parse.TString && strings.HasPrefix(toks[j].Str, "zemi.")
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
			if t.Str != "require" || !luaBuiltinRequire(toks, i) {
				add(t.Str)
			}
		case t.Str == "zemi":
			var path []string
			for i+2 < len(toks) && toks[i+1].Type == '.' && toks[i+2].Type == parse.TIdent {
				path = append(path, toks[i+2].Str)
				i += 2
			}
			// a path nothing calls is a field read: it does nothing on its
			// own, and a function read into a variable still meets the guard.
			// zemi itself with no path is the aliasing route, always listed.
			called := i+1 < len(toks) && (toks[i+1].Type == '(' || toks[i+1].Type == parse.TString || toks[i+1].Type == '{')
			if name := strings.Join(path, "."); len(path) == 0 || called && !luaReadOnly[name] {
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
		Name: tools.ToolLua,
		Description: "Run Lua 5.1 inside zemi to read or change its live state: this session, agent runs, settings, keymaps, widgets, skills, errors. Load the zemi-lua skill for the API first." +
			" What the chunk prints and returns (as JSON) comes back. It runs on the interpreter the UI draws with, for at most 10s: no waiting loops, start slow work with zemi.job or zemi.timer." +
			" A script that only reads (zemi.config.get, zemi.session.messages, zemi.errors.list, zemi.ui.screenshot, the zemi.fs readers, ...) runs at once; one that changes anything asks the user once, so write every call out by name." +
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

func (t *luaEvalTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	h := t.h
	var in struct {
		Description string `json:"description"`
		Script      string `json:"script"`
		Path        string `json:"path"`
	}
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop, err := h.preCheck("lua", call.Input); stop {
		return resp, err
	}
	defer h.postNotify("lua", call.Input, &reply)
	src := in.Script
	if in.Path != "" {
		// the file is read like the read tool reads it: a Lua error echoes
		// tokens of what it ran, so a secret or a file outside the jail asks
		if filepath.Ext(in.Path) != ".lua" {
			return fantasy.NewTextErrorResponse(in.Path + ": not a .lua file"), nil
		}
		path := in.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(h.Dirs.ProjectDir, path)
		}
		path = perm.RealPath(path)
		if v := h.PipelineFor().CheckDenied(tools.Request{Tool: "lua", Subject: "fs.read", Arg: path}); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse(tools.DeniedText(v, "fs.read "+in.Path)), nil
		}
		// opened the way the file tools open theirs, so a directory swapped
		// for a symlink after the check fails instead of leading elsewhere
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
	rel := luaToolChunk
	// the script can do anything user config can, so it asks where the
	// model's text is less trusted or the user wants nothing changed, and
	// before any call that is not a read: no mode may settle that, since
	// what it changes or starts runs unconfined
	unlisted := luaUnlisted(src)
	if len(unlisted) > 0 || !h.ProjectTrusted() || h.readOnly() {
		arg := "script"
		if in.Path != "" {
			arg = in.Path
		}
		req := tools.Request{Tool: "lua", Subject: "tool.lua", Arg: arg, Description: in.Description, Preview: src}
		if len(unlisted) > 0 {
			req.Unsandboxed = true
			req.Reason = "uses " + strings.Join(unlisted, ", ") + ", which can change zemi or start programs outside the sandbox"
		}
		if v := h.PipelineFor().CheckDenied(req); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse(tools.DeniedText(v, "Lua "+arg)), nil
		}
		if len(unlisted) > 0 {
			rel = fmt.Sprintf("%s#%d", luaToolChunk, h.luaRuns.Add(1))
			h.luaApproved.Store(rel, true)
			defer h.luaApproved.Delete(rel)
		}
	}
	out, err := h.runModelLua(ctx, src, rel)
	if err != nil {
		msg := err.Error()
		// the ceiling, not an interrupt: the model can tell them apart and
		// move the work, as the bash tool's timeout line lets it
		if ctx.Err() == nil && errors.Is(err, context.Canceled) {
			msg = fmt.Sprintf("stopped after %s: the script must finish quickly; move long work to zemi.job or a bash command", luaToolCeiling)
		}
		if in.Path != "" {
			// the chunk keeps its tool name, which the sandbox guard reads;
			// the error names the file the model can edit
			msg = strings.ReplaceAll(msg, rel+":", in.Path+":")
		}
		return fantasy.NewTextErrorResponse(h.truncOutput("lua", strings.TrimSpace(out+"\nerror: "+msg))), nil
	}
	if out == "" {
		out = "(no output)"
	}
	return fantasy.NewTextResponse(h.truncOutput("lua", out)), nil
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
