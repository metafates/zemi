package ui

// The chat widget: the transcript, the live turn and the todo list. A turn's
// activity (thinking and tool calls) reads as a rail, which once settled
// folds the calls a family counts into one digest row, its rail a click away.

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

const (
	_railKeep    = 6      // a rail keeps its last entries, the earlier behind one row
	_railEdge    = 4      // the air a rail row keeps after the text it cuts
	_noticesKeep = 3      // notice rows of one burst kept in view
	_sinkRows    = 4      // a clipped block's first rows sink at its top
	_prose       = 80     // the widest a thought draws, in cells
	_hang        = "  ⎿ " // a detail row hangs under its head by one corner
	_indent      = "    " // every later row stands the same cells in
)

// runMode is how a run of activity rows draws.
type runMode int

const (
	_runNone   runMode = iota // not a run of activity: the row stands alone
	_runFold                  // its digest alone
	_runUnfold                // that digest over its whole rail
	_runRail                  // a rail of one row per step: the live turn, a view
)

// the window fill: a pass draws the view plus this slack at once, and the
// older rows it still owes another _fillBudget at a time (DESIGN §Rendering loop).
const (
	_fillSlack  = 60
	_fillBudget = 30 * time.Millisecond
)

// blockKind is the block a row belongs to: blocks stand one blank row apart,
// activity rows together.
type blockKind int

const (
	_blockNone blockKind = iota
	_blockUser
	_blockActivity
	_blockText
	_blockNote
	_blockStats
	_blockQueued
)

// entry is one row the chat draws: a settled transcript row or a live call,
// in one shape, since a rail draws the two alike.
type entry struct {
	kind    hostapi.RowKind
	role    fantasy.MessageRole
	tool    string
	text    string
	input   string
	label   string
	status  hostapi.CallStatus
	detail  string
	run     string
	job     string
	diff    string
	output  string
	live    bool          // a call of the turn running now
	elapsed time.Duration // a live call's run, a reply's turn
	thought time.Duration // a thought's, or the turn's on its reply
	out     int64         // a reply: the tokens it wrote
	model   string        // a reply: the model that wrote it
	ended   int64         // a reply: when the turn ended, unix seconds
}

// entryOf is a transcript row as the chat draws it.
func entryOf(m hostapi.ChatMessage) entry {
	e := entry{
		kind: m.Kind, role: m.Role, tool: string(m.Tool), text: m.Text, label: m.Label,
		status: m.Status, detail: m.Detail, run: m.Run, job: m.Job, diff: m.Diff, output: m.Output,
	}
	if m.Stat != nil {
		e.elapsed = time.Duration(m.Stat.ElapsedMS) * time.Millisecond
		e.thought = time.Duration(m.Stat.ThoughtMS) * time.Millisecond
		e.out, e.model, e.ended = m.Stat.Out, m.Stat.Model, m.Stat.Ended
	}
	return e
}

// entryOfCall is a live call as the chat draws it.
func entryOfCall(t hostapi.AgentTool) entry {
	s := t.Stream
	return entry{
		kind: hostapi.RowTool, tool: s.Name, input: s.Input, label: s.Label, status: s.Status,
		detail: s.Detail, run: s.Run, job: s.Job, diff: s.Diff, output: s.Output,
		live: true, elapsed: t.Elapsed,
	}
}

// name is the tool a row is about.
func (e entry) name() string {
	if e.tool == "" {
		return "?"
	}
	return e.tool
}

// inputText is the call's input as its row shows it: a settled row's summary,
// a live call's arguments.
func (e entry) inputText() string {
	if e.text != "" {
		return e.text
	}
	return e.input
}

// activityKind is activity for a transcript row as the session holds it.
func activityKind(m hostapi.ChatMessage) bool {
	return m.Kind == hostapi.RowReasoning || m.Kind == hostapi.RowTool
}

// activity says whether a row is thinking or a call: a run of them is one unit.
func activity(e entry) bool { return e.kind == hostapi.RowReasoning || e.kind == hostapi.RowTool }

// notice says whether a row is a notice: a burst of them draws as one block.
func notice(e entry) bool { return e.kind == hostapi.RowNotice }

// _markOf is how a finished row's status marks it. A call running or not
// started yet spins, unless the open prompt asks about it: that one waits on
// the user, so it neither spins nor counts.
var _markOf = map[hostapi.CallStatus]tools.MarkKind{
	hostapi.CallOK: tools.MarkOK, hostapi.CallErr: tools.MarkErr, hostapi.CallStopped: tools.MarkStop,
	hostapi.CallBG: tools.MarkBG, hostapi.CallWarn: tools.MarkWarn,
}

// markSeg is a row's mark: the spinner while it runs, else how it ended. A
// mark never hovers: a hovered header changes only its text.
func railMark(kind tools.MarkKind, click string) hostapi.Segment {
	m := hostapi.Marks[kind]
	seg := hostapi.Segment{Text: m.Glyph + " ", HL: m.HL, NoCopy: true, NoHover: true, Click: click}
	if kind == tools.MarkSpin {
		seg.Effect = hostapi.EffectSpin
	}
	return seg
}

// barSeg is the rail a turn's activity stands on: column 0, its rows' marks
// from column 2 and their text from column 4.
func barSeg(pad string) hostapi.Segment {
	return hostapi.Segment{Text: "│ " + pad, HL: "ThinkingBar", NoCopy: true, NoHover: true}
}

// airRow is a rail row with only the bar: air around prose on the rail.
func airRow() []hostapi.Segment {
	return []hostapi.Segment{{Text: "│", HL: "ThinkingBar", NoCopy: true, NoHover: true}}
}

// sink is how far a clipped block's row at k sinks toward the surface: its
// first rows, deepest first, easing off as the hidden rows come back.
func sink(k, past int) float64 {
	if k >= _sinkRows || past == 0 {
		return 0
	}
	return float64(_sinkRows-k) / (_sinkRows + 1) * min(1, float64(past)/_sinkRows)
}

// settling says whether the next flush settles a call beside the thought: the
// digest then names the calls and no digest row stands for the thought alone.
func settling(calls []entry) bool {
	for _, t := range calls {
		if t.status != hostapi.CallRun && t.status != hostapi.CallWait {
			return true
		}
	}
	return false
}

// foldLabel is the one row that stands for a folded run: a notice burst and a
// rail fold the same way, and opened it says what its click does.
func foldLabel(open bool, n int) string {
	if open {
		return "… collapse"
	}
	return "… " + strconv.Itoa(n) + " earlier"
}

// oneLine is text with its whitespace runs collapsed, as a row reads it.
func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

// relPath is text with every path under cwd written relative to it, and every
// other path under home from ~.
func relPath(s, cwd, home string) string {
	if home != "" && home != "/" {
		s = strings.ReplaceAll(s, home+"/", "~/")
	}
	if cwd == "" {
		return s
	}
	return strings.ReplaceAll(s, cwd+"/", "")
}

// _verb is what a tool call does, as its row and the working rule say it.
var _verb = map[string]string{
	"read": "Read", "write": "Write", "edit": "Edit", "glob": "Find", "grep": "Search", "shell": "Run",
	"job_start": "Start", "job_read": "Check", "job_list": "List jobs", "job_kill": "Stop",
	"todos": "Update", "question": "Ask", "plan": "Plan", "skill": "Load skill", "tool_search": "Find tools",
	"lua": "Run", "update": "Report",
}

// noun is a call family the chat counts: its verb, then its noun, one and
// many. A family with no noun is a verb alone and never counts.
type noun struct{ verb, one, many string }

// _countFamilies are the call families a digest counts. A call of any other
// name keeps a row of its own, and family adds the entry an MCP call counts
// under.
var _countFamilies = map[string]noun{
	"read":        {"read", "file", "files"},
	"glob":        {"searched for", "pattern", "patterns"},
	"grep":        {"searched for", "pattern", "patterns"},
	"shell":       {"ran", "command", "commands"},
	"edit":        {"edited", "edit", "edits"},
	"write":       {"wrote", "write", "writes"},
	"skill":       {"loaded", "skill", "skills"},
	"job_read":    {"checked", "job", "jobs"},
	"job_list":    {"listed jobs", "", ""},
	"job_kill":    {"stopped", "job", "jobs"},
	"todos":       {"updated todos", "", ""},
	"agent":       {"started", "agent run", "agent runs"},
	"workflow":    {"ran", "workflow", "workflows"},
	"lua":         {"ran", "script", "scripts"},
	"tool_search": {"looked for", "tool", "tools"},
}

