package hostapi

import (
	"errors"
	"os"
	"testing"
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
	// The composer history of a resumed session is what was typed in it.
	if got := h.InputHistory(first); len(got) != 2 || got[0] != "one" || got[1] != "three" {
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
