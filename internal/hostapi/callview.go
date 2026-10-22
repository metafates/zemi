// callview.go opens the view a chat row stands for: a tool call or a
// thought, with its command, its diff or its thinking drawn in the form it
// is written in. The chat's rows name the row they drew; a hidden /call
// resolves it against the transcript on screen, the live turn or the run
// the chat shows.

package hostapi

import (
	"cmp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

func (h *Host) callCommands() []*Command {
	return []*Command{{
		Name: "call", NArgs: NArgsSome, Hidden: true, Desc: "open a chat row's own view (the chat's rows do this when clicked)",
		GoFn: func(h *Host, args string) error {
			n, err := strconv.Atoi(strings.TrimSpace(args))
			if err != nil {
				return Usagef("/call <row> (a chat row's index, negative for the live turn's calls)")
			}
			return h.openCall(n)
		},
	}}
}

// openCall opens the view of the row at n, which is 1-based into the
// transcript the chat draws and negative for the live turn's calls (-1 is
// its first). A call still running refreshes under it until it ends.
func (h *Host) openCall(n int) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	row, took, ok := h.chatRow(n)
	if !ok {
		return Usagef("the chat shows no row %d", n)
	}
	if row.Kind != RowTool && row.Kind != RowReasoning {
		return Usagef("row %d is not a call", n)
	}
	h.msgMu.Lock()
	h.callRow, h.callID = n, row.CallID
	h.msgMu.Unlock()
	ui.OpenOverlay(h.callView(n, row, took))
	// a live view follows the turn as it is written; a settled one is put
	// right by the transcript hook when the rows it steps along land
	if n < 0 || row.Status == CallRun || row.Status == CallWait {
		h.watchCall(n)
	}
	return nil
}

// refreshCall rebuilds the call view open, if any: the rows it steps along
// and the status it shows come from a transcript that just grew, and a call
// that settled after the view opened is a step target at once.
func (h *Host) refreshCall() {
	h.msgMu.Lock()
	n := h.callRow
	h.msgMu.Unlock()
	ui := h.hooks()
	if ui == nil || n == 0 {
		return
	}
	row, took, ok := h.callRowAt(n)
	if !ok {
		return
	}
	ui.RefreshOverlay("call:"+strconv.Itoa(n), func() Overlay { return h.callView(n, row, took) })
}

// callRowAt is the row a call view shows: a live call while its tool is in
// the stream, and the row it settles at once the turn lands. It reads the
// transcript alone, so a turn's flush, which holds the agent lock, can call
// it: the row a view stepped to is never a live one there.
func (h *Host) callRowAt(n int) (ChatMessage, time.Duration, bool) {
	if n > 0 {
		return h.chatRow(n)
	}
	if m := h.settledRow(n); m > 0 {
		return h.chatRow(m)
	}
	return ChatMessage{}, 0, false
}

// settledRow is the transcript row the live call at n landed at, 0 while it
// still runs or the view has moved on: a turn settles its calls under the
// agent lock, so this reads the transcript and the id the view opened on.
func (h *Host) settledRow(n int) int {
	h.msgMu.Lock()
	id, tracked := h.callID, h.callRow
	h.msgMu.Unlock()
	// a live call is a session turn's: a run on screen has none, and a turn
	// settles under the agent lock, which this path must never take
	if id == "" || tracked != n || h.viewedRun() != nil {
		return 0
	}
	for i, m := range h.Session().Transcript {
		if m.CallID == id {
			return i + 1
		}
	}
	return 0
}

// chatRow is the row the chat draws at n: the live turn's calls, the rows of
// the run the chat shows, else the session's. took is how long a live call
// has run, 0 for a settled one, since no row records that.
func (h *Host) chatRow(n int) (row ChatMessage, took time.Duration, ok bool) {
	if n < 0 {
		s := h.AgentStream()
		i := -n - 1
		if i >= len(s.Tools) {
			return ChatMessage{}, 0, false
		}
		t := s.Tools[i]
		return ChatMessage{
			Kind: RowTool, CallID: t.ID, Tool: t.Name, Text: t.Input, Label: t.Label,
			Status: t.Status, Detail: t.Detail, Tail: t.Tail, Run: t.Run, Diff: t.Diff, Output: t.Output,
		}, time.Since(t.Started), true
	}
	tr := h.Session().Transcript
	if run := h.viewedRun(); run != nil {
		tr = run.transcript()
	}
	if n < 1 || n > len(tr) {
		return ChatMessage{}, 0, false
	}
	return tr[n-1], 0, true
}

