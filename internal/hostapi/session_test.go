package hostapi

import (
	"testing"
	"time"
)

func waitIdle(t *testing.T, h *Host) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.AgentActive() {
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish within 3s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSessionLifecycle: /new starts clean, /resume rebuilds the transcript
// and the model history from the store, in order.
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
}
