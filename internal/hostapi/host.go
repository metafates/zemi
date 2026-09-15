package hostapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// UIHooks is what the host API needs from the UI layer. Implemented by
// internal/ui; hostapi never imports the UI.
type UIHooks interface {
	Notify(level, title, body string)
	OpenCmdline(init string)
	CloseCmdline()
	OpenOverlay(o Overlay)
	CloseOverlay(id string) bool
	Refresh()
	AgentEvent(ev AgentEvent)
	AskPermission(req PermRequest) (allowed bool, decidedBy string)
	PickModel(items []ModelOption, current string) (string, bool)
	ReviewMemories(items []MemoryRow) (deleteIdx []int)
	AskQuestion(q tools.Question) (answer string, ok bool)
}

// Overlay is the Go-side record of an open Lua overlay. Items are plain
// marshaled data; render/on_select stay as interpreter-owned functions.
type Overlay struct {
	ID         string
	Title      string
	Origin     lua.Origin
	Path       string
	Items      []any
	RenderItem *glua.LFunction
	OnSelect   *glua.LFunction
	OnClose    *glua.LFunction
	Gen        Gen
}

// ChatMessage is an M0 transcript entry (composer submissions).
type ChatMessage struct {
	Role string
	Text string
}

// Host bundles the core services the zemi.* namespaces adapt.
type Host struct {
	RT       *lua.Runtime
	Log      *logging.Logger
	Settings *config.Store
	Dirs     config.Dirs

	// M1 agent core (DESIGN §6.3, §13). DB is the project store, GlobalDB
	// the state-dir store; Perm holds config + session rules with the §9
	// built-in defaults.
	Providers *agent.Registry
	Perm      *perm.Store
	DB        *store.DB
	GlobalDB  *store.DB

	Keymaps  *KeymapRegistry
	Commands *CommandRegistry
	Widgets  *WidgetRegistry
	Events   *EventRegistry
	Themes   *ThemeRegistry

	// initErr surfaces store-open failures at the namespaces boot step.
	initErr error

	uiMu sync.RWMutex
	ui   UIHooks

	gen atomic.Int64

	renderMu   sync.Mutex
	renderSize int

	tMu     sync.Mutex
	session *Session

	watches *watchReg
	agents  *agentHost
	luaTools *toolReg
	timers  *timerReg

	// memReviewMu serializes the :memory review overlay: the hook runs on
	// its own goroutine (cmdline execution sits on the UI goroutine, which
	// must never block on a hook).
	memReview sync.Mutex

	// jobs owns the model's background processes (job_* tools).
	jobs *tools.Jobs

	hostActions map[string]func(h *Host, args string) error
}

// Session is the M0 session state (title, cwd, transcript).
type Session struct {
	Title      string
	CWD        string
	Transcript []ChatMessage
}

// NewHost wires the registries and opens the M1 stores (best effort:
// failures surface at the namespaces boot step).
func NewHost(rt *lua.Runtime, log *logging.Logger, settings *config.Store, dirs config.Dirs) *Host {
	h := &Host{
		RT:       rt,
		Log:      log,
		Settings: settings,
		Dirs:     dirs,
		Keymaps:  NewKeymapRegistry(),
		Commands: NewCommandRegistry(),
		Widgets:  NewWidgetRegistry(),
		Events:   NewEventRegistry(),
		Themes:   NewThemeRegistry(),
		session:  &Session{Title: "zemi", CWD: dirs.ProjectDir},
		watches:  newWatchReg(),
		Providers: agent.NewRegistry(),
		Perm:      perm.NewStore(),
		agents:    newAgentHost(),
		luaTools:  newToolReg(),
		timers:    newTimerReg(),
		jobs:      tools.NewJobs(),
	}
	h.Perm.Default = h.fsDefault
	if dirs.StateHome != "" {
		h.DB, h.GlobalDB, h.initErr = store.OpenForDirs(dirs.StateHome, dirs.ProjectDir)
	}
	h.hostActions = map[string]func(h *Host, args string) error{
		"zemi.cmdline.open":      func(h *Host, args string) error { h.hooks().OpenCmdline(args); return nil },
		"zemi.ui.sidebar.toggle": func(h *Host, _ string) error { h.ToggleSidebar(nil); return nil },
		"zemi.ui.help":           func(h *Host, _ string) error { h.OpenHelp(); return nil },
		"zemi.model.pick": func(h *Host, _ string) error {
			if id, ok := h.pickModel(); ok {
				_, _ = h.Settings.SetQuiet("agent.model", id)
				h.refresh()
			}
			return nil
		},
		"zemi.reload":            nil, // Go-reserved; never dispatched here
	}
	return h
}