// watchCall keeps an open call view current while the call runs: its output
// and its mark change in place. A live row's index names another call once
// the next turn starts, so the ticker stops with the turn.
func (h *Host) watchCall(n int) {
	h.overlayTicker("call view", nil, func() bool {
		row, took, ok := h.chatRow(n)
		if !ok || n < 0 && !h.AgentActive() {
			return false
		}
		if ui := h.hooks(); ui != nil {
			ui.RefreshOverlay("call:"+strconv.Itoa(n), func() Overlay { return h.callView(n, row, took) })
		}
		// a live view keeps up with the run it steps along; a settled one
		// has nothing to say until the transcript grows, and refreshCall
		// takes it then
		return n < 0 || row.Status == CallRun || row.Status == CallWait
	})
}

// callTitles name a call's view by what it is, not by the verb its row reads
// with; any other tool reads as its own name's words.
var callTitles = map[string]string{
	tools.ToolShell: "Command", tools.ToolEdit: "Edit", tools.ToolWrite: "Write", tools.ToolRead: "Read",
	tools.ToolGrep: "Search", tools.ToolGlob: "Find", tools.ToolSkill: "Skill", tools.ToolLSP: "Lookup",
	tools.ToolPlan: "Plan", tools.ToolLua: "Script", tools.ToolTodos: "Todos", tools.ToolMemory: "Memory",
}

// callWords say where a call stands, beside its mark.
var callWords = map[CallStatus]string{
	CallWait: "waiting", CallRun: "running", CallOK: "ok", CallWarn: "warned",
	CallErr: "failed", CallStopped: "stopped", CallBG: "in the background", CallCancelled: "cancelled",
}

// callMarks are the chat's own marks for a call's state.
var callMarks = map[CallStatus]MarkKind{
	CallWait: MarkWait, CallRun: MarkSpin, CallOK: MarkOK, CallWarn: MarkWarn,
	CallErr: MarkErr, CallStopped: MarkStop, CallBG: MarkBG, CallCancelled: MarkStop,
}

// callArg names what a call was given, so the facts table says what it acted
// on before the body says what came back.
var callArg = map[string]string{
	tools.ToolRead: "file", tools.ToolEdit: "file", tools.ToolWrite: "file", tools.ToolGrep: "pattern",
	tools.ToolGlob: "pattern", tools.ToolSkill: "skill", tools.ToolLSP: "symbol",
	tools.ToolMemory: "memory", tools.ToolTodos: "what",
}

// callView is one row's view: the mark and the facts of the call, then its
// content in parts, each drawn the way its text is written.
func (h *Host) callView(n int, m ChatMessage, took time.Duration) Overlay {
	o := Overlay{
		ID: "call:" + strconv.Itoa(n), Title: callTitle(m), Origin: lua.OriginBuiltin,
		Mark: callMarks[m.Status], Live: m.Status == CallRun || m.Status == CallWait,
	}
	var parts []BodyPart
	fields := []Field{{Name: "status", Value: callWords[m.Status]}}
	if m.Kind == RowReasoning {
		parts = append(parts, BodyPart{Text: m.Text, Lang: "markdown"})
		if m.Stat != nil && m.Stat.ThoughtMS > 0 {
			fields = append(fields, Field{Name: "time", Value: fmtDuration(time.Duration(m.Stat.ThoughtMS) * time.Millisecond)})
		}
	} else {
		if took >= time.Second {
			fields = append(fields, Field{Name: "time", Value: fmtDuration(took)})
		}
		if m.Label != "" {
			fields = append(fields, Field{Name: "about", Value: m.Label})
		}
		if (m.Tool == tools.ToolShell || m.Tool == tools.ToolLua) && m.Text != "" {
			// a command is code, so it heads the body rather than the facts,
			// and sinks into the well its answer lifts out of
			parts = append(parts, BodyPart{Text: m.Text, Lang: "bash", Fill: FillCanvas})
		} else if arg := callArg[m.Tool]; arg != "" && m.Text != "" {
			fields = append(fields, Field{Name: arg, Value: m.Text})
		}
		if m.Diff != "" {
			parts = append(parts, BodyPart{Text: m.Diff, Lang: "diff"})
		}
		switch out := cmp.Or(h.skillBody(m), callOutput(m), m.Tail); {
		case out != "":
			// a skill answers in markdown, on its own surface; anything else
			// reads in the well the command opened
			if m.Tool == tools.ToolSkill {
				parts = append(parts, BodyPart{Text: out, Lang: "markdown"})
			} else {
				parts = append(parts, BodyPart{Text: out, Fill: FillWell})
			}
		case m.Detail != "":
			// a running call with no output yet says what it is doing
			parts = append(parts, BodyPart{Text: m.Detail, Fill: FillWell})
		}
	}
	o.Prev, o.Next = h.callStep(n)
	if m.Job != "" {
		fields = append(fields, Field{Name: "job", Value: m.Job})
		o.Keys = append(o.Keys, OverlayKey{Key: "o", Desc: "open job", Run: "jobs view " + m.Job})
	}
	o.Fields, o.Parts = fields, parts
	for _, p := range parts {
		if p.Text != "" {
			if o.Body != "" {
				o.Body += "\n\n"
			}
			o.Body += p.Text
		}
	}
	return o
}

