package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
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

// addToolResult stores one tool call and its result the way a turn
// persists them: the call part on an assistant row, the result on a tool
// row; an error result when errText is set.
func addToolResult(t *testing.T, h *Host, sid, name, text, errText string) {
	t.Helper()
	var out fantasy.ToolResultOutputContent = fantasy.ToolResultOutputContentText{Text: text}
	if errText != "" {
		out = fantasy.ToolResultOutputContentError{Error: errors.New(errText)}
	}
	for _, m := range []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "c-" + name, ToolName: name, Input: "{}"}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "c-" + name, Output: out}}},
	} {
		parts, err := json.Marshal(m)
		if err == nil {
			err = h.DB.AddMessage(&store.Message{SessionID: sid, Role: string(m.Role), PartsJSON: string(parts)})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// learnedMemory stores one learned memory in scope and returns its id.
func learnedMemory(t *testing.T, h *Host, scope, text, quote string) string {
	t.Helper()
	m := &store.Memory{Scope: scope, Text: text, Quote: quote, ProvenanceSessionID: "seed", Origin: learn.OriginLearned}
	if err := h.memoryDB(scope).AddMemory(m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// TestReflectApply pins what becomes of a proposal: it is parsed out of
// prose, scrubbed and gated like a live memory (stored under auto, queued
// under ask or when instruction-shaped, a user memory needs a quote the
// user wrote, a replaced memory is superseded, a retirement waits for
// review, a near-duplicate and a skills key do nothing), provenance is the
// read session's, whose budget and ledger it uses, and only what the
// prompt held is marked seen.
func TestReflectApply(t *testing.T) {
	const fact = "the deploy script requires the labels sequence bumped first"
	for _, c := range []struct {
		name, autonomy string
		seed           func(t *testing.T, h *Host, sid string) string // returns the id OLD stands for
		doc            string
		stored, queued int // memories this run stored live, proposals it queued
		check          func(t *testing.T, h *Host, sid string)
	}{
		{name: "a project fact stores under auto", doc: "Sure! Here it is:\n" + `{"memories":[{"scope":"project","text":"` + fact + `"}]}` + "\nHope that helps.", stored: 1,
			check: func(t *testing.T, h *Host, sid string) {
				if s, _ := h.DB.GetSession(sid); s == nil || s.CompletionTokens != 7 || h.agents.sessOut != 0 {
					t.Fatalf("reflector usage goes to the session it read: %+v live=%d", s, h.agents.sessOut)
				}
			}},
		{name: "ask queues it", autonomy: "ask", doc: `{"memories":[{"scope":"project","text":"` + fact + `"}]}`, queued: 1,
			check: func(t *testing.T, h *Host, sid string) {
				if p := h.memoryProposals(); p[0].Text != fact || p[0].ProvenanceSessionID != sid {
					t.Fatalf("proposal: %+v", p[0])
				}
			}},
		{name: "a user preference with the user's words stores", doc: `{"memories":[{"scope":"user","text":"the user wants short replies","quote":"Short replies please"}]}`, stored: 1},
		{name: "a user preference without a quote queues", doc: `{"memories":[{"scope":"user","text":"the user wants short replies"}]}`, queued: 1},
		{name: "a quote the user never wrote drops the memory", doc: `{"memories":[{"scope":"user","text":"the user wants short replies","quote":"long replies please"}]}`},
		{name: "replaces by the index's id prefix supersedes the old memory, near-duplicate or not",
			seed: func(t *testing.T, h *Host, sid string) string {
				return learnedMemory(t, h, "project", "the deploy script requires the labels sequence bumped", "")[:8]
			},
			doc: `{"memories":[{"scope":"project","text":"` + fact + `","replaces":"OLD"}]}`, stored: 1,
			check: func(t *testing.T, h *Host, sid string) {
				if !slices.ContainsFunc(projectMemories(t, h), func(m *store.Memory) bool { return m.SupersededBy != "" }) {
					t.Fatal("old memory not superseded")
				}
			}},
		{name: "replaces under ask queues and links on accept", autonomy: "ask",
			seed: func(t *testing.T, h *Host, sid string) string {
				return learnedMemory(t, h, "project", "deploy by hand", "")
			},
			doc: `{"memories":[{"scope":"project","text":"` + fact + `","replaces":"OLD"}]}`, queued: 1,
			check: func(t *testing.T, h *Host, sid string) {
				p := h.memoryProposals()[0]
				again, _ := parseReflectProposal(`{"memories":[{"scope":"project","text":"` + fact + `"}]}`)
				if _, queued, _, _ := h.applyReflectProposal(sid, again); queued != 0 {
					t.Fatalf("a duplicate of a pending proposal counted as queued: %d", queued)
				}
				if _, err := h.acceptProposal(p.ID, ""); err != nil {
					t.Fatal(err)
				}
				if old, _ := h.DB.GetMemory(p.Supersedes); old.SupersededBy != p.ID {
					t.Fatalf("superseded_by=%q want %q", old.SupersededBy, p.ID)
				}
			}},
		{name: "replaces with empty text queues a forget",
			seed: func(t *testing.T, h *Host, sid string) string {
				return learnedMemory(t, h, "project", "deploy by hand", "")
			},
			doc: `{"memories":[{"text":"","replaces":"OLD"}]}`, queued: 1,
			check: func(t *testing.T, h *Host, sid string) {
				if p := h.memoryProposals(); !p[0].Forget || len(projectMemories(t, h)) != 1 {
					t.Fatalf("want a queued forget and the memory kept: %+v", p)
				}
			}},
		{name: "a near-duplicate of an indexed memory is skipped",
			seed: func(t *testing.T, h *Host, sid string) string { return learnedMemory(t, h, "project", fact, "") },
			doc:  `{"memories":[{"scope":"project","text":"the deploy script requires the labels sequence bumped"}]}`},
		{name: "instruction-shaped text queues", doc: `{"memories":[{"scope":"project","text":"always run the deploy before reviews"}]}`, queued: 1},
		{name: "a secret is refused", doc: `{"memories":[{"scope":"project","text":"the api_key = sk-abcdefghijklmnopqrs lives in deploy"}]}`,
			check: func(t *testing.T, h *Host, sid string) {
				if len(projectMemories(t, h)) != 0 {
					t.Fatal("a redacted memory was stored")
				}
			}},
		{name: "a bad scope is skipped", doc: `{"memories":[{"scope":"galaxy","text":"` + fact + `"}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.Settings.SetHost("memory.autonomy", cmp.Or(c.autonomy, "auto"))
			h.SetProjectTrusted(true)
			sid := seedSession(t, h)
			seedTranscript(t, h, sid)
			addText(t, h, sid, "user", "short   replies please, and the deploy script keeps failing")
			id := ""
			if c.seed != nil {
				id = c.seed(t, h, sid)
			}
			seedSession(t, h) // the reflected session is closed, another one is live
			m := reflectStub(t, h, strings.ReplaceAll(c.doc, "OLD", id))
			m.during = func() { addText(t, h, sid, "user", "sent while the reflector ran") }
			if err := h.reflectSession(t.Context(), sid); err != nil {
				t.Fatalf("reflectSession: %v", err)
			}
			if msgs, seen, _, _ := h.reflectDelta(sid); seen != len(msgs)-1 {
				t.Fatalf("seen=%d of %d: a message the run never read was marked seen", seen, len(msgs))
			}
			stored := 0
			for _, m := range must(h.memoryList(false)) {
				if m.ProvenanceSessionID == sid {
					stored++
				}
			}
			if queued := len(h.memoryProposals()); stored != c.stored || queued != c.queued {
				t.Fatalf("stored %d queued %d, want %d and %d", stored, queued, c.stored, c.queued)
			}
			if h.agents.memAuto != 0 {
				t.Fatalf("closed session spent the live budget: memAuto=%d", h.agents.memAuto)
			}
			if c.check != nil {
				c.check(t, h, sid)
			}
		})
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestReflectPrompt pins what the reflector reads: the delta's tail with a
// compaction summary labeled as one and secrets redacted, the failed tool
// results, this session's pending proposals, the whole index with budget
// use and quotes, the recipes, and a trusted project's instruction files.
func TestReflectPrompt(t *testing.T) {
	const secret = "api_key = sk-abcdefghijklmnopqrs"
	failedLine := "- bash: " + learn.ScrubShapes("exit status 1: "+secret)
	notes := func(t *testing.T, h *Host, sid string) {
		for i := range reflectMsgsMax + 5 {
			addText(t, h, sid, "user", fmt.Sprintf("note %d.", i))
		}
		addText(t, h, sid, "user", compactPrefix+"the gist"+compactSuffix)
	}
	for _, c := range []struct {
		name         string
		seed         func(t *testing.T, h *Host, sid string)
		want, absent []string
	}{
		{name: "the tail of the delta", seed: notes,
			want:   []string{fmt.Sprintf("note %d.", reflectMsgsMax+4), "earlier messages omitted", "summary: the gist"},
			absent: []string{"tool bash", "deploy script keeps failing", "note 0."}},
		{name: "only what came after seen", seed: func(t *testing.T, h *Host, sid string) {
			notes(t, h, sid)
			msgs, _ := h.DB.Messages(sid)
			if err := h.DB.SetKV(reflectSeenKey(sid), strconv.Itoa(len(msgs)-3)); err != nil {
				t.Fatal(err)
			}
		}, want: []string{fmt.Sprintf("note %d.", reflectMsgsMax+3)}, absent: []string{fmt.Sprintf("note %d.", reflectMsgsMax+2), "omitted"}},
		{name: "failed tool results, first line, scrubbed, once after a compaction", seed: func(t *testing.T, h *Host, sid string) {
			addToolResult(t, h, sid, "read", "package main", "")
			addToolResult(t, h, sid, "bash", "", "exit status 1: "+secret+"\nsecond line")
			msgs, _ := h.DB.Messages(sid)
			var tail []*store.Message
			for _, m := range msgs[len(msgs)-2:] {
				tail = append(tail, &store.Message{Role: compactTailRole, PartsJSON: m.PartsJSON})
			}
			parts, _ := json.Marshal(agent.UserMessage(compactPrefix + "the gist" + compactSuffix))
			if err := h.DB.AddCompaction(sid, &store.Message{Role: "user", PartsJSON: string(parts)}, tail); err != nil {
				t.Fatal(err)
			}
		}, want: []string{"# Failed tool calls\n" + failedLine + "\n"}, absent: []string{"package main", "second line", "sk-abcdefghijklmnopqrs", failedLine + "\n" + failedLine}},
		{name: "a secret in a user line is redacted", seed: func(t *testing.T, h *Host, sid string) {
			addText(t, h, sid, "user", "the "+secret+" lives in deploy")
		}, want: []string{"user: the [redacted"}, absent: []string{"sk-abcdefghijklmnopqrs"}},
		{name: "a user line keeps only what was typed", seed: func(t *testing.T, h *Host, sid string) {
			addText(t, h, sid, "user", "read @notes.md"+agent.AttachMarker+"\nattached notes\n</attached-files>")
			addText(t, h, sid, "user", agent.Note("mark it done"))
		}, want: []string{"user: read @notes.md\n"}, absent: []string{"attached notes", "mark it done"}},
		{name: "this session's pending proposals only", seed: func(t *testing.T, h *Host, sid string) {
			for _, m := range []store.Memory{
				{Scope: "project", Text: "ours to review", ProvenanceSessionID: sid},
				{Scope: "user", Text: "fetched elsewhere", ProvenanceSessionID: "other"},
			} {
				if err := h.addProposal(memoryProposal{Memory: m}); err != nil {
					t.Fatal(err)
				}
			}
		}, want: []string{"[project] ours to review"}, absent: []string{"fetched elsewhere"}},
		{name: "the index with budget use and quotes", seed: func(t *testing.T, h *Host, sid string) {
			learnedMemory(t, h, "project", "deploy needs the labels sequence bumped", "")
			learnedMemory(t, h, "user", "the user prefers short replies", "keep it short")
		}, want: []string{"# Memory index (project 0.1/3 KB, user 0.1/3 KB)", " project learned] deploy needs the labels sequence bumped", " user learned] the user prefers short replies\n  quote: \"keep it short\""}},
		{name: "a scope past 80% of its budget is marked full", seed: func(t *testing.T, h *Host, sid string) {
			for i := range 9 {
				learnedMemory(t, h, "project", strings.Repeat(string(rune('a'+i)), 280), "")
			}
		}, want: []string{"project 2.7/3 KB (full), user 0.0/3 KB)"}},
		{name: "recipes and project skills by name, no global skills", seed: func(t *testing.T, h *Host, sid string) {
			writeSkill(t, filepath.Join(h.Dirs.ProjectDir, ".zemi", "skills"), "lint", "project lint", "")
			h.SetProjectTrusted(true)
			writeSkill(t, h.recipesDir(), "rig", "recipe rig", "")
			h.skillsReg.set([]skills.Skill{{Name: "cut-release", Description: "when cutting a release", Source: skills.Authored}}, map[string]bool{"cut-release": true})
		}, want: []string{"# Recipes and project skills\n- lint: project lint\n- rig: recipe rig"}, absent: []string{"cut-release"}},
		{name: "an untrusted project's instruction files stay out", seed: func(t *testing.T, h *Host, sid string) {
			if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("run make check before committing"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, absent: []string{"run make check before committing"}},
		{name: "a trusted project's instruction files", seed: func(t *testing.T, h *Host, sid string) {
			if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("run make check before committing"), 0o644); err != nil {
				t.Fatal(err)
			}
			h.SetProjectTrusted(true)
		}, want: []string{"## Instructions: AGENTS.md\nrun make check before committing"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			sid := seedSession(t, h)
			seedTranscript(t, h, sid)
			c.seed(t, h, sid)
			prompt, _, err := h.reflectPrompt(sid)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range c.want {
				if !strings.Contains(prompt, s) {
					t.Errorf("missing %q in:\n%s", s, prompt)
				}
			}
			for _, s := range c.absent {
				if strings.Contains(prompt, s) {
					t.Errorf("unexpected %q in:\n%s", s, prompt)
				}
			}
		})
	}
}

// TestReflectForgetThroughReview: an empty text with replaces retires a
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
		reflectStub(t, h, `{"memories":[{"text":"","replaces":"`+old.ID[:8]+`"},{"text":" ","replaces":"`+old.ID+`"},{"text":"","replaces":"ghost123"}]}`)
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
			if v, _ := h.DB.GetKV(reflectClaimKey(sid)); v != "" {
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
		{name: "another window's claim, at any count, is skipped", seed: seedTranscript, act: func(t *testing.T, h *Host, sid string, m *reflectStubModel) {
			if _, err := h.DB.SetKVOnce(reflectClaimKey(sid), "1"); err != nil {
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
