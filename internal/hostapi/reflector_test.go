package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/store"
)

// reflectStubModel answers Generate and Stream with a fixed text (Stream
// backs agent turns and records the prompt size); object calls refuse.
type reflectStubModel struct {
	resp       string
	err        error
	block      time.Duration
	calls      atomic.Int32
	lastPrompt atomic.Int32
	streams    atomic.Int32
	overflow   atomic.Int32                          // Stream calls left that fail as context too large
	noImages   atomic.Int32                          // Stream calls left that fail as a 400 over image input
	toolStep   atomic.Int64                          // context the next Stream call reports along with a tool call; 0 = text
	failOnce   atomic.Pointer[fantasy.ProviderError] // the next Stream call's error
	cutReply   bool                                  // Generate stops at the output limit
	during     func()                                // runs inside Generate, while the model "thinks"
}

func (m *reflectStubModel) Generate(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	m.calls.Add(1)
	if m.during != nil {
		m.during()
	}
	if m.block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.block):
		}
	}
	if m.err != nil {
		return nil, m.err
	}
	finish := fantasy.FinishReasonStop
	if m.cutReply {
		finish = fantasy.FinishReasonLength
	}
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: m.resp}},
		FinishReason: finish,
	}, nil
}

func (m *reflectStubModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.streams.Add(1)
	m.lastPrompt.Store(int32(len(call.Prompt)))
	if used := m.toolStep.Swap(0); used > 0 {
		return func(yield func(fantasy.StreamPart) bool) {
			_ = yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "nope", ToolCallInput: "{}"}) &&
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: used}})
		}, nil
	}
	if m.overflow.Add(-1) >= 0 {
		return nil, &fantasy.ProviderError{Message: "prompt is too long", ContextTooLargeErr: true}
	}
	if m.noImages.Add(-1) >= 0 {
		return nil, &fantasy.ProviderError{StatusCode: 400, Message: "<path> is not a multimodal model"}
	}
	if pe := m.failOnce.Swap(nil); pe != nil {
		return nil, pe
	}
	parts := []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "t"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: m.resp},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "t"},
		{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *reflectStubModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *reflectStubModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *reflectStubModel) Provider() string { return "stub" }
func (m *reflectStubModel) Model() string    { return "stub-1" }

// seedTranscript fills the session with one exchange plus one tool call so
// the reflect prompt has material.
func seedTranscript(t *testing.T, h *Host, sid string) {
	t.Helper()
	addText(t, h, sid, "user", "the deploy script keeps failing")
	addText(t, h, sid, "assistant", "fixed: it needs the labels sequence bumped first")
	if err := h.DB.AddMessage(&store.Message{
		SessionID: sid, Role: "tool", PartsJSON: `{"tool":"bash","output":"ok"}`,
	}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := h.DB.AddToolCall(&store.Message{SessionID: sid, Role: "tool", PartsJSON: `{}`},
		&store.ToolCall{Tool: "bash", OK: false}); err != nil {
		t.Fatalf("AddToolCall: %v", err)
	}
}

// addText stores a text message the way persistMessage does.
func addText(t *testing.T, h *Host, sid, role, text string) {
	t.Helper()
	parts, err := json.Marshal(fantasy.Message{Role: fantasy.MessageRole(role), Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}}})
	if err == nil {
		err = h.DB.AddMessage(&store.Message{SessionID: sid, Role: role, PartsJSON: string(parts)})
	}
	if err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
}

func reflectStub(t *testing.T, h *Host, resp string) *reflectStubModel {
	t.Helper()
	m := &reflectStubModel{resp: resp}
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
	return m
}

const proposalDoc = `{"memories":[{"scope":"project","kind":"gotcha",
	"text":"UpsertLabels must stay read-then-insert; ON CONFLICT burns the labels sequence",
	"tags":["labels","postgres"],"supersedes":""}]}`

