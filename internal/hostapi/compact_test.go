package hostapi

import "testing"

// TestCompaction: a prompt past the threshold summarizes the history
// before the next turn, the summary becomes the session's resume point,
// and /resume starts from it.
func TestCompaction(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	h.reflectMu.Lock() // no detached reflect racing the DB teardown
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
	h.agents.mu.Lock()
	h.agents.ctxWin, h.agents.lastIn = 100, 90
	h.agents.mu.Unlock()
	send("two")
	if n := m.lastPrompt.Load(); n != 3 {
		t.Fatalf("compacted prompt has %d messages, want 3 (system, summary, user)", n)
	}
	compacted := false
	for _, c := range h.Session().Transcript {
		compacted = compacted || c.Kind == "compact"
	}
	if !compacted {
		t.Fatal("transcript misses the compaction row")
	}
	if sess, err := h.DB.GetSession(first); err != nil || sess.SummaryMessageID == "" {
		t.Fatalf("summary pointer: %+v %v", sess, err)
	}

	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.ResumeSession(first); err != nil {
		t.Fatal(err)
	}
	send("three")
	if n := m.lastPrompt.Load(); n != 5 {
		t.Fatalf("resumed prompt has %d messages, want 5 (system, summary, two, ok, three)", n)
	}
}