// family is the family a call counts as, false for a call whose name has
// none. An MCP call counts under its server: its name is mcp_<server>_<tool>
// and a server name holds no "_", one entry per server however many of its
// tools ran.
func family(name string) (noun, bool) {
	if f, ok := _countFamilies[name]; ok {
		return f, true
	}
	rest, ok := strings.CutPrefix(name, "mcp_")
	if !ok {
		return noun{}, false
	}
	server, _, ok := strings.Cut(rest, "_")
	if !ok {
		return noun{}, false
	}
	return noun{verb: "called", one: server + " tool", many: server + " tools"}, true
}

// oneName is the name of a run of calls that holds exactly one, "?" for none:
// a digest asks what a lone call was without walking the list twice.
func oneName(calls []entry) string {
	if len(calls) != 1 {
		return ""
	}
	return calls[0].name()
}

// _waits are the calls the turn _waits on: their work is another agent loop, so
// nothing streams until they end and their row says what holds the turn.
var _waits = map[string]bool{"agent": true, "workflow": true}

// gateStopped is a call the permission gate stopped: denied or dismissed, the
// reader's own act. An interrupted call's stop the turn's own row tells.
func gateStopped(e entry) bool {
	return e.status == hostapi.CallStopped &&
		(strings.HasPrefix(e.output, "permission denied (user") || strings.HasPrefix(e.output, "The user dismissed"))
}

// stoppedWhy is a gate stop as the digest names it, the unfolded row's wording.
func stoppedWhy(output string) string {
	switch {
	case strings.HasPrefix(output, "permission denied (user"):
		return "denied by you"
	case strings.HasPrefix(output, "The user dismissed"):
		return "dismissed by you"
	default:
		return "stopped"
	}
}

// counted is whether a call counts: a family counts it and it went well.
func counted(e entry) bool {
	if e.kind != hostapi.RowTool || e.status != hostapi.CallOK {
		return false
	}
	_, ok := family(e.name())
	return ok
}

// waitOnly is a call the turn only waited on: its row names the waiting,
// which a digest would fold into a run that never started.
func waitOnly(e entry) bool {
	if !_waits[e.name()] {
		return false
	}
	if e.text == "all" {
		return true
	}
	id, ok := strings.CutPrefix(e.text, "wait ")
	return e.name() == "workflow" && ok && id != "" && !strings.Contains(id, " ")
}

// news are the tools whose diff is news rather than their work: a command's
// change is news, an edit's is its own.
func news(name string) bool { return name == "shell" }

// digests is whether a call folds into its turn's digest: it counts, and its
// diff is its own work or there is none. A background hand-off counts too, or
// a turn that left its work running folds to nothing and its rail hangs open.
func digests(e entry) bool {
	if e.status == hostapi.CallBG {
		_, ok := family(e.name())
		return ok && (e.diff == "" || !news(e.name()))
	}
	return counted(e) && !waitOnly(e) && (e.diff == "" || !news(e.name()))
}

// looks is whether a call only looked around and reads as its step's row: it
// counts and shows no diff of its own.
func looks(e entry) bool { return counted(e) && e.diff == "" }

// grouped is whether a call folds into its step's counted row: the same test
// steps groups by, so a fold can walk a rail's entries without drawing them.
func grouped(e entry) bool { return looks(e) && e.label == "" }

// nextStep is the entry after the step starting at i, for a fold that needs
// the entry count and the kept tail's first entry before any row is built.
func nextStep(list []entry, i, last int) int {
	t := list[i]
	if t.kind == hostapi.RowReasoning || !grouped(t) {
		return i + 1
	}
	g := 0
	for i+g <= last && grouped(list[i+g]) {
		g++
	}
	if g == 1 {
		return i + 1
	}
	return i + g
}

// counts is calls counted by noun, first seen first: "2 files, 1 command".
// With withVerb each part is led by what the calls did: "read 2 files".
// commands are the commands a shell family ran, listed after its part, which
// then reads last.
func counts(calls []entry, commands []string, withVerb bool) string {
	var order []string
	n := map[string]int{}
	fam := map[string]noun{}
	first := map[string]entry{}
	for _, t := range calls {
		k, ok := family(t.name())
		if !ok {
			continue
		}
		key := cmp.Or(k.one, k.verb)
		if _, seen := n[key]; !seen {
			order = append(order, key)
			fam[key], first[key] = k, t
		}
		n[key]++
	}
	var parts []string
	ran := ""
	shell := _countFamilies["shell"]
	for _, key := range order {
		k := fam[key]
		var part string
		if k.one == "" {
			part = k.verb
		} else {
			if withVerb {
				part = k.verb + " "
			}
			part += count(n[key], k.one, k.many)
		}
		if commands != nil && k == shell {
			ran = part + ": " + strings.Join(commands, ", ")
			continue
		}
		// a family of one names its call, as a lone rail row would: a count
		// over it would hide the thing it touched
		if n[key] == 1 && k.one != "" {
			t := first[key]
			name := t.label
			if name == "" && t.name() != "shell" {
				name = t.text
			}
			if name != "" {
				part += ": " + name
			}
		}
		parts = append(parts, part)
	}
	if ran != "" {
		parts = append(parts, ran)
	}
	return strings.Join(parts, ", ")
}

// title is a tool name as words for a row with no verb of its own:
// underscores split and an MCP tool's mcp_ dropped, so the row's lower takes
// back no capital ("mcp_tracker_list_issues" -> "tracker list issues").
func title(name string) string {
	return strings.ReplaceAll(strings.TrimPrefix(name, "mcp_"), "_", " ")
}

// headText is a call's headline as a verb and the rest: "Run", " the tests".
// A command's about is itself a verb phrase, so its first word is the verb; an
// unlabelled call reads as its input; a call the turn waits on says so in
// place of its verb.
func headText(name, label, input string, status hostapi.CallStatus) (string, string) {
	rest := ""
	switch {
	case label != "":
		rest = " " + lower(label)
	case input != "":
		rest = " " + input
	}
	waiting := status == hostapi.CallRun || status == hostapi.CallWait
	if _waits[name] && input == "all" {
		if waiting {
			return "waiting for", " all agents and workflows"
		}
		return "waited for", " all agents and workflows"
	}
	if id, ok := strings.CutPrefix(input, "wait "); name == "workflow" && ok && id != "" && !strings.Contains(id, " ") {
		if waiting {
			return "waiting on", " workflow " + id
		}
		return "waited on", " workflow " + id
	}
	if status == hostapi.CallRun && _waits[name] {
		return "waiting on " + name, rest
	}
	if name == "shell" && label != "" {
		word, after, _ := strings.Cut(label, " ")
		if after != "" {
			return word, " " + after
		}
		return word, ""
	}
	return cmp.Or(_verb[name], title(name)), rest
}

// thoughtText is how long a thought took, as a digest or a thought row says it.
func thoughtText(d time.Duration) string {
	if d >= 50*time.Millisecond {
		return "thought " + duration(d.Seconds(), false)
	}
	return "thought"
}

// diffstat is "+4 -1": the lines a diff adds and removes, in the colors a
// diff marks the same counts with. A count of zero is left out: the sign
// alone would report nothing, and the "+++"/"---" headers are no changes.
func diffstat(diff string) []hostapi.Segment {
	add, del := 0, 0
	for l := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++ "):
			add++
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--- "):
			del++
		}
	}
	var out []hostapi.Segment
	if add > 0 {
		out = append(out, hostapi.Segment{Text: "+" + strconv.Itoa(add), HL: "Ok"})
	}
	if del > 0 {
		lead := "-"
		if add > 0 {
			lead = " -"
		}
		out = append(out, hostapi.Segment{Text: lead + strconv.Itoa(del), HL: "Danger"})
	}
	return out
}