func (h *Host) hooks() UIHooks {
	h.uiMu.RLock()
	defer h.uiMu.RUnlock()
	return h.ui
}

// SetUIHooks attaches the UI layer.
func (h *Host) SetUIHooks(ui UIHooks) {
	h.uiMu.Lock()
	h.ui = ui
	h.uiMu.Unlock()
}

func (h *Host) refresh() {
	if ui := h.hooks(); ui != nil {
		ui.Refresh()
	}
}

// ToggleSidebar flips the sidebar config value. L is the interpreter state
// when called from a Lua closure (watches are delivered inline on it); nil
// means a non-interpreter caller and watches go back through the queue.
func (h *Host) ToggleSidebar(L *glua.LState) {
	cur := false
	if v, ok := h.Settings.Get("ui.sidebar.open"); ok {
		if b, ok := v.(bool); ok {
			cur = b
		}
	}
	fired, err := h.Settings.SetQuiet("ui.sidebar.open", !cur)
	if err != nil {
		h.Log.Error("config", "sidebar toggle failed", map[string]any{"error": err.Error()})
		return
	}
	for _, f := range fired {
		if L != nil {
			h.deliverFired(L, fired)
			break
		}
		if rec, ok := h.watches.rec(f.ID); ok {
			h.deliverWatch(rec, f.ID, f.Old, f.New)
		}
	}
	h.refresh()
}

func (h *Host) SidebarOpen() bool {
	if v, ok := h.Settings.Get("ui.sidebar.open"); ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// Session returns a snapshot of the session state.
func (h *Host) Session() Session {
	h.tMu.Lock()
	defer h.tMu.Unlock()
	return Session{
		Title:      h.session.Title,
		CWD:        h.session.CWD,
		Transcript: append([]ChatMessage(nil), h.session.Transcript...),
	}
}

// AppendMessage adds a transcript entry and refreshes the UI.
func (h *Host) AppendMessage(role, text string) {
	h.tMu.Lock()
	h.session.Transcript = append(h.session.Transcript, ChatMessage{Role: role, Text: text})
	h.tMu.Unlock()
	h.refresh()
}

// AppendChatLine appends a transcript line from Lua (via zemi.notify).
func (h *Host) AppendChatLine(text string) { h.AppendMessage("user", text) }

// OpenHelp opens the help overlay listing keymaps and commands.
func (h *Host) OpenHelp() {
	ui := h.hooks()
	if ui == nil {
		return
	}
	var items []any
	for _, b := range h.Keymaps.List() {
		desc := b.Desc
		if desc == "" {
			desc = describeRHS(b.RHS)
		}
		items = append(items, map[string]any{
			"kind": "key", "keys": b.Keys, "mode": b.Mode, "desc": desc,
		})
	}
	for _, c := range h.Commands.List() {
		items = append(items, map[string]any{
			"kind": "command", "name": c.Name, "desc": c.Desc,
		})
	}
	ui.OpenOverlay(Overlay{
		ID:     "help",
		Title:  "Help - keys and commands",
		Origin: lua.OriginBuiltin,
		Items:  items,
	})
}

func describeRHS(rhs RHS) string {
	switch {
	case rhs.Command != "":
		return ":" + rhs.Command
	case rhs.API != "":
		return rhs.API
	default:
		return "<function>"
	}
}

// ---------------------------------------------------------------------------
// host -> Lua event plumbing

// Emit delivers a host event to registered Lua handlers asynchronously.
func (h *Host) Emit(ctx context.Context, name string, payload map[string]any) {
	handlers := h.Events.Handlers(name)
	if len(handlers) == 0 {
		return
	}
	_, _ = h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		h.emitSync(L, handlers, name, payload, "host")
		return nil, nil
	})
}

