package ui

import (
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/hostapi"
)

// The statusline: statusline.left holds the project and what needs attention,
// statusline.right the model and context use. Each side draws from its outer
// edge in rank order (priority where no rank is set) and a short row drops
// the highest priority first, so
// what lasts sits outermost and what comes and goes sits inner, where it
// moves nothing.
func statuslineWidgets() []*widget {
	return []*widget{
		{id: "statusline.cwd", region: _regionStatusLeft, priority: 10, eases: true, render: statusCwd},
		{id: "statusline.goal", region: _regionStatusLeft, priority: 11, eases: true, render: statusGoal},
		{id: "statusline.loop", region: _regionStatusLeft, priority: 12, eases: true, render: statusLoop},
		{id: "statusline.agents", region: _regionStatusLeft, priority: 25, eases: true, render: statusAgents},
		{id: "statusline.jobs", region: _regionStatusLeft, priority: 26, eases: true, render: statusJobs},
		{id: "statusline.keys", region: _regionStatusLeft, priority: 95, eases: true, render: statusKeys},
		{
			id: "statusline.scroll", region: _regionStatusLeft, priority: 100, eases: true, render: statusScroll,
			onClick: func(a *App, _ map[string]any) bool {
				a.applyScroll(hostapi.ScrollBottom, _regionNone)
				return true
			},
		},
		{id: "statusline.quit", region: _regionStatusLeft, priority: 110, eases: true, render: statusQuit},

		{id: "statusline.model", region: _regionStatusRight, priority: 8, eases: true, dip: _dipDuration, dipWhole: true, render: statusModel},
		{id: "statusline.effort", region: _regionStatusRight, priority: 9, eases: true, render: statusEffort},
		{id: "statusline.context", region: _regionStatusRight, priority: 40, rank: 42, eases: true, dip: _dipDuration, dipWhole: true, render: statusContext},
		{id: "statusline.cost", region: _regionStatusRight, priority: 42, rank: 40, eases: true, dip: _dipDuration, render: statusCost},
		{id: "statusline.hints", region: _regionStatusRight, priority: 130, eases: true, render: statusHints},
	}
}

// shortPath is fish's prompt_pwd: every directory but the last down to a
// cell, a leading dot kept.
func shortPath(p string) string {
	if p == "" {
		return p
	}
	parts := strings.Split(p, "/")
	for i := range len(parts) - 1 {
		if parts[i] == "" {
			continue
		}
		n := 1
		if parts[i][0] == '.' {
			n = 2
		}
		if len(parts[i]) > n {
			parts[i] = parts[i][:n]
		}
	}
	return strings.Join(parts, "/")
}

// statusCwd is the project, shortPath'd, the name alone when even that
// outgrows a quarter of the row, and whether it is untrusted.
func statusCwd(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	p := st.Project
	if p == "" {
		p = st.CWD
	}
	p = shortPath(p)
	if ansiWidth(p) > st.Width/4 {
		if i := strings.LastIndex(strings.TrimRight(p, "/"), "/"); i >= 0 {
			p = strings.TrimRight(p, "/")[i+1:]
		}
	}
	line := []hostapi.Segment{{Text: p, HL: hostapi.GroupDim}}
	if st.Branch != "" {
		line = append(line, hostapi.Segment{Text: ":" + st.Branch, HL: "Accent"})
	}
	if !st.Trusted {
		// an empty project leaves the pill alone, with no path to point at
		if p == "" {
			return [][]hostapi.Segment{pointAt("untrusted", "/trust", "")}, false
		}
		line = append(line[:1], hostapi.Segment{Text: "   ", HL: hostapi.GroupDim})
		line = append(line, pointAt("untrusted", "/trust", "")...)
	}
	return [][]hostapi.Segment{line}, false
}

// statusGoal is the session's goal while one is set; no count, so a check
// moves nothing.
func statusGoal(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if st.Goal == nil {
		return nil, false
	}
	// the pill is just a marker; /goal is the way to see or change it
	return [][]hostapi.Segment{pointAt("⚑ goal", "/goal", "goal")}, false
}

// statusLoop is the session's loop and when its next run fires, five cells
// whatever the time, so a reschedule moves nothing.
func statusLoop(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if st.Loop == nil {
		return nil, false
	}
	return [][]hostapi.Segment{pointAt("↻ "+clockText(st.Loop.Next), "/loop", "")}, false
}