// TestReflectQueuesUnderAsk covers the ask-autonomy path: the proposal is
// schema-parsed, scrubbed by intake and parked in the review queue with
// provenance stamped by Go (never the model).
func TestReflectQueuesUnderAsk(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Settings.SetHost("memory.autonomy", "ask")
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	m := reflectStub(t, h, "Sure! Here is my analysis:\n"+proposalDoc+"\nHope that helps.")
	m.during = func() { addText(t, h, sid, "user", "sent while the reflector ran") }

	if err := h.reflectSession(context.Background(), sid); err != nil {
		t.Fatalf("reflectSession: %v", err)
	}
	if m.calls.Load() != 1 {
		t.Fatalf("model calls=%d", m.calls.Load())
	}
	if !h.reflectDue(sid) {
		t.Fatal("a message the run never read was marked seen")
	}
	props := h.memoryProposals()
	if len(props) != 1 {
		t.Fatalf("queued proposals=%d", len(props))
	}
	rows, err := h.DB.ListMemory("project", true)
	if err != nil || len(rows) != 0 {
		t.Fatalf("ask mode must not store: rows=%d err=%v", len(rows), err)
	}
	text, prov := props[0].Text, props[0].ProvenanceSessionID
	if text != "UpsertLabels must stay read-then-insert; ON CONFLICT burns the labels sequence" {
		t.Fatalf("proposal text: %q", text)
	}
	if prov != sid {
		t.Fatalf("provenance=%q want session id", prov)
	}
}

// TestReflectAppliesUnderAuto covers the auto-autonomy path: proposal
// stored, supersede linked in the same run, and a second proposal with a
// bad kind skipped without failing the run.
func TestReflectAppliesUnderAuto(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Settings.SetHost("memory.autonomy", "auto")
	h.SetProjectTrusted(true)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	seedSession(t, h) // the reflected session is closed, another one is live

	oldID := store.NewID()
	if err := h.DB.AddMemory(&store.Memory{
		ID: oldID, Scope: "project", Kind: "gotcha", Text: "deploy manually every time",
		ProvenanceSessionID: "seed", Origin: learn.OriginLearned,
	}); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}

	doc := `{"memories":[
		{"scope":"project","kind":"decision","text":"deploy script requires the labels sequence bumped first","tags":["deploy"],"supersedes":"` + oldID + `"},
		{"scope":"galaxy","kind":"fact","text":"wrong scope is skipped"},
		{"scope":"project","kind":"nonsense","text":"bad kind is skipped"},
		{"scope":"project","kind":"fact","text":"deploy runs from bin/deploy"}]}`
	reflectStub(t, h, doc)

	if err := h.reflectSession(context.Background(), sid); err != nil {
		t.Fatalf("reflectSession: %v", err)
	}
	rows := projectMemories(t, h)
	if len(rows) != 3 {
		t.Fatalf("memories=%d want 3 (old + 2 stored; bad entries skipped)", len(rows))
	}
	old, err := h.DB.GetMemory(oldID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if old.SupersededBy == "" {
		t.Fatal("old memory not superseded")
	}
	var fresh *store.Memory
	for _, r := range rows {
		if r.ID == old.SupersededBy {
			fresh = r
		}
	}
	if fresh == nil || fresh.Origin != learn.OriginLearned || fresh.Kind != "decision" {
		t.Fatalf("replacing memory: %+v", fresh)
	}
	if fresh.ProvenanceSessionID != sid {
		t.Fatalf("provenance=%q", fresh.ProvenanceSessionID)
	}
	if h.agents.memAuto != 0 {
		t.Fatalf("closed session spent the live budget: memAuto=%d", h.agents.memAuto)
	}
}

// TestReflectPromptWindow: tool rows stay out, the newest messages win the
// budget, and a re-reflection only shows messages after the last run.
func TestReflectPromptWindow(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	for i := range reflectMsgsMax + 5 {
		addText(t, h, sid, "user", fmt.Sprintf("note %d.", i))
	}
	msgs, _ := h.DB.Messages(sid)
	for _, tc := range []struct {
		name, seen   string
		want, absent []string
	}{
		{"last messages", "", []string{fmt.Sprintf("note %d.", reflectMsgsMax+4), "earlier messages omitted"}, []string{"tool bash", "deploy script keeps failing", "note 0."}},
		{"after seen", fmt.Sprint(len(msgs) - 2), []string{fmt.Sprintf("note %d.", reflectMsgsMax+3)}, []string{fmt.Sprintf("note %d.", reflectMsgsMax+2), "omitted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.DB.SetKV(reflectSeenKey(sid), tc.seen); err != nil {
				t.Fatal(err)
			}
			prompt, _, err := h.reflectPrompt(sid)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.want {
				if !strings.Contains(prompt, s) {
					t.Errorf("missing %q", s)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(prompt, s) {
					t.Errorf("unexpected %q", s)
				}
			}
		})
	}
}

