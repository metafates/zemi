package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// CheckToolNames records a zemi.tool.override no tool answers to, once boot
// has added every Lua tool: the tool one names may come from a project or a
// plugin, so the call that adds it cannot tell.
func (h *Host) CheckToolNames() {
	names := h.builtinToolNames()
	for _, t := range h.luaTools.list() {
		names = append(names, t.Name)
	}
	for _, name := range h.luaTools.overridden() {
		if !slices.Contains(names, name) && !strings.HasPrefix(name, "mcp_") {
			h.RecordErrorOnce("tools", "", fmt.Sprintf("zemi.tool.override names %q, which no tool is, so it never runs: tools are %s and mcp_<server>_<tool>", name, strings.Join(names, ", ")))
		}
	}
}

// approvePlan is the plan tool's hook: it shows a plan in the approval popup.
// The plan joins the transcript, then the user approves it, keeps it without
// starting, types what to change, or dismisses it; the file is written once
// the verdict is in, so a dismissed plan cannot overwrite the session's own.
func (h *Host) approvePlan(plan string) (tools.PlanVerdict, bool) {
	ui := h.hooks()
	if ui == nil {
		return tools.PlanVerdict{}, false
	}
	var v tools.PlanVerdict
	h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: sanitize.String(plan)})
	verdict, ok := ui.AskPlan(sanitize.String(plan))
	if !ok {
		return v, false
	}
	if !verdict.Approved {
		v.Feedback = verdict.Feedback
		return v, true
	}
	if path := h.planPath(h.AgentSessionID()); path != "" {
		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err == nil {
			err = os.WriteFile(path, []byte(plan+"\n"), 0o600)
		}
		if err != nil {
			h.Log.Warn("plan not saved", "origin", "plan", "error", err)
		} else {
			v.Path = path
		}
	}
	v.Approved, v.Hold = true, verdict.Hold
	return v, true
}

// permNamespace is zemi.perm: the plugin ask surface. The `perm` capability
// in the manifest's uses is the consent to steer the gate: a plugin that can
// open the overlay can force it on actions of its choosing.
func (h *Host) permNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("ask", L.NewFunction(func(L *glua.LState) int {
		h.requirePermUse(L, "zemi.perm.ask")
		lua.CheckWait(L, "zemi.perm.ask")
		spec := L.CheckTable(1)
		subject := rawString(L, spec, "subject")
		if subject == "" {
			L.RaiseError("zemi.perm.ask: subject is required")
		}
		name := h.PluginOf(h.RT.CurrentPath(L))
		owner := string(h.RT.CurrentOrigin(L))
		if name != "" {
			owner = perm.PluginPrincipal(name)
		}
		req := tools.Request{
			Subject: subject,
			Arg:     rawString(L, spec, "arg"),
			Reason:  rawString(L, spec, "reason"),
			Preview: rawString(L, spec, "preview"),
			Owner:   owner,
		}
		done := make(chan struct{})
		answer := "deny" // a panic in the ask fails closed
		cut := h.askCut.Load()
		h.GoSafe("zemi.perm.ask", func() {
			// closed even when the ask panics
			defer close(done)
			allowed, _ := h.ask(req)
			switch {
			case allowed:
				answer = "allow"
			case h.askCut.Load() != cut: // the user denied and told zemi what to do instead
				answer = "told"
			}
		})
		handle := L.NewTable()
		handle.RawSetString("wait", lua.Suspending(L, L.NewFunction(func(L *glua.LState) int {
			return waitHandle(L, "perm:wait", done, time.Hour, nil, func(L *glua.LState, err error) []glua.LValue {
				if err != nil {
					return []glua.LValue{glua.LNil, glua.LString(err.Error())}
				}
				return []glua.LValue{lua.FromGo(L, map[string]any{"answer": answer})}
			})
		})))
		lua.GuardFuncs(L, handle, "perm:", h.guardLua)
		L.Push(handle)
		return 1
	}))
	return tb
}

// requirePermUse raises unless the calling plugin declared the perm
// capability: registering a permission.request handler and zemi.perm.ask
// both steer the gate, so enabling the plugin is the consent. Code of no
// plugin passes.
func (h *Host) requirePermUse(L *glua.LState, api string) {
	if o := h.RT.CurrentOrigin(L); o != lua.OriginPlugin {
		return
	}
	name := h.PluginOf(h.RT.CurrentPath(L))
	if h.pluginDeclares(name, "perm") {
		return
	}
	L.RaiseError("%s: plugin %s declares no %q; add %q to uses in %s", api, name, "perm", "perm", plugin.ManifestFile)
}
