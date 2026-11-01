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
	"encoding/json/jsontext"
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
// process, its terminal, how long it has been open, and its own words for
// what it is on.
type PeerRow struct {
	Origin string
	PID    int
	Task   string
	Term   string
	Since  int64 // when it opened, unix ms; 0 names a heartbeat from an older zemi
}

// windowAge is how long a window has been open, in the chat's own "15m ago";
// "" under a minute and when no start is named.
func windowAge(ms int64) string {
	if ms <= 0 || ago(ms) == "now" {
		return ""
	}
	return ago(ms)
}

// termName names the terminal this window runs in: the program when it says
// one, the terminal type otherwise.
func termName() string {
	return cmp.Or(os.Getenv("TERM_PROGRAM"), os.Getenv("TERM"))
}

// OpenPeers sets this window's presence up and claims the lease, synchronously
// before the first prompt: a prompt held for the first turn must already know
// whether this window writes the chat, which the ticker would decide too late.
func (h *Host) OpenPeers() {
	if h.DB == nil || h.Dirs.StateHome == "" {
		return
	}
	h.boot = time.Now()
	h.peersPath = filepath.Join(h.Dirs.StateHome, "peers",
		filepath.Base(filepath.Dir(h.DB.Path())), h.origin+".json")
	if err := os.MkdirAll(filepath.Dir(h.peersPath), 0o700); err != nil {
		h.Log.Warn("peers dir not made", "origin", "boot", "error", err)
		h.peersPath = ""
		return
	}
	h.listenPeers()
	h.streamPoke = make(chan struct{}, 1)
	h.GoSafe("stream push", h.pushStreamLoop)
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
			h.leaseTick(false)
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
// a hello makes this window re-read its siblings now, a yours re-reads the
// lease the write just moved to, a yield asks this window for the write, and
// a stream frame is the writing window's live turn, applied with the rows the
// same flush logged. The reply tells the sender which happened; a bind probe
// or an unreadable frame goes unanswered, a read frame is always answered.
func (h *Host) servePeer(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(peerReadWait))
	req, err := ipc.ReadRequest(conn)
	if err != nil {
		return
	}
	if req.Stream != nil {
		var w streamWire
		if err := json.Unmarshal([]byte(req.Stream), &w); err != nil {
			h.RecordErrorOnce("peer stream", "", "a pushed frame did not parse: "+err.Error())
			return
		}
		h.watchStream(wireToStream(w))
		h.SyncChat() // the frame's rows ride the shared log: splice them now, not at the next tick
		// answered only once applied: the sender waits for this reply before
		// its next frame, so an older frame cannot land after a newer one
		_ = ipc.WriteReply(conn, ipc.Reply{OK: true})
		return
	}
	if req.Hello {
		_ = ipc.WriteReply(conn, ipc.Reply{OK: true})
		h.beat()
		// a sibling just arrived or left: read the lease now, so a closed
		// window's write moves at once instead of at the next tick
		h.leaseTick(false)
		h.refresh()
		return
	}
	if req.Yours {
		_ = ipc.WriteReply(conn, ipc.Reply{OK: true})
		h.beat()
		h.leaseTick(true) // the write was handed here: read the row now
		h.refresh()
		return
	}
	if req.Yield {
		reply := ipc.Reply{Note: clipLine(h.agentsTask(), 60)}
		if h.writerFlag() {
			reply.OK = true
			h.yieldWrite(req.Origin, req.Sock)
		} else {
			reply.Error = errNotWriter.Error()
		}
		_ = ipc.WriteReply(conn, reply)
		return
	}
	reply := ipc.Reply{OK: true, Note: clipLine(h.agentsTask(), 60)}
	queued, ran, err := h.sendPrompt(req.Text)
	if err != nil {
		reply.OK, reply.Error = false, err.Error()
	} else if queued && !ran && !h.writerFlag() {
		// a viewer parks a delivered message for the write; it joins no turn
		reply.Parked = true
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
	for _, sock := range h.siblingSocks() {
		conn, err := ipc.Dial(sock, time.Second)
		if err != nil {
			continue
		}
		_, _ = ipc.RoundTrip(conn, &ipc.Request{Hello: true}, 2*time.Second)
		_ = conn.Close()
	}
}

// siblingSocks reads the peers dir into the sockets a delivered frame reaches,
// this window's own heartbeat left out: a stale heartbeat's socket simply
// refuses the dial.
func (h *Host) siblingSocks() []string {
	if h.peersPath == "" {
		return nil
	}
	dir := filepath.Dir(h.peersPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var socks []string
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
		socks = append(socks, hb.Sock)
	}
	return socks
}

// pokeViewers asks for the live view to reach the sibling windows now, not at
// their next sync tick: every stream flush and every logged row calls here.
// The send never blocks and never queues - a poke that finds the gate full is
// dropped, because the pusher's next snapshot carries the newer state whole.
func (h *Host) pokeViewers() {
	if h.peersPath == "" || h.streamPoke == nil {
		return
	}
	select {
	case h.streamPoke <- struct{}{}:
	default:
	}
}

// streamWire is the pushed snapshot on the wire: StreamSnapshot with its
// clocks as milliseconds, which json has a default representation for -
// time.Duration it does not. Every StreamSnapshot field rides it; a new one
// joins here too, or the watching windows never see it.
type streamWire struct {
	Active       bool         `json:"active"`
	Text         string       `json:"text"`
	Reasoning    string       `json:"reasoning"`
	Thinking     bool         `json:"thinking"`
	Task         string       `json:"task"`
	Tools        []StreamTool `json:"tools"`
	Model        string       `json:"model"`
	Steering     []Queued     `json:"steering"`
	ThoughtMS    int64        `json:"thought_ms"`
	ElapsedMS    int64        `json:"elapsed_ms"`
	Retrying     bool         `json:"retrying"`
	RetryInMS    int64        `json:"retry_in_ms"`
	Retries      int          `json:"retries"`
	RetryWhy     string       `json:"retry_why"`
	Turn         int          `json:"turn"`
	CheckingGoal bool         `json:"checking_goal"`
	ByLoop       bool         `json:"by_loop"`
	SessionIn    int64        `json:"session_in"`
	SessionOut   int64        `json:"session_out"`
	LastIn       int64        `json:"last_in"`
	CtxWindow    int64        `json:"ctx_window"`
	SessionCost  float64      `json:"session_cost"`
	CacheRead    int64        `json:"cache_read"`
	CacheBilled  int64        `json:"cache_billed"`
}

func streamToWire(s StreamSnapshot) streamWire {
	return streamWire{
		Active: s.Active, Text: s.Text, Reasoning: s.Reasoning, Thinking: s.Thinking,
		Task: s.Task, Tools: s.Tools, Model: s.Model, Steering: s.Steering,
		ThoughtMS: s.Thought.Milliseconds(), ElapsedMS: s.Elapsed.Milliseconds(),
		Retrying: s.Retrying, RetryInMS: s.RetryIn.Milliseconds(),
		Retries: s.Retries, RetryWhy: s.RetryWhy, Turn: s.Turn,
		CheckingGoal: s.CheckingGoal, ByLoop: s.ByLoop,
		SessionIn: s.SessionIn, SessionOut: s.SessionOut, LastIn: s.LastIn,
		CtxWindow: s.CtxWindow, SessionCost: s.SessionCost,
		CacheRead: s.CacheRead, CacheBilled: s.CacheBilled,
	}
}

func wireToStream(w streamWire) StreamSnapshot {
	return StreamSnapshot{
		Active: w.Active, Text: w.Text, Reasoning: w.Reasoning, Thinking: w.Thinking,
		Task: w.Task, Tools: w.Tools, Model: w.Model, Steering: w.Steering,
		Thought:  time.Duration(w.ThoughtMS) * time.Millisecond,
		Elapsed:  time.Duration(w.ElapsedMS) * time.Millisecond,
		Retrying: w.Retrying, RetryIn: time.Duration(w.RetryInMS) * time.Millisecond,
		Retries: w.Retries, RetryWhy: w.RetryWhy, Turn: w.Turn,
		CheckingGoal: w.CheckingGoal, ByLoop: w.ByLoop,
		SessionIn: w.SessionIn, SessionOut: w.SessionOut, LastIn: w.LastIn,
		CtxWindow: w.CtxWindow, SessionCost: w.SessionCost,
		CacheRead: w.CacheRead, CacheBilled: w.CacheBilled,
	}
}

// pushStreamLoop is the live sync's push half: one wake ships the current
// snapshot to every sibling socket, the pokes that piled up meanwhile folded
// into it, so a viewer draws the writer's turn at the cadence the writer
// draws its own.
func (h *Host) pushStreamLoop() {
	for range h.streamPoke {
		for len(h.streamPoke) > 0 {
			<-h.streamPoke
		}
		h.pushStream()
	}
}

// pushStream ships the current snapshot to every sibling socket. The loop and
// a hand-over's in-line call share it under one lock, so two rounds never
// interleave their frames; a round trip waits for the viewer to apply before
// it replies, so a stalled one holds the lock no longer than its dial and
// reply deadlines.
func (h *Host) pushStream() {
	h.pushMu.Lock()
	defer h.pushMu.Unlock()
	if !h.writerFlag() {
		return // a viewer's own flushes poke too; only the writer pushes
	}
	socks := h.siblingSocks()
	if len(socks) == 0 {
		return
	}
	raw, err := json.Marshal(streamToWire(h.AgentStream()))
	if err != nil {
		h.RecordErrorOnce("stream push", "", "a frame did not marshal: "+err.Error())
		return
	}
	for _, sock := range socks {
		conn, err := ipc.Dial(sock, time.Second)
		if err != nil {
			continue
		}
		_, _ = ipc.RoundTrip(conn, &ipc.Request{Stream: jsontext.Value(raw)}, 2*time.Second)
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
		Sock  string `json:"sock,omitempty"`  // where a second zemi delivers a prompt
		Pid   int    `json:"pid,omitempty"`   // the process, for /windows kill
		Task  string `json:"task,omitempty"`  // what it is on, one clipped line
		Term  string `json:"term,omitempty"`  // the terminal it runs in, for /windows
		Since int64  `json:"since,omitempty"` // when it opened, unix ms, for the /windows age
	}{Sock: h.sockPath, Pid: os.Getpid(), Task: clipLine(h.agentsTask(), 60), Term: termName(), Since: h.boot.UnixMilli()})
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
			Sock  string `json:"sock"`
			Pid   int    `json:"pid"`
			Task  string `json:"task"`
			Term  string `json:"term"`
			Since int64  `json:"since"`
		}
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil && json.Unmarshal(raw, &hb) == nil {
			view.n++
			view.rows = append(view.rows, PeerRow{
				Origin: strings.TrimSuffix(name, ".json"), PID: hb.Pid, Task: hb.Task, Term: hb.Term, Since: hb.Since,
			})
		}
	}
	h.peers.Store(&view)
	// an open /windows list reads the change now, not at its next keypress
	if ui := h.hooks(); ui != nil {
		ui.RefreshOverlay("windows", h.windowsOverlay)
	}
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
// nothing. The poke waits its round out - the caller is on its way out, and a
// poke left to a goroutine can die with the process.
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
	h.closePoke(500 * time.Millisecond)
}