// emitSync runs the handlers synchronously on the given interpreter state;
// callers are boot steps and the Lua-side emit, already on the interpreter.
func (h *Host) emitSync(L *glua.LState, handlers []*EventHandler, name string, payload map[string]any, from string) {
	for _, hd := range handlers {
		h.callHandler(L, hd, name, payload, from, lua.VetoDeadline)
	}
}

// EmitOnState is the boot-step entry: run handlers for a host event on the
// interpreter goroutine that is executing boot steps.
func (h *Host) EmitOnState(L *glua.LState, name string, payload map[string]any) {
	h.emitSync(L, h.Events.Handlers(name), name, payload, "host")
}

// VetoResult is the aggregated outcome of a vetoable event.
type VetoResult struct {
	Denied   bool
	Reason   string
	TimedOut bool
	Handled  bool
}

// EmitVetoable runs handlers synchronously on the interpreter, each bounded
// by the veto deadline (armed at handler start); a timed-out handler
// contributes nothing and the aggregated decision or the default wins
// (DESIGN §4, §6.2).
func (h *Host) EmitVetoable(ctx context.Context, name string, payload map[string]any, deadline time.Duration) VetoResult {
	handlers := h.Events.Handlers(name)
	if len(handlers) == 0 {
		return VetoResult{}
	}
	type outcome struct {
		denied bool
		reason string
		timed  bool
	}
	outcomes := make([]outcome, len(handlers))
	_, err := h.RT.Do(ctx, lua.ClassUI, time.Duration(len(handlers))*deadline+2*time.Second, 0,
		func(L *glua.LState) ([]any, error) {
			for i, hd := range handlers {
				denied, reason, timedOut := h.callVetoHandler(L, hd, name, payload, "host", deadline)
				outcomes[i] = outcome{denied: denied, reason: reason, timed: timedOut}
			}
			return nil, nil
		})
	if err != nil && errors.Is(err, lua.ErrAbandoned) {
		return VetoResult{TimedOut: true}
	}
	res := VetoResult{}
	for _, o := range outcomes {
		if o.timed {
			res.TimedOut = true
		}
		if o.denied && !res.Denied {
			res.Denied = true
			res.Reason = o.reason
			res.Handled = true
		}
	}
	if res.Denied {
		h.Log.Info("events", "vetoable event denied", map[string]any{
			"event": name, "reason": res.Reason,
		})
	} else if res.TimedOut {
		h.Log.Warn("events", "vetoable event timed out; host default applies", map[string]any{"event": name})
	}
	return res
}

// setHandlerContext arms a per-handler deadline. gopher-lua's RemoveContext
// also nils the context polled by any still-running VM loop, so a handler
// invoked from a live Lua frame (e.g. zemi.event.emit under a chunk) must
// restore the enclosing request context instead of removing it: the outer
// mainLoopWithContext would otherwise dereference a nil ctx on its next
// instruction.
func setHandlerContext(L *glua.LState, deadline time.Duration) (context.Context, context.CancelFunc) {
	prev := L.Context()
	handlerCtx, cancel := context.WithTimeout(context.Background(), deadline)
	L.SetContext(handlerCtx)
	return handlerCtx, func() {
		if prev != nil {
			L.SetContext(prev)
		} else {
			L.RemoveContext()
		}
		cancel()
	}
}

