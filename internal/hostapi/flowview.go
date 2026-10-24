package hostapi

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
)

// The workflow views: the /workflows list, one run's agents and a single
// agent, as the overlays the screens read - and the agent-run preview /agents
// rows open too.

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
	fields := []string{
		"status", flowWord(v), "agents", fmt.Sprintf("%d/%d done · %d running · %d at once", v.done, len(v.agents), v.running, v.limit),
		"usage", usage, "time", fmtDuration(v.took), "run", f.id, "script", script,
	}
	if f.err != nil {
		fields = append(fields, "error", f.err.Error())
	}
	return fields
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
func (h *Host) flowItem(f *flowRun, section string) OverlayItem {
	v := f.view()
	return OverlayItem{
		ID: f.id, Text: f.description, Desc: flowDesc(f, v) + h.fromSession(f.parent), Section: section,
		Run: "workflows view " + f.id, Verb: VerbOpen, Mark: RunMarks[flowWord(v)],
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
	var items []OverlayItem
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
		Reload: func() []OverlayItem { return h.flowOverlay(f).Items },
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

// agentView is an agent run's row view, the same in /agents and a workflow's
// run: its facts, then its task and, once it has ended, what it answered. A
// row with nothing to read (queued, reused, stored) sets the facts alone; a
// failed one carries its error as a field.
func (h *Host) agentView(a agentRow, run *agentRun) (fields []Field, detail string, parts []BodyPart) {
	usage := fmt.Sprintf("%d %s · %s tok", a.calls, Plural(a.calls, "call", "calls"), fmtTokens(a.tokens))
	if a.cost > 0 {
		usage += " · " + fmtCost(a.cost)
	}
	def := a.def
	if run != nil && run.model != "" {
		def += " · " + run.model
	}
	fields = fieldList("status", string(a.status), "time", fmtDuration(a.took), "agent", def, "usage", usage, "run", a.id)
	if run == nil {
		return fields, "", nil
	}
	prompt := sanitize.String(run.prompt())
	if a.status == RunFailed {
		if err := run.errText(); err != "" {
			fields = append(fields, Field{"error", err})
		}
	} else if answer := run.answer(); answer != "" && (a.status == RunDone || a.status == RunWarn) {
		parts = append(parts, BodyPart{Text: answer, Lang: "markdown"})
	}
	if prompt != "" {
		parts = append([]BodyPart{{Text: prompt}}, parts...)
	}
	for _, p := range parts {
		if detail != "" {
			detail += "\n\n"
		}
		detail += p.Text
	}
	return fields, detail, parts
}

// flowAgentItem is an agent's row in a run's view, keyed by its call's
// index, which a call made later in an earlier phase does not move.
func (h *Host) flowAgentItem(f *flowRun, a agentRow, section string) OverlayItem {
	word := statusCol(string(a.status))
	desc := word + " · " + a.def
	if a.reused {
		desc = word + " · reused"
	}
	if a.status != "queued" {
		desc += fmt.Sprintf(" · %d %s · %s tok · %s", a.calls, Plural(a.calls, "call", "calls"), fmtTokens(a.tokens), fmtDuration(a.took))
	}
	run := h.tasks.find(a.id)
	fields, detail, parts := h.agentView(a, run)
	item := OverlayItem{
		ID: strconv.Itoa(a.call), Text: a.description, Desc: desc, Section: section, Mark: RunMarks[string(a.status)],
		Fields: fields, Detail: detail, Parts: parts,
	}
	if a.id == "" {
		return item
	}
	keys := []ItemKey{{Key: "v", Desc: "view its transcript", Run: "workflows view " + f.id + " " + a.id}}
	if run != nil && a.status == RunRunning {
		keys = append(keys,
			ItemKey{Key: "r", Desc: "restart", Run: "workflows restart " + f.id + " " + a.id},
			// s stops this agent; x on the view stops the whole run
			ItemKey{Key: "s", Desc: "stop", Run: "agents stop " + a.id, Danger: true})
	}
	item.Keys = keys
	return item
}
