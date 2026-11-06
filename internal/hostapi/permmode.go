package hostapi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
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
		if !slices.Contains(names, name) && !strings.HasPrefix(name, _mcpToolPrefix) {
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
