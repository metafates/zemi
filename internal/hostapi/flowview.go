package hostapi

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// The workflow views: the /workflows list, one run's agents and a single
// agent, as the overlays the screens read.

// flowWord is a run's status word, paused while it holds its queue.
func flowWord(v flowView) string {
	if v.paused && v.status == RunRunning {
		return "paused"
	}
	return string(v.status)
}

// flowFields are the name, value pairs a run's detail and view open with,
// each there from the start, so a run that moves adds no row.
func flowFields(f *flowRun, v flowView) []string {
	usage := fmtTokens(v.tokens) + " tok"
	if v.cost > 0 {
		usage += " · " + fmtCost(v.cost)
	}
	script := ""
	if f.script != "" {
		script = perm.TildePath(f.script)
	}
	return []string{
		"status", flowWord(v), "agents", fmt.Sprintf("%d/%d done · %d running · %d at once", v.done, len(v.agents), v.running, v.limit),
		"usage", usage, "time", fmtDuration(v.took), "run", f.id, "script", script,
	}
}

// flowDesc is a run's row desc in /workflows and /agents: the padded status
// and the id, then what grows, the cost last since it appears late.
func flowDesc(f *flowRun, v flowView) string {
	desc := fmt.Sprintf("%s · %s · %d/%d agents · %s", statusCol(flowWord(v)), f.id, v.done, len(v.agents), fmtDuration(v.took))
	if v.cost > 0 {
		desc += " · " + fmtCost(v.cost)
	}
	return desc
}

// flowItem is a run's row in /workflows: enter opens its view, whose keys
// act on it.
func (h *Host) flowItem(f *flowRun, section string) map[string]any {
	v := f.view()
	return map[string]any{
		"id": f.id, "text": f.description, "desc": flowDesc(f, v) + h.fromSession(f.parent), "section": section,
		"run": "workflows view " + f.id, "verb": "open", "mark": RunMarks[flowWord(v)],
	}
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug is a saved workflow name made from a description.
func slug(s string) string {
	s = notSlug.ReplaceAllString(strings.ToLower(s), "-") // ascii from here
	return cmp.Or(strings.Trim(s[:min(len(s), 40)], "-"), "workflow")
}

// openFlow opens a run's view: its agents by phase.
func (h *Host) openFlow(id string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	f := h.tasks.flow(id)
	if f == nil {
		if f, err = h.loadFlow(h.AgentSessionID(), id); err != nil {
			return err
		}
	}
	ui.OpenOverlay(h.flowOverlay(f))
	return nil
}

// flowOverlay is a run's view: its fields, then its agents under their
// phases; enter on one opens its detail, whose actions view, restart or
// stop it. Its keys act on the run: while it runs p pauses or resumes, + and
// - change how many run at once and x stops it; once it ended r reruns it
// in a view of its own, and a run of this session opens its script, which s
// saves when the model wrote it and this process still holds it as it ran.
func (h *Host) flowOverlay(f *flowRun) Overlay {
	v := f.view()
	var items []any
	names, by := f.phases(v.agents)
	for _, p := range names {
		section := fmt.Sprintf("%s  %d/%d", p, ended(by[p]), len(by[p]))
		for _, a := range by[p] {
			items = append(items, h.flowAgentItem(f, a, section))
		}
	}
	o := Overlay{
		ID: "workflow:" + f.id, Title: "Workflows / " + f.description, Origin: lua.OriginBuiltin, Items: items, Wide: true, Phases: true,
		Mark: RunMarks[flowWord(v)],
		// one row, so a long one never pushes the list down
		Body: clipLine(f.about, 76), Empty: "No agent has started yet.",
		Reload: func() ([]any, [][]Segment) { return h.flowOverlay(f).Items, nil },
	}
	for kv := flowFields(f, v); len(kv) > 0; kv = kv[2:] {
		o.Fields = append(o.Fields, Field{kv[0], kv[1]})
	}
	if v.status == RunRunning {
		// pause is padded to resume's width, so pausing shifts no footer text
		pause := OverlayKey{Key: "p", Desc: "pause ", Run: "workflows pause " + f.id}
		if v.paused {
			pause = OverlayKey{Key: "p", Desc: "resume", Run: "workflows resume " + f.id}
		}
		o.Keys = []OverlayKey{
			pause,
			{Key: "+", Desc: "at once", Run: "workflows concurrency " + f.id + " +1"},
			{Key: "-", Desc: "at once", Run: "workflows concurrency " + f.id + " -1"},
			{Key: "x", Desc: "stop workflow", Run: "workflows stop " + f.id, Danger: true},
		}
	}
	if f.script != "" && f.parent == h.AgentSessionID() {
		if v.status != RunRunning {
			o.Keys = append(o.Keys, OverlayKey{Key: "r", Desc: "rerun", Run: "workflows rerun " + f.id})
		}
		o.Keys = append(o.Keys, OverlayKey{Key: "o", Desc: "script", Run: "workflows open " + f.id})
	}
	if f.src != "" && f.name == "" && f.parent == h.AgentSessionID() { // what saveFlow takes; a saved workflow's run is saved already
		o.Keys = append(o.Keys, OverlayKey{Key: "s", Desc: "save", Run: "workflows save " + f.id})
	}
	return o
}

// flowAgentItem is an agent's row in a run's view, keyed by its call's
// index, which a call made later in an earlier phase does not move.
func (h *Host) flowAgentItem(f *flowRun, a agentRow, section string) map[string]any {
	word := statusCol(string(a.status))
	desc := word + " · " + a.def
	if a.reused {
		desc = word + " · reused"
	}
	if a.status != "queued" {
		desc += fmt.Sprintf(" · %d %s · %s tok · %s", a.calls, Plural(a.calls, "call", "calls"), fmtTokens(a.tokens), fmtDuration(a.took))
	}
	item := map[string]any{"id": strconv.Itoa(a.call), "text": a.description, "desc": desc, "section": section, "mark": RunMarks[string(a.status)]}
	// a queued row opens too, so starting it moves no footer text
	item["fields"] = fieldList("status", string(a.status), "agent", a.def)
	if a.id == "" {
		return item
	}
	run := h.tasks.find(a.id)
	def := a.def
	if run != nil && run.model != "" {
		def += " · " + run.model
	}
	usage, detail := fmt.Sprintf("%d %s · %s tok", a.calls, Plural(a.calls, "call", "calls"), fmtTokens(a.tokens)), ""
	if a.cost > 0 {
		usage += " · " + fmtCost(a.cost)
	}
	actions := []any{map[string]any{"text": "View its transcript", "hotkey": "v", "run": "workflows view " + f.id + " " + a.id}}
	if run != nil {
		detail = run.prompt()
		if a.status == RunRunning {
			actions = append(actions,
				map[string]any{"text": "Restart", "hotkey": "r", "run": "workflows restart " + f.id + " " + a.id},
				// no hotkey: x on the view stops the whole run
				map[string]any{"text": "Stop", "run": "agents stop " + a.id, "danger": true})
		}
	}
	item["fields"] = fieldList("status", string(a.status), "time", fmtDuration(a.took), "agent", def, "usage", usage, "run", a.id)
	item["detail"], item["actions"] = detail, actions
	return item
}