// TestReflectPromptSkills: disabled drafts are listed too, with their
// description, so the reflector does not draft them again.
func TestReflectPromptSkills(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	h.skillsReg.set([]skills.Skill{
		{Name: "run-harness", Description: "when asked to run the harness", Source: skills.Learned},
		{Name: "cut-release", Description: "when cutting a release", Source: skills.Authored},
	}, map[string]bool{"cut-release": true})

	prompt, _, err := h.reflectPrompt(sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- run-harness (disabled): when asked to run the harness",
		"- cut-release (enabled): when cutting a release",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q in:\n%s", want, prompt)
		}
	}
}

// TestReflectPromptMemoryIndex: the index holds the newest memories plus
// older ones matching what the user said, with short ids, and says how many
// exist.
func TestReflectPromptMemoryIndex(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	// a long first message must not crowd out what the user said later
	var long strings.Builder
	for i := range 100 {
		fmt.Fprintf(&long, "w%d ", i)
	}
	addText(t, h, sid, "user", long.String())
	seedTranscript(t, h, sid) // the user says "the deploy script keeps failing"
	var ids []string
	for i, text := range append([]string{"deploy needs the labels sequence bumped"}, make([]string, reflectIndexMax+1)...) {
		if text == "" {
			text = fmt.Sprintf("fact number %d.", i)
		}
		m := &store.Memory{Scope: "project", Kind: "fact", Text: text, ProvenanceSessionID: "seed", Origin: learn.OriginLearned}
		if err := h.DB.AddMemory(m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
		time.Sleep(2 * time.Millisecond) // the first one is the oldest
	}
	prompt, _, err := h.reflectPrompt(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, fmt.Sprintf("(%d of %d: the newest", reflectIndexMax+1, reflectIndexMax+2)) ||
		strings.Count(prompt, "[project/fact] fact number") != reflectIndexMax ||
		!strings.Contains(prompt, ids[0][:8]+" [project/fact] deploy needs the labels sequence bumped") {
		t.Fatalf("index not the newest plus related:\n%s", prompt)
	}
	for _, id := range ids {
		if strings.Contains(prompt, id) {
			t.Fatalf("full id %s in index", id)
		}
	}
}

// TestReflectPromptInstructions: the reflector sees the instruction files it
// must not repeat, and an untrusted project's stay out as they do for the
// agent.
func TestReflectPromptInstructions(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("run make check before committing"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, trusted := range []bool{false, true} {
		h.SetProjectTrusted(trusted)
		prompt, _, err := h.reflectPrompt(sid)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(prompt, "## Instructions: AGENTS.md\nrun make check before committing"); got != trusted {
			t.Fatalf("trusted=%v: instructions in prompt = %v:\n%s", trusted, got, prompt)
		}
	}
}

// TestReflectSupersedeThroughReview: a queued supersede links on accept, and
// replacing a user memory queues even under auto.
func TestReflectSupersedeThroughReview(t *testing.T) {
	for _, tc := range []struct{ name, autonomy, oldOrigin, scope string }{
		{"ask queues and links on accept", "ask", learn.OriginLearned, "project"},
		{"user memory queues under auto", "auto", learn.OriginUser, "project"},
		{"a replacement in the other scope links too", "ask", learn.OriginLearned, "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.Settings.SetHost("memory.autonomy", tc.autonomy)
			sid := seedSession(t, h)
			seedTranscript(t, h, sid)
			oldID := store.NewID()
			if err := h.DB.AddMemory(&store.Memory{ID: oldID, Scope: "project", Kind: "fact", Text: "deploy by hand", ProvenanceSessionID: "seed", Origin: tc.oldOrigin}); err != nil {
				t.Fatal(err)
			}
			reflectStub(t, h, `{"memories":[{"scope":"`+tc.scope+`","kind":"fact","text":"deploy runs from bin/deploy","supersedes":"`+oldID[:8]+`"}]}`)
			if err := h.reflectSession(context.Background(), sid); err != nil {
				t.Fatal(err)
			}
			props := h.memoryProposals()
			if len(props) != 1 || len(projectMemories(t, h)) != 1 {
				t.Fatalf("want one queued proposal and nothing stored: %d", len(props))
			}
			if _, err := h.acceptProposal(props[0].ID, ""); err != nil {
				t.Fatalf("accept: %v", err)
			}
			if old, _ := h.DB.GetMemory(oldID); old.SupersededBy != props[0].ID {
				t.Fatalf("superseded_by=%q want %q", old.SupersededBy, props[0].ID)
			}
		})
	}
}

// TestReflectForgetThroughReview: an empty text with supersedes retires a
// memory only through review, even under auto, once per memory; accepting
// deletes it, rejecting keeps it, accepting after a rewrite keeps the
// rewrite, accepting after a move deletes it where it went, and a pending
// forget does not block saving its text again.
func TestReflectForgetThroughReview(t *testing.T) {
	for _, decide := range []string{"accept", "reject", "rewritten", "moved", "deleted"} {
		h, r := newTestHostDB(t)
		h.Settings.SetHost("memory.autonomy", "auto")
		h.SetProjectTrusted(true)
		sid := seedSession(t, h)
		seedTranscript(t, h, sid)
		old := &store.Memory{Scope: "project", Kind: "fact", Text: "deploy by hand", ProvenanceSessionID: "seed", Origin: learn.OriginUser}
		if err := h.DB.AddMemory(old); err != nil {
			t.Fatal(err)
		}
		reflectStub(t, h, `{"memories":[{"text":"","supersedes":"`+old.ID[:8]+`"},{"text":" ","supersedes":"`+old.ID+`"},{"text":"","supersedes":"ghost123"}]}`)
		if err := h.reflectSession(context.Background(), sid); err != nil {
			t.Fatal(err)
		}
		props := h.memoryProposals()
		if len(props) != 1 || !props[0].Forget || len(projectMemories(t, h)) != 1 {
			t.Fatalf("want one queued forget and the memory kept: %+v", props)
		}
		runUser(t, r, `
			local p = zemi.learn.proposals()
			assert(#p == 1 and p[1].forget == true and p[1].text == "deploy by hand", "forget row")
		`)
		if rows := h.memoryRows(); rows[0].Text != "forget: deploy by hand" {
			t.Fatalf("review row: %+v", rows[0])
		}
		want := 1
		switch decide {
		case "accept":
			want = 0
			if _, err := h.acceptProposal(props[0].ID, ""); err != nil {
				t.Fatal(err)
			}
		case "reject":
			if h.takeProposal(props[0].ID) == nil {
				t.Fatal("reject found nothing")
			}
		case "rewritten":
			if err := h.DB.UpdateMemoryText(old.ID, "deploy with bin/deploy", learn.OriginUser); err != nil {
				t.Fatal(err)
			}
			if _, err := h.acceptProposal(props[0].ID, ""); err == nil || len(h.memoryProposals()) != 0 {
				t.Fatalf("stale forget: err %v, proposals %d", err, len(h.memoryProposals()))
			}
		case "moved":
			// a pending suggestion with the same text is no stored copy to merge into
			if err := h.addProposal(memoryProposal{Memory: store.Memory{Scope: "user", Kind: "fact", Text: "deploy by hand"}}); err != nil {
				t.Fatal(err)
			}
			if err := h.moveMemory(old.ID, "project", "user"); err != nil {
				t.Fatal(err)
			}
			if m, err := h.GlobalDB.GetMemory(old.ID); err != nil || m.Text != "deploy by hand" {
				t.Fatalf("move lost the memory: %v", err)
			}
			want = 0
			if _, err := h.acceptProposal(props[0].ID, ""); err != nil {
				t.Fatal(err)
			}
		case "deleted":
			// the pending forget holds no copy of the text
			if err := h.DB.DeleteMemory(old.ID); err != nil {
				t.Fatal(err)
			}
			if err := h.Remember("deploy by hand"); err != nil {
				t.Fatal(err)
			}
		}
		if all, _ := h.memoryList(true); len(all) != want {
			t.Fatalf("%s: memories left %d", decide, len(all))
		}
	}
}

// TestReflectScrubAndInstructionShape checks that proposal text passes the
// intake scrubber (secrets redacted) and that instruction-shaped text
// queues even under auto autonomy.
func TestReflectScrubAndInstructionShape(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Settings.SetHost("memory.autonomy", "auto")
	h.SetProjectTrusted(true)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)

	doc := `{"memories":[
		{"scope":"project","kind":"fact","text":"the api_key = sk-abcdefghijklmnopqrs lives in deploy"},
		{"scope":"project","kind":"preference","text":"always run the deploy before reviews"}]}`
	reflectStub(t, h, doc)

	if err := h.reflectSession(context.Background(), sid); err != nil {
		t.Fatalf("reflectSession: %v", err)
	}
	rows := projectMemories(t, h)
	if len(rows) != 1 {
		t.Fatalf("memories=%d want 1 (scrubbed secret text must store, instruction-shaped must queue)", len(rows))
	}
	if strings.Contains(rows[0].Text, "sk-abcdefghijklmnopqrs") {
		t.Fatalf("secret survived: %q", rows[0].Text)
	}
	if !strings.Contains(rows[0].Text, "[redacted") {
		t.Fatalf("expected redaction marker: %q", rows[0].Text)
	}
	if queued := len(h.memoryProposals()); queued != 1 {
		t.Fatalf("instruction-shaped proposal not queued: %d", queued)
	}
}

