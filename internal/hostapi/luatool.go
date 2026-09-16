// luatool.go is the model's Lua tool: it runs a chunk on the live
// interpreter as user origin, so the agent can inspect and change zemi's own
// state (this session, agent runs, memories, settings, skills, errors)
// through the API every Lua caller uses. It asks (tool.lua) in an untrusted
// project or plan mode, and always before calls that widen what zemi allows
// or start programs outside the sandbox (refuseModelLua); the tool.pre
// events always run so user hooks can deny or halt a script.
package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// luaToolCeiling bounds one run: it holds the interpreter the UI renders
// through. Cancelling stops the script at its next opcode.
const luaToolCeiling = 10 * time.Second

// luaToolChunk names the tool's chunks; an approved run appends "#<n>".
const luaToolChunk = "lua-tool"

// luaWidens spots the calls refuseModelLua holds for the user, so the tool
// asks before the run. A call it misses (a name built from strings) raises.
var luaWidens = regexp.MustCompile(`permission\s*\.\s*rule|mode\s*\.\s*(add|remove)|config\s*\.\s*set\s*\(\s*["']permissions|` +
	`job\s*\.\s*(start|out)|win\s*\.\s*open|:\s*(send|keys|mouse)\b|mcp\s*\.\s*add|lsp\s*\.\s*add|command\s*\.\s*run\s*\(\s*["']mode`)

type luaEvalTool struct{ h *Host }

func (h *Host) luaEvalTool() fantasy.AgentTool { return &luaEvalTool{h: h} }

func (t *luaEvalTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: "Lua",
		Description: "Run Lua 5.1 inside zemi to read or change zemi's live state: this session, agent runs, settings, keymaps, widgets, skills, errors. Load the zemi-lua skill for the API first." +
			" What the chunk prints and returns (as JSON) comes back. It runs on the interpreter the UI draws with, for at most 10s: no waiting loops; start slow work with zemi.job or zemi.timer." +
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
	// before it widens permissions or starts programs: no mode may settle
	// that, since what it allows or starts runs unconfined
	widens := luaWidens.MatchString(src)
	if widens || !h.ProjectTrusted() || h.permModeDef().Base == "plan" {
		arg := "script"
		if in.Path != "" {
			arg = in.Path
		}
		preview := "+ " + strings.ReplaceAll(strings.TrimRight(src, "\n"), "\n", "\n+ ")
		req := tools.Request{Tool: "Lua", Subject: "tool.lua", Arg: arg, Reason: in.Description, Preview: preview}
		if widens {
			req.Unsandboxed = true
			req.Reason = strings.TrimSuffix("changes permissions, config or servers, or starts programs outside the sandbox: "+in.Description, ": ")
		}
		if v := h.PipelineFor().Check(req); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): Lua " + arg), nil
		}
		if widens {
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
// what a user init.lua can, but refuseModelLua holds widening calls for the
// user) and returns what it printed, then its return values as JSON.
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

// refuseModelLua raises when code from the model's Lua tool (its chunk, a
// function it defined, or a callback it registered) makes a call that widens
// what zemi allows or starts a program, unless the user approved that run.
// It reports whether an approved run is calling, which settles the call's
// own ask.
func (h *Host) refuseModelLua(L *glua.LState, what string) bool {
	approved := func(name string) bool {
		_, ok := h.luaApproved.Load(name)
		return ok
	}
	refuse := func() {
		L.RaiseError("%s: the Lua tool asks the user before this call; write the call out in the script so the ask covers it", what)
	}
	path := h.RT.CurrentPath(L)
	if strings.HasPrefix(path, luaToolChunk) && !approved(path) {
		refuse()
	}
	// the innermost tool function decides: an approved run calling code an
	// earlier run defined does not approve that code
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
			if !approved(name) {
				refuse()
			}
			return true
		}
	}
	return approved(path)
}