// skillBody is what a skill call answers in its view: the skill's own file
// whole, formatted as markdown, where the row keeps only the first and last
// lines of the result. Empty for any other call, or a skill gone since.
func (h *Host) skillBody(m ChatMessage) string {
	if m.Tool != tools.ToolSkill {
		return ""
	}
	name := strings.TrimSpace(m.Text)
	s, ok := h.skillsReg.get(name)
	if !ok { // a project's skill or a recipe lives outside the stored table
		for _, p := range h.projectOnlySkills() {
			if p.Name == name {
				s, ok = p, true
				break
			}
		}
	}
	if !ok {
		return ""
	}
	text, err := tools.SkillText(tools.SkillInfo{Name: s.Name, Path: s.Dir, File: s.File, Root: s.Root, Data: s.Data})
	if err != nil {
		return ""
	}
	_, body, found := strings.Cut(text, "\n\n")
	if !found {
		return text
	}
	return body
}

// callStep names the views around row n in its rail: the run of thinking and
// call rows the chat draws as one unit between two messages, so ctrl+j and
// ctrl+k walk it. The turn settles its calls into the transcript's tail while
// it still writes the later ones, so the settled rows and the live calls are
// one rail and a step crosses between them. A run digest is no call view (it
// opens its run), so a step passes over one, settled or live.
func (h *Host) callStep(n int) (prev, next string) {
	tr := h.viewedTranscript()
	s := h.AgentStream()
	// a shown run has no live stream of its own, so the live calls are steps
	// of the session's rail alone
	cross := h.viewedRun() == nil && s.Active
	rail := func(i int) bool { return tr[i].Kind == RowTool || tr[i].Kind == RowReasoning }
	step := func(i int) string {
		switch {
		case !rail(i), tr[i].Kind == RowTool && tr[i].Run != "":
			return ""
		}
		return "call " + strconv.Itoa(i+1)
	}
	back := func(from int) string {
		for i := from; i >= 0 && rail(i); i-- {
			if cmd := step(i); cmd != "" {
				return cmd
			}
		}
		return ""
	}
	// live scans the turn's unsettled calls for the first step target from
	// j, walking the way dir points
	live := func(j, dir int) string {
		for ; j >= 0 && j < len(s.Tools); j += dir {
			if s.Tools[j].Run == "" {
				return "call " + strconv.Itoa(-j-1)
			}
		}
		return ""
	}
	// the live turn's calls are named by negative rows, -1 its first: a view
	// of one steps along the run still being written, not a snapshot of
	// whatever had settled when it opened
	if n < 0 {
		if k := -n; k <= len(s.Tools) {
			if prev = live(k-2, -1); prev == "" && cross {
				prev = back(len(tr) - 1)
			}
			return prev, live(k, 1)
		}
		// the turn landed since the view opened: it steps along the rows the
		// calls settled at
		if m := h.settledRow(n); m > 0 {
			return h.callStep(m)
		}
		return "", ""
	}
	if n < 1 || n > len(tr) {
		return "", ""
	}
	prev = back(n - 2)
	var i int
	for i = n; i < len(tr) && rail(i); i++ {
		if cmd := step(i); cmd != "" {
			next = cmd
			break
		}
	}
	// the walk ran to the transcript's end: the rail runs on into the calls
	// the turn is still writing
	if i == len(tr) && cross {
		next = live(0, 1)
	}
	return prev, next
}

// callTitle names the view: a thought is a thought, a question it asks, and
// every other call what it is.
func callTitle(m ChatMessage) string {
	switch {
	case m.Kind == RowReasoning:
		return "Thought"
	case m.Tool == tools.ToolQuestion:
		return "Question"
	}
	if t, ok := callTitles[m.Tool]; ok {
		return t
	}
	return capitalize(strings.ReplaceAll(strings.TrimPrefix(m.Tool, "mcp_"), "_", " "))
}

// callOutput is what the call answered, as its view shows it: a question's
// answers read as a table under their questions, and a file call's diff is
// the answer it gives, so it stands alone.
func callOutput(m ChatMessage) string {
	switch m.Tool {
	case tools.ToolQuestion:
		var b strings.Builder
		for i, l := range strings.Split(strings.TrimRight(m.Output, "\n"), "\n") {
			fields := strings.SplitN(l, "\t", 3)
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(fields[0])
			if len(fields) > 1 {
				b.WriteString("  " + fields[1])
			}
			if len(fields) > 2 && fields[2] != "" {
				b.WriteString("\nnote: " + fields[2])
			}
		}
		return b.String()
	}
	if m.Diff != "" {
		return ""
	}
	return m.Output
}

// capitalize upper-cases a name's first rune, leaving the rest alone.
func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
