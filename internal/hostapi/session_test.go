package hostapi

import (
	"errors"
	"os"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
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

	if err := h.ResumeSession(first); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if tr := h.Session().Transcript; len(tr) != 2 || tr[0].Text != "one" || tr[1].Text != "ok" {
		t.Fatalf("resumed transcript = %+v", tr)
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
	if err := h.DeleteSession(first); err == nil {
		t.Fatal("deleted the live session")
	}
	if err := h.ResumeSession(second); err != nil {
		t.Fatal(err)
	}
	pad := h.scratchFor(first)
	if err := os.MkdirAll(pad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteSession(first); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if _, err := os.Stat(pad); err == nil {
			return errors.New("scratchpad still there")
		}
		if _, err := h.DB.GetSession(first); err == nil {
			return errors.New("session still stored")
		}
		return nil
	})
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
	h.recordToolCall(sid, "c1", "edit", "12 +added\n13 -gone", true)

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
