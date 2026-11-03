package hostapi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
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
		if !slices.Contains(names, name) && !strings.HasPrefix(name, mcpToolPrefix) {
			h.RecordErrorOnce("tools", "", fmt.Sprintf("zemi.tool.override names %q, which no tool is, so it never runs: tools are %s and mcp_<server>_<tool>", name, strings.Join(names, ", ")))
		}
	}
}

// plansDir is where the project's approved plans are saved, "" with no state
// home or project root. They sit with the project's own store, so deleting the
// project takes them.
func (h *Host) plansDir() string {
	if h.Dirs.StateHome == "" || h.trustRoot() == "" {
		return ""
	}
	return filepath.Join(projectState(h.Dirs.StateHome, h.trustRoot()), "plans")
}

// savePlan writes plan into dir under a name of its own and returns the path.
// A name another plan holds takes -2, -3; the same text keeps the name it
// already has, so a path handed out once still holds its plan.
func savePlan(dir, name, plan string) (string, error) {
	stem := slug(name, "plan")
	for i := 1; ; i++ {
		file := stem + ".md"
		if i > 1 {
			file = fmt.Sprintf("%s-%d.md", stem, i)
		}
		path := filepath.Join(dir, file)
		old, err := os.ReadFile(path)
		switch {
		case err == nil && string(old) == plan+"\n":
			return path, nil
		case errors.Is(err, fs.ErrNotExist):
			return path, config.WriteFileAtomic(path, []byte(plan+"\n"))
		case err != nil:
			return "", err
		}
	}
}

// planName is the name a plan file takes: the model's own, else the plan's
// first heading, else "plan".
func planName(name, plan string) string {
	if n := slug(name, ""); n != "" {
		return n
	}
	return slug(planHeading(plan), "plan")
}

// planHeading is the plan's first markdown heading, "" when it has none.
func planHeading(plan string) string {
	for line := range strings.Lines(plan) {
		if h, ok := strings.CutPrefix(strings.TrimSpace(line), "#"); ok {
			return strings.TrimLeft(h, "# ")
		}
	}
	return ""
}

// approvePlan is the plan tool's hook: it shows a plan in the approval popup.
// The plan joins the transcript, then the user approves it, keeps it without
// starting, types what to change, or dismisses it; the file is written once
// the verdict is in, so a dismissed plan cannot overwrite the project's own.
func (h *Host) approvePlan(plan, name string) (tools.PlanVerdict, bool) {
	ui := h.hooks()
	if ui == nil {
		return tools.PlanVerdict{}, false
	}
	var v tools.PlanVerdict
	h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: sanitize.String(plan)})
	done := h.holdPrompt()
	verdict, ok := ui.AskPlan(sanitize.String(plan))
	done()
	if !ok {
		return v, false
	}
	if !verdict.Approved {
		v.Feedback = verdict.Feedback
		return v, true
	}
	if dir := h.plansDir(); dir != "" {
		path, err := savePlan(dir, planName(name, plan), plan)
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
				return []glua.LValue{lua.FromGo(L, map[string]any{"decision": answer})}
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
	if h.pluginDeclares(name, plugin.CapPerm) {
		return
	}
	L.RaiseError("%s: plugin %s declares no %q; add %q to uses in %s", api, name, plugin.CapPerm, plugin.CapPerm, plugin.ManifestFile)
}