// digest is what rows first..last of a turn fold into, the chat's digest row:
// "1 command: run the tests", what it did then what the model said about the
// first call, or "6 files, 8 commands  thought 8s"; "" when it holds neither,
// so its rail stays.
func digest(rows []entry, first, last int, cwd, home string) string {
	var calls, fails, works []entry
	var secs time.Duration
	reasoned := false
	for i := first; i <= last; i++ {
		m := rows[i]
		switch {
		case m.kind == hostapi.RowReasoning:
			secs += m.thought
			reasoned = true
		case digests(m):
			calls = append(calls, m)
		case m.kind == hostapi.RowTool && (m.status == hostapi.CallErr || gateStopped(m)):
			// a failed call leaves no count, so the digest says it did: the
			// model's reply repeating the error is otherwise the only trace.
			// A call the gate stopped is the reader's own act, same story.
			fails = append(fails, m)
		case m.kind == hostapi.RowNotice:
			works = append(works, m)
		}
	}
	// the turn's thought, when the run drew no thought rows: logs written
	// before thoughts were kept have none, and read the turn's total from the
	// stat the reply after the run carries.
	// NOTE: a multi-run turn's whole total lands on its last run's digest.
	at := last + 1
	for at < len(rows) && rows[at].kind == hostapi.RowMessage && rows[at].role == fantasy.MessageRoleAssistant {
		at++
	}
	if at > last+1 && !reasoned {
		secs += rows[at-1].thought
	}
	var parts []string
	if len(calls) > 0 {
		// an unlabelled command names itself, as the rail's step row does: a
		// mixed run's digest saying "1 command" would hide which one ran
		var commands []string
		for _, m := range calls {
			if m.name() == "shell" && m.label == "" {
				commands = append(commands, oneLineText(relPath(m.text, cwd, home)))
			}
		}
		// counts names a family of one itself: a count over it would hide the
		// thing it touched, and the name reads relative, as a command's does
		named := make([]entry, len(calls))
		for i, m := range calls {
			if m.label == "" && m.text != "" && m.name() != "shell" {
				m.text = relPath(m.text, cwd, home)
			}
			named[i] = m
		}
		if len(commands) == 0 {
			commands = nil
		}
		parts = append(parts, counts(named, commands, false))
	}
	// a report the turn waited on exists nowhere else: its body rides the lone
	// agent run's fold row, the way a failed call's digest carries its
	// refusal. A run the turn itself waited on reports as its result, the body
	// sitting past the run id head that result carries.
	if f, ok := family(oneName(calls)); len(calls) == 1 && ok && f.one == "agent run" {
		body := ""
		if len(works) == 1 {
			if _, after, found := strings.Cut(works[0].text, "  "); found {
				body = after
			}
		} else {
			for line := range strings.SplitSeq(calls[0].output, "\n") {
				if line != "" && !strings.HasPrefix(line, "[run ") {
					body = line
					break
				}
			}
		}
		if body != "" {
			parts = append(parts, body)
		}
	}
	if len(fails) > 0 {
		switch {
		case len(fails) == 1 && len(calls) == 0:
			name := lower(cmp.Or(_verb[fails[0].name()], title(fails[0].name())))
			if fails[0].status == hostapi.CallStopped {
				parts = append(parts, name+" "+stoppedWhy(fails[0].output))
				break
			}
			// the row's output is the refusal; the input summary only says what
			// was attempted. The "error: " stamp is model-facing.
			why := fails[0].output
			if i := strings.IndexByte(why, '\n'); i >= 0 {
				why = why[:i]
			}
			why = strings.TrimPrefix(why, "error: ")
			if why == "" {
				why = fails[0].text
			}
			if why != "" {
				parts = append(parts, name+" failed: "+why)
			} else {
				parts = append(parts, name+" failed")
			}
		case len(fails) > 0:
			n := 0
			for _, f := range fails {
				if f.status == hostapi.CallErr {
					n++
				}
			}
			switch {
			case n == len(fails):
				parts = append(parts, count(len(fails), "failed call", "failed calls"))
			case n == 0:
				parts = append(parts, count(len(fails), "denied call", "denied calls"))
			default:
				parts = append(parts, count(n, "failed call", "failed calls")+", "+count(len(fails)-n, "denied call", "denied calls"))
			}
		}
	}
	// a run whose only content is a thought still names it: an empty digest
	// would leave its rail standing, a block no fold row closes
	if secs >= 50*time.Millisecond || len(parts) == 0 && reasoned {
		parts = append(parts, thoughtText(secs))
	}
	return strings.Join(parts, "  ")
}

// chatFolds is what a chat render keeps between frames: the folds a click
// opened, and the runs they fold. Renders run on a frame goroutine and clicks
// on the update one, so it carries its own lock. Keys are a row's fold
// identity, which a head trim does not renumber.
type chatFolds struct {
	mu       sync.Mutex
	steps    map[int64]bool  // a step's calls, opened under its counted row
	unfolded map[int64]bool  // a run drawn whole, under its digest
	whole    map[int64]bool  // a rail or a notice burst shown whole
	runs     map[int64]int64 // the run a digest folds: its key to its last row's
	ident    string          // the transcript these folds belong to
	calls    int             // the live calls the last render drew
	// md and hl memoize the costly rows by text and width: a marked-down reply
	// and a highlighted command cost far more than the rows around them, and
	// a frame rebuilds every settled row it is handed back.
	md map[string][][]hostapi.Segment
	hl map[string][]hostapi.Segment
	// settled is the transcript's rows as they were built, and the state they
	// were built from. fillUnit is the first unit the build still owes: 0
	// once the window is whole, len(units) before the first pass drew.
	// built is the rows per unit under that state, so the fill adds to it
	// instead of rebuilding what the head pass already drew.
	settled     [][]hostapi.Segment
	settledKey  settledKey
	settledPrev blockKind
	fillUnit    int
	built       map[int][][]hostapi.Segment
	ver         int // the folds' version: any change draws the rows again
}

// settledKey is what the transcript's rows are built from: the log they read
// and the screen they are drawn on.
type settledKey struct {
	epoch, cut, edit int64
	rows, older      int
	tail             int  // the run the turn is working in starts here
	replying         bool // its answer streams: the run it closes folds
	width            int
	folds            int
	view             string
}

// viewID names the agent run on screen, "" for the session's own chat.
func viewID(st *hostapi.ScreenState) string {
	if st.View == nil {
		return ""
	}
	return st.View.ID
}

// chatWidget is the chat region: the transcript, the live turn and the todos.
func chatWidget(h *hostapi.Host) *widget {
	f := &chatFolds{
		steps: map[int64]bool{}, unfolded: map[int64]bool{}, whole: map[int64]bool{}, runs: map[int64]int64{},
		md: map[string][][]hostapi.Segment{}, hl: map[string][]hostapi.Segment{},
	}
	return &widget{
		id: "chat", region: _regionChat, priority: 10, wrap: true,
		render:  func(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) { return f.render(h, st) },
		onClick: f.click,
	}
}

// key is the identity a fold names a transcript row by: its place in the log's
// tail, the window index plus the head cut, which a trim shifts without
// renumbering, so a fold a click opened stays open while its row is in view.
// The live turn's rows are no transcript rows and keep their own index.
func keyOf(cut int64, oneBased int) int64 {
	if oneBased > 0 {
		return cut + int64(oneBased)
	}
	return int64(oneBased)
}

// reset drops the folds that belong to another transcript: a reset or a splice
// renumbers under a new epoch, a shown run is an identity of its own, and a
// new turn restarts the live calls its folds named.
func (f *chatFolds) reset(st *hostapi.ScreenState) {
	view := ""
	if st.View != nil {
		view = st.View.ID
	}
	if ident := fmt.Sprintf("%d|%s", st.TranscriptEpoch, view); ident != f.ident {
		f.steps, f.unfolded, f.whole, f.runs = map[int64]bool{}, map[int64]bool{}, map[int64]bool{}, map[int64]int64{}
		f.ident, f.ver = ident, f.ver+1
	}
	if n := len(st.Agent.Tools); n < f.calls {
		for _, m := range []map[int64]bool{f.steps, f.unfolded, f.whole} {
			for k := range m {
				if k <= 0 {
					delete(m, k)
				}
			}
		}
		delete(f.runs, 0)
	}
	f.calls = len(st.Agent.Tools)
}

// toggle opens or closes the fold a click names.
func (f *chatFolds) toggle(kind string, key int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var set map[int64]bool
	switch kind {
	case "step":
		set = f.steps
	case "turn":
		set = f.unfolded
	case "more":
		set = f.whole
	default:
		return
	}
	if set[key] {
		delete(set, key)
	} else {
		set[key] = true
	}
	f.ver++
	// what was opened in a folded run has no row left to close it by
	if kind == "turn" && !set[key] {
		if last, ok := f.runs[key]; ok {
			for k := key; k <= last; k++ {
				delete(f.steps, k)
				delete(f.whole, k)
			}
		}
	}
}

// click runs the chat's own click ids and hands the rest back, so a command
// pill on a row still runs its command.
func (f *chatFolds) click(a *App, ev map[string]any) bool {
	id, _ := ev["id"].(string)
	switch {
	case strings.HasPrefix(id, "agent:"):
		a.runCommand("agents", "show "+strings.TrimPrefix(id, "agent:"), false)
	case strings.HasPrefix(id, "workflow:"):
		a.runCommand("workflows", "view "+strings.TrimPrefix(id, "workflow:"), false)
	case strings.HasPrefix(id, "call:"):
		a.runCommand("call", strings.TrimPrefix(id, "call:"), false)
	case strings.HasPrefix(id, "job:"):
		a.runCommand("jobs", "view "+strings.TrimPrefix(id, "job:"), false)
	default:
		kind, num, ok := strings.Cut(id, ":")
		if !ok {
			return false
		}
		key, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return false
		}
		f.toggle(kind, key)
	}
	return true
}

