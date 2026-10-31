// presence.go is the live half of a shared session: every window writes a
// heartbeat file under <state>/peers/<store key>/ and reads its siblings, so
// the statusline can say how many windows share this chat, and answers a
// socket a second zemi delivers prompts through. The file is the presence,
// not the log: a log origin is per boot, so rows outlive their window, while
// a heartbeat older than peersTTL names one that quit or died. Who may write
// the chat is the lease's to say (lease.go); presence says who is watching.

package hostapi

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/metafates/zemi/internal/ipc"
	"github.com/metafates/zemi/internal/lua"
)

const (
	peersTick = 2 * time.Second
	peersTTL  = 6 * time.Second // two beats of grace, so a missed write never flickers
	// peerReadWait bounds one delivered prompt's read: a client that dials
	// and stalls is dropped, and the reply takes as long as the send does.
	peerReadWait = 5 * time.Second
)

// PeersTTL is how long a heartbeat names a live window, shared with the
// presence readers outside this package, so a stale peer is stale everywhere.
const PeersTTL = peersTTL

// peersView is the snapshot a render reads: the windows sharing this store
// besides this one.
type peersView struct {
	n    int
	rows []PeerRow
}

// PeerRow is one sibling window as /windows lists it: its per-boot id, its
// process, and its own words for what it is on.
type PeerRow struct {
	Origin string
	PID    int
	Task   string
}

// OpenPeers sets this window's presence up and claims the lease, synchronously
// before the first prompt: a prompt held for the first turn must already know
// whether this window writes the chat, which the ticker would decide too late.
func (h *Host) OpenPeers() {
	if h.DB == nil || h.Dirs.StateHome == "" {
		return
	}
	h.peersPath = filepath.Join(h.Dirs.StateHome, "peers",
		filepath.Base(filepath.Dir(h.DB.Path())), h.origin+".json")
	if err := os.MkdirAll(filepath.Dir(h.peersPath), 0o700); err != nil {
		h.Log.Warn("peers dir not made", "origin", "boot", "error", err)
		h.peersPath = ""
		return
	}
	h.listenPeers()
	h.claimLease()
}

