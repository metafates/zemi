// presence.go is the live half of a shared session: every window writes a
// heartbeat file under <state>/peers/<store key>/ and reads its siblings, so
// the statusline can say how many windows share this chat, and answers a
// socket a second zemi delivers prompts through. The file is the presence,
// not the log: a log origin is per boot, so rows outlive their window, while
// a heartbeat older than peersTTL names one that quit or died.

package hostapi

import (
	"encoding/json/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/ipc"
)

const (
	peersTick = 2 * time.Second
	peersTTL  = 6 * time.Second // two beats of grace, so a missed write never flickers
	// peerReadWait bounds one delivered prompt's read: a client that dials
	// and stalls is dropped, and the reply takes as long as the send does.
	peerReadWait = 5 * time.Second
)

// peersView is the snapshot a render reads: the windows sharing this store
// besides this one, and how many of them run a turn.
type peersView struct {
	n       int
	working int
}

// StartPeers writes this window's heartbeat, serves its socket, and refreshes
// the peer view on one ticker for the process's life; a beat never waits on
// the store.
func (h *Host) StartPeers() {
	if h.DB == nil || h.Dirs.StateHome == "" {
		return
	}
	h.peersPath = filepath.Join(h.Dirs.StateHome, "peers",
		filepath.Base(filepath.Dir(h.DB.Path())), h.origin+".json")
	if err := os.MkdirAll(filepath.Dir(h.peersPath), 0o700); err != nil {
		h.Log.Warn("peers dir not made", "origin", "boot", "error", err)
		return
	}
	h.listenPeers()
	h.beat() // the first view comes now, not at the first tick
	h.GoSafe("peers", func() {
		var last peersView
		for range time.Tick(peersTick) {
			h.beat()
			if view := h.peerView(); view != last {
				last = view
				h.refresh()
			}
		}
	})
	h.GoSafe("peer hello", h.peerPoke)
}

// listenPeers binds the window's socket and serves delivered prompts; a
// window that cannot bind still beats, and only delivery is missing. The
// socket lives in a private temp dir, since a state path can outgrow a
// sun_path on darwin and windows.
func (h *Host) listenPeers() {
	dir, err := os.MkdirTemp("", "zemi-ipc-")
	if err != nil {
		h.Log.Warn("peer socket dir not made", "origin", "boot", "error", err)
		return
	}
	path := filepath.Join(dir, "prompt.sock")
	ln, err := ipc.Listen(path)
	if err != nil {
		_ = os.RemoveAll(dir)
		h.Log.Warn("peer socket not bound", "origin", "boot", "error", err)
		return
	}
	h.sockDir, h.sockPath, h.sockLn = dir, path, ln
	h.GoSafe("peer socket", func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // closed: the window is shutting down
			}
			h.GoSafe("peer prompt", func() { h.servePeer(conn) })
		}
	})
}

// servePeer reads one delivered frame: a prompt goes the way the composer
// would (a command runs, a message starts a turn or joins the running one),
// and a hello makes this window re-read its siblings now. The reply tells the
// sender which happened; a connection with no frame is a bind probe and goes
// unanswered.
func (h *Host) servePeer(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(peerReadWait))
	req, err := ipc.ReadRequest(conn)
	if err != nil || strings.TrimSpace(req.Text) == "" && !req.Hello {
		return
	}
	if req.Hello {
		_ = ipc.WriteReply(conn, ipc.Reply{OK: true})
		h.beat()
		h.refresh()
		return
	}
	reply := ipc.Reply{OK: true}
	queued, err := h.sendPrompt(req.Text)
	if err != nil {
		reply.OK, reply.Error = false, err.Error()
	} else {
		reply.Queued = queued
	}
	_ = ipc.WriteReply(conn, reply)
}

// peerPoke announces this window by dialing each sibling's socket, so their
// presence view moves now instead of at their next beat. The window's own
// heartbeat is on disk before it pokes; a poke that misses costs nothing, the
// ticker sweeps it up.
func (h *Host) peerPoke() {
	if h.peersPath == "" {
		return
	}
	dir := filepath.Dir(h.peersPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	mine := filepath.Base(h.peersPath)
	for _, e := range entries {
		if e.Name() == mine || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var hb struct {
			Sock string `json:"sock"`
		}
		if json.Unmarshal(raw, &hb) != nil || hb.Sock == "" {
			continue
		}
		conn, err := ipc.Dial(hb.Sock, time.Second)
		if err != nil {
			continue
		}
		_, _ = ipc.RoundTrip(conn, &ipc.Request{Hello: true}, 2*time.Second)
		_ = conn.Close()
	}
}

// beat rewrites this window's heartbeat, then re-reads the siblings' into
// the snapshot a render reads.
func (h *Host) beat() {
	if h.peersPath == "" {
		return
	}
	raw, _ := json.Marshal(struct {
		Working bool   `json:"working"`
		Sock    string `json:"sock,omitempty"` // where a second zemi delivers a prompt
	}{Working: h.AgentActive(), Sock: h.sockPath})
	tmp := h.peersPath + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		// the peers dir can vanish under a running window (a cleanup, a
		// prune): rebuild it, so presence recovers within a beat instead of
		// at the next restart
		if os.MkdirAll(filepath.Dir(h.peersPath), 0o700) == nil {
			_ = os.WriteFile(tmp, raw, 0o600)
		}
	}
	_ = os.Rename(tmp, h.peersPath)
	view := peersView{}
	entries, err := os.ReadDir(filepath.Dir(h.peersPath))
	if err != nil {
		h.peers.Store(&view)
		return
	}
	mine := filepath.Base(h.peersPath)
	for _, e := range entries {
		name := e.Name()
		if name == mine || filepath.Ext(name) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) > peersTTL {
			continue
		}
		var hb struct {
			Working bool `json:"working"`
		}
		if raw, err := os.ReadFile(filepath.Join(filepath.Dir(h.peersPath), name)); err == nil && json.Unmarshal(raw, &hb) == nil {
			view.n++
			if hb.Working {
				view.working++
			}
		}
	}
	h.peers.Store(&view)
}

// peerView is the last snapshot the ticker read.
func (h *Host) peerView() peersView {
	if v := h.peers.Load(); v != nil {
		return *v
	}
	return peersView{}
}

// ClosePeers drops this window's heartbeat and its socket: the pill names
// only the windows that are here, and the socket answers nothing. The poke
// may not land before the process exits, and then the ticker sweeps it.
func (h *Host) ClosePeers() {
	if h.sockLn != nil {
		h.sockLn.Close()
	}
	if h.sockDir != "" {
		_ = os.RemoveAll(h.sockDir)
	}
	if h.peersPath != "" {
		_ = os.Remove(h.peersPath)
	}
	h.GoSafe("peer poke", h.peerPoke)
}