// statusAgents is the subagents at work: the turn's own, background ones,
// workflows and Lua.
func statusAgents(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	flows, agents := st.WorkflowsRunning, st.AgentsRunning
	if flows == 0 && agents == 0 {
		return nil, false
	}
	n, word := flows, "workflow"
	if flows == 0 {
		n, word = agents, "agent"
	}
	line := pointAt("⧗ "+strconv.Itoa(n), plural(word, n), word+"s")
	if flows > 0 && agents > 0 {
		line = append(line, hostapi.Segment{Text: "   ", HL: hostapi.GroupDim})
		line = append(line, pointAt(strconv.Itoa(agents), plural("agent", agents), "agents")...)
	}
	return [][]hostapi.Segment{line}, false
}

// statusJobs is the background processes the model started: their own
// screen, apart from the agents one.
func statusJobs(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if st.JobsRunning == 0 {
		return nil, false
	}
	return [][]hostapi.Segment{pointAt("⚙ "+strconv.Itoa(st.JobsRunning), plural("job", st.JobsRunning), "jobs")}, false
}

// statusKeys is the keys of a bound sequence typed so far, waiting for the
// rest.
func statusKeys(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if st.KeySeq == "" {
		return nil, false
	}
	return [][]hostapi.Segment{{{Text: st.KeySeq + " …", HL: hostapi.GroupTitle}}}, false
}

// statusScroll is, while the chat is scrolled up, how many rows are below and
// the key that goes back.
func statusScroll(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if st.RowsBelow <= 0 {
		return nil, false
	}
	return [][]hostapi.Segment{pointAt("↓ "+strconv.Itoa(st.RowsBelow), "ctrl+b", "bottom")}, false
}

// statusQuit says what a second ctrl+c does while the first armed it.
func statusQuit(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if !st.QuitArmed {
		return nil, false
	}
	return [][]hostapi.Segment{pointAt("ctrl+c", "again to quit", "")}, false
}

// statusModel is the model running the conversation; a click opens the model
// picker.
func statusModel(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if st.Model == "" {
		return nil, false
	}
	return [][]hostapi.Segment{{{Text: st.Model, HL: hostapi.GroupDim, Click: "model"}}}, false
}

// statusEffort is the reasoning effort; a click opens its picker.
func statusEffort(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if st.Effort == "" {
		return nil, false
	}
	return [][]hostapi.Segment{{{Text: st.Effort, HL: hostapi.GroupDim, Click: "effort"}}}, false
}

// statusContext is how much of the window is in use, not the running session
// total: the total says nothing about how much room is left.
func statusContext(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	used, window := tokens(st.Context.Tokens), tokens(st.Context.Window)
	if window != "" {
		// a known window with no use yet reads as nothing, not as a total
		if used == "" {
			return nil, false
		}
		return [][]hostapi.Segment{{{Text: used + "/" + window + " ctx", HL: hostapi.GroupDim, Click: "usage"}}}, false
	}
	if total := tokens(st.Usage.Total); total != "" {
		return [][]hostapi.Segment{{{Text: total + " tok", HL: hostapi.GroupDim, Click: "usage"}}}, false
	}
	return nil, false
}

// statusCost is the session's tally, shown only when the conversation's own
// model declares prices.
func statusCost(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if !st.Usage.Priced {
		return nil, false
	}
	c := costText(st.Usage.Cost)
	if c == "" {
		return nil, false
	}
	return [][]hostapi.Segment{{{Text: c, HL: hostapi.GroupDim, Click: "usage"}}}, false
}

// statusHints is the two keys worth knowing, on an empty session only:
// innermost, so the first send takes them away without moving the model or
// the cost, and the first to go on a short row.
func statusHints(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	if st.TranscriptCount > 0 || st.AgentActive {
		return nil, false
	}
	return [][]hostapi.Segment{{
		{Text: "/", HL: "SplashKey", Click: "help"},
		{Text: " commands  ", HL: hostapi.GroupDim, Click: "help"},
		{Text: "@", HL: "SplashKey"},
		{Text: " attachments", HL: hostapi.GroupDim},
	}}, false
}

// plural is a noun with its count's number left off: "agents" for 2.
func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}
