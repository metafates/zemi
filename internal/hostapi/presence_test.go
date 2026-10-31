package hostapi

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/ipc"
	"github.com/metafates/zemi/internal/store"
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

// TestSiblingWorkingIgnoresItsOwnHeartbeat: the window's own heartbeat reads
// working until its next beat, and its socket answers; a settle must not
// read it as a sibling at work and skip closing the todo list.
func TestSiblingWorkingIgnoresItsOwnHeartbeat(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	// a bind dir short enough for a sun_path on every platform, the way the
	// send tests make one: a test temp dir can outgrow the address limit
	dir0, err := os.MkdirTemp("", "zemi-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir0) })
	sock := filepath.Join(dir0, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // the socket the window's own heartbeat advertises: alive
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if req, err := ipc.ReadRequest(conn); err == nil && req.Hello {
				_ = ipc.WriteReply(conn, ipc.Reply{OK: true})
			}
			conn.Close()
		}
	}()
	h.sockPath = sock
	dir := filepath.Join(h.Dirs.StateHome, "peers", filepath.Base(filepath.Dir(h.DB.Path())))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	own := `{"working":true,"sock":"` + sock + `"}`
	if err := os.WriteFile(filepath.Join(dir, "mine.json"), []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}
	if h.siblingWorking() {
		t.Fatal("the window's own heartbeat reads as a sibling at work")
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

// TestPeersBeatRebuildsTheDir: a peers dir deleted under a running window
// recovers within a beat, own heartbeat first, siblings after.
func TestPeersBeatRebuildsTheDir(t *testing.T) {
	h := newTestHostUI(t)
	h.peersPath = filepath.Join(t.TempDir(), "peers", "key", "mine.json")
	if err := os.MkdirAll(filepath.Dir(h.peersPath), 0o700); err != nil {
		t.Fatal(err)
	}
	h.beat()
	if _, err := os.Stat(h.peersPath); err != nil {
		t.Fatalf("the beat wrote no heartbeat: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(h.peersPath))); err != nil {
		t.Fatal(err)
	}
	h.beat()
	if _, err := os.Stat(h.peersPath); err != nil {
		t.Fatalf("the beat did not rebuild the peers dir: %v", err)
	}
	sibling := filepath.Join(filepath.Dir(h.peersPath), "aaaa.json")
	if err := os.WriteFile(sibling, []byte(`{"working":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.beat()
	if got := h.peerView(); got.n != 1 || got.working != 1 {
		t.Fatalf("beat read %d peers, %d working, want 1 and 1", got.n, got.working)
	}
}

// TestPeersBeatPrunesTheStale: a beat deletes a sibling heartbeat past the
// freshness window and keeps a fresh one - senders read these files for
// socket paths, so stale junk goes.
func TestPeersBeatPrunesTheStale(t *testing.T) {
	h := newTestHostUI(t)
	h.peersPath = filepath.Join(t.TempDir(), "peers", "key", "mine.json")
	if err := os.MkdirAll(filepath.Dir(h.peersPath), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(filepath.Dir(h.peersPath), "dead.json")
	fresh := filepath.Join(filepath.Dir(h.peersPath), "live.json")
	for path, body := range map[string]string{stale: `{"working":true}`, fresh: `{"working":false}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-peersTTL - time.Second)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	h.beat()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the beat kept the stale heartbeat: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("the beat pruned the fresh heartbeat: %v", err)
	}
}

// TestPeerDelivery: a second zemi delivers a prompt over the window's socket.
// Idle, the message starts a turn; mid-turn, it joins the steering queue; the
// reply tells the sender which happened.
func TestPeerDelivery(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	if _, err := a.Commands.Add(&Command{Name: "ping", GoFn: func(*Host, string, bool) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	a.peersPath = filepath.Join(state, "peers", filepath.Base(filepath.Dir(a.DB.Path())), a.origin+".json")
	a.listenPeers()
	a.beat() // the heartbeat advertises the socket

	// idle: the delivered message starts a turn on the stub
	conn, err := ipc.Dial(a.sockPath, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	reply, err := ipc.RoundTrip(conn, &ipc.Request{Text: "delivered idle"}, time.Second)
	conn.Close()
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !reply.OK || reply.Queued {
		t.Fatalf("idle reply = %+v, want ok and not queued", reply)
	}
	waitIdle(t, a)
	rows, err := a.DB.LogTail(5)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rows, func(e store.LogEntry) bool {
		return e.Kind == store.KindUser && e.Text == "delivered idle"
	}) {
		t.Fatalf("the log holds no delivered row: %+v", rows)
	}

	// mid-turn: the delivered message joins the steering queue
	a.agents.mu.Lock()
	a.claim()
	a.agents.mu.Unlock()
	conn, err = ipc.Dial(a.sockPath, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	reply, err = ipc.RoundTrip(conn, &ipc.Request{Text: "steer me"}, time.Second)
	conn.Close()
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !reply.OK || !reply.Queued {
		t.Fatalf("steer reply = %+v, want ok and queued", reply)
	}
	if q := a.AgentTakeQueue(); len(q) != 1 || q[0] != "steer me" {
		t.Fatalf("queue = %q, want the delivered message", q)
	}
	a.AgentCancel()

	// every read frame is answered: the window's own refusals ride the reply,
	// and a command reports ran instead of a turn
	for _, tc := range []struct {
		text      string
		wantOK    bool
		wantRan   bool
		wantError string
	}{
		{"   ", false, false, "nothing to send"},
		{"/ping", true, true, ""},
	} {
		conn, err := ipc.Dial(a.sockPath, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		reply, err := ipc.RoundTrip(conn, &ipc.Request{Text: tc.text}, time.Second)
		conn.Close()
		if err != nil {
			t.Fatalf("round trip %q: %v", tc.text, err)
		}
		if reply.OK != tc.wantOK || reply.Ran != tc.wantRan || reply.Error != tc.wantError {
			t.Fatalf("reply to %q = %+v, want ok=%v ran=%v error=%q", tc.text, reply, tc.wantOK, tc.wantRan, tc.wantError)
		}
	}
}