// closePoke tells the sibling windows this one left, and waits out the round:
// a terminal closed by hand must vanish from their lists at once, not at the
// next tick. The budget bounds the whole round; a sibling that cannot answer
// in it is swept by its ticker.
func (h *Host) closePoke(budget time.Duration) {
	deadline := time.Now().Add(budget)
	for _, sock := range h.siblingSocks() {
		wait := min(time.Until(deadline), 300*time.Millisecond)
		conn, err := ipc.Dial(sock, wait)
		if err != nil {
			continue
		}
		_, _ = ipc.RoundTrip(conn, &ipc.Request{Hello: true}, wait)
		_ = conn.Close()
	}
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
	if rest == "close-others" {
		return h.closeOthers(yes)
	}
	if rest != "" {
		return Usagef("usage: /windows [kill <origin> | close-others]")
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	h.beat() // the list reads the siblings now, not at the last tick
	ui.OpenOverlay(h.windowsOverlay())
	return nil
}

// windowsOverlay is the /windows list. The beat refreshes it while it is
// open, so a window that arrives or leaves shows at once.
func (h *Host) windowsOverlay() Overlay {
	build := h.windowItems
	return Overlay{
		ID: "windows", Title: "Windows", Origin: lua.OriginBuiltin,
		Items: build(), Reload: build,
		Keys:  []OverlayKey{{Key: "X", Desc: "close others", Run: "windows close-others", Danger: true}},
		Empty: "No other zemi windows share this chat.",
	}
}

// windowItems is the /windows list: this window first, then one row per
// sibling heartbeat, the writer named among them.
func (h *Host) windowItems() []OverlayItem {
	leader := ""
	if h.DB != nil {
		if l, err := h.DB.ReadLeader(); err == nil {
			leader = l.Origin
		}
	}
	role := "watching"
	if h.writerFlag() {
		role = "writes this chat"
	}
	task := cmp.Or(h.agentsTask(), "idle")
	self := []string{"pid " + strconv.Itoa(os.Getpid())}
	if term := termName(); term != "" {
		self = append(self, term)
	}
	if age := windowAge(h.boot.UnixMilli()); age != "" {
		self = append(self, age)
	}
	self = append(self, "this window", role)
	pid := strconv.Itoa(os.Getpid())
	fields := append(fieldList("origin", h.origin, "task", task), Field{Name: "pid", Value: pid})
	if term := termName(); term != "" {
		fields = append(fields, Field{Name: "term", Value: term})
	}
	items := []OverlayItem{{
		Text:   task,
		Desc:   strings.Join(self, " · "),
		Fields: fields,
	}}
	for _, p := range h.peerView().rows {
		role := "watching"
		if p.Origin == leader {
			role = "writes this chat"
		}
		parts := []string{}
		if p.PID != 0 {
			parts = append(parts, "pid "+strconv.Itoa(p.PID))
		}
		if p.Term != "" {
			parts = append(parts, p.Term)
		}
		if age := windowAge(p.Since); age != "" {
			parts = append(parts, age)
		}
		parts = append(parts, role)
		text := cmp.Or(p.Task, "idle")
		fields := fieldList("origin", p.Origin, "task", text)
		if p.PID != 0 {
			fields = append(fields, Field{Name: "pid", Value: strconv.Itoa(p.PID)})
		}
		if p.Term != "" {
			fields = append(fields, Field{Name: "term", Value: p.Term})
		}
		items = append(items, OverlayItem{
			Text:   text,
			Desc:   strings.Join(parts, " · "),
			Fields: fields,
			Keys:   []ItemKey{{Key: "x", Desc: "kill", Danger: true, Run: "windows kill " + p.Origin}},
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
		return errors.New("no process to kill for that window")
	}
	if !yes {
		return AskFirst("windows kill "+origin+" --yes", "", func(confirm string) error {
			return h.openKillConfirm(p, confirm)
		})
	}
	if err := killPeer(p.PID); err != nil {
		return fmt.Errorf("could not kill pid %d: %w", p.PID, err)
	}
	body := fmt.Sprintf("closed window %d", p.PID)
	if !h.writerFlag() {
		body += "; this window writes now"
	}
	h.Notify(slog.LevelWarn, "Window killed", body)
	// the killed window releases its lease as it exits: look for it once
	// before the ticker does
	h.GoSafe("lease nudge", func() {
		time.Sleep(300 * time.Millisecond)
		h.leaseTick(false)
	})
	return nil
}

// openKillConfirm is the ask a kill opens: one row, its key armed.
func (h *Host) openKillConfirm(p PeerRow, confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	detail := fmt.Sprintf("window %d closes; its unsent drafts go with it.", p.PID)
	if !h.writerFlag() {
		detail += " This window writes next."
	}
	text := "Kill the window"
	if p.Task != "" {
		text += " on " + p.Task
	}
	ui.OpenOverlay(Overlay{
		ID: "windows kill", Title: "Kill", Origin: lua.OriginBuiltin, Confirm: confirm,
		Items: []OverlayItem{{
			Text:   text,
			Detail: detail,
			Keys:   []ItemKey{{Key: "x", Desc: "kill", Danger: true, Run: "windows kill " + p.Origin}},
		}},
	})
	return nil
}

// killableRows is the sibling view without the heartbeats that name no
// process: what a kill can reach.
func (h *Host) killableRows() []PeerRow {
	return slices.DeleteFunc(slices.Clone(h.peerView().rows), func(p PeerRow) bool { return p.PID == 0 })
}

// closeOthers ends every sibling window at once. The ask comes first, like
// every kill; the armed press and a typed --yes skip it.
func (h *Host) closeOthers(yes bool) error {
	rows := h.killableRows()
	if len(rows) == 0 {
		return errors.New("no other windows share this chat")
	}
	if !yes {
		return AskFirst("windows close-others --yes", "", h.openCloseOthersConfirm)
	}
	closed := 0
	var errs []error
	for _, p := range rows {
		if err := killPeer(p.PID); err != nil {
			errs = append(errs, fmt.Errorf("pid %d: %w", p.PID, err))
			continue
		}
		closed++
	}
	if closed == 0 {
		return errors.Join(errs...)
	}
	noun := "windows"
	if closed == 1 {
		noun = "window"
	}
	body := fmt.Sprintf("closed %d %s", closed, noun)
	if !h.writerFlag() {
		body += "; this window writes now"
	}
	h.Notify(slog.LevelWarn, "Windows closed", body)
	// the killed windows release their leases as they exit: look for it once
	// before the ticker does
	h.GoSafe("lease nudge", func() {
		time.Sleep(300 * time.Millisecond)
		h.leaseTick(false)
	})
	return errors.Join(errs...)
}

// openCloseOthersConfirm is the ask closing the rest opens: one row, its key
// armed.
func (h *Host) openCloseOthersConfirm(confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	detail := fmt.Sprintf("%d windows close; their unsent drafts go with them.", len(h.killableRows()))
	if !h.writerFlag() {
		detail += " This window writes next."
	}
	ui.OpenOverlay(Overlay{
		ID: "windows close", Title: "Close others", Origin: lua.OriginBuiltin, Confirm: confirm,
		Items: []OverlayItem{{
			Text:   "Close the other windows",
			Detail: detail,
			Keys:   []ItemKey{{Key: "x", Desc: "close", Danger: true, Run: "windows close-others"}},
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
