package hostapi

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

func waitIdle(t *testing.T, h *Host) {
	t.Helper()
	waitFor(t, func() error {
		if h.AgentActive() {
			return errors.New("turn did not finish within 3s")
		}
		return nil
	})
}

// TestResumeDrawsLiveRows: a turn that thinks, says what it will do, calls
// a tool, thinks again and answers keeps its rows in the order they
// streamed, and a resume rebuilds the same rows from the store. A report
// riding the typed message draws under it.
func TestResumeDrawsLiveRows(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
			if toolResult {
				return append(reasoningParts("now answer"), textParts("done")...)
			}
			step := append(reasoningParts("look first"), textParts("I will look first.")[:3]...)
			return append(step, toolCallParts("c1", "glob", `{"pattern":"*.go"}`)...)
		}}, nil
	}
	if err := h.AgentSend("go" + agent.AttachMarker + "\n<agent-report run=\"a1\" agent=\"explore\" status=\"finished\" description=\"look around\">\nr\n</agent-report>\n</attached-files>"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	shape := func(tr []ChatMessage) string {
		var b strings.Builder
		for _, m := range tr {
			fmt.Fprintf(&b, "%s/%s %s %s %q\n", m.Role, m.Kind, m.Tool, m.Status, m.Text)
		}
		return b.String()
	}
	live := shape(h.Session().Transcript)
	want := `user/   "go"
assistant/notice  ok "Background agent a1 (look around) finished"
assistant/reasoning   "look first"
assistant/   "I will look first."
assistant/tool glob err "*.go"
assistant/reasoning   "now answer"
assistant/   "done"
`
	if live != want {
		t.Fatalf("live rows:\n%s", live)
	}
	rows, err := h.DB.Messages(h.AgentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	if _, tr := historyFrom(rows); shape(tr) != live {
		t.Fatalf("resumed rows:\n%s\nlive:\n%s", shape(tr), live)
	}
}

// TestResumeKeepsTurnStats: the readout under a turn's reply - how long it
// ran, what it spent, when it ended - matches the live one after a resume,
// which the stored message rows alone cannot carry.
func TestResumeKeepsTurnStats(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			parts := textParts("done")
			parts[len(parts)-1].Usage = fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}
			return parts
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	live := h.Session().Transcript
	last := live[len(live)-1]
	if last.Stat == nil || last.Stat.Out != 20 {
		t.Fatalf("live stat = %+v", last.Stat)
	}
	rows, err := h.DB.Messages(h.AgentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	_, tr := historyFrom(rows)
	got := tr[len(tr)-1]
	if got.Text != "done" || got.Stat == nil {
		t.Fatalf("resumed rows: %+v", got)
	}
	if *got.Stat != *last.Stat {
		t.Fatalf("resumed stat = %+v, live %+v", got.Stat, last.Stat)
	}
}

// TestSessionLifecycle: /new starts clean, /resume rebuilds the transcript
// and the model history from the store, in order, and a stored session
// can be deleted.
func TestSessionLifecycle(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	// Hold the reflector lock so session close does not start a detached
	// reflect run racing the test's DB teardown.
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	send := func(text string) {
		t.Helper()
		if err := h.AgentSend(text); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		waitIdle(t, h)
	}

	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	send("one")
	first := h.AgentSessionID()
	if err := h.NewSession(); err != nil {
		t.Fatalf("new: %v", err)
	}
	if h.AgentSessionID() != "" || len(h.Session().Transcript) != 0 {
		t.Fatal("new session must start clean")
	}
	send("two")
	second := h.AgentSessionID()
	if n := m.lastPrompt.Load(); n != 3 {
		t.Fatalf("fresh session prompt has %d messages, want 3 (system, context, user)", n)
	}

	// a list the session was left with in progress settles on resume: nothing
	// is running while the agent is idle
	if err := h.DB.SetTodos(first, []store.Todo{{Content: "left over", Status: store.TodoInProgress}}); err != nil {
		t.Fatal(err)
	}
	// a bare /resume from the latest session goes to the one before it
	if err := h.ResumeSession(""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if todos, err := h.DB.Todos(first); err != nil || len(todos) != 1 || todos[0].Status != store.TodoPending {
		t.Fatalf("resumed list = %+v %v", todos, err)
	}
	if tr := h.Session().Transcript; len(tr) != 2 || tr[0].Text != "one" || tr[1].Text != "ok" {
		t.Fatalf("resumed transcript = %+v", tr)
	}
	// The context readout needs a window before the first new step; the
	// estimate must survive a resume, not wait for the next turn.
	if got := h.AgentStream().LastIn; got == 0 {
		t.Fatal("resume left the context estimate at 0")
	}
	send("three")
	if n := m.lastPrompt.Load(); n != 5 {
		t.Fatalf("resumed prompt has %d messages, want 5", n)
	}
	// the resumed session sends the context message it stored, and stores no other
	rows, _ := h.DB.Messages(first)
	if n := len(slices.DeleteFunc(rows, func(r *store.Message) bool { return r.Role != contextRole })); n != 1 {
		t.Fatalf("the session stored %d context messages, want 1", n)
	}
	// The composer history spans sessions, oldest first.
	if got := h.InputHistory(10); len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("input history = %q", got)
	}
	// a memory forgotten since the context was stored is not sent again
	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.memoryCommand("forget " + projectMemories(t, h)[0].ID + " --yes"); err != nil {
		t.Fatal(err)
	}
	if err := h.ResumeSession(first); err != nil {
		t.Fatal(err)
	}
	send("four")
	m.mu.Lock()
	ctx0, ctx1 := concatText(m.prompts[0][1].Content), concatText(m.prompts[len(m.prompts)-1][1].Content)
	m.mu.Unlock()
	if !strings.Contains(ctx0, "deploy by hand") || strings.Contains(ctx1, "deploy by hand") {
		t.Fatalf("context before the forget:\n%s\nafter it:\n%s", ctx0, ctx1)
	}

	// /sessions delete: the live session is refused, another one goes with its scratchpad
	if err := h.DeleteSession(first, true); err == nil {
		t.Fatal("deleted the live session")
	}
	if err := h.ResumeSession(second); err != nil {
		t.Fatal(err)
	}
	// typed, a delete asks first: headless nobody can answer, with a screen
	// its Delete opens armed
	if err := h.DeleteSession(first, false); err == nil || !strings.Contains(err.Error(), "/sessions delete "+first+" --yes runs it without asking") {
		t.Fatalf("a headless delete: %v", err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.DeleteSession(first, false); err != nil || ui.opened[0].Confirm != "sessions delete "+first+" --yes" {
		t.Fatalf("a typed delete: %v, %+v", err, ui.opened)
	}
	// another session's detail keys its actions by letter, the permanent one a danger
	acts := ""
	for _, it := range ui.opened[0].Items {
		if a, ok := it.(map[string]any)["actions"]; ok {
			acts = fmt.Sprint(a)
		}
	}
	if acts != "[map[hotkey:r run:resume "+first+" text:Resume] map[hotkey:n run:sessions rename "+first+"  text:Rename] map[danger:true hotkey:x run:sessions delete "+first+" --yes text:Delete]]" {
		t.Fatalf("session actions: %s", acts)
	}
	pad := h.scratchFor(first)
	if err := os.MkdirAll(pad, 0o700); err != nil {
		t.Fatal(err)
	}
	// headless the delete is done when the command returns: zemi run exits then
	h.SetUIHooks(nil)
	if err := h.DeleteSession(first, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pad); err == nil {
		t.Error("scratchpad still there")
	}
	if _, err := h.DB.GetSession(first); err == nil {
		t.Error("session still stored")
	}
}

// TestTurnRunningRefusals: what cannot run mid-turn is refused as a usage
// error, which warns, never as a failure /errors records.
func TestTurnRunningRefusals(t *testing.T) {
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	h.agents.mu.Lock()
	h.agents.active = true
	h.agents.mu.Unlock()
	for name, run := range map[string]func() error{
		"new":    h.NewSession,
		"fork":   func() error { _, err := h.ForkSession(); return err },
		"rewind": func() error { _, err := h.RewindTo("x"); return err },
		"btw":    func() error { return h.Btw("why") },
	} {
		if err := run(); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "a turn is running") {
			t.Errorf("%s mid-turn: %v", name, err)
		}
	}
}

// TestResumeKeepsToolDiffs: the diff a file tool showed live is stored
// as the tool ends, before its call's message, so a resumed session renders
// it on that call and not on a later one a provider gave the same id.
func TestResumeKeepsToolDiffs(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	sid := seedSession(t, h)
	const diff = "12 +added\n13 -gone"
	for _, turn := range []struct {
		tool string
		diff string
	}{{"edit", diff}, {"shell", ""}} {
		if err := h.persistMessage(agent.UserMessage("run " + turn.tool)); err != nil {
			t.Fatal(err)
		}
		if turn.diff != "" {
			h.recordToolCall(sid, StreamTool{ID: "c1", Name: turn.tool, Diff: turn.diff}, true)
		}
		call := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: turn.tool, Input: `{"path":"x.go"}`},
		}}
		if err := h.persistMessage(call); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.ResumeSession(sid); err != nil {
		t.Fatal(err)
	}
	var diffs []string
	for _, m := range h.Session().Transcript {
		if m.Kind == RowTool {
			diffs = append(diffs, m.Diff)
		}
	}
	if !slices.Equal(diffs, []string{diff, ""}) {
		t.Fatalf("resumed tool row diffs = %q", diffs)
	}
}