func (h *Host) callHandler(L *glua.LState, hd *EventHandler, name string, payload map[string]any, from string, deadline time.Duration) {
	h.RT.PushFrame(hd.Origin, hd.Path)
	defer h.RT.PopFrame()
	ev := map[string]any{"name": name, "from": from}
	maps.Copy(ev, payload)
	_, restore := setHandlerContext(L, deadline)
	defer restore()
	base := L.GetTop()
	L.Push(hd.Fn)
	L.Push(lua.FromGo(L, ev))
	if err := L.PCall(1, 0, nil); err != nil {
		h.logHandlerError(hd, err)
	}
	L.SetTop(base)
}

func (h *Host) callVetoHandler(L *glua.LState, hd *EventHandler, name string, payload map[string]any, from string, deadline time.Duration) (denied bool, reason string, timedOut bool) {
	h.RT.PushFrame(hd.Origin, hd.Path)
	defer h.RT.PopFrame()
	ev := map[string]any{"name": name, "from": from}
	maps.Copy(ev, payload)
	handlerCtx, restore := setHandlerContext(L, deadline)
	defer restore()
	base := L.GetTop()
	L.Push(hd.Fn)
	L.Push(lua.FromGo(L, ev))
	err := L.PCall(1, 1, nil)
	if handlerCtx.Err() != nil {
		L.SetTop(base)
		return false, "", true
	}
	if err != nil {
		L.SetTop(base)
		h.logHandlerError(hd, err)
		return false, "", false
	}
	decision := L.Get(-1)
	L.SetTop(base)
	if decision == glua.LNil {
		return false, "", false
	}
	tb, ok := decision.(*glua.LTable)
	if !ok {
		return false, "", false
	}
	if tb.RawGetString("decision") == glua.LString("deny") {
		r := ""
		if s, ok := tb.RawGetString("reason").(glua.LString); ok {
			r = string(s)
		}
		return true, r, false
	}
	return false, "", false
}

func (h *Host) logHandlerError(hd *EventHandler, err error) {
	h.Log.Error("events", "event handler failed", map[string]any{
		"event": hd.Event, "id": hd.ID, "origin": string(hd.Origin), "error": err.Error(),
	})
}

// ---------------------------------------------------------------------------
// widget rendering

// Segment is one styled run of text.
type Segment struct {
	Text string
	HL   string
}

// RenderWidget runs a widget's Lua render for the given width and returns
// sanitized segment lines (DESIGN §6.6, §8).
func (h *Host) RenderWidget(ctx context.Context, w *Widget, width int, state UIState) ([][]Segment, error) {
	h.renderMu.Lock()
	h.renderSize = width
	h.renderMu.Unlock()
	defer func() {
		h.renderMu.Lock()
		h.renderSize = 0
		h.renderMu.Unlock()
	}()

	linesAny, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		h.RT.PushFrame(w.Origin, w.Path)
		defer h.RT.PopFrame()
		base := L.GetTop()
		L.Push(w.Render)
		L.Push(h.widgetCtxTable(L, width, state))
		if err := L.PCall(1, 1, nil); err != nil {
			L.SetTop(base)
			return nil, err
		}
		v, err := lua.ToGo(L.Get(-1))
		L.SetTop(base)
		if err != nil {
			return nil, err
		}
		return []any{v}, nil
	})
	if err != nil {
		return nil, err
	}
	if len(linesAny) == 0 || linesAny[0] == nil {
		return nil, nil
	}
	return convertSegments(linesAny[0])
}

