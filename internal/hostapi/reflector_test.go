package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	lastOpts   atomic.Pointer[fantasy.ProviderOptions] // the last Generate call's provider options
	streams    atomic.Int32
	overflow   atomic.Int32                          // Stream calls left that fail as context too large
	noImages   atomic.Int32                          // Stream calls left that fail as a 400 over image input
	toolStep   atomic.Int64                          // context the next Stream call reports along with a tool call; 0 = text
	failOnce   atomic.Pointer[fantasy.ProviderError] // the next Stream call's error
	cutReply   bool                                  // Generate stops at the output limit
	during     func()                                // runs inside Generate, while the model "thinks"
}

func (m *reflectStubModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.calls.Add(1)
	m.lastOpts.Store(&call.ProviderOptions)
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
		Usage:        fantasy.Usage{OutputTokens: 7},
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

const proposalDoc = `{"memories":[{"scope":"project","text":"UpsertLabels must stay read-then-insert; ON CONFLICT burns the labels sequence",
	"supersedes":""}]}`

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
	if msgs, seen, _ := h.reflectDelta(sid); seen != len(msgs)-1 {
		t.Fatalf("seen=%d of %d: a message the run never read was marked seen", seen, len(msgs))
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
// stored, supersede linked in the same run, and a proposal with a bad
// scope skipped without failing the run.
func TestReflectAppliesUnderAuto(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Settings.SetHost("memory.autonomy", "auto")
	h.SetProjectTrusted(true)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	seedSession(t, h) // the reflected session is closed, another one is live

	oldID := store.NewID()
	if err := h.DB.AddMemory(&store.Memory{
		ID: oldID, Scope: "project", Text: "deploy manually every time",
		ProvenanceSessionID: "seed", Origin: learn.OriginLearned,
	}); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}

	doc := `{"memories":[
		{"scope":"project","text":"deploy script requires the labels sequence bumped first","supersedes":"` + oldID + `"},
		{"scope":"galaxy","text":"wrong scope is skipped"},
		{"scope":"project","text":"deploy runs from bin/deploy"}]}`
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
	if fresh == nil || fresh.Origin != learn.OriginLearned {
		t.Fatalf("replacing memory: %+v", fresh)
	}
	if fresh.ProvenanceSessionID != sid {
		t.Fatalf("provenance=%q", fresh.ProvenanceSessionID)
	}
	if h.agents.memAuto != 0 {
		t.Fatalf("closed session spent the live budget: memAuto=%d", h.agents.memAuto)
	}
	if s, _ := h.DB.GetSession(sid); s == nil || s.CompletionTokens != 7 || h.agents.sessOut != 0 {
		t.Fatalf("reflector usage goes to the session it read: %+v live=%d", s, h.agents.sessOut)
	}
}

// TestReflectPromptWindow: tool rows stay out, a compaction summary is
// labeled as one, the newest messages win the budget, and a re-reflection
// only shows messages after the last run.
func TestReflectPromptWindow(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	for i := range reflectMsgsMax + 5 {
		addText(t, h, sid, "user", fmt.Sprintf("note %d.", i))
	}
	addText(t, h, sid, "user", compactPrefix+"the gist"+compactSuffix)
	msgs, _ := h.DB.Messages(sid)
	for _, tc := range []struct {
		name, seen   string
		want, absent []string
	}{
		{"last messages", "", []string{fmt.Sprintf("note %d.", reflectMsgsMax+4), "earlier messages omitted", "summary: the gist"}, []string{"tool bash", "deploy script keeps failing", "note 0.", "user: The earlier part"}},
		{"after seen", fmt.Sprint(len(msgs) - 3), []string{fmt.Sprintf("note %d.", reflectMsgsMax+3)}, []string{fmt.Sprintf("note %d.", reflectMsgsMax+2), "omitted"}},
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

// TestReflectPromptPending: only this session's suggestions show, since
// what the reflector returns is stored under this session's autonomy.
func TestReflectPromptPending(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	for _, m := range []store.Memory{
		{Scope: "project", Text: "ours to review", ProvenanceSessionID: sid},
		{Scope: "user", Text: "fetched elsewhere", ProvenanceSessionID: "other"},
	} {
		if err := h.addProposal(memoryProposal{Memory: m}); err != nil {
			t.Fatal(err)
		}
	}
	prompt, _, err := h.reflectPrompt(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "[project] ours to review") || strings.Contains(prompt, "fetched elsewhere") {
		t.Fatalf("pending block not limited to this session:\n%s", prompt)
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

// TestReflectPromptMemoryIndex: the index lists every live memory of both
// scopes with short ids.
func TestReflectPromptMemoryIndex(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	var ids []string
	for _, m := range []*store.Memory{
		{Scope: "project", Text: "deploy needs the labels sequence bumped"},
		{Scope: "user", Text: "the user prefers short replies"},
	} {
		m.ProvenanceSessionID, m.Origin = "seed", learn.OriginLearned
		if err := h.memoryDB(m.Scope).AddMemory(m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	prompt, _, err := h.reflectPrompt(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, ids[0][:8]+" [project] deploy needs the labels sequence bumped") ||
		!strings.Contains(prompt, ids[1][:8]+" [user] the user prefers short replies") {
		t.Fatalf("index:\n%s", prompt)
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
// replacing a user memory or one in the other scope queues even under auto,
// and the same suggestion again queues and counts nothing.
func TestReflectSupersedeThroughReview(t *testing.T) {
	for _, tc := range []struct{ name, autonomy, oldOrigin, scope string }{
		{"ask queues and links on accept", "ask", learn.OriginLearned, "project"},
		{"user memory queues under auto", "auto", learn.OriginUser, "project"},
		{"another scope's memory queues under auto", "auto", learn.OriginLearned, "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.Settings.SetHost("memory.autonomy", tc.autonomy)
			h.SetProjectTrusted(true)
			sid := seedSession(t, h)
			seedTranscript(t, h, sid)
			oldID := store.NewID()
			if err := h.DB.AddMemory(&store.Memory{ID: oldID, Scope: "project", Text: "deploy by hand", ProvenanceSessionID: "seed", Origin: tc.oldOrigin}); err != nil {
				t.Fatal(err)
			}
			reflectStub(t, h, `{"memories":[{"scope":"`+tc.scope+`","text":"deploy runs from bin/deploy","supersedes":"`+oldID[:8]+`"}]}`)
			if err := h.reflectSession(context.Background(), sid); err != nil {
				t.Fatal(err)
			}
			props := h.memoryProposals()
			if len(props) != 1 || len(projectMemories(t, h)) != 1 {
				t.Fatalf("want one queued proposal and nothing stored: %d", len(props))
			}
			if tc.scope == "user" && !strings.HasSuffix(h.memoryRows()[0].Text, "(replaces project memory "+oldID[:8]+")") {
				t.Fatalf("review row: %q", h.memoryRows()[0].Text)
			}
			again, _ := parseReflectProposal(`{"memories":[{"scope":"` + tc.scope + `","text":"deploy runs from bin/deploy"}]}`)
			if _, queued, _, _ := h.applyReflectProposal(sid, again); queued != 0 {
				t.Fatalf("a duplicate counted as queued: %d", queued)
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
	for _, decide := range []string{"accept", "reject", "rewritten", "moved", "merged", "deleted"} {
		h, r := newTestHostDB(t)
		h.Settings.SetHost("memory.autonomy", "auto")
		h.SetProjectTrusted(true)
		sid := seedSession(t, h)
		seedTranscript(t, h, sid)
		old := &store.Memory{Scope: "project", Text: "deploy by hand", ProvenanceSessionID: "seed", Origin: learn.OriginUser}
		if err := h.DB.AddMemory(old); err != nil {
			t.Fatal(err)
		}
		reflectStub(t, h, `{"memories":[{"text":"","supersedes":"`+old.ID[:8]+`"},{"text":" ","supersedes":"`+old.ID+`"},{"text":"","supersedes":"ghost123"}]}`)
		if err := h.reflectSession(t.Context(), sid); err != nil {
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
			if err := h.addProposal(memoryProposal{Scope: "user", Text: "deploy by hand"}); err != nil {
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
		case "merged":
			if err := h.GlobalDB.AddMemory(&store.Memory{Scope: "user", Text: "deploy by hand", ProvenanceSessionID: "seed", Origin: learn.OriginLearned}); err != nil {
				t.Fatal(err)
			}
			if err := h.moveMemory(old.ID, "project", "user"); err != nil {
				t.Fatal(err)
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
		{"scope":"project","text":"the api_key = sk-abcdefghijklmnopqrs lives in deploy"},
		{"scope":"project","text":"always run the deploy before reviews"}]}`
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

// TestReflectBadOutput pins the failure path: prose without JSON and broken
// JSON fail the run without storing anything.
func TestReflectBadOutput(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	seedTranscript(t, h, sid)
	for _, out := range []string{"no json here at all", `{"memories": [`} {
		reflectStub(t, h, out)
		if err := h.reflectSession(context.Background(), sid); !errors.Is(err, errNoProposal) {
			t.Fatalf("%q: %v, want errNoProposal", out, err)
		}
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

// reflectNow is reflectOnClose run inline: it reports whether a run
// happened.
func reflectNow(h *Host, sid string) bool {
	n, claim := h.claimReflect(sid, h.reflectDue(sid))
	return claim != "" && h.reflectClaimed(sid, claim, n)
}

func addTool(t *testing.T, h *Host, sid string, ok bool) {
	t.Helper()
	if err := h.DB.AddToolCall(&store.Message{SessionID: sid, Role: "tool", PartsJSON: `{}`}, &store.ToolCall{Tool: "bash", OK: ok}); err != nil {
		t.Fatal(err)
	}
}

func setSeen(t *testing.T, h *Host, sid string) {
	t.Helper()
	msgs, _ := h.DB.Messages(sid)
	if err := h.DB.SetKV(reflectSeenKey(sid), strconv.Itoa(len(msgs))); err != nil {
		t.Fatal(err)
	}
}

// TestReflectTrigger pins when a session is reflected: the delta since the
// seen count needs two user messages and a tool call or one failed tool
// call, the startup sweep takes only idle sessions, a claim runs once
// across windows and runs, and a failed delta is given up on.
func TestReflectTrigger(t *testing.T) {
	exchange := func(t *testing.T, h *Host, sid string) {
		addText(t, h, sid, "user", "hi")
		addText(t, h, sid, "assistant", "hello")
	}
	twoUsers := func(t *testing.T, h *Host, sid string) {
		exchange(t, h, sid)
		addText(t, h, sid, "user", "and this")
	}
	for _, c := range []struct {
		name  string
		seed  func(t *testing.T, h *Host, sid string)
		act   func(t *testing.T, h *Host, sid string, m *reflectStubModel)
		due   bool  // after act
		calls int32 // model calls act made
		badge bool  // act left an /errors entry
	}{
		{name: "one user row and an ok tool", seed: func(t *testing.T, h *Host, sid string) {
			exchange(t, h, sid)
			addTool(t, h, sid, true)
		}},
		{name: "two user rows and no tool", seed: twoUsers},
		{name: "two user rows and a tool", seed: func(t *testing.T, h *Host, sid string) {
			twoUsers(t, h, sid)
			addTool(t, h, sid, true)
		}, due: true},
		{name: "one user row and a failed tool", seed: seedTranscript, due: true},
		{name: "a summary row is not the user", seed: func(t *testing.T, h *Host, sid string) {
			addText(t, h, sid, "user", compactPrefix+"earlier"+compactSuffix)
			exchange(t, h, sid)
			addTool(t, h, sid, true)
		}},
		{name: "seen leaves only tool rows", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			setSeen(t, h, sid)
			addTool(t, h, sid, true)
		}},
		{name: "rewind clamps seen", seed: func(t *testing.T, h *Host, sid string) {
			twoUsers(t, h, sid)
			addText(t, h, sid, "assistant", "sure")
			addTool(t, h, sid, true)
		}, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			setSeen(t, h, sid)
			msgs, _ := h.DB.Messages(sid)
			if _, err := h.RewindTo(msgs[2].ID); err != nil {
				t.Fatal(err)
			}
			// one user row after the cut: not due unless the whole session counts again
			addText(t, h, sid, "user", "again")
			addTool(t, h, sid, true)
		}},
		{name: "sweep skips a session touched a minute ago", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			h.reflectSweep(time.Now().Add(-time.Minute))
		}, due: true},
		{name: "sweep runs an idle due session", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			h.reflectSweep(time.Now().Add(time.Minute))
		}, calls: 1},
		{name: "prose sets seen without an error badge", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			m.resp = "no json here"
			if !reflectNow(h, sid) || h.agents.reflectErr == nil {
				t.Fatal("run did not happen or left no /health news")
			}
		}, calls: 1},
		{name: "two provider errors set seen", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			m.err = errors.New("boom")
			if !reflectNow(h, sid) || h.reflectDue(sid) == 0 {
				t.Fatal("the first failure must leave the delta due")
			}
			reflectNow(h, sid)
		}, calls: 2, badge: true},
		{name: "two concurrent /reflect start one run", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			m.block = 100 * time.Millisecond
			if err := h.reflectCommand(); err != nil {
				t.Fatal(err)
			}
			if err := h.reflectCommand(); err == nil {
				t.Fatal("the second /reflect must be refused")
			}
			waitFor(t, func() error {
				if m.calls.Load() == 0 || h.reflecting.Load() {
					return errors.New("still reflecting")
				}
				return nil
			})
		}, calls: 1},
		{name: "/reflect while a run is active is refused before it claims", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			h.reflecting.Store(true)
			defer h.reflecting.Store(false)
			if err := h.reflectCommand(); err == nil || !strings.Contains(err.Error(), "already active") {
				t.Fatalf("err = %v", err)
			}
			if v, _ := h.DB.GetKV(reflectClaimKey(sid, h.reflectDue(sid))); v != "" {
				t.Fatal("a refused /reflect left a claim")
			}
		}, due: true},
		{name: "/reflect skips the floor", seed: exchange, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			if err := h.reflectCommand(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() error {
				if m.calls.Load() == 0 || h.reflecting.Load() {
					return errors.New("still reflecting")
				}
				return nil
			})
		}, calls: 1},
		{name: "/reflect with nothing new says so", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			setSeen(t, h, sid)
			if err := h.reflectCommand(); err == nil || !strings.Contains(err.Error(), "nothing new") {
				t.Fatalf("err = %v", err)
			}
		}},
		{name: "another window's claim at the same count is skipped", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			if _, err := h.DB.SetKVOnce(reflectClaimKey(sid, h.reflectDue(sid)), "1"); err != nil {
				t.Fatal(err)
			}
			if reflectNow(h, sid) {
				t.Fatal("ran over another window's claim")
			}
		}, due: true},
		{name: "a run while another is active is skipped and releases its claim", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			h.reflectMu.Lock()
			ran := reflectNow(h, sid)
			h.reflectMu.Unlock()
			if ran || !reflectNow(h, sid) {
				t.Fatalf("ran under the lock %v, or the claim was not released", ran)
			}
		}, calls: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			sid := seedSession(t, h)
			m := reflectStub(t, h, `{"memories":[]}`)
			c.seed(t, h, sid)
			if c.act != nil {
				c.act(t, h, sid, m)
			}
			if due := h.reflectDue(sid) != 0; due != c.due {
				t.Errorf("due = %v, want %v", due, c.due)
			}
			if m.calls.Load() != c.calls {
				t.Errorf("model calls = %d, want %d", m.calls.Load(), c.calls)
			}
			if badge := h.errs.entries != nil; badge != c.badge {
				t.Errorf("error badge = %v, want %v: %+v", badge, c.badge, h.errs.entries)
			}
		})
	}
}
