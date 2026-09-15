package hostapi

import (
	"testing"
	"time"
)

// TestAutoTitle: the first finished exchange renames the session with the
// model's short title, live and in the store; later turns keep it.
func TestAutoTitle(t *testing.T) {
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "Parser Fix")
	h.reflectMu.Lock() // no detached reflector in this test
	defer h.reflectMu.Unlock()
	if err := h.AgentSend("the parser drops the last token, can you look?"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	deadline := time.Now().Add(3 * time.Second)
	for h.Session().Title != "Parser Fix" {
		if time.Now().After(deadline) {
			t.Fatalf("title = %q", h.Session().Title)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s, err := h.DB.GetSession(h.AgentSessionID()); err != nil || s.Title != "Parser Fix" {
		t.Fatalf("stored title = %+v, %v", s, err)
	}
}