func convertSegments(v any) ([][]Segment, error) {
	rows, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("widget render must return a list of lines, got %T", v)
	}
	out := make([][]Segment, 0, len(rows))
	for _, row := range rows {
		cells, ok := row.([]any)
		if !ok {
			return nil, fmt.Errorf("widget line must be a list of segments, got %T", row)
		}
		line := make([]Segment, 0, len(cells))
		for _, cell := range cells {
			switch c := cell.(type) {
			case string:
				line = append(line, Segment{Text: sanitize.String(c)})
			case map[string]any:
				seg := Segment{}
				if t, ok := c["text"].(string); ok {
					seg.Text = sanitize.String(t)
				}
				if hl, ok := c["hl"].(string); ok {
					seg.HL = hl
				}
				line = append(line, seg)
			default:
				return nil, fmt.Errorf("widget segment must be a string or table, got %T", cell)
			}
		}
		out = append(out, line)
	}
	return out, nil
}

// UIState is the context snapshot passed to widget renders.
type UIState struct {
	Mode    string
	Pending string
	Time    string
}

func (h *Host) widgetCtxTable(L *glua.LState, width int, state UIState) *glua.LTable {
	sess := h.Session()
	tb := L.NewTable()
	tb.RawSetString("width", glua.LNumber(width))
	tb.RawSetString("mode", glua.LString(state.Mode))
	tb.RawSetString("pending", glua.LString(state.Pending))
	tb.RawSetString("time", glua.LString(state.Time))
	tb.RawSetString("cwd", glua.LString(sess.CWD))
	stream := h.AgentStream()
	if stream.Profile == "" {
		stream.Profile = "default"
	}
	tb.RawSetString("profile", glua.LString(stream.Profile))
	tb.RawSetString("model", glua.LString(stream.Model))
	tb.RawSetString("agent_active", glua.LBool(stream.Active))
	at := L.NewTable()
	at.RawSetString("active", glua.LBool(stream.Active))
	at.RawSetString("model", glua.LString(stream.Model))
	at.RawSetString("text", glua.LString(stream.Text))
	at.RawSetString("reasoning", glua.LString(stream.Reasoning))
	at.RawSetString("steering", glua.LNumber(stream.Steering))
	toolsTb := L.NewTable()
	for _, t := range stream.Tools {
		row := L.NewTable()
		row.RawSetString("name", glua.LString(t.Name))
		row.RawSetString("input", glua.LString(t.Input))
		row.RawSetString("status", glua.LString(t.Status))
		toolsTb.Append(row)
	}
	at.RawSetString("tools", toolsTb)
	tb.RawSetString("agent", at)
	tr := L.NewTable()
	for _, m := range sess.Transcript {
		row := L.NewTable()
		row.RawSetString("role", glua.LString(m.Role))
		row.RawSetString("text", glua.LString(m.Text))
		tr.Append(row)
	}
	tb.RawSetString("transcript", tr)
	return tb
}

// ---------------------------------------------------------------------------
// overlay actions from the UI

// SelectOverlay runs the overlay's on_select with the chosen item, then
// closes it.
func (h *Host) SelectOverlay(ctx context.Context, o Overlay, index int) {
	if o.OnSelect == nil {
		if ui := h.hooks(); ui != nil {
			ui.CloseOverlay(o.ID)
		}
		return
	}
	_, _ = h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		h.RT.PushFrame(o.Origin, o.Path)
		defer h.RT.PopFrame()
		base := L.GetTop()
		var itemV glua.LValue = glua.LNil
		if index >= 0 && index < len(o.Items) {
			itemV = lua.FromGo(L, o.Items[index])
		}
		L.Push(o.OnSelect)
		L.Push(itemV)
		if err := L.PCall(1, 0, nil); err != nil {
			h.Log.Error("ui", "overlay on_select failed", map[string]any{
				"id": o.ID, "error": err.Error(),
			})
		}
		L.SetTop(base)
		return nil, nil
	})
	if ui := h.hooks(); ui != nil {
		ui.CloseOverlay(o.ID)
	}
}