// TestRenameSession: one path names a session, the live one or a stored id:
// the title is sanitized and clipped, the live transcript's title moves with
// it, and an unknown id is named rather than renaming nothing.
func TestRenameSession(t *testing.T) {
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	sid := seedSession(t, h)

	if err := h.RenameSession("", "fix \x1b[31mthe\x1b[0m parser"); err != nil {
		t.Fatal(err)
	}
	if got := h.SessionTitle(); got != "fix the parser" {
		t.Fatalf("live title = %q", got)
	}
	if sess, err := h.DB.GetSession(sid); err != nil || sess.Title != "fix the parser" {
		t.Fatalf("stored title = %q, %v", sess.Title, err)
	}
	other := store.ShortID()
	if err := h.DB.CreateSession(other, "", "other", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.RenameSession(other, strings.Repeat("x", 90)); err != nil {
		t.Fatal(err)
	}
	if got := h.SessionTitle(); got != "fix the parser" {
		t.Fatalf("renaming another session moved the live title: %q", got)
	}
	if sess, err := h.DB.GetSession(other); err != nil || sess.Title != clipRunes(strings.Repeat("x", 90), 60) {
		t.Fatalf("clipped title = %q, %v", sess.Title, err)
	}
	if err := h.RenameSession("nosuchid", "x"); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("unknown id: %v", err)
	}
	// no title: the prompt the picker's row action opens, on the live session
	if err := h.RenameSession("", ""); err != nil {
		t.Fatal(err)
	}
	if o := ui.overlays(); len(o) != 1 || !o[0].Input || o[0].Run != "rename " {
		t.Fatalf("prompt = %+v", o)
	}
}

