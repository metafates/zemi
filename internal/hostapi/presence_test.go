package hostapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPeersBeat: a beat counts the sibling heartbeats that are still fresh,
// reads their working flag, and ignores what a stale, broken or half-written
// file names.
func TestPeersBeat(t *testing.T) {
	for _, c := range []struct {
		name        string
		files       map[string]string // sibling name -> body
		age         time.Duration     // how old every sibling file is
		wantN       int
		wantWorking int
	}{
		{"alone", nil, 0, 0, 0},
		{"one fresh peer", map[string]string{"aaaa.json": `{"working":false}`}, 0, 1, 0},
		{"one at work", map[string]string{"aaaa.json": `{"working":true}`}, 0, 1, 1},
		{"stale names no one", map[string]string{"aaaa.json": `{"working":true}`}, peersTTL + time.Second, 0, 0},
		{"broken names no one", map[string]string{"aaaa.json": "not json"}, 0, 0, 0},
		{"a half write is no peer", map[string]string{"aaaa.json.tmp": `{"working":true}`}, 0, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHostUI(t)
			h.peersPath = filepath.Join(t.TempDir(), "peers", "key", "mine.json")
			if err := os.MkdirAll(filepath.Dir(h.peersPath), 0o700); err != nil {
				t.Fatal(err)
			}
			for name, body := range c.files {
				p := filepath.Join(filepath.Dir(h.peersPath), name)
				if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if c.age > 0 {
					past := time.Now().Add(-c.age)
					if err := os.Chtimes(p, past, past); err != nil {
						t.Fatal(err)
					}
				}
			}
			h.beat()
			if got := h.peerView(); got.n != c.wantN || got.working != c.wantWorking {
				t.Fatalf("beat read %d peers, %d working, want %d and %d", got.n, got.working, c.wantN, c.wantWorking)
			}
		})
	}
}

// TestPeersBeatWritesOwn: a beat leaves this window's own heartbeat behind
// and ClosePeers takes it away.
func TestPeersBeatWritesOwn(t *testing.T) {
	h := newTestHostUI(t)
	h.peersPath = filepath.Join(t.TempDir(), "peers", "key", "mine.json")
	if err := os.MkdirAll(filepath.Dir(h.peersPath), 0o700); err != nil {
		t.Fatal(err)
	}
	h.beat()
	if _, err := os.Stat(h.peersPath); err != nil {
		t.Fatalf("the beat wrote no heartbeat: %v", err)
	}
	h.ClosePeers()
	if _, err := os.Stat(h.peersPath); !os.IsNotExist(err) {
		t.Fatalf("ClosePeers left the heartbeat behind: %v", err)
	}
}