// TestReflectBadOutput pins the failure path: prose without JSON, broken
// JSON and an empty session all fail the run without storing anything.
func TestReflectBadOutput(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)

	for _, out := range []string{"no json here at all", `{"memories": [`} {
		reflectStub(t, h, out)
		if err := h.reflectSession(context.Background(), sid); err == nil {
			t.Fatalf("expected error for %q", out)
		}
	}

	reflectStub(t, h, `{"memories":[]}`)
	empty := store.NewID()
	if err := h.DB.CreateSession(empty, "", "empty", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := h.reflectSession(context.Background(), empty); err == nil || !strings.Contains(err.Error(), "no messages") {
		t.Fatalf("empty session: %v", err)
	}
}

// TestReflectTimeout skips the run when the model outlives the context:
// the deadline lives in the caller (runReflectDetached), so the test
// applies one.
func TestReflectTimeout(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	m := reflectStub(t, h, `{"memories":[]}`)
	m.block = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.reflectSession(ctx, sid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

// TestReflectGate covers the every-N trigger math: 0 disables, N=2 runs
// on every second closed session (the counter carries across calls).
func TestReflectGate(t *testing.T) {
	h, _ := newTestHostDB(t)
	if h.reflectEveryN() != 1 {
		t.Fatalf("default every_n=%d", h.reflectEveryN())
	}
	h.Settings.SetHost("reflector.every_n_sessions", 3.0) // config files decode numbers as float64
	if h.reflectEveryN() != 3 {
		t.Fatalf("float every_n=%d", h.reflectEveryN())
	}
	h.Settings.SetHost("reflector.every_n_sessions", 0)
	h.reflectOnClose("prev")
	if h.agents.reflected != 1 {
		t.Fatalf("counter must bump even when disabled: %d", h.agents.reflected)
	}

	h.Settings.SetHost("reflector.every_n_sessions", 2)
	m := reflectStub(t, h, `{"memories":[]}`)
	closed := seedSession(t, h)
	seedTranscript(t, h, closed)
	h.agents.mu.Lock()
	h.agents.reflected = 0
	h.agents.mu.Unlock()
	// The gate returns before a detached run would set reflecting.
	h.reflectOnClose(closed)
	if h.reflecting.Load() {
		t.Fatal("reflected on a non-multiple of 2")
	}
	h.reflectOnClose(closed)
	waitFor(t, func() error {
		if m.calls.Load() != 1 {
			return errors.New("reflector did not run on the 2nd close")
		}
		return nil
	})
}

// TestReflectSerializesRuns pins the one-run-at-a-time rule: a second
// detached run while one holds the lock is skipped without touching the
// model.
func TestReflectSerializesRuns(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	m := reflectStub(t, h, `{"memories":[]}`)
	h.reflectMu.Lock()
	if h.runReflectDetached(sid) || m.calls.Load() != 0 {
		t.Fatal("second run was not skipped")
	}
	h.reflectMu.Unlock()
}

// TestReflectCommand covers :reflect: no session errors, live session
// starts a detached run.
func TestReflectCommand(t *testing.T) {
	h, _ := newTestHostDB(t)
	if err := h.reflectCommand(); err == nil {
		t.Fatal("expected error without a session")
	}
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	m := reflectStub(t, h, `{"memories":[]}`)
	if err := h.reflectCommand(); err != nil {
		t.Fatalf("reflectCommand: %v", err)
	}
	waitFor(t, func() error {
		if m.calls.Load() != 1 {
			return errors.New("detached run never started")
		}
		return nil
	})
}