// TestResumeSeedsReads: a resumed session keeps the files its conversation
// read, so the next edit is not refused; one touched after the session's
// last message must be read again.
func TestResumeSeedsReads(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	sid := seedSession(t, h)
	dir := t.TempDir()
	read, touched := filepath.Join(dir, "read.go"), filepath.Join(dir, "touched.go")
	for _, p := range []string{read, touched} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(touched, time.Now(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	call := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "read", Input: `{"path":"` + read + `"}`},
		fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "edit", Input: `{"path":"` + touched + `"}`},
	}}
	if err := h.persistMessage(call); err != nil {
		t.Fatal(err)
	}
	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.ResumeSession(sid); err != nil {
		t.Fatal(err)
	}
	seen := h.readLog()
	if _, ok := seen.Load(perm.CanonPath(read)); !ok {
		t.Fatal("resume forgot a file the session read")
	}
	if _, ok := seen.Load(perm.CanonPath(touched)); ok {
		t.Fatal("a file touched after the session must be read again")
	}
}

// TestSeedReadsUsesTheRowThatReadIt: a file the user changed in the middle
// of a session is not seeded, however many rows came after the change.
func TestSeedReadsUsesTheRowThatReadIt(t *testing.T) {
	h, _ := newTestHostDB(t)
	p := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := os.Chtimes(p, at, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "read", Input: `{"path":"` + p + `"}`},
	}}, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	later, err := json.Marshal(agent.UserMessage("and then some"), json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	h.seedReads([]*store.Message{
		{Role: "assistant", PartsJSON: string(call), CreatedAt: at.UnixMilli()},
		{Role: "user", PartsJSON: string(later), CreatedAt: at.Add(2 * time.Minute).UnixMilli()},
	})
	if _, ok := h.readLog().Load(perm.CanonPath(p)); ok {
		t.Fatal("seeded a file changed after the row that read it")
	}
}