// CancelOverlay closes an overlay and runs on_close if present.
func (h *Host) CancelOverlay(ctx context.Context, o Overlay) {
	if o.OnClose != nil {
		_, _ = h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			h.RT.PushFrame(o.Origin, o.Path)
			defer h.RT.PopFrame()
			base := L.GetTop()
			L.Push(o.OnClose)
			if err := L.PCall(0, 0, nil); err != nil {
				h.Log.Error("ui", "overlay on_close failed", map[string]any{
					"id": o.ID, "error": err.Error(),
				})
			}
			L.SetTop(base)
			return nil, nil
		})
	}
}

// ---------------------------------------------------------------------------
// registration guard (mutators refuse during widget render)

func (h *Host) refuseDuringRender(L *glua.LState, what string) bool {
	h.renderMu.Lock()
	w := h.renderSize
	h.renderMu.Unlock()
	if w != 0 {
		L.RaiseError("zemi: %s is not allowed during widget render (render must be pure)", what)
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// boot step: register all zemi.* namespaces on a fresh generation

// NamespacesStep returns the boot step that installs the host API.
func (h *Host) NamespacesStep() lua.BootStep {
	return lua.BootStep{
		Name: "namespaces",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			return h.installNamespaces(g)
		},
	}
}

func (h *Host) installNamespaces(g *lua.Generation) error {
	if h.initErr != nil {
		return fmt.Errorf("stores: %w", h.initErr)
	}
	L := g.L
	h.gen.Store(g.ID)

	register := func(name string, tbl *glua.LTable, call *glua.LFunction) error {
		if call != nil {
			return g.SB.RegisterCallableNamespace(name, tbl, call)
		}
		return g.SB.RegisterNamespace(name, tbl)
	}
	pairs := []struct {
		name string
		tbl  *glua.LTable
		call *glua.LFunction
	}{
		{"zemi.config", h.configNamespace(L), nil},
		{"zemi.keymap", h.keymapNamespace(L), nil},
		{"zemi.command", h.commandNamespace(L), nil},
		{"zemi.event", h.eventNamespace(L), nil},
		{"zemi.log", h.logNamespace(L), h.logCall(L)},
		{"zemi.notify", h.notifyNamespace(L), h.notifyCall(L)},
		{"zemi.cmdline", h.cmdlineNamespace(L), nil},
		{"zemi.ui", h.uiNamespace(L, g.SB), nil},
		{"zemi.env", h.envNamespace(L), nil},
		{"zemi.state", h.stateNamespace(L), nil},
		{"zemi.timer", h.timerNamespace(L), nil},
		{"zemi.fs", h.fsNamespace(L), nil},
		{"zemi.http", h.httpNamespace(L), nil},
		{"zemi.job", h.jobNamespace(L), nil},
		{"zemi.tool", h.toolNamespace(L), nil},
		{"zemi.permission", h.permissionNamespace(L), nil},
		{"zemi.provider", h.providerNamespace(L), nil},
		{"zemi.model", h.modelNamespace(L), nil},
		{"zemi.agent", h.agentNamespace(L), nil},
		{"zemi.session", h.sessionNamespace(L), nil},
		{"zemi.memory", h.memoryNamespace(L), nil},
	}
	for _, p := range pairs {
		if err := register(p.name, p.tbl, p.call); err != nil {
			return err
		}
	}
	api := L.NewTable()
	api.RawSetString("version", glua.LString("0.1.0-m1"))
	return g.SB.RegisterProtectedValue("zemi.api", api)
}

func (h *Host) currentGen() Gen { return Gen(h.gen.Load()) }

// PurgeGeneration drops every registration belonging to the live
// generation plus config watches; the force-reload path calls it before
// replaying the boot steps so no stale callback fires into a dropped
// LState (DESIGN §4).
func (h *Host) PurgeGeneration() {
	h.Keymaps.Purge()
	h.Commands.Purge()
	h.Widgets.Purge()
	h.Events.Purge()
	h.Themes.Purge()
	h.purgeWatches()
	h.luaTools.purge(h.currentGen())
	h.timers.purge()
}