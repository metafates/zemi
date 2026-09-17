package hostapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/internal/win"
	glua "github.com/yuin/gopher-lua"
)

// UIHooks is what the host API needs from the UI layer. Implemented by
// internal/ui; hostapi never imports the UI.
type UIHooks interface {
	Notify(level, title, body string)
	OpenOverlay(o Overlay)
	CloseOverlay(id string)
	// CloseLuaOverlays drops every overlay the interpreter owns (Gen != 0).
	CloseLuaOverlays()
	// Reload replays the boot steps on a fresh generation (/reload).
	Reload()
	Refresh()
	ScrollChat(kind string)
	AgentEvent(ev AgentEvent)
	AskPermission(req PermRequest) (allowed bool, decidedBy string)
	PickModel(items []ModelOption, current string) (string, bool)
	ReviewMemories(items []MemoryRow) map[string]string // row id -> accept, reject or delete
	// Screenshot is the last drawn frame as plain text, "" before the
	// first one; safe from any goroutine.
	Screenshot() string
	AskQuestion(qs []tools.Question) (answers []string, ok bool)
	// ComposerEdit applies a zemi.composer edit on the UI goroutine: op is
	// "set" (cursor -1 = end), "insert" or "submit".
	ComposerEdit(op, text string, cursor int)
	// EditFile opens a file in the user's editor and blocks until it closes;
	// it returns where the editor ran (fullscreen, popup, split). Never
	// call it on the UI goroutine.
	EditFile(ctx context.Context, req EditRequest) (mode string, err error)
	// FocusWindow gives the user's keys to a window (0 = back to zemi).
	FocusWindow(id int)
}

// EditRequest is one editor handoff: an absolute path, a 1-based line to
// start at (0 = none) and a short title for the popup and the working row.
type EditRequest struct {
	Path  string
	Line  int
	Title string
}

// Overlay is the Go-side record of an open Lua overlay. Items are plain
// marshaled data; render/on_select stay as interpreter-owned functions.
type Overlay struct {
	ID       string
	Title    string
	Origin   lua.Origin
	Path     string
	Items    []any
	ItemText []string // render_item's line per item, rendered when it opened
	OnSelect *glua.LFunction
	OnClose  *glua.LFunction
	Gen      Gen
	Anchor   string // "bottom": over the chat's bottom-left, by the statusline; "" centered

	// Body is text above the items; with no items the overlay is a
	// read-only view. Empty is the line shown when there are no items and
	// no body. Selected is the item selected when it opens.
	Body     string
	Empty    string
	Selected int

	// Input makes the overlay a one-line text prompt (zemi.ui.input) that
	// starts with Default; OnSelect receives the text.
	Input   bool
	Default string

	// Reload makes a Go-built list a toggle list: enter and space run the
	// row's "run" command and rebuild the items, keeping the overlay open.
	Reload func() []any
}

// ChatMessage is a transcript entry (composer submissions); assistant
// replies carry the turn stats the chat widget renders. Kind marks the
// entry shape: "" (plain message) or "reasoning" (a finished thinking
// block the chat widget collapses to a one-line summary).
type ChatMessage struct {
	Role   string
	Kind   string // "", "reasoning", "tool", "compact" or "error"
	Text   string // tool rows: the input summary
	Label  string // tool rows: the model's description of a command
	Tool   string
	Status string // tool rows: "ok" or "err"; error rows: "cancelled" for an interrupt
	Detail string // tool rows: subagent or workflow progress summary
	Diff   string // file tool rows: the applied change as "- "/"+ " lines
	Output string // tool rows: the last lines of the result (outputTail)
	Stat   *TurnStat
}

// outputTail keeps the last 10 lines of a tool result, each capped at 300
// runes and sanitized, for the transcript row.
func outputTail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	for i, l := range lines {
		lines[i] = sanitize.String(truncRunes(l, 300))
	}
	return strings.Join(lines, "\n")
}

// TurnStat summarizes one completed agent turn: token counts and wall
// time (thought = time spent in reasoning blocks).
type TurnStat struct {
	In        int64
	Out       int64
	ThoughtMS int64
	ElapsedMS int64
}

// Host bundles the core services the zemi.* namespaces adapt.
type Host struct {
	RT       *lua.Runtime
	Log      *logging.Logger
	Settings *config.Store
	Dirs     config.Dirs

	// Agent core (DESIGN §6.3). DB is the project store, GlobalDB
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
	view    string // the agent run the chat shows instead of the session, "" for none

	// trEpoch changes whenever the transcript is replaced (new, resume,
	// rewind), so the chat widget can cache the rows of settled messages.
	trEpoch atomic.Int64

	watches  *watchReg
	agents   *agentHost
	luaTools *toolReg
	stops    *stopReg

	// memReview serializes the /memory review overlay; the hook runs off
	// the UI goroutine.
	memReview sync.Mutex

	// reflectMu serializes reflector runs (one at a time, DESIGN §10.3);
	// modelFn stubs the agent and reflector model in tests.
	reflectMu  sync.Mutex
	reflecting atomic.Bool
	modelFn    func() (fantasy.LanguageModel, error)

	md markdownCache

	// pendingMemory counts proposals awaiting review for the statusline
	// badge (refreshPending).
	pendingMemory atomic.Int32

	selectSeq atomic.Int64 // ids for zemi.ui.select overlays

	errs  errLog   // recent runtime errors for /errors
	evlog eventLog // recent events and notifications for Lua reflection

	// jobs owns the model's background processes (job_* tools).
	jobs *tools.Jobs

	// skillsReg caches discovery + enabled state (DESIGN §10.5); DB
	// rows in the global store are the durable state.
	skillsReg *skillReg

	// healthReg holds the /health checks Lua registered.
	healthReg healthReg

	hostActions map[string]func(h *Host, args string) error

	// Version is the zemi build version for the splash screen.
	Version string

	composerMu     sync.Mutex
	composerText   string
	composerCursor int

	recentMu    sync.Mutex
	recent      []*store.Session
	recentStale atomic.Bool

	mcp   *mcpReg  // MCP servers and their tools
	tasks *taskReg // subagent runs
	lsp   *lspReg  // language servers
	Wins  *win.Manager

	todoMu     sync.Mutex
	todoSID    string
	todos      []store.Todo
	todosStale atomic.Bool

	// autoApproved remembers requests the auto-mode model approved, so the
	// same command does not cost a second check.
	autoApproved sync.Map

	// luaApproved holds the chunk names of Lua tool runs the user approved
	// (guardModelLua); luaRuns numbers them.
	luaApproved sync.Map
	luaRuns     atomic.Int64

	// turn is the scope of the turn runTurns is on: cancelling it stops
	// compaction, the model, tools and auto-mode screening.
	turn atomic.Pointer[turnScope]

	modes modeReg // permission modes in tab order

	// luaRules are the rules zemi.permission.rule added: a reload replays
	// the config that adds them. PurgeGeneration moves them to staleRules,
	// which stay in Perm until the replay is done so no call runs without
	// them; ForceReload then drops those the replay did not add again.
	luaRulesMu sync.Mutex
	luaRules   []perm.Rule
	staleRules []perm.Rule

	// reloadDone is non-nil and open while reloads run; reloadSeq counts
	// reloads started (see awaitReload). reloadRun keeps one reload's sweep
	// of staleRules from racing the next one's replay.
	reloadMu   sync.Mutex
	reloadRun  sync.Mutex
	reloads    int
	reloadSeq  uint64
	reloadDone chan struct{}

	// projectTrusted is the trust boot step's decision (DESIGN §5): project
	// config, instructions and language servers load only when it is set,
	// and auto and plan mode settle less without it.
	projectTrusted atomic.Bool
	// project is the trust root, ~-shortened, found once: widgets read it
	// every frame and finding it walks the disk
	project func() string

	// sandbox is the startup probe's result (sandbox.go)
	sandbox atomic.Pointer[sandboxProbe]
	// guards holds, per running sandboxed command, the protected paths that
	// were missing when it started (guardSandbox)
	guardMu sync.Mutex
	guards  map[*sandboxGuard]bool
	// winGuards maps a sandboxed window's id to its guardSandbox check
	winGuards sync.Map
	// trustHeld keeps the project untrusted until /trust, after a sandboxed
	// command created a protected path
	trustHeld atomic.Bool
}

// SetProjectTrusted records the trust decision. A change drops the auto-mode
// approvals, which were given under the other decision.
func (h *Host) SetProjectTrusted(ok bool) {
	if h.projectTrusted.Swap(ok) != ok {
		h.autoApproved.Clear()
	}
}

// TrustUnsaved reports a trust root no decision is saved for: the home
// directory and the filesystem root hold everything, so they stay untrusted
// unless /trust or --trust says otherwise for one run.
func TrustUnsaved(root string) bool {
	home, err := os.UserHomeDir()
	return root == "" || filepath.Dir(root) == root || (err == nil && home != "" && perm.CanonPath(root) == perm.CanonPath(home))
}

// ProjectTrusted reports whether the user trusts this project.
func (h *Host) ProjectTrusted() bool { return h.projectTrusted.Load() }

// Session is the session state (title, cwd, transcript).
type Session struct {
	Title      string
	CWD        string
	Transcript []ChatMessage
}

// NewHost wires the registries and opens the stores (best effort:
// failures surface at the namespaces boot step).
func NewHost(rt *lua.Runtime, log *logging.Logger, settings *config.Store, dirs config.Dirs) *Host {
	h := &Host{
		RT:        rt,
		Log:       log,
		Settings:  settings,
		Dirs:      dirs,
		Keymaps:   NewKeymapRegistry(),
		Commands:  NewCommandRegistry(),
		Widgets:   NewWidgetRegistry(),
		Events:    NewEventRegistry(),
		Themes:    NewThemeRegistry(),
		session:   &Session{Title: "zemi", CWD: dirs.ProjectDir},
		watches:   newWatchReg(),
		Providers: agent.NewRegistry(),
		Perm:      perm.NewStore(),
		agents:    newAgentHost(),
		luaTools:  newToolReg(),
		stops:     newStopReg(),
		jobs:      tools.NewJobs(),
		skillsReg: &skillReg{},
		mcp:       newMCPReg(),
		tasks:     newTaskReg(),
		lsp:       newLSPReg(),
		Wins:      &win.Manager{},
	}
	h.Perm.Default = h.fsDefault
	h.Wins.OnExit = h.windowExited
	h.project = sync.OnceValue(func() string { return TildePath(h.Dirs.TrustRoot()) })
	if dirs.StateHome != "" {
		h.GlobalDB, h.DB, h.initErr = store.OpenForDirs(dirs.StateHome, dirs.ProjectDir)
	}
	h.hostActions = map[string]func(h *Host, args string) error{
		"zemi.ui.help": func(h *Host, _ string) error { h.OpenHelp(); return nil },
		"zemi.ui.reasoning.toggle": func(h *Host, _ string) error {
			h.ToggleReasoning()
			return nil
		},
		"zemi.ui.scroll.up":          func(h *Host, _ string) error { h.scrollChat("up"); return nil },
		"zemi.ui.scroll.down":        func(h *Host, _ string) error { h.scrollChat("down"); return nil },
		"zemi.ui.scroll.top":         func(h *Host, _ string) error { h.scrollChat("top"); return nil },
		"zemi.ui.scroll.bottom":      func(h *Host, _ string) error { h.scrollChat("bottom"); return nil },
		"zemi.model.pick":            func(h *Host, _ string) error { h.pickAndStoreModel(); return nil },
		"zemi.permission.mode.cycle": func(h *Host, _ string) error { return h.cyclePermMode(1) },
		"zemi.permission.mode.prev":  func(h *Host, _ string) error { return h.cyclePermMode(-1) },
		"zemi.reload":                nil, // Go-reserved; never dispatched here
	}
	h.refreshPending()
	h.recentStale.Store(true)
	h.trEpoch.Store(1) // 0 means "an agent run's transcript; do not cache"
	// sync: the first turn's tool schema and --health read it (the probe is fast)
	b, err := sandbox.Probe()
	h.sandbox.Store(&sandboxProbe{b, err})
	return h
}

func (h *Host) hooks() UIHooks {
	h.uiMu.RLock()
	defer h.uiMu.RUnlock()
	return h.ui
}

func (h *Host) SetUIHooks(ui UIHooks) {
	h.uiMu.Lock()
	h.ui = ui
	h.uiMu.Unlock()
}

// Reflecting reports whether a reflector run is active; the UI keeps
// re-rendering widgets while it is, so the statusline badge animates.
func (h *Host) Reflecting() bool { return h.reflecting.Load() }

// SessionTitle is the current session's title, for the terminal tab.
func (h *Host) SessionTitle() string {
	h.tMu.Lock()
	defer h.tMu.Unlock()
	return h.session.Title
}

func (h *Host) refresh() {
	if ui := h.hooks(); ui != nil {
		ui.Refresh()
	}
}

func (h *Host) Notify(level, title, body string) {
	h.recordNotify(level, title, body)
	if ui := h.hooks(); ui != nil {
		ui.Notify(level, title, body)
	}
}

// scrollChat routes a transcript scroll request to the UI (no-op before
// the UI attaches, e.g. headless runs).
func (h *Host) scrollChat(kind string) {
	if ui := h.hooks(); ui != nil {
		ui.ScrollChat(kind)
	}
}

// OpenModelPicker opens the Go-owned model picker overlay without blocking
// the caller, so the UI goroutine may call it (the /model slash command);
// the chosen model is stored when the user answers.
func (h *Host) OpenModelPicker() { h.GoSafe("model picker", h.pickAndStoreModel) }

// pickAndStoreModel blocks on the picker, so it must run off the UI and
// interpreter goroutines; the config watches it fires go through the queue.
func (h *Host) pickAndStoreModel() {
	id, ok := h.pickModel()
	if !ok {
		return
	}
	fired, err := h.Settings.SetQuiet("agent.model", id)
	if err != nil {
		h.Notify("error", "Could not switch the model", err.Error())
		return
	}
	if len(fired) > 0 {
		_, _ = h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			h.deliverFired(L, fired)
			return nil, nil
		})
	}
	h.refresh()
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

func (h *Host) AppendMessage(role, text string) {
	h.appendChatMessage(ChatMessage{Role: role, Text: text})
}

func (h *Host) appendChatMessage(m ChatMessage) {
	h.tMu.Lock()
	h.session.Transcript = append(h.session.Transcript, m)
	h.tMu.Unlock()
	h.refresh()
}

// AppendTurnResult adds the assistant reply with its turn stats (tokens,
// thought time, elapsed time) for the chat widget to render.
func (h *Host) AppendTurnResult(text string, stat TurnStat) {
	h.appendChatMessage(ChatMessage{Role: "assistant", Text: text, Stat: &stat})
}

// AppendReasoning adds the persisted thinking block of a finished turn:
// the chat widget collapses it to a one-line summary unless the reasoning
// toggle is on (§8). Called by the agent loop's TurnDone.
func (h *Host) AppendReasoning(text string, stat TurnStat) {
	h.appendChatMessage(ChatMessage{Role: "assistant", Kind: "reasoning", Text: text, Stat: &stat})
}

// OpenHelp opens the help overlay listing keymaps and slash commands.
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
		items = append(items, map[string]any{"text": b.Key, "desc": desc, "section": "Keys"})
	}
	// Hidden keeps a command out of the palette's default list, not out of
	// /help: this is the index the README points people at.
	for _, c := range h.Commands.List() {
		items = append(items, map[string]any{"text": "/" + c.Name, "desc": c.Desc, "section": "Commands", "fill": "/" + c.Name + " "})
	}
	ui.OpenOverlay(Overlay{ID: "help", Title: "Help", Origin: lua.OriginBuiltin, Items: items})
}

func describeRHS(rhs RHS) string {
	switch {
	case rhs.Command != "":
		return "/" + rhs.Command
	case rhs.API != "":
		return rhs.API
	default:
		return "<function>"
	}
}

// ToggleReasoning flips the session-scoped reasoning expansion override
// (ctrl+o, §8): finished thinking blocks collapse to a one-line summary by
// default and this shows them in full, or hides them again.
func (h *Host) ToggleReasoning() {
	ag := h.agents
	ag.mu.Lock()
	v := h.reasoningShown()
	nv := !v
	ag.reasoningShown = &nv
	ag.mu.Unlock()
	h.refresh()
}

// ReasoningShown reports whether finished reasoning blocks render expanded:
// the session toggle wins, then the ui.chat.reasoning config knob.
func (h *Host) ReasoningShown() bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return h.reasoningShown()
}

// reasoningShown reads the effective expansion state (agents.mu held).
func (h *Host) reasoningShown() bool {
	if h.agents.reasoningShown != nil {
		return *h.agents.reasoningShown
	}
	if v, ok := h.Settings.Get("ui.chat.reasoning"); ok {
		if b, ok := v.(bool); ok {
			return b
		}
		if f, ok := v.(float64); ok {
			return f != 0
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// host -> Lua event plumbing

// emitSync runs the handlers synchronously on the given interpreter state;
// callers are boot steps and the Lua-side emit, already on the interpreter.
func (h *Host) emitSync(L *glua.LState, handlers []*EventHandler, name string, payload map[string]any, from string) {
	ev := eventTable(name, from, payload)
	for _, hd := range handlers {
		if _, _, err := h.callEvent(L, hd, ev, lua.VetoDeadline); err != nil {
			h.logHandlerError(hd, err)
		}
	}
}

// EmitOnState is the boot-step entry: run handlers for a host event on the
// interpreter goroutine that is executing boot steps.
func (h *Host) EmitOnState(L *glua.LState, name string, payload map[string]any) {
	h.recordEvent(name, "host", payload)
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
	h.recordEvent(name, "host", payload)
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
	ev := eventTable(name, "host", payload)
	_, err := h.RT.Do(ctx, lua.ClassUI, time.Duration(len(handlers))*deadline+2*time.Second, 0,
		func(L *glua.LState) ([]any, error) {
			for i, hd := range handlers {
				ret, timedOut, err := h.callEvent(L, hd, ev, deadline)
				switch {
				case timedOut:
					outcomes[i].timed = true
				case err != nil:
					h.logHandlerError(hd, err)
				default:
					// a repo may not keep the user from quitting
					if name == "app.quit" && hd.Origin.Rank() <= lua.OriginProject.Rank() {
						break
					}
					if tb, ok := ret.(*glua.LTable); ok && rawString(tb, "decision") == "deny" {
						outcomes[i] = outcome{denied: true, reason: rawString(tb, "reason")}
					}
				}
			}
			return nil, nil
		})
	if errors.Is(err, lua.ErrAbandoned) {
		return VetoResult{TimedOut: true}
	}
	if err != nil && name == "permission.request" {
		// a hot runtime cannot run the deny hooks: fail closed
		return VetoResult{Denied: true, Handled: true, Reason: "permission.request hooks could not run (lua runtime unavailable, /reload): " + err.Error()}
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

func eventTable(name, from string, payload map[string]any) map[string]any {
	ev := map[string]any{"name": name, "from": from}
	maps.Copy(ev, payload)
	return ev
}

// callEvent runs one handler fn(ev) under its origin with the deadline armed
// at start and returns its first result. A handler that ran past the
// deadline reports timedOut and no result, even if it returned.
func (h *Host) callEvent(L *glua.LState, hd *EventHandler, ev map[string]any, deadline time.Duration) (ret glua.LValue, timedOut bool, err error) {
	if hd.Once {
		h.Events.drop(hd)
	}
	defer h.RT.PushFrame(L, hd.Origin, hd.Path)()
	handlerCtx, restore := setHandlerContext(L, deadline)
	defer restore()
	base := L.GetTop()
	defer L.SetTop(base)
	L.Push(hd.Fn)
	L.Push(lua.FromGo(L, ev))
	err = L.PCall(1, 1, nil)
	if timedOut = handlerCtx.Err() != nil; timedOut || err != nil {
		return glua.LNil, timedOut, err
	}
	return L.Get(-1), false, nil
}

func (h *Host) logHandlerError(hd *EventHandler, err error) {
	h.Log.Error("events", "event handler failed", map[string]any{
		"event": hd.Event, "id": hd.ID, "origin": string(hd.Origin), "error": err.Error(),
	})
	h.RecordError("event "+hd.Event, err.Error())
}

// ---------------------------------------------------------------------------
// widget rendering

// Segment is one styled run of text.
type Segment struct {
	Text string
	HL   string
	Hit  int // Go chrome only: 1-based index of the clickable item this text belongs to
}

// RenderWidget runs a widget's Lua render for the given width and returns
// sanitized segment lines (DESIGN §6.6, §8).
func (h *Host) RenderWidget(ctx context.Context, w *Widget, width int, state UIState) ([][]Segment, error) {
	linesAny, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		// The purity guard is raised on the interpreter goroutine, only
		// while render runs: raised before Do, it also refused Lua that ran
		// while this render merely waited in the queue (boot code behind
		// the trust prompt).
		h.renderMu.Lock()
		h.renderSize = width
		h.renderMu.Unlock()
		defer func() {
			h.renderMu.Lock()
			h.renderSize = 0
			h.renderMu.Unlock()
		}()
		defer h.RT.PushFrame(L, w.Origin, w.Path)()
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
	Time   string
	Pulse  bool // shared blink phase for running markers
	Height int  // chat viewport rows at the last frame (0 before the first)
}

// TildePath shortens the home directory, or a path under it, to ~.
func TildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

func (h *Host) widgetCtxTable(L *glua.LState, width int, state UIState) *glua.LTable {
	sess := h.Session()
	tb := L.NewTable()
	tb.RawSetString("width", glua.LNumber(width))
	tb.RawSetString("time", glua.LString(state.Time))
	tb.RawSetString("pulse", glua.LBool(state.Pulse))
	tb.RawSetString("height", glua.LNumber(state.Height))
	tb.RawSetString("version", glua.LString(h.Version))
	tb.RawSetString("cwd", glua.LString(TildePath(sess.CWD)))
	tb.RawSetString("project", glua.LString(h.project()))
	tb.RawSetString("trusted", glua.LBool(h.ProjectTrusted()))
	stream := h.AgentStream()
	if stream.Profile == "" {
		stream.Profile = "default"
	}
	tb.RawSetString("profile", glua.LString(stream.Profile))
	tb.RawSetString("model", glua.LString(stream.Model))
	tb.RawSetString("effort", glua.LString(h.Effort()))
	tb.RawSetString("agent_active", glua.LBool(stream.Active))
	mode := h.permModeDef()
	tb.RawSetString("permission_mode", glua.LString(mode.Name))
	tb.RawSetString("permission_mode_base", glua.LString(mode.Base))
	tb.RawSetString("show_reasoning", glua.LBool(h.ReasoningShown()))
	tb.RawSetString("reflecting", glua.LBool(h.reflecting.Load()))
	tb.RawSetString("pending_memory", glua.LNumber(h.pendingMemory.Load()))
	tb.RawSetString("agents_running", glua.LNumber(h.tasks.running()))
	// Showing an agent run, the chat renders its transcript alone: the
	// session's live turn and todos stay out of it.
	run := h.viewedRun()
	transcript := sess.Transcript
	var todoRows []store.Todo
	if run != nil {
		status, _, _, _ := run.state()
		v := L.NewTable()
		v.RawSetString("id", glua.LString(run.id))
		v.RawSetString("agent", glua.LString(run.agent))
		v.RawSetString("description", glua.LString(sanitize.String(run.description)))
		v.RawSetString("status", glua.LString(status))
		tb.RawSetString("view", v)
		transcript = run.transcript()
		stream.Text, stream.Reasoning, stream.Tools, stream.Steering = "", "", nil, nil
	} else {
		todoRows = h.todoSnapshot()
	}
	todos := L.NewTable()
	for _, td := range todoRows {
		row := L.NewTable()
		row.RawSetString("content", glua.LString(sanitize.String(td.Content)))
		row.RawSetString("status", glua.LString(td.Status))
		row.RawSetString("active_form", glua.LString(sanitize.String(td.ActiveForm)))
		todos.Append(row)
	}
	tb.RawSetString("todos", todos)
	tb.RawSetString("usage", usageTable(L, stream))
	tb.RawSetString("context", contextTable(L, stream))
	at := L.NewTable()
	at.RawSetString("active", glua.LBool(stream.Active))
	at.RawSetString("model", glua.LString(stream.Model))
	at.RawSetString("text", glua.LString(stream.Text))
	at.RawSetString("reasoning", glua.LString(stream.Reasoning))
	steering := L.NewTable()
	for _, s := range stream.Steering {
		steering.Append(glua.LString(s))
	}
	at.RawSetString("steering", steering)
	at.RawSetString("thought", glua.LNumber(stream.Thought.Seconds()))
	at.RawSetString("elapsed", glua.LNumber(stream.Elapsed.Seconds()))
	toolsTb := L.NewTable()
	for _, t := range stream.Tools {
		row := L.NewTable()
		row.RawSetString("name", glua.LString(t.Name))
		row.RawSetString("input", glua.LString(t.Input))
		row.RawSetString("status", glua.LString(t.Status))
		row.RawSetString("detail", glua.LString(t.Detail))
		row.RawSetString("label", glua.LString(sanitize.String(t.Label)))
		if !t.Started.IsZero() {
			row.RawSetString("elapsed", glua.LNumber(time.Since(t.Started).Seconds()))
		}
		row.RawSetString("diff", glua.LString(sanitize.String(t.Diff)))
		row.RawSetString("output", glua.LString(t.Output))
		toolsTb.Append(row)
	}
	at.RawSetString("tools", toolsTb)
	tb.RawSetString("agent", at)
	tr := L.NewTable()
	for _, m := range transcript {
		row := L.NewTable()
		row.RawSetString("role", glua.LString(m.Role))
		row.RawSetString("kind", glua.LString(m.Kind))
		row.RawSetString("text", glua.LString(m.Text))
		row.RawSetString("tool", glua.LString(m.Tool))
		row.RawSetString("status", glua.LString(m.Status))
		row.RawSetString("detail", glua.LString(m.Detail))
		row.RawSetString("label", glua.LString(sanitize.String(m.Label)))
		row.RawSetString("diff", glua.LString(sanitize.String(m.Diff)))
		row.RawSetString("output", glua.LString(m.Output))
		if m.Stat != nil {
			row.RawSetString("in", glua.LNumber(m.Stat.In))
			row.RawSetString("out", glua.LNumber(m.Stat.Out))
			row.RawSetString("thought", glua.LNumber(float64(m.Stat.ThoughtMS)/1000))
			row.RawSetString("elapsed", glua.LNumber(float64(m.Stat.ElapsedMS)/1000))
		}
		tr.Append(row)
	}
	tb.RawSetString("transcript", tr)
	// 0 while an agent run is shown: its rows mutate in place as the run
	// goes, so the chat widget must not cache them.
	epoch := h.trEpoch.Load()
	if run != nil {
		epoch = 0
	}
	tb.RawSetString("transcript_epoch", glua.LNumber(epoch))
	if len(sess.Transcript) == 0 {
		recent := L.NewTable()
		for _, r := range h.recentSessions() {
			row := L.NewTable()
			row.RawSetString("id", glua.LString(r.ID))
			row.RawSetString("title", glua.LString(sanitize.String(r.Title)))
			row.RawSetString("ago", glua.LString(ago(r.UpdatedAt)))
			recent.Append(row)
		}
		tb.RawSetString("recent", recent)
	}
	return tb
}

// usageTable is the session token tally for widgets (DESIGN §8): totals
// across all turns of this session.
func usageTable(L *glua.LState, s StreamSnapshot) *glua.LTable {
	u := L.NewTable()
	u.RawSetString("in", glua.LNumber(s.SessionIn))
	u.RawSetString("out", glua.LNumber(s.SessionOut))
	u.RawSetString("total", glua.LNumber(s.SessionIn+s.SessionOut))
	u.RawSetString("cost", glua.LNumber(s.SessionCost))
	return u
}

// contextTable is the current-context estimate: the last prompt size
// against the model's context window, with the percent for the statusline.
func contextTable(L *glua.LState, s StreamSnapshot) *glua.LTable {
	c := L.NewTable()
	c.RawSetString("tokens", glua.LNumber(s.LastIn))
	c.RawSetString("window", glua.LNumber(s.CtxWindow))
	pct := 0.0
	if s.CtxWindow > 0 {
		pct = float64(s.LastIn) / float64(s.CtxWindow) * 100
	}
	c.RawSetString("pct", glua.LNumber(pct))
	return c
}

// ---------------------------------------------------------------------------
// overlay actions from the UI

// SelectOverlay runs the overlay's on_select with the chosen item. The UI
// closes the overlay first, so on_select may reopen one with the same id.
func (h *Host) SelectOverlay(ctx context.Context, o Overlay, index int) {
	var item any
	if index >= 0 && index < len(o.Items) {
		item = o.Items[index]
	}
	h.submitOverlay(ctx, o, item)
}

// SubmitOverlayInput hands an input overlay's text to on_select.
func (h *Host) SubmitOverlayInput(ctx context.Context, o Overlay, text string) {
	h.submitOverlay(ctx, o, text)
}

// staleOverlay reports an overlay left open across a reload: its callbacks
// belong to a retired LState, so they are dropped rather than called
// (DESIGN §4). Go-built overlays carry no generation.
func (h *Host) staleOverlay(o Overlay) bool {
	return o.Gen != 0 && o.Gen != h.currentGen()
}

func (h *Host) submitOverlay(ctx context.Context, o Overlay, item any) {
	if o.OnSelect == nil || h.staleOverlay(o) {
		return
	}
	_, _ = h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		defer h.RT.PushFrame(L, o.Origin, o.Path)()
		base := L.GetTop()
		var itemV glua.LValue = glua.LNil
		if item != nil {
			itemV = lua.FromGo(L, item)
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
}

// CancelOverlay closes an overlay and runs on_close if present.
func (h *Host) CancelOverlay(ctx context.Context, o Overlay) {
	if o.OnClose != nil && !h.staleOverlay(o) {
		_, _ = h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			defer h.RT.PushFrame(L, o.Origin, o.Path)()
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

func (h *Host) refuseDuringRender(L *glua.LState, what string) {
	h.renderMu.Lock()
	w := h.renderSize
	h.renderMu.Unlock()
	if w != 0 {
		L.RaiseError("zemi: %s is not allowed during widget render (render must be pure)", what)
	}
}

// refuseBelowUser raises unless user config or shipped code is calling:
// the call acts with the user's authority, like command.run's Go commands.
func (h *Host) refuseBelowUser(L *glua.LState, what string) {
	if o := h.RT.CurrentOrigin(L); o != lua.OriginUser && o != lua.OriginBuiltin {
		L.RaiseError("zemi.%s: only user config may call this, not %s code", what, o)
	}
}

// ---------------------------------------------------------------------------
// boot step: register all zemi.* namespaces on a fresh generation

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
	h.syncSkills()
	h.syncTypes()

	g.SB.SetGuard(h.guardModelLua)
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
		{"zemi.ui", h.uiNamespace(L, g.SB), nil},
		{"zemi.env", h.envNamespace(L), nil},
		{"zemi.state", h.stateNamespace(L), nil},
		{"zemi.timer", h.timerNamespace(L), nil},
		{"zemi.fs", h.fsNamespace(L), nil},
		{"zemi.http", h.httpNamespace(L), nil},
		{"zemi.job", h.jobNamespace(L), nil},
		{"zemi.win", h.winNamespace(L), nil},
		{"zemi.tool", h.toolNamespace(L), nil},
		{"zemi.permission", h.permissionNamespace(L), nil},
		{"zemi.provider", h.providerNamespace(L), nil},
		{"zemi.model", h.modelNamespace(L), nil},
		{"zemi.agent", h.agentNamespace(L), nil},
		{"zemi.workflow", h.workflowNamespace(L), nil},
		{"zemi.json", h.jsonNamespace(L), nil},
		{"zemi.composer", h.composerNamespace(L), nil},
		{"zemi.mcp", h.mcpNamespace(L), nil},
		{"zemi.lsp", h.lspNamespace(L), nil},
		{"zemi.session", h.sessionNamespace(L), nil},
		{"zemi.memory", h.memoryNamespace(L), nil},
		{"zemi.skills", h.skillsNamespace(L), nil},
		{"zemi.learn", h.learnNamespace(L), nil},
		{"zemi.health", h.healthNamespace(L), nil},
		{"zemi.errors", h.errorsNamespace(L), nil},
	}
	for _, p := range pairs {
		if err := register(p.name, p.tbl, p.call); err != nil {
			return err
		}
	}
	api := L.NewTable()
	api.RawSetString("version", glua.LString("0.21.0"))
	return g.SB.RegisterProtectedValue("zemi.api", api)
}

func (h *Host) currentGen() Gen { return Gen(h.gen.Load()) }

// ReloadConfig runs /reload: main owns the closure and the UI dispatches
// it, so config zemi wrote for itself takes effect without a restart.
func (h *Host) ReloadConfig() error {
	// config that reloads while it is loading would replay itself forever
	if h.RT.Booting() {
		return errors.New("config is still loading; a reload from config would repeat forever")
	}
	ui := h.hooks()
	if ui == nil {
		return errors.New("reload needs the TUI")
	}
	ui.Reload()
	return nil
}

// ForceReload replays the boot steps on a fresh generation (DESIGN §4).
// Until it returns, tool vetoes wait (awaitReload) and the old Lua rules
// stay in force, so a running turn is never checked against half a config.
func (h *Host) ForceReload(ctx context.Context, steps []lua.BootStep) error {
	h.reloadMu.Lock()
	if h.reloads == 0 {
		h.reloadDone = make(chan struct{})
	}
	h.reloads++
	h.reloadSeq++
	h.reloadMu.Unlock()
	defer func() {
		h.reloadMu.Lock()
		if h.reloads--; h.reloads == 0 {
			close(h.reloadDone)
			h.reloadDone = nil
		}
		h.reloadMu.Unlock()
	}()

	h.reloadRun.Lock()
	defer h.reloadRun.Unlock()
	err := h.RT.ForceReload(ctx, steps, h.PurgeGeneration)
	// a failed replay keeps the old rules: the last whole config beats part of one
	if err == nil {
		h.luaRulesMu.Lock()
		for _, r := range h.staleRules {
			h.Perm.Remove(r)
		}
		h.staleRules = nil
		h.luaRulesMu.Unlock()
	}
	return err
}

// claimStale reports whether r is a rule the replaced generation added,
// taking it off the list its reload drops.
func (h *Host) claimStale(r perm.Rule) bool {
	h.luaRulesMu.Lock()
	defer h.luaRulesMu.Unlock()
	i := slices.Index(h.staleRules, r)
	if i >= 0 {
		h.staleRules = slices.Delete(h.staleRules, i, i+1)
	}
	return i >= 0
}

// awaitReload waits out running reloads and returns how many have started.
func (h *Host) awaitReload() uint64 {
	for {
		h.reloadMu.Lock()
		done, seq := h.reloadDone, h.reloadSeq
		h.reloadMu.Unlock()
		if done == nil {
			return seq
		}
		<-done
	}
}

// outsideReload runs a tool veto until no reload overlapped it. Mid-reload
// the handlers are purged or not yet replayed, and a veto with no handler
// is no objection, so the user's tool.pre and permission.request denials
// would be skipped.
func (h *Host) outsideReload(veto func()) {
	seq := h.awaitReload()
	for {
		veto()
		next := h.awaitReload()
		if next == seq {
			return
		}
		seq = next
	}
}

// PurgeGeneration drops every registration belonging to the live
// generation plus config watches; the force-reload path calls it before
// replaying the boot steps so no stale callback fires into a dropped
// LState (DESIGN §4).
func (h *Host) PurgeGeneration() {
	h.modes.mu.Lock()
	h.modes.list = nil
	h.modes.mu.Unlock()
	h.luaRulesMu.Lock()
	h.staleRules = append(h.staleRules, h.luaRules...)
	h.luaRules = nil
	h.luaRulesMu.Unlock()
	h.Keymaps.Purge()
	h.Commands.Purge()
	h.Widgets.Purge()
	h.Events.Purge()
	h.Themes.Purge()
	h.purgeWatches()
	h.healthReg.purge()
	h.luaTools.purge(h.currentGen())
	h.stops.purge()
	h.mcp.stopAll()
	h.lsp.stopAll()
	// An overlay left open would keep drawing with callbacks that are now
	// dropped (submitOverlay), which reads as a frozen picker.
	if ui := h.hooks(); ui != nil {
		ui.CloseLuaOverlays()
	}
}

type turnScope struct {
	ctx    context.Context
	cancel context.CancelFunc
}
