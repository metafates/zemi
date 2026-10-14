package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// builtinModes are the two permission modes, in cycle order: ask prompts
// before every action, allow runs everything. The permission system is a UX
// feature - it keeps the user aware of what the agent is doing - not
// isolation; a run that must not touch the machine runs in a container.
var builtinModes = []PermModeDef{
	{Name: config.PermModeAsk, Description: config.PermModeAskDesc},
	{Name: config.PermModeAllow, Description: config.PermModeAllowDesc},
}

// PermModeDef is one permission mode: a name and what it means.
type PermModeDef struct {
	Name, Description string
}

// modeReg holds the read-only agent's restore point and the asks a headless
// run denied.
type modeReg struct {
	mu             sync.Mutex
	prePlanMode    string // the mode active before a read-only agent was picked
	prePlanProfile string // the agent active before a read-only agent was picked

	headless []Denial
}

// Denial is an ask nobody could answer because no UI runs (zemi run), any
// pipeline's: subagents and workflows run no rows the main transcript shows.
// Arg and Reason are sanitized.
type Denial struct {
	Tool    string    `json:"tool"`
	Subject string    `json:"subject"`
	Arg     string    `json:"arg"`
	Reason  string    `json:"reason"`
	Why     DenialWhy `json:"why"`
}

// DenialWhy is what a denied ask needed.
type DenialWhy string

const (
	DenyApproval DenialWhy = "approval"  // the mode left it to the user
	DenyNeedsTUI DenialWhy = "needs_tui" // a Lua script or a memory edit
)

func (r *modeReg) noteHeadless(d Denial) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headless = append(r.headless, d)
}

// HeadlessDenials is every ask denied so far; zemi run reads the ones past
// its start.
func (h *Host) HeadlessDenials() []Denial {
	h.modes.mu.Lock()
	defer h.modes.mu.Unlock()
	return slices.Clip(h.modes.headless)
}

// PermMode reports the active permission mode. An unknown value reads as
// the default, allow.
func (h *Host) PermMode() string {
	if mode := h.Settings.Str("permissions.mode"); mode == config.PermModeAsk || mode == config.PermModeAllow {
		return mode
	}
	return config.PermModeAllow
}

// SetPermMode switches the mode at runtime (/mode, ctrl+y).
func (h *Host) SetPermMode(mode string) error {
	if mode != config.PermModeAsk && mode != config.PermModeAllow {
		return Usagef("unknown permission mode %q (want %s or %s)", mode, config.PermModeAsk, config.PermModeAllow)
	}
	return h.setWatched("permissions.mode", mode)
}

// OpenModePicker lists the modes with what they mean; enter switches.
func (h *Host) OpenModePicker() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	cur, sel := h.PermMode(), 0
	var items []any
	for i, m := range builtinModes {
		item := map[string]any{"text": m.Name, "desc": m.Description, "run": "mode " + m.Name}
		if m.Name == cur {
			item["key"], sel = "current", i
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "mode", Title: "Permission mode", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Anchor: OverlayBottomLeft, Verb: "pick"})
	return nil
}

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
			h.RecordErrorOnce("tools", fmt.Sprintf("zemi.tool.override names %q, which no tool is, so it never runs: tools are %s and mcp_<server>_<tool>", name, strings.Join(names, ", ")))
		}
	}
}

// approvePlan is the plan tool's hook: it shows a plan in the approval popup.
// The plan joins the transcript, then the user approves it, keeps it without
// starting, types what to change, or dismisses it; the file is written once
// the verdict is in, so a dismissed plan cannot overwrite the session's own.
// A read-only agent (planner) remembers what ran before it (prePlan), so any
// approval returns to that agent and mode; an approval that starts the work
// records an implementation handoff (planApproved) that the ended turn passes
// on to planFollowup, and a hold skips that handoff.
func (h *Host) approvePlan(plan string) (tools.PlanVerdict, bool) {
	ui := h.hooks()
	if ui == nil {
		return tools.PlanVerdict{}, false
	}
	var v tools.PlanVerdict
	h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: sanitize.String(plan)})
	restore, mode := "", ""
	if h.readOnly() {
		h.modes.mu.Lock()
		restore, mode = h.modes.prePlanProfile, h.modes.prePlanMode
		h.modes.mu.Unlock()
		if restore == "" { // a read-only agent picked from config: the shipped profile, the mode as it is
			restore = "default"
		}
		v.Profile = restore
	}
	verdict, ok := ui.AskPlan(sanitize.String(plan), restore)
	if !ok {
		return v, false
	}
	if !verdict.Approved {
		v.Feedback = verdict.Feedback
		return v, true
	}
	if path := h.planPath(h.AgentSessionID()); path != "" {
		if os.MkdirAll(filepath.Dir(path), 0o700) == nil && os.WriteFile(path, []byte(plan+"\n"), 0o600) == nil {
			v.Path = path
		}
	}
	if restore != "" {
		// a profile or mode the config dropped since is no reason to read the
		// approval as a dismissal: the fallbacks (default, allow) stand
		if err := h.SetProfile(restore); err != nil {
			h.Log.Info("plan restore skipped a profile", "origin", "permissions", "profile", restore, "error", err)
		}
		if mode != "" {
			if err := h.SetPermMode(mode); err != nil {
				h.Log.Info("plan restore skipped a mode", "origin", "permissions", "mode", mode, "error", err)
			}
		}
		if !verdict.Hold {
			// the read-only turn cannot implement the plan itself; the turn-end
			// hook runs it as the restored agent
			h.setPlanApproved(implementNow)
		}
	}
	v.Approved, v.Hold = true, verdict.Hold
	return v, true
}

// cyclePermMode steps through the modes (the mode next and prev actions).
func (h *Host) cyclePermMode(step int) error {
	n := len(builtinModes)
	i := slices.IndexFunc(builtinModes, func(d PermModeDef) bool { return d.Name == h.PermMode() })
	return h.SetPermMode(builtinModes[((i+step)%n+n)%n].Name)
}

// permissionNamespace is zemi.permission: the mode table, and nothing else -
// the permission system prompts or allows, it keeps no rules.
func (h *Host) permissionNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("mode", h.modeNamespace(L))
	return tb
}

func (h *Host) modeNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		h.refuseBelowUser(L, "permission.mode.set")
		return luaResult(L, h.SetPermMode(strings.ToLower(name)))
	}))
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		row := L.NewTable()
		row.RawSetString("name", glua.LString(h.PermMode()))
		row.RawSetString("description", glua.LString(h.modeDef().Description))
		L.Push(row)
		return 1
	}))
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, d := range builtinModes {
			row := L.NewTable()
			row.RawSetString("name", glua.LString(d.Name))
			row.RawSetString("description", glua.LString(d.Description))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	return tb
}

func (h *Host) modeDef() PermModeDef {
	mode := h.PermMode()
	i := slices.IndexFunc(builtinModes, func(d PermModeDef) bool { return d.Name == mode })
	return builtinModes[max(i, 0)]
}
