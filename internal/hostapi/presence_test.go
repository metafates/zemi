package hostapi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/ipc"
	"github.com/metafates/zemi/internal/store"
)

// TestPeersBeat: a beat counts the sibling heartbeats that are still fresh
// and ignores what a stale, broken or half-written file names.
func TestPeersBeat(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string // sibling name -> body
		age   time.Duration     // how old every sibling file is
		wantN int
	}{
		{"alone", nil, 0, 0},
		{"one fresh peer", map[string]string{"aaaa.json": `{"working":false}`}, 0, 1},
		{"stale names no one", map[string]string{"aaaa.json": `{"working":true}`}, peersTTL + time.Second, 0},
		{"broken names no one", map[string]string{"aaaa.json": "not json"}, 0, 0},
		{"a half write is no peer", map[string]string{"aaaa.json.tmp": `{"working":true}`}, 0, 0},
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
			if got := h.peerView(); got.n != c.wantN {
				t.Fatalf("beat read %d peers, want %d", got.n, c.wantN)
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
	if got := h.peerView(); got.n != 1 {
		t.Fatalf("beat read %d peers, want 1", got.n)
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

// TestWindowsListsTheSharingWindows: /windows reads the sibling heartbeats
// into rows, names the lease's holder among them, ages each window, and the
// kill asks first; a window nobody holds and a heartbeat without a process
// refuse.
func TestWindowsListsTheSharingWindows(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	h := openClaimed(t, state, proj)
	f := h.hooks().(*fakeUI)
	dir := filepath.Dir(h.peersPath)
	for name, body := range map[string]string{
		"aged.json": fmt.Sprintf(`{"pid":4321,"task":"an old friend","term":"wezterm","since":%d}`,
			time.Now().Add(-15*time.Minute).UnixMilli()),
		"sib.json": `{"pid":1234,"task":"writing docs","term":"ghostty"}`,
		"old.json": `{"task":"from an older zemi"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.OpenWindows("", false); err != nil {
		t.Fatal(err)
	}
	ov := f.overlays()[len(f.overlays())-1]
	if ov.ID != "windows" || len(ov.Items) != 4 {
		t.Fatalf("overlay = %s over %d items, want windows over 4", ov.ID, len(ov.Items))
	}
	if k := ov.Keys[0]; k.Key != "X" || k.Run != "windows close-others" || !k.Danger {
		t.Fatalf("the list's key = %+v", k)
	}
	if ov.Items[0].Text != "idle" || !strings.Contains(ov.Items[0].Desc, "this window · writes this chat") {
		t.Fatalf("this window's row = %q, %q", ov.Items[0].Text, ov.Items[0].Desc)
	}
	if strings.Contains(ov.Items[0].Desc, "ago") {
		t.Fatalf("a window just opened carries no age: %q", ov.Items[0].Desc)
	}
	if ov.Items[1].Text != "an old friend" || ov.Items[1].Desc != "pid 4321 · wezterm · 15m ago · watching" {
		t.Fatalf("the aged row = %q, %q", ov.Items[1].Text, ov.Items[1].Desc)
	}
	if ov.Items[2].Text != "from an older zemi" || ov.Items[2].Desc != "watching" {
		t.Fatalf("the pidless row = %q, %q", ov.Items[2].Text, ov.Items[2].Desc)
	}
	if ov.Items[3].Text != "writing docs" || ov.Items[3].Desc != "pid 1234 · ghostty · watching" {
		t.Fatalf("the sibling row = %q, %q", ov.Items[3].Text, ov.Items[3].Desc)
	}
	if k := ov.Items[3].Keys[0]; k.Key != "x" || k.Run != "windows kill sib" || !k.Danger {
		t.Fatalf("the sibling's key = %+v", k)
	}

	// the lease's holder reads as the writer: this window hands the row to
	// the sibling, as a kill would leave it
	if _, err := h.DB.ReleaseLeader(h.origin); err != nil {
		t.Fatal(err)
	}
	if won, err := h.DB.ClaimLeader("sib", "", store.Leader{}); err != nil || !won {
		t.Fatalf("the sibling did not take the lease: won=%v err=%v", won, err)
	}
	if got := h.windowItems()[3].Desc; got != "pid 1234 · ghostty · writes this chat" {
		t.Fatalf("the holder's row = %q, want the write named", got)
	}

	if err := h.OpenWindows("kill sib", false); err != nil {
		t.Fatal(err)
	}
	ask := f.overlays()[len(f.overlays())-1]
	if ask.ID != "windows kill" || ask.Confirm != "windows kill sib --yes" {
		t.Fatalf("the ask = %s armed with %q", ask.ID, ask.Confirm)
	}
	if err := h.OpenWindows("kill nobody", true); err == nil || !strings.Contains(err.Error(), "no window nobody") {
		t.Fatalf("an unknown window's kill err = %v", err)
	}
	if err := h.OpenWindows("kill old", true); err == nil || !strings.Contains(err.Error(), "no process to kill") {
		t.Fatalf("a pidless window's kill err = %v", err)
	}
}

// TestWindowsClosesTheOthers: close-others ends every sibling process at
// once, asks first, and a window with nothing to close says so.
func TestWindowsClosesTheOthers(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	h := openClaimed(t, state, proj)
	f := h.hooks().(*fakeUI)
	if err := h.OpenWindows("close-others", true); err == nil || !strings.Contains(err.Error(), "no other windows") {
		t.Fatalf("a lone window's close-others err = %v", err)
	}
	dir := filepath.Dir(h.peersPath)
	var dead []<-chan struct{}
	for _, name := range []string{"a.json", "b.json"} {
		sleep := exec.Command("sleep", "30")
		if err := sleep.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sleep.Process.Kill() })
		d := make(chan struct{})
		go func() { _ = sleep.Wait(); close(d) }()
		dead = append(dead, d)
		body := fmt.Sprintf(`{"pid":%d,"task":"a window"}`, sleep.Process.Pid)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.beat()
	if err := h.OpenWindows("close-others", false); err != nil {
		t.Fatal(err)
	}
	ask := f.overlays()[len(f.overlays())-1]
	if ask.ID != "windows close" || ask.Confirm != "windows close-others --yes" {
		t.Fatalf("the ask = %s armed with %q", ask.ID, ask.Confirm)
	}
	if err := h.OpenWindows("close-others", true); err != nil {
		t.Fatal(err)
	}
	for _, d := range dead {
		select {
		case <-d:
		case <-time.After(2 * time.Second):
			t.Fatal("a sibling process survived the close")
		}
	}
	if got := f.notified(); !strings.Contains(got[len(got)-1], "closed 2 windows") {
		t.Fatalf("the notice = %q", got)
	}
}

// TestACloseTellsTheSiblingsAtOnce: a window's close pokes its siblings in
// the closing call, and the poked survivor takes the write at once - no
// ticker runs here, so the poke is the only path the takeover has.
func TestACloseTellsTheSiblingsAtOnce(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj) // the writer
	b, _ := newWindow(t, state, proj)
	b.OpenPeers() // a viewer over the same store
	b.beat()      // its heartbeat is what the closing window dials
	if b.writerFlag() {
		t.Fatal("the second window opened as the writer")
	}
	a.ClosePeers()
	waitForWithin(t, 2*time.Second, func() error {
		if !b.writerFlag() {
			return fmt.Errorf("the survivor still watches")
		}
		return nil
	})
	if _, err := os.Stat(a.peersPath); !os.IsNotExist(err) {
		t.Fatalf("the close left the heartbeat behind: %v", err)
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