// TestForkKeepsTodos: /fork carries the todo list its history set.
func TestForkKeepsTodos(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.reflectMu.Lock() // no detached reflect run racing the teardown
	defer h.reflectMu.Unlock()
	parts, _ := json.Marshal(fantasy.NewUserMessage("plan it"), json.Deterministic(true))
	h.DB.CreateSession("s", "", "t", "")
	h.DB.AddMessage(&store.Message{SessionID: "s", Role: "user", PartsJSON: string(parts)})
	h.DB.SetTodos("s", []store.Todo{{Content: "a", Status: "pending"}})
	if err := h.ResumeSession("s"); err != nil {
		t.Fatal(err)
	}
	id, err := h.ForkSession()
	if err != nil {
		t.Fatal(err)
	}
	if todos := h.todoSnapshot(); h.AgentSessionID() != id || len(todos) != 1 || todos[0].Content != "a" {
		t.Fatalf("fork %s has todos %+v", h.AgentSessionID(), todos)
	}
}

// TestPlanRowOutput: a plan row shows the verdict, not the text written for
// the model, both live and after a resume.
func TestPlanRowOutput(t *testing.T) {
	const raw = "The user approved the plan. " + tools.ImplementPlan + "\nThe plan is saved at /p/s1.md."
	const want = "approved, saved to /p/s1.md"
	for name, got := range map[string]func() string{
		"live": func() string {
			h, _ := newTestHostDB(t)
			hk := h.streamHooks()
			hk.ToolCallStart("c1", tools.ToolPlan, `{"plan":"x"}`)
			hk.ToolCallEnd("c1", tools.ToolPlan, out(raw), "")
			hk.TurnDone(nil, fantasy.Usage{})
			tr := h.Session().Transcript
			return tr[len(tr)-1].Output
		},
		"resume": func() string {
			var rows []*store.Message
			for _, m := range []fantasy.Message{
				{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "c1", ToolName: tools.ToolPlan, Input: `{"plan":"x"}`}}},
				{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: raw}}}},
			} {
				parts, err := json.Marshal(m, json.Deterministic(true))
				if err != nil {
					t.Fatal(err)
				}
				rows = append(rows, &store.Message{Role: string(m.Role), PartsJSON: string(parts)})
			}
			_, tr := historyFrom(rows)
			return tr[len(tr)-1].Output
		},
	} {
		if g := got(); g != want {
			t.Errorf("%s: output %q, want %q", name, g, want)
		}
	}
}
