package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// streamed, and a resume rebuilds the same rows from the store.
func TestResumeDrawsLiveRows(t *testing.T) {
	h, _ := newTestHostDB(t)
	think := func(text string) []fantasy.StreamPart {
		return []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"},
			{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: text},
			{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"},
		}
	}
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
			if toolResult {
				return append(think("now answer"), textParts("done")...)
			}
			step := append(think("look first"), textParts("I will look first.")[:3]...)
			return append(step, toolCallParts("c1", "glob", `{"pattern":"*.go"}`)...)
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
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
	if n := m.lastPrompt.Load(); n != 2 {
		t.Fatalf("fresh session prompt has %d messages, want 2", n)
	}

	// a bare /resume from the latest session goes to the one before it
	if err := h.ResumeSession(""); err != nil {
		t.Fatalf("resume: %v", err)
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
	if n := m.lastPrompt.Load(); n != 4 {
		t.Fatalf("resumed prompt has %d messages, want 4", n)
	}
	// The composer history spans sessions, oldest first.
	if got := h.InputHistory(10); len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("input history = %q", got)
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
	if acts != "[map[hotkey:r run:resume "+first+" text:Resume] map[danger:true hotkey:x run:sessions delete "+first+" --yes text:Delete]]" {
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

// TestResumeKeepsToolDiffs: the diff a file tool showed live is stored
// with its call, so a resumed session renders it instead of a bare row.
func TestResumeKeepsToolDiffs(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	sid := seedSession(t, h)
	if err := h.persistMessage("user", agent.UserMessage("edit it")); err != nil {
		t.Fatal(err)
	}
	call := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "edit", Input: `{"path":"x.go"}`},
	}}
	if err := h.persistMessage("assistant", call); err != nil {
		t.Fatal(err)
	}
	h.recordToolCall(sid, StreamTool{ID: "c1", Name: "edit", Diff: "12 +added\n13 -gone"}, true)

	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.ResumeSession(sid); err != nil {
		t.Fatal(err)
	}
	for _, m := range h.Session().Transcript {
		if m.Kind == "tool" {
			if m.Diff != "12 +added\n13 -gone" {
				t.Fatalf("resumed tool row diff = %q", m.Diff)
			}
			return
		}
	}
	t.Fatalf("no tool row: %+v", h.Session().Transcript)
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
	if err := h.persistMessage("assistant", call); err != nil {
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
	}})
	if err != nil {
		t.Fatal(err)
	}
	later, err := json.Marshal(agent.UserMessage("and then some"))
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
	parts, _ := json.Marshal(fantasy.NewUserMessage("plan it"))
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
			hk.ToolCallEnd("c1", tools.ToolPlan, true, raw, "")
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
				parts, err := json.Marshal(m)
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