// chatBuilder is one render's rows, drawn at width.
type chatBuilder struct {
	lines [][]hostapi.Segment
	prev  blockKind // the block the last row belongs to
	width int
	st    *hostapi.ScreenState
	host  *hostapi.Host
	folds *chatFolds
}

func (b *chatBuilder) push(row []hostapi.Segment) { b.lines = append(b.lines, row) }

// gap stands one blank row between blocks, activity rows staying together.
func (b *chatBuilder) gap(block blockKind) {
	if b.prev != _blockNone && !(b.prev == _blockActivity && block == _blockActivity) {
		b.push([]hostapi.Segment{{Text: ""}})
	}
	b.prev = block
}

// frame bounds the rail and sets its entries apart; never two in a row, so a
// rail opening on one draws no second.
func (b *chatBuilder) frame() {
	if n := len(b.lines); n > 0 && len(b.lines[n-1]) == 1 && b.lines[n-1][0].Text == "│" {
		return
	}
	b.push(airRow())
}

// suffixed ends a row in tail flush with the right edge.
func (b *chatBuilder) suffixed(row, tail []hostapi.Segment, click string) []hostapi.Segment {
	if len(tail) == 0 {
		return row
	}
	row = append(row, hostapi.Segment{Text: strings.Repeat(" ", max(2, b.width-segsCells(row)-segsCells(tail)))})
	for _, sg := range tail {
		row = append(row, hostapi.Segment{Text: sg.Text, HL: sg.HL, Click: click})
	}
	return row
}

// markdown draws a reply: indented, not bulleted, its lines sharing the user's
// left edge.
func (b *chatBuilder) markdown(text string) {
	key := strconv.Itoa(b.width) + "\x00" + text
	rows, ok := b.folds.md[key]
	if !ok {
		for _, segs := range b.host.Markdown(text, b.width-2) {
			row := []hostapi.Segment{{Text: "  ", HL: "Normal", NoCopy: true}}
			rows = append(rows, append(row, segs...))
		}
		// NOTE: the memo holds one render's worth of replies; a chat past it
		// re-renders each reply once more
		if len(b.folds.md) > 2048 {
			b.folds.md = map[string][][]hostapi.Segment{}
		}
		b.folds.md[key] = rows
	}
	for _, row := range rows {
		// the memo's rows are shared read-only: a downstream paint builds its
		// own row arrays and mutates segments only as value copies
		b.push(row)
	}
}

// highlight is a shell command as the row reads it, memoized like markdown.
func (b *chatBuilder) highlight(text, lang string) []hostapi.Segment {
	key := strconv.Itoa(b.width) + "\x00" + lang + "\x00" + text
	if segs, ok := b.folds.hl[key]; ok {
		return segs
	}
	var segs []hostapi.Segment
	if rows := b.host.Highlight(text, lang, 0); len(rows) > 0 {
		segs = rows[0]
	}
	if len(b.folds.hl) > 4096 {
		b.folds.hl = map[string][]hostapi.Segment{}
	}
	b.folds.hl[key] = segs
	return segs
}

// user draws the user's own words: the bar on every row, in the accent for a
// message that went and dim for one still waiting its turn. A sent message's
// rows are sticky: the chat pins its first row while the turn's rows fill the
// view.
func (b *chatBuilder) user(text string, waiting bool) {
	barHL, textHL, pin := "UserMsg", "Normal", hostapi.PinTurn
	if waiting {
		barHL, textHL, pin = "Queued", "Dim", hostapi.PinNone
	}
	wrapped := hostapi.WrapText(text, b.width-2)
	// a user row past the composer's own paste threshold folds: the model read
	// every line, the chat shows the head and names the way to the rest
	draw := len(wrapped)
	if draw > 10 {
		draw = 9
	}
	for i := range draw {
		row := []hostapi.Segment{{Text: "▌ ", HL: barHL, NoCopy: true, NoHover: true, Pin: pin, Fill: hostapi.FillPanel}}
		for _, sg := range wrapped[i] {
			sg.HL = textHL
			sg.Fill = hostapi.FillPanel
			row = append(row, sg)
		}
		b.push(row)
	}
	if len(wrapped) > draw {
		b.push([]hostapi.Segment{
			{Text: "▌ ", HL: barHL, NoCopy: true, NoHover: true, Pin: pin, Fill: hostapi.FillPanel},
			{Text: "… +" + strconv.Itoa(len(wrapped)-draw) + " lines", HL: "Dim", NoCopy: true, Fill: hostapi.FillPanel},
		})
	}
}

// reasoning draws thinking text on the rail at column 4, prose wide at most,
// its last tail lines when tail is set and sunk at the top while lines are
// hidden above; a blank line keeps only the bar. Its rows are body: no hover,
// but a click acts as the row above them.
func (b *chatBuilder) reasoning(text string, tail int, click string) {
	text = strings.TrimRight(text, " \t\n\v\f\r")
	body := hostapi.WrapText(text, max(min(b.width-4, _prose), 20))
	from := 0
	if tail > 0 {
		from = max(0, len(body)-tail)
	}
	for n := from; n < len(body); n++ {
		row := []hostapi.Segment{barSeg("  ")}
		blank := true
		fade := sink(n-from, from)
		for _, sg := range body[n] {
			sg.HL, sg.Click, sg.NoHover, sg.Fade = "AgentReason", click, true, fade
			row = append(row, sg)
			blank = blank && sg.Text == ""
		}
		if blank {
			b.push(airRow())
		} else {
			b.push(row)
		}
	}
}

// thoughtRow is "│ ✻ thought 8s"; the caller draws the thought's text itself
// when it unrolls, so this is the header row alone.
func (b *chatBuilder) thoughtRow(d time.Duration, click string) {
	b.push([]hostapi.Segment{barSeg(""), railMark(tools.MarkThought, click), {Text: thoughtText(d), HL: "Dim", Click: click}})
}

// thinking draws the model thinking now: drawn outside the rail's own loop, so
// it frames itself as an entry does. The label breathes while the row lives; a
// settled thought is a thought row, static.
func (b *chatBuilder) thinking(tail string) {
	b.frame()
	b.push([]hostapi.Segment{barSeg(""), railMark(tools.MarkSpin, ""), {Text: "thinking", HL: "Dim", Effect: hostapi.EffectShimmer}})
	if tail != "" {
		b.reasoning(tail, 4, "")
	}
	b.frame()
}

// group draws a step's unlabelled counted calls as one rail row saying what
// they did, marked as they are: every call it counts went well.
func (b *chatBuilder) group(group []entry, click string) {
	b.push([]hostapi.Segment{
		barSeg(""),
		railMark(tools.MarkOK, click),
		{Text: clipText(b.said(group), b.width-4-_railEdge), HL: "Normal", Click: click},
	})
}

// said is what counted calls did: "read 2 files, ran 1 command: go vet".
func (b *chatBuilder) said(group []entry) string {
	var commands []string
	for _, t := range group {
		if t.name() == "shell" {
			commands = append(commands, oneLineText(relPath(t.inputText(), b.st.CWD, b.st.Home)))
		}
	}
	return counts(group, commands, true)
}

// toolOpts are what a call's row takes beyond the call itself.
type toolOpts struct {
	nest  string // the two cells a row hangs by, its branch mark
	terse bool   // a superseded call of the live rail reads as its header
}

