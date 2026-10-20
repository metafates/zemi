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
	ui.OpenOverlay(h.callView(n, row, took))
	if row.Status == CallRun || row.Status == CallWait {
		h.watchCall(n)
	}
	return nil
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
			Kind: RowTool, Tool: t.Name, Text: t.Input, Label: t.Label,
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
		return row.Status == CallRun || row.Status == CallWait
	})
}

// callTitles name a call's view by what it is, not by the verb its row reads
// with; any other tool reads as its own name's words.
var callTitles = map[string]string{
	"shell": "Command", "edit": "Edit", "write": "Write", "read": "Read",
	"grep": "Search", "glob": "Find", "skill": "Skill", "lsp": "Lookup",
	"plan": "Plan", "lua": "Script", "todos": "Todos", "memory": "Memory",
}

// callWords say where a call stands, beside its mark.
var callWords = map[CallStatus]string{
	CallWait: "waiting", CallRun: "running", CallOK: "ok", CallWarn: "warned",
	CallErr: "failed", CallStopped: "stopped", CallBG: "in the background", CallCancelled: "cancelled",
}

// callMarks are the chat's own marks for a call's state.
var callMarks = map[CallStatus]string{
	CallWait: "wait", CallRun: "spin", CallOK: "ok", CallWarn: "warn",
	CallErr: "err", CallStopped: "stop", CallBG: "bg", CallCancelled: "stop",
}

// callArg names what a call was given, so the facts table says what it acted
// on before the body says what came back.
var callArg = map[string]string{
	"read": "file", "edit": "file", "write": "file", "grep": "pattern",
	"glob": "pattern", "skill": "skill", "lsp": "symbol",
	"memory": "memory", "todos": "what",
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
		if (m.Tool == "shell" || m.Tool == "lua") && m.Text != "" {
			// a command is code, so it heads the body rather than the facts
			parts = append(parts, BodyPart{Text: m.Text, Lang: "bash"})
		} else if arg := callArg[m.Tool]; arg != "" && m.Text != "" {
			fields = append(fields, Field{Name: arg, Value: m.Text})
		}
		if m.Diff != "" {
			parts = append(parts, BodyPart{Text: m.Diff, Lang: "diff"})
		}
		switch out := cmp.Or(h.skillBody(m), callOutput(m), m.Tail); {
		case out != "":
			// a skill answers in markdown, the rest as its text is written
			parts = append(parts, BodyPart{Text: out, Lang: map[bool]string{true: "markdown"}[m.Tool == tools.ToolSkill]})
		case m.Detail != "":
			// a running call with no output yet says what it is doing
			parts = append(parts, BodyPart{Text: m.Detail})
		}
	}
	if n > 0 {
		o.Prev, o.Next = callStep(h.viewedTranscript(), n)
	}
	if job := jobOf(m.Tool, m.Status, m.Output); job != "" {
		fields = append(fields, Field{Name: "job", Value: job})
		o.Keys = append(o.Keys, OverlayKey{Key: "o", Desc: "open job", Run: "jobs view " + job})
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
// ctrl+k walk it. A run digest is no call view (it opens its run), so a step
// passes over it.
func callStep(tr []ChatMessage, n int) (prev, next string) {
	if n < 1 || n > len(tr) {
		return "", ""
	}
	rail := func(i int) bool { return tr[i].Kind == RowTool || tr[i].Kind == RowReasoning }
	step := func(i int) string {
		switch {
		case !rail(i), tr[i].Kind == RowTool && tr[i].Run != "":
			return ""
		}
		return "call " + strconv.Itoa(i+1)
	}
	for i := n - 2; i >= 0 && rail(i); i-- {
		if cmd := step(i); cmd != "" {
			prev = cmd
			break
		}
	}
	for i := n; i < len(tr) && rail(i); i++ {
		if cmd := step(i); cmd != "" {
			next = cmd
			break
		}
	}
	return prev, next
}

// callTitle names the view: a thought is a thought, a question it asks, and
// every other call what it is.
func callTitle(m ChatMessage) string {
	switch {
	case m.Kind == RowReasoning:
		return "Thought"
	case m.Tool == "question":
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
	case "question":
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