// StartPeers writes this window's heartbeat, refreshes the peer view and
// renews the lease on one ticker for the process's life; the socket and the
// lease claim are already up (OpenPeers). The beat writes no store - the lease
// renewal does, behind it on this goroutine.
func (h *Host) StartPeers() {
	if h.peersPath == "" {
		return
	}
	h.beat() // the first view comes now, not at the first tick
	h.GoSafe("peers", func() {
		for range time.Tick(peersTick) {
			h.beat()
			// unconditional: a beat another goroutine ran (a poke's hello)
			// stores a snapshot this ticker's own last view never saw
			h.refresh()
			h.leaseTick()
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

// agentsTask is the live turn's own words, what the statusline names: a
// sender with two windows up reads which one took the message.
func (h *Host) agentsTask() string {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.task
}

// servePeer reads one delivered frame: a prompt goes the way the composer
// would (a command runs, a message starts a turn or joins the running one),
// and a hello makes this window re-read its siblings now. The reply tells the
// sender which happened; a bind probe or an unreadable frame goes
// unanswered, a read frame is always answered.
func (h *Host) servePeer(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(peerReadWait))
	req, err := ipc.ReadRequest(conn)
	if err != nil {
		return
	}
	if req.Hello {
		_ = ipc.WriteReply(conn, ipc.Reply{OK: true})
		h.beat()
		h.refresh()
		return
	}
	reply := ipc.Reply{OK: true, Note: clipLine(h.agentsTask(), 60)}
	queued, ran, err := h.sendPrompt(req.Text)
	if err != nil {
		reply.OK, reply.Error = false, err.Error()
	} else {
		reply.Queued, reply.Ran = queued, ran
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
		Sock string `json:"sock,omitempty"` // where a second zemi delivers a prompt
		Pid  int    `json:"pid,omitempty"`  // the process, for /windows kill
		Task string `json:"task,omitempty"` // what it is on, one clipped line
	}{Sock: h.sockPath, Pid: os.Getpid(), Task: clipLine(h.agentsTask(), 60)})
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
	dir := filepath.Dir(h.peersPath)
	entries, err := os.ReadDir(dir)
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
			// stale names a window that quit or died, and senders read these
			// files for socket paths: the junk goes
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		var hb struct {
			Sock string `json:"sock"`
			Pid  int    `json:"pid"`
			Task string `json:"task"`
		}
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil && json.Unmarshal(raw, &hb) == nil {
			view.n++
			view.rows = append(view.rows, PeerRow{
				Origin: strings.TrimSuffix(name, ".json"), PID: hb.Pid, Task: hb.Task,
			})
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

// ClosePeers releases the lease, drops this window's heartbeat and its socket:
// the pill names only the windows that are here, and the socket answers
// nothing. The poke may not land before the process exits, and then the ticker
// sweeps it.
func (h *Host) ClosePeers() {
	h.releaseLease()
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

// OpenWindows runs /windows: the other zemi windows sharing this chat, each
// with the kill that frees its hold. Killing the writer leaves the lease to
// this window, which takes it over as the dead one's beat goes stale or its
// clean exit releases it. "kill <origin>" is the list's key spelled, and it
// asks first.
func (h *Host) OpenWindows(args string, confirm bool) error {
	rest, yes := ConfirmArgs(args, confirm)
	if origin, ok := strings.CutPrefix(rest, "kill "); ok {
		return h.killWindow(strings.TrimSpace(origin), yes)
	}
	if rest != "" {
		return Usagef("usage: /windows [kill <origin>]")
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	h.beat() // the list reads the siblings now, not at the last tick
	build := h.windowItems
	ui.OpenOverlay(Overlay{
		ID: "windows", Title: "Windows", Origin: lua.OriginBuiltin,
		Items: build(), Reload: build,
		Empty: "No other zemi windows share this chat.",
	})
	return nil
}

// windowItems is the /windows list: one row per sibling heartbeat, the writer
// named among them.
func (h *Host) windowItems() []OverlayItem {
	leader := ""
	if h.DB != nil {
		if l, err := h.DB.ReadLeader(); err == nil {
			leader = l.Origin
		}
	}
	rows := h.peerView().rows
	items := make([]OverlayItem, 0, len(rows))
	for _, p := range rows {
		role := "watching"
		if p.Origin == leader {
			role = "writes this chat"
		}
		desc := role
		if p.PID != 0 {
			desc = "pid " + strconv.Itoa(p.PID) + " · " + role
		}
		text := cmp.Or(p.Task, "idle")
		fields := fieldList("origin", p.Origin, "task", text)
		if p.PID != 0 {
			fields = append(fields, Field{Name: "pid", Value: strconv.Itoa(p.PID)})
		}
		items = append(items, OverlayItem{
			Text:   text,
			Desc:   desc,
			Fields: fields,
			Keys:   []ItemKey{{Key: "k", Desc: "kill", Danger: true, Run: "windows kill " + p.Origin}},
		})
	}
	return items
}

// killWindow ends another window's process. The ask comes first, like every
// kill; the armed press and a typed --yes skip it.
func (h *Host) killWindow(origin string, yes bool) error {
	rows := h.peerView().rows
	row := slices.IndexFunc(rows, func(p PeerRow) bool { return p.Origin == origin })
	if row < 0 {
		return fmt.Errorf("no window %s shares this chat", origin)
	}
	p := rows[row]
	if p.PID == 0 {
		return errors.New("that window's heartbeat names no process to kill")
	}
	if !yes {
		return AskFirst("windows kill "+origin+" --yes", "", func(confirm string) error {
			return h.openKillConfirm(p, confirm)
		})
	}
	if err := killPeer(p.PID); err != nil {
		return fmt.Errorf("could not kill pid %d: %w", p.PID, err)
	}
	h.Notify(slog.LevelWarn, "Window killed",
		fmt.Sprintf("%s (pid %d) was told to quit; the write comes here as its lease frees", cmp.Or(p.Task, "that window"), p.PID))
	// the killed window releases its lease as it exits: look for it once
	// before the ticker does
	h.GoSafe("lease nudge", func() {
		time.Sleep(300 * time.Millisecond)
		h.leaseTick()
	})
	return nil
}

// openKillConfirm is the ask a kill opens: one row, its key armed.
func (h *Host) openKillConfirm(p PeerRow, confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(Overlay{
		ID: "windows kill", Title: "Kill", Origin: lua.OriginBuiltin, Confirm: confirm,
		Items: []OverlayItem{{
			Text: "Kill the window on " + cmp.Or(p.Task, "nothing"),
			Detail: fmt.Sprintf("The zemi process (pid %d) is told to quit; its unsent drafts go with it. "+
				"Its lease comes free as it exits, and this window takes the write at its next beat.", p.PID),
			Keys: []ItemKey{{Key: "x", Desc: "kill", Danger: true, Run: "windows kill " + p.Origin}},
		}},
	})
	return nil
}

// killPeer tells another zemi's process to quit: a clean SIGTERM first, so
// its exit releases the lease and drops its heartbeat; a process that will
// not take it is killed.
func killPeer(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err == nil {
		return nil
	}
	return p.Kill()
}