// toolRow draws one call: the verb and the label or input, what it changed or
// how long it runs at the right, and under it what it said, unless terse folds
// it into that header alone.
func (b *chatBuilder) toolRow(t entry, oneBased int, o toolOpts) {
	name := t.name()
	// NOTE: every running row reads as asked about while a prompt is open, a
	// parallel call that runs on meanwhile included.
	asked := t.status == hostapi.CallRun && b.st.Waiting != ""
	pad := o.nest
	// a call nested under its step's row hangs by its branch mark, and the
	// rows under it keep a line in that mark's place: a bar while more
	// siblings follow, a gap under the last one, so the tree stays joined
	below := ""
	switch {
	case pad == "└ ":
		below = "  "
	case pad != "":
		below = "│ "
	}
	// a running row reads Dim beside its spinner until it ends
	_, known := _markOf[t.status]
	dim := !asked && !known
	inner := b.width - 2
	if pad != "" {
		inner -= 2
	}
	input := oneLineText(relPath(t.inputText(), b.st.CWD, b.st.Home))
	label := t.label
	// the row's right-aligned tail: what it changed, then how long it ran
	var tail []hostapi.Segment
	if t.diff != "" {
		tail = diffstat(t.diff)
	}
	add := func(text string) {
		if len(tail) > 0 {
			tail = append(tail, hostapi.Segment{Text: "  ", HL: "Dim"})
		}
		tail = append(tail, hostapi.Segment{Text: text, HL: "Dim"})
	}
	if t.status == hostapi.CallRun && !asked && t.elapsed >= time.Second {
		add(duration(t.elapsed.Seconds(), true))
	}
	sw := segsCells(tail)
	verbText, rest := headText(name, label, input, t.status)
	verbText = lower(verbText)
	room := inner - 2 - ansiWidth(verbText)
	if sw > 0 {
		room -= sw + 2
	} else {
		room -= 1 + _railEdge
	}
	// a command is highlighted where it is what the row reads, the head; under
	// its label it stays a Dim detail
	var code []hostapi.Segment
	if name == "shell" && input != "" && label == "" && !dim {
		if segs := b.highlight(clipText(input, room-1), "bash"); len(segs) > 0 {
			code = segs
		}
	}
	// a click opens the row's own view; an agent's row opens its transcript and
	// a workflow's its view
	click := "call:" + strconv.Itoa(oneBased)
	if t.run != "" {
		if name == "workflow" {
			click = "workflow:" + t.run
		} else {
			click = "agent:" + t.run
		}
	}
	// a call that only looked around reads plainly, as its step's row does
	vhl := "AgentToolName"
	if dim {
		vhl = "Dim"
	} else if looks(t) {
		vhl = "Normal"
	}
	kind := _markOf[t.status]
	if asked {
		kind = tools.MarkWait
	} else if !known {
		kind = tools.MarkSpin
	}
	head := []hostapi.Segment{railMark(kind, ""), {Text: verbText, HL: vhl}}
	staleHL := "Normal"
	if dim {
		staleHL = "Dim"
	}
	switch {
	case code != nil && label == "":
		head = append(head, hostapi.Segment{Text: " ", HL: "Normal"})
		head = append(head, code...)
	case rest != "":
		head = append(head, hostapi.Segment{Text: clipText(rest, room), HL: staleHL})
	}
	for i := range head {
		head[i].Click = click
	}
	// the bar opens every row and the mark follows it; a nested row hangs by
	// its branch mark where its own would stand, its step's state for it
	if pad != "" {
		head[0] = barSeg(pad)
	} else {
		head = append([]hostapi.Segment{barSeg("")}, head...)
	}
	first := len(b.lines)
	b.push(b.suffixed(head, tail, click))
	hangLead, indentLead := _hang, _indent
	if pad != "" {
		hangLead, indentLead = "⎿ ", "  "
	}
	corner := true
	detail := func(segs []hostapi.Segment) {
		lead := indentLead
		if corner {
			lead = hangLead
		}
		row := []hostapi.Segment{{Text: lead, HL: "Dim", NoCopy: true}}
		for _, sg := range segs {
			row = append(row, hostapi.Segment{Text: sg.Text, HL: sg.HL, Click: click, NoHover: true})
		}
		b.push(row)
		corner = false
	}
	detailText := func(text, hl string) {
		detail([]hostapi.Segment{{Text: text, HL: hl}})
	}
	output := t.output
	// an answered question's output holds "question\tanswer\tnote" lines, which
	// take the place of the question under its header
	answered := name == "question" && strings.Contains(output, "\t")
	// the command a description stands for, then progress, then the job a
	// timed-out command went on as, which the row carries
	if !o.terse {
		if !answered && label != "" && input != "" && !looks(t) {
			// a labelled counted call on the rail shows its command
			if code != nil {
				detail(code)
			} else {
				detailText(clipText(input, inner-2-_railEdge), "Dim")
			}
		}
		// a zoom or date row previews the line it opened: the first line only,
		// so a split zoom's second child stays behind the click
		if (name == "zoom" || name == "date") && t.status == hostapi.CallOK && output != "" {
			line, _, _ := strings.Cut(output, "\n")
			if i := strings.IndexByte(line, '|'); i > 0 && zoomHead(line[:i]) {
				line = line[i+1:]
			}
			if line = clipText(oneLineText(line), inner-2-_railEdge); line != "" {
				detailText(line, "Dim")
			}
		}
		if t.detail != "" {
			detailText(clipText(oneLineText(t.detail), inner-2-_railEdge), "Dim")
		}
		// the job a timed-out command went on as opens its own view; the
		// wording holds after the job exits and after a restart
		if t.job != "" {
			row := []hostapi.Segment{{Text: hangLead, HL: "Dim", NoCopy: true}}
			if !corner {
				row[0].Text = indentLead
			}
			row = append(row, hostapi.Segment{Text: "backgrounded as " + t.job, HL: "Dim", Click: "job:" + t.job, NoHover: true})
			b.push(row)
			corner = false
		}
	}
	// the result's last lines: always for a failure, and for what the user
	// answered. A running call keeps its line to the row; its view says the
	// rest.
	output = strings.TrimPrefix(output, "permission denied (user")
	if strings.HasPrefix(t.output, "permission denied (user") {
		output = "denied by you"
	}
	if strings.HasPrefix(t.output, "The user dismissed") {
		output = "dismissed by you"
	}
	if output != "" && (t.status == hostapi.CallErr || t.status == hostapi.CallStopped || name == "question" || name == "plan") {
		out := strings.Split(output, "\n")
		// a failure shows three lines where it says what went wrong: a tool's
		// first, a command's last (its stderr and exit status); a click shows
		// the rest
		failed, fromTail := t.status == hostapi.CallErr, name == "shell"
		from, to := 0, len(out)
		if failed && len(out) > 3 {
			if fromTail {
				from = len(out) - 3
			} else {
				to = 3
			}
		}
		// the host's own cut stands for the lines it dropped
		// NOTE: the marker's wording restates hostapi.MoreLines, so a Go change
		// stops the count and a cut reads as one line
		hidden := 0
		for i, l := range out {
			if i >= from && i < to {
				continue
			}
			n := 1
			num, marked := strings.CutPrefix(l, "… ")
			if marked && (strings.HasSuffix(num, " more line") || strings.HasSuffix(num, " more lines")) {
				if v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(num, "s"), " more line")); err == nil {
					n = v
				}
			}
			hidden += n
		}
		cut := "… " + count(hidden, "more line", "more lines")
		if hidden > 0 && from > 0 {
			detailText(cut, "Dim")
		}
		for i := from; i < to; i++ {
			if answered {
				if q, rest, ok := strings.Cut(out[i], "\t"); ok {
					if a, note, ok := strings.Cut(rest, "\t"); ok {
						if ansiWidth(q)+2+ansiWidth(a) <= inner-4 {
							detail([]hostapi.Segment{{Text: q, HL: "Dim"}, {Text: "  " + a, HL: "Normal"}})
						} else {
							detailText(q, "Dim")
							detailText(a, "Normal")
						}
						if note != "" {
							detailText("note: "+note, "Dim")
						}
						continue
					}
				}
			}
			hl := "Dim"
			if failed && (fromTail && i == len(out)-1 || !fromTail && i == 0) {
				hl = "Danger"
			}
			detailText(out[i], hl)
		}
		if hidden > 0 && from == 0 {
			detailText(cut, "Dim")
		}
	}
	for n := first + 1; n < len(b.lines); n++ {
		b.lines[n] = append([]hostapi.Segment{barSeg(below)}, b.lines[n]...)
	}
}

