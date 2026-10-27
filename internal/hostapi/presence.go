// presence.go is the live half of a shared session: every window writes a
// heartbeat file under <state>/peers/<store key>/ and reads its siblings, so
// the statusline can say how many windows share this chat. The file is the
// presence, not the log: a log origin is per boot, so rows outlive their
// window, while a heartbeat older than peersTTL names one that quit or died.

package hostapi

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"time"
)

const (
	peersTick = 2 * time.Second
	peersTTL  = 6 * time.Second // two beats of grace, so a missed write never flickers
)

// peersView is the snapshot a render reads: the windows sharing this store
// besides this one, and how many of them run a turn.
type peersView struct {
	n       int
	working int
}

// StartPeers writes this window's heartbeat and refreshes the peer view on
// one ticker for the process's life; a beat never waits on the store.
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
}

// beat rewrites this window's heartbeat, then re-reads the siblings' into
// the snapshot a render reads.
func (h *Host) beat() {
	raw, _ := json.Marshal(struct {
		Working bool `json:"working"`
	}{Working: h.AgentActive()})
	tmp := h.peersPath + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, h.peersPath)
	}
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

// ClosePeers drops this window's heartbeat: the pill names only the windows
// that are here.
func (h *Host) ClosePeers() {
	if h.peersPath != "" {
		_ = os.Remove(h.peersPath)
	}
}
