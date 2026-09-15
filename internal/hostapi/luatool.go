// luatool.go is the model's lua tool: it runs a chunk on the live
// interpreter with user config's access to zemi.*, so the agent can inspect
// and change zemi's own state (this session, agent runs, memories, settings,
// errors) through the API every Lua caller uses. Each run passes the
// tool.lua permission check with the script as preview; rules and modes
// answer it like any other ask, and plan mode turns its allows into asks.
package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// luaToolCeiling bounds one run: it holds the interpreter the UI renders
// through. Cancelling stops the script at its next opcode.
const luaToolCeiling = 10 * time.Second

type luaEvalTool struct{ h *Host }

func (h *Host) luaEvalTool() fantasy.AgentTool { return &luaEvalTool{h: h} }

func (t *luaEvalTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: "lua",
		Description: "Run Lua 5.1 inside zemi with user config's access to the zemi.* API, to read or change zemi's live state: this session, agent runs, memories, settings, keymaps, widgets, errors. Load the zemi-lua skill for the API first." +
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
	if resp, stop, err := h.preCheck("lua", call.Input); stop {
		return resp, err
	}
	src, name := in.Script, "script"
	if in.Path != "" {
		path := in.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(h.Dirs.ProjectDir, path)
		}
		if v := h.PipelineFor().Check(tools.Request{Tool: "lua", Subject: "fs.read", Arg: path, Reason: "read file"}); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): fs.read " + path), nil
		}
		s, err := readRegularFile(path)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		src, name = s, path
	}
	if strings.TrimSpace(src) == "" {
		return fantasy.NewTextErrorResponse("pass script or path"), nil
	}
	preview := "+ " + strings.ReplaceAll(strings.TrimRight(src, "\n"), "\n", "\n+ ")
	if v := h.PipelineFor().Check(tools.Request{Tool: "lua", Subject: "tool.lua", Arg: name, Reason: in.Description, Preview: preview}); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): lua " + name), nil
	}
	out, err := h.runModelLua(ctx, src)
	if err != nil {
		return fantasy.NewTextErrorResponse(truncOutput(strings.TrimSpace(out + "\nerror: " + err.Error()))), nil
	}
	if out == "" {
		out = "(no output)"
	}
	return fantasy.NewTextResponse(truncOutput(out)), nil
}

// runModelLua runs src as a user-origin chunk and returns what it printed,
// then its return values as JSON.
func (h *Host) runModelLua(ctx context.Context, src string) (string, error) {
	var b strings.Builder
	vals, err := h.RT.Eval(ctx, src, "lua-tool", lua.OriginUser, luaToolCeiling, func(line string) {
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