// zoomHead says whether head is a zoom row's line address, "16032+4".
func zoomHead(head string) bool {
	id, rest, ok := strings.Cut(head, "+")
	if !ok {
		return false
	}
	if _, err := strconv.Atoi(id); err != nil {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
}

// railOpts is how a rail draws.
type railOpts struct {
	live   bool        // the turn running now
	run    int         // the run's own first index, 1-based; 0 for a live rail
	unroll bool        // draw a thought as its text
	head   hostapi.Pin // the rail's first row's part in the header stack
}

// foldRun draws a folded run: its digest alone, a click away from its whole
// rail; a digest of nothing folds nothing, and the rail is drawn alone.
func (b *chatBuilder) foldRun(list []entry, first, last int, opened bool) {
	key := keyOf(b.st.TranscriptCut, first+1)
	b.folds.runs[key] = keyOf(b.st.TranscriptCut, last+1)
	text := digest(list, first, last, b.st.CWD, b.st.Home)
	if text == "" {
		// no digest row to pin: the rail's own head takes the section, so the
		// prompt's pin never draws over the rail's rows
		at := len(b.lines)
		b.rail(list, first, last, railOpts{head: hostapi.PinSection})
		if len(b.lines) > at {
			b.lines[len(b.lines)-1][0].Pin = hostapi.PinEnd
		}
		return
	}
	pin := hostapi.PinNone
	if opened {
		pin = hostapi.PinSection
	}
	// a collapsed run of exactly one thought opens its view at once: a rail of
	// one row expanded draws what the view shows. Opened, the digest's click
	// closes it again.
	one := first == last && list[first].kind == hostapi.RowReasoning
	click := "turn:" + strconv.FormatInt(key, 10)
	if one && !opened {
		click = "call:" + strconv.Itoa(first+1)
	}
	b.digestRow(text, click, pin, opened)
	if !opened {
		return
	}
	at := len(b.lines)
	// a lone thought opens as its text: the digest above it already says how
	// long it thought
	b.rail(list, first, last, railOpts{unroll: one})
	if len(b.lines) > at {
		b.lines[len(b.lines)-1][0].Pin = hostapi.PinEnd
	}
}

// digestRow draws the digest row a run folds to, pinned as head says. The
// glyph says what the click does: ▸ opens the rail, ▾ folds it back.
func (b *chatBuilder) digestRow(text, click string, pin hostapi.Pin, open bool) {
	glyph := "▸ "
	if open {
		glyph = "▾ "
	}
	b.push([]hostapi.Segment{
		{Text: glyph, HL: "Dim", NoCopy: true, NoHover: true, Click: click, Pin: pin},
		{Text: clipText(text, max(b.width-_railEdge-2, 6)), HL: "Dim", Click: click, Pin: pin},
	})
}

// noteRow is a notice row: the status mark it came in with, what it says, and
// any detail in brackets after it.
func (b *chatBuilder) noteRow(status hostapi.CallStatus, text, detail, click string) []hostapi.Segment {
	kind, ok := _markOf[status]
	if !ok {
		kind = tools.MarkNote
	}
	row := []hostapi.Segment{railMark(kind, click), {Text: text, HL: "Dim", Click: click}}
	if detail != "" {
		row = append(row, hostapi.Segment{Text: " (" + detail + ")", HL: "Dim", NoCopy: true, Click: click})
	}
	return row
}

// notices draws a burst of notice rows as one block, no blank row between
// them: the older ones of a long burst sit behind one row a click opens.
func (b *chatBuilder) notices(list []entry, first, last int) {
	b.gap(_blockNote)
	hidden := last - first + 1 - _noticesKeep
	if hidden > 1 {
		key := keyOf(b.st.TranscriptCut, first+1)
		open := b.folds.whole[key]
		b.push([]hostapi.Segment{
			{Text: "  ", NoCopy: true, NoHover: true},
			{Text: foldLabel(open, hidden), HL: "Dim", Click: "more:" + strconv.FormatInt(key, 10)},
		})
		if !open {
			first = last - _noticesKeep + 1
		}
	}
	for i := first; i <= last; i++ {
		m := list[i]
		// a work row names the run it reports for; a workflow's report opens
		// the workflow's view, an agent's its transcript
		click := ""
		if m.run != "" {
			if m.tool == "workflow" || strings.HasPrefix(m.tool, "workflow:") {
				click = "workflow:" + m.run
			} else {
				click = "agent:" + m.run
			}
		}
		b.push(b.noteRow(m.status, m.text, m.detail, click))
	}
}

// rail draws a run of activity rows, one row per entry: a step's unlabelled
// counted calls, two or more, as one row whose click opens one row per call, a
// lone one as its own row, a thought a row of its own.
// NOTE: a step is the calls between two thoughts, so a model that does not
// think runs its steps together into one row.
func (b *chatBuilder) rail(list []entry, first, last int, o railOpts) {
	b.frame()
	at := len(b.lines)
	// a long rail keeps its last entries in view, each with the rows under it,
	// the earlier ones behind one row that a click opens or closes
	var key int64
	switch {
	case o.live:
		key = 0
	case o.run != 0:
		key = keyOf(b.st.TranscriptCut, o.run)
	default:
		key = keyOf(b.st.TranscriptCut, first+1)
	}
	n := 0
	for i := first; i <= last; {
		i = nextStep(list, i, last)
		n++
	}
	n -= _railKeep
	from := first
	if n >= 2 && !b.folds.whole[key] {
		for range n {
			from = nextStep(list, from, last)
		}
	}
	// the fold row bounds the rail's head where its bar would stand, so the
	// kept tail draws no opening bar of its own; steps' own leading frame
	// collapses into the air row under it
	if n >= 2 {
		b.push([]hostapi.Segment{barSeg(""), {Text: foldLabel(b.folds.whole[key], n), HL: "Dim", Click: "more:" + strconv.FormatInt(key, 10)}})
		b.push(airRow())
	}
	b.steps(list, from, last, o, from > first)
	if o.head != hostapi.PinNone && len(b.lines) > at {
		b.lines[at][0].Pin = o.head
	}
	b.frame()
}

// markCall stamps every row from at on with the call or thought it draws, so a
// list naming one call bands those rows alone.
func (b *chatBuilder) markCall(at, oneBased int) {
	for k := at; k < len(b.lines); k++ {
		if len(b.lines[k]) > 0 {
			b.lines[k][0].Call = oneBased
		}
	}
}

// steps draws one row per rail entry from first to last.
func (b *chatBuilder) steps(list []entry, first, last int, o railOpts, headed bool) {
	i := first
	prev := ""
	prevFail, prevSet := false, false
	for i <= last {
		t := list[i]
		oneBased := i + 1
		if o.live {
			oneBased = -i - 1
		}
		// a counted call with no sibling is its own row: a "read 1 file" row
		// over it would hide which file
		lone := grouped(t) && (i+1 > last || !grouped(list[i+1]))
		kind := t.name()
		if t.kind == hostapi.RowReasoning {
			kind = "thought"
		}
		fail := t.status == hostapi.CallErr || t.status == hostapi.CallStopped
		// the rail opens on a bar and closes on one; between entries one stands
		// where their kinds differ, and around one that failed or was stopped
		if prevSet && (prev != kind || prevFail || fail) || !prevSet && !headed {
			b.frame()
		}
		prev, prevFail, prevSet = kind, fail, true
		switch {
		case t.kind == hostapi.RowReasoning:
			at := len(b.lines)
			click := "call:" + strconv.Itoa(oneBased)
			if o.unroll && t.text != "" {
				b.reasoning(t.text, 0, click)
			} else {
				b.thoughtRow(t.thought, click)
			}
			b.markCall(at, oneBased)
			i++
		case grouped(t) && !lone:
			var group []entry
			for i+len(group) <= last && grouped(list[i+len(group)]) {
				group = append(group, list[i+len(group)])
			}
			// the step's row heads its calls, nested two cells in, and its click
			// closes them again
			stepKey := keyOf(b.st.TranscriptCut, oneBased)
			b.group(group, "step:"+strconv.FormatInt(stepKey, 10))
			if b.folds.steps[stepKey] {
				for k, g := range group {
					at := len(b.lines)
					nest := "├ "
					if k == len(group)-1 {
						nest = "└ "
					}
					n := oneBased + sign(oneBased)*k
					b.toolRow(g, n, toolOpts{nest: nest})
					b.markCall(at, n)
				}
			}
			i += len(group)
		default:
			at := len(b.lines)
			// a live rail folds a call the turn has superseded into its header:
			// the newest entry, a running one, a failure and an ask keep the
			// rows under theirs
			keep := i == last || t.status == hostapi.CallRun || fail || kind == "question" || kind == "plan"
			b.toolRow(t, oneBased, toolOpts{terse: o.live && !keep})
			b.markCall(at, oneBased)
			i++
		}
	}
}

// sign is 1 for a settled index and -1 for a live one, so a group's members
// step away from the row it heads.
func sign(oneBased int) int {
	if oneBased < 0 {
		return -1
	}
	return 1
}

// anchor stamps the rows from..to of this render with the transcript row they
// draw, so a following list scrolls the chat here and bands the whole message.
func (b *chatBuilder) anchor(oneBased, from, to int) {
	for i := from; i < to && i < len(b.lines); i++ {
		if len(b.lines[i]) == 0 || segsCells(b.lines[i]) == 0 {
			continue
		}
		if b.lines[i][0].Anchor != oneBased {
			// the memo's rows are shared across the replies whose text matches:
			// stamping one in place would move every copy's anchor
			b.lines[i] = slices.Clone(b.lines[i])
			b.lines[i][0].Anchor = oneBased
		}
	}
}

// unit draws the transcript rows first..last, one message or one run of
// activity rows, in mode. It returns where the message's own rows end, the
// rows after it being meta (a reply's readout).
func (b *chatBuilder) unit(tr []entry, first, last int, mode runMode) {
	m := tr[first]
	start := len(b.lines)
	content := -1 // where the message ends, -1 for all of it
	switch {
	case mode == _runFold || mode == _runUnfold:
		b.gap(_blockActivity)
		b.foldRun(tr, first, last, mode == _runUnfold)
	case mode == _runRail:
		b.gap(_blockActivity)
		b.rail(tr, first, last, railOpts{run: first + 1})
	case m.kind == hostapi.RowShell:
		// a shell command: the command, then its output, already cut to a head
		// and a tail by the host; a failure's mark takes the prompt's place
		b.gap(_blockUser)
		head := []hostapi.Segment{{Text: "$ ", HL: "PromptShell", NoCopy: true}}
		if m.status == hostapi.CallErr {
			head[0] = railMark(tools.MarkErr, "")
		}
		b.push(append(head, hostapi.Segment{Text: m.text, HL: "Normal"}))
		for ol := range strings.SplitSeq(m.detail, "\n") {
			b.push([]hostapi.Segment{{Text: "  ", NoCopy: true}, {Text: ol, HL: "Dim"}})
		}
	case notice(m):
		b.notices(tr, first, last)
	case m.kind == hostapi.RowError:
		// the headline, then the raw error as its detail
		b.gap(_blockNote)
		cancelled := m.status == hostapi.CallCancelled
		n := 0
		for text := range strings.SplitSeq(m.text, "\n") {
			switch {
			case n == 0 && cancelled:
				b.push([]hostapi.Segment{railMark(tools.MarkStop, ""), {Text: text, HL: "Dim"}})
			case n == 0:
				b.push([]hostapi.Segment{railMark(tools.MarkErr, ""), {Text: text, HL: "Danger"}})
			case n == 1:
				b.push([]hostapi.Segment{{Text: _hang, HL: "Dim", NoCopy: true}, {Text: text, HL: "Dim"}})
			default:
				b.push([]hostapi.Segment{{Text: _indent, HL: "Dim", NoCopy: true}, {Text: text, HL: "Dim"}})
			}
			n++
		}
	case m.role == fantasy.MessageRoleUser:
		// plain text: what the user typed is not markdown
		b.gap(_blockUser)
		b.user(m.text, false)
	default:
		b.gap(_blockText)
		b.markdown(m.text)
		content = b.readout(tr, last, m)
	}
	to := len(b.lines)
	if content >= 0 {
		to = content
	}
	b.anchor(first+1, start, to)
}

// readout draws a reply's turn stats when it ends the exchange, and returns
// where its rows end. Only the next text row decides: a call, notice or error
// after it changes nothing, while another reply means a sentence between runs.
func (b *chatBuilder) readout(tr []entry, last int, m entry) int {
	final := true
	for i := last + 1; i < len(tr); i++ {
		if tr[i].kind == hostapi.RowMessage {
			final = tr[i].role == fantasy.MessageRoleUser
			break
		}
	}
	content := len(b.lines)
	if !final {
		return content
	}
	var stats []hostapi.Segment
	stat := func(text string) {
		if len(stats) > 0 {
			stats = append(stats, hostapi.Segment{Text: "  ", HL: "Dim"})
		}
		stats = append(stats, hostapi.Segment{Text: text, HL: "Dim"})
	}
	if m.model != "" {
		stat(m.model)
	}
	if m.elapsed > 0 {
		stat(duration(m.elapsed.Seconds(), false))
	}
	if out := tokens(m.out); out != "" {
		stat(out + " out")
	}
	// the model leads and sheds first: a narrow row keeps what the turn cost
	// over the name that wrote it
	if len(stats) > 1 && m.model != "" && 2+segsCells(stats) > b.width {
		stats = stats[2:]
	}
	if len(stats) == 0 {
		return content
	}
	b.gap(_blockStats)
	b.push(append([]hostapi.Segment{{Text: "  ", NoCopy: true}}, stats...))
	return content
}

// render is the chat's frame: the settled transcript, the turn running now,
// what the session still waits on, and the todo list.
func (f *chatFolds) render(h *hostapi.Host, st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	// 0 until this render says where its pending row sits: the working rule
	// reads the count with the viewport's rows below
	setRowsAfterPending(0)
	// the splash owns the empty chat: it draws the pending row too, in a block
	// that fits, so nothing here can scroll it off the top
	if emptyChat(st) {
		return nil, false
	}
	b := &chatBuilder{width: cmp.Or(st.Width, 80), st: st, host: h, folds: f}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset(st)
	// work that outlives this view of the session sits at the tail: a
	// session's counts are about other work, so a run's view leaves it out
	var pending []hostapi.Segment
	if st.View == nil {
		pending = pendingLine(st)
	}
	// A reply closes the run it follows at once, streaming or settled: the run
	// the turn is working in is its last activity run, while that reaches the
	// tail and the turn is not answering. The row that run starts at is also
	// the one the picker names it by.
	tailFirst := 0
	if st.AgentActive && len(st.Transcript) > 0 && activityKind(st.Transcript[len(st.Transcript)-1]) {
		tailFirst = len(st.Transcript)
		for tailFirst > 1 && activityKind(st.Transcript[tailFirst-2]) {
			tailFirst--
		}
	}
	liveFirst := 0
	if st.Agent.Text == "" {
		liveFirst = tailFirst
	}
	// The settled transcript draws the same rows until it, the width, the rail
	// view or a fold changes: a frame hands back the rows it built last time,
	// so neither the reader's wrap cache nor the app rebuilds them.
	key := settledKey{
		epoch: st.TranscriptEpoch, cut: st.TranscriptCut, edit: st.TranscriptEdit,
		rows: len(st.Transcript), older: st.LogRows(), width: b.width,
		folds: f.ver, view: viewID(st), tail: tailFirst, replying: st.Agent.Text == "",
	}
	if key == f.settledKey && f.fillUnit == 0 {
		// appending may write into the spare capacity, which the settled
		// slice's own rows never reach
		b.lines, b.prev = f.settled, f.settledPrev
	} else {
		tr := make([]entry, len(st.Transcript))
		for i, m := range st.Transcript {
			tr[i] = entryOf(m)
		}
		modeOf := func(first int) runMode {
			if !activity(tr[first]) {
				return _runNone
			}
			// the live turn's own run reads whole; every other run folds, the one
			// an agent view shows included
			if first+1 == liveFirst {
				return _runRail
			}
			if f.unfolded[keyOf(st.TranscriptCut, first+1)] {
				return _runUnfold
			}
			return _runFold
		}
		// The window fills newest first, 30 ms at a time but never less than a
		// full view, topped with the prompt it pins even when that prompt sits
		// hundreds of rows further back (DESIGN §Rendering loop). A unit's rows
		// carry the leading gap its real predecessor asks for, so any run of
		// them concatenates in order, half built included.
		type unitSpan struct{ first, last int }
		var units []unitSpan
		for i := 0; i < len(tr); {
			first, last := i, i
			for last+1 < len(tr) && (activity(tr[first]) && activity(tr[last+1]) || notice(tr[first]) && notice(tr[last+1])) {
				last++
			}
			units = append(units, unitSpan{first, last})
			i = last + 1
		}
		blockOf := func(k int) blockKind {
			m, first := modeOf(units[k].first), units[k].first
			switch {
			case m == _runFold || m == _runUnfold || m == _runRail:
				return _blockActivity
			case tr[first].kind == hostapi.RowShell:
				return _blockUser
			case notice(tr[first]), tr[first].kind == hostapi.RowError:
				return _blockNote
			case tr[first].role == fantasy.MessageRoleUser:
				return _blockUser
			default:
				return _blockText
			}
		}
		unitLines := func(k int) [][]hostapi.Segment {
			ub := &chatBuilder{width: b.width, st: b.st, host: b.host, folds: f}
			if k > 0 {
				ub.prev = blockOf(k - 1)
			}
			ub.unit(tr, units[k].first, units[k].last, modeOf(units[k].first))
			return ub.lines
		}
		fresh := key != f.settledKey
		if fresh {
			f.fillUnit = len(units) // a new state rebuilds from the tail
			f.built = map[int][][]hostapi.Segment{}
		}
		// The pass builds the tail-most units the window still owes: on a
		// first build down to the view plus slack, later ones 30 ms at a time.
		// fillUnit is the first unit still owed; 0 once the window is whole.
		newX, covered, t0 := f.fillUnit, 0, time.Now()
		for newX > 0 {
			k := newX - 1
			if _, ok := f.built[k]; !ok {
				f.built[k] = unitLines(k)
				covered += len(f.built[k])
			}
			newX = k
			if covered >= b.st.Height+_fillSlack && (fresh || time.Since(t0) >= _fillBudget) {
				break
			}
		}
		// The first pass carries the window's first exchange at the head (what
		// the "… N earlier" row names) and the prompt the view pins, built out
		// of order when the fill has not reached it; the middle waits.
		if fresh {
			headEnd := 0
			prompts := 0
			for headEnd < len(units) {
				if e := tr[units[headEnd].first]; e.role == fantasy.MessageRoleUser && e.kind == hostapi.RowMessage {
					prompts++
					if prompts > 1 {
						break
					}
				}
				headEnd++
			}
			for k := range slices.Backward(units) {
				if e := tr[units[k].first]; e.role == fantasy.MessageRoleUser && e.kind == hostapi.RowMessage {
					if k >= headEnd && k < newX {
						f.built[k] = unitLines(k)
					}
					break
				}
			}
			for k := range headEnd {
				if _, ok := f.built[k]; !ok {
					f.built[k] = unitLines(k)
				}
			}
		}
		var lines [][]hostapi.Segment
		// the transcript's first row stands one row off the window's top edge, and
		// the window is the log's tail: name what the head cut left out
		if key.older > 0 {
			lines = append(lines, []hostapi.Segment{{
				Text: "… " + strconv.Itoa(key.older) + " earlier " + hostapi.Plural(key.older, "message", "messages"),
				HL:   "Dim", NoCopy: true, NoHover: true,
			}})
		}
		if len(tr) > 0 {
			lines = append(lines, []hostapi.Segment{{Text: ""}})
		}
		for k := range units {
			if l, ok := f.built[k]; ok {
				lines = append(lines, l...)
			}
		}
		f.settled = lines
		f.fillUnit = newX
		f.settledPrev, f.settledKey = _blockNone, key
		if len(units) > 0 {
			f.settledPrev = blockOf(len(units) - 1)
		}
		b.lines, b.prev = f.settled, f.settledPrev
	}

	// the live turn: the thinking, the calls, the reply, drawn on the rail
	// after the rows of it already settled
	ag, calls := st.Agent, make([]entry, 0, len(st.Agent.Tools))
	for _, t := range st.Agent.Tools {
		calls = append(calls, entryOfCall(t))
	}
	queued := ag.Steering
	if ag.Active || len(calls) > 0 || ag.Text != "" || len(queued) > 0 {
		// the thinking shows while the model thinks: a reasoning block of its
		// own is open, so a thought that ended does not go on spinning over the
		// calls that follow it
		switch {
		case ag.Thinking && ag.Reasoning != "" && ag.Text == "":
			b.gap(_blockActivity)
			b.thinking(ag.Reasoning)
		// The thought no longer draws as the thinking block (the reply
		// streams): the digest row it will settle as stands here already, so
		// the settle shifts nothing. A thought a flush settles beside its calls
		// draws nothing: the digest names the calls, not the thought.
		case tailFirst == 0 && st.View == nil && (ag.Thought >= 50*time.Millisecond || !settling(calls)) && strings.TrimSpace(ag.Reasoning) != "":
			b.gap(_blockActivity)
			b.digestRow(thoughtText(ag.Thought), "", hostapi.PinNone, false)
		case ag.Text == "" && len(calls) == 0 && ag.Active && st.View == nil && ag.Retry == nil &&
			!st.Cancelling && st.Waiting == "" && ag.Elapsed >= 300*time.Millisecond:
			// the wait before any output: the thinking row opens with the turn,
			// after the same 300ms that guards the working rule's block
			b.gap(_blockActivity)
			b.thinking("")
		}
		liveFrom := 0
		if len(calls) > 0 {
			liveFrom = len(b.lines)
			b.gap(_blockActivity)
			b.rail(calls, 0, len(calls)-1, railOpts{live: true})
		}
		// the live rail is rows of the run it extends: the settled tail's run,
		// else the live-only one the picker names by its first call
		if liveFrom > 0 {
			run := -1
			if tailFirst > 0 {
				run = tailFirst
			}
			for n := liveFrom; n < len(b.lines); n++ {
				if len(b.lines[n]) > 0 && segsCells(b.lines[n]) > 0 {
					b.lines[n][0].Anchor = run
				}
			}
		}
		if ag.Text != "" {
			b.gap(_blockText)
			b.markdown(ag.Text)
		}
		// what waits for the turn's next step boundary: the user's own words as
		// typed, folding past the paste threshold like any user row
		for _, q := range queued {
			b.gap(_blockQueued)
			b.user(q, true)
		}
	}
	// what will change this conversation later sits between it and the todos
	pendingAt := 0
	if pending != nil {
		if len(b.lines) > 0 {
			b.push([]hostapi.Segment{{Text: ""}})
		}
		b.push(pending)
		pendingAt = len(b.lines)
	}
	b.todos(st.Todos())
	// what sits below the pending row (the todo list): the rule carries the
	// row only once the reader scrolled this much out of the view
	if pendingAt > 0 {
		setRowsAfterPending(len(b.lines) - pendingAt)
	}
	return b.lines, f.fillUnit > 0
}

// todos draws the list under the conversation until every item is done.
func (b *chatBuilder) todos(todos []store.Todo) {
	left := 0
	for _, td := range todos {
		if td.Status != store.TodoDone {
			left++
		}
	}
	if left == 0 {
		return
	}
	b.push([]hostapi.Segment{{Text: ""}})
	// at most 12 rows and a third of the chat, in the order the model wrote
	// them: the window trails the item in progress, so a long list never hides
	// what is being worked on
	rows := min(12, max(3, b.st.Height/3))
	live := 0
	for n, td := range todos {
		if td.Status == store.TodoInProgress {
			live = n + 1
		}
	}
	first := max(1, min(live-rows+1, len(todos)-rows+1))
	last := min(len(todos), first+rows-1)
	// a header in the composer rule's voice: the name, how far along, and a
	// meter over the whole list: two cells per item while they fit, else
	// scaled, so the meter never changes meaning
	head := []hostapi.Segment{
		{Text: "Todos ", HL: "TodoTitle"},
		{Text: strconv.Itoa(len(todos)-left) + "/" + strconv.Itoa(len(todos)) + "  ", HL: "Dim"},
	}
	cells := 2 * min(len(todos), rows)
	// a cell fills at the done fraction, rounded up: a part-done list lights
	// its first cells even when no cell's whole range is done, so the meter
	// never reads emptier than the count; hue keeps the in-flight cell
	done := len(todos) - left
	filled := (done*cells + len(todos) - 1) / len(todos)
	for n := 1; n <= cells; n++ {
		// the items cell n's share spans, its top bound rounded up: an item two
		// cells wide lights both of them
		lo, hi := (n-1)*len(todos)/cells, (n*len(todos)+cells-1)/cells
		hl := "Rule"
		for i := lo + 1; i <= hi && i <= len(todos); i++ {
			if todos[i-1].Status == store.TodoInProgress {
				hl = "TodoMark"
			}
		}
		if hl == "Rule" && n <= filled {
			hl = "TodoDone"
		}
		head = append(head, hostapi.Segment{Text: "━", HL: hl, NoCopy: true})
	}
	b.push(head)
	fold := func(hidden int, above bool) {
		text := count(hidden, "more todo", "more todos")
		if above {
			text += " above"
		}
		b.push([]hostapi.Segment{{Text: "  … ", HL: "Dim", NoCopy: true}, {Text: text, HL: "Dim"}})
	}
	if first > 1 {
		fold(first-1, true)
	}
	for n := first; n <= last; n++ {
		td := todos[n-1]
		tick, mhl, hl, item := "  ○ ", "Rule", "Normal", td.Content
		switch td.Status {
		case store.TodoDone:
			tick, mhl, hl = "  ✓ ", "TodoDone", "Dim"
		case store.TodoInProgress:
			tick, mhl, hl = "  ▸ ", "TodoMark", "TodoActive"
			if td.ActiveForm != "" {
				item = td.ActiveForm
			}
		}
		b.push([]hostapi.Segment{{Text: tick, HL: mhl, NoCopy: true}, {Text: item, HL: hl}})
	}
	if last < len(todos) {
		fold(len(todos)-last, false)
	}
}
