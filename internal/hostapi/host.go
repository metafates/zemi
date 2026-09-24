package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// UIHooks is what the host API needs from the UI layer. Implemented by
// internal/ui; hostapi never imports the UI.
type UIHooks interface {
	Notify(level, title, body string)
	OpenOverlay(o Overlay)
	// CloseOverlay closes an overlay and runs its on_close, but only for a
	// caller of user or builtin origin or the origin that opened it: a
	// project timer must not dismiss the errors it caused.
	CloseOverlay(id string, origin lua.Origin)
	// CloseLuaOverlays drops every overlay the interpreter owns (Gen != 0).
	CloseLuaOverlays()
	// Reload replays the boot steps on a fresh generation (/reload).
	Reload()
	Refresh()
	// Scroll moves a scrolled region of the layout ("up", "down", "top",
	// "bottom", "line_up" or "line_down"): the node id, or the stage's when
	// id is "".
	Scroll(kind, id string)
	// LayoutRects is where the last frame drew each node with an id, plus
	// "stage" and "composer"; safe from any goroutine.
	LayoutRects() map[string]Rect
	// AgentStream says a streaming update landed; the UI only invalidates
	// the frame, so these coalesce.
	AgentStream()
	// TurnDone ends a turn; err is "" when it finished cleanly, and stopped
	// says the user ended it by dismissing a question or plan.
	TurnDone(err string, stopped bool)
	AskPermission(req PermRequest) (allowed bool, decidedBy string)
	ReviewMemories(items []MemoryRow) []MemoryRow // the rows with Action and Scope as edited
	// Screenshot is the last drawn frame as plain text, "" before the
	// first one; safe from any goroutine.
	Screenshot() string
	AskQuestion(qs []tools.Question) (answers []string, ok bool)
	// AskPlan shows a plan for approval (restore is the agent to return to on
	// approval, "" when none); ok is false when the user dismisses it.
	AskPlan(plan, restore string) (tools.PlanVerdict, bool)
	// ComposerEdit applies a zemi.composer edit on the UI goroutine: op is
	// "set" (cursor -1 = end), "insert", "submit", "history_prev",
	// "history_next", "paste" or "edit".
	ComposerEdit(op, text string, cursor int)
	// EditFile opens a file in the user's editor and blocks until it exits.
	// Never call it on the UI goroutine.
	EditFile(ctx context.Context, req EditRequest) error
	// Copy puts text on the clipboard and says so.
	Copy(text string)
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
	ItemText [][]Segment // render_item's row per item, rendered when it opened
	OnSelect *glua.LFunction
	OnAction *glua.LFunction // on_action(item, action), for a detail action with an id
	OnClose  *glua.LFunction
	Keys     []OverlayKey // list-level keys, named in the footer
	Gen      Gen
	Anchor   string // "bottom-left" or "bottom-right": in line with the composer frame, by the pill that opened it; "" centered

	// Body is text above the items; with no items the overlay is a
	// read-only view. Empty is the line shown when there are no items and
	// no body. Selected is the item selected when it opens. Verb names what
	// enter does to a row, when "confirm" does not say it.
	Body     string
	BodyMD   bool        // render Body (and every detail view's) as markdown
	Fields   bool        // set the "key: value" lines a plain Body (and every detail view) opens with as a table
	BodySegs [][]Segment // Body as styled rows, drawn in its place; Body is what y copies
	Empty    string
	Selected int
	Verb     string
	Refresh  bool // only replaces the open overlay with this ID, so one the user closed stays closed

	// Input makes the overlay a one-line text prompt (zemi.ui.input) that
	// starts with Default; OnSelect receives the text.
	Input   bool
	Default string

	// Reload makes a list a toggle list: enter and space run the row's "run"
	// command or on_select and rebuild the items, keeping the overlay open.
	// It runs on the UI worker, never on the key path.
	Reload func() (items []any, text [][]Segment)
}

// OverlayKey is one character that runs Fn(selected item) on a list.
type OverlayKey struct {
	Key, Desc string
	Fn        *glua.LFunction
}

// ChatMessage is a transcript entry (composer submissions); assistant
// replies carry the turn stats the chat widget renders. Kind marks the
// entry shape: "" (plain message) or "reasoning" (a finished thinking
// block the chat widget collapses to a one-line summary).
type ChatMessage struct {
	Role   string
	Kind   string // "", "reasoning", "tool", "compact", "notice" or "error"
	Text   string // tool rows: the input summary
	Label  string // tool rows: the model's description of a command
	Tool   string
	Status string // tool rows: "ok", "err" or "stopped" (denied or dismissed by the user); error rows: "cancelled" for an interrupt
	Detail string // tool rows: subagent or workflow progress summary
	Run    string // agent tool rows: the run id
	Diff   string // file tool rows: the applied change as "- "/"+ " lines
	Output string // tool rows: the last lines of the result (shortOutput)
	Stat   *TurnStat
}

var cutLines = regexp.MustCompile(`^\.\.\. \(lines (\d+)-(\d+) of \d+ cut\) \.\.\.$`)

// MoreLines is the one wording of what a cut left out: "… 3 more lines".
func MoreLines(n int) string {
	return fmt.Sprintf("… %d more %s", n, plural(n, "line", "lines"))
}

// shortOutput keeps 10 lines of a tool result for the transcript row, each
// capped at 300 runes and sanitized: the first three, which say what went
// wrong, and the last six, with a row counting what fell out between them.
func shortOutput(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if n := len(lines) - 9; n > 1 {
		// a tools.TruncOut marker stands for the lines it cut
		for _, l := range lines[3 : len(lines)-6] {
			if m := cutLines.FindStringSubmatch(l); m != nil {
				a, _ := strconv.Atoi(m[1])
				b, _ := strconv.Atoi(m[2])
				n += b - a
			}
		}
		cut := append([]string{MoreLines(n)}, lines[len(lines)-6:]...)
		lines = append(lines[:3:3], cut...)
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
	// Builtin seeds Providers on every generation: the catalog with the
	// env keys resolved once per process, since the environment cannot
	// change under a running zemi.
	Builtin  agent.Catalog
	Perm     *perm.Store
	DB       *store.DB
	GlobalDB *store.DB

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

	rendering atomic.Bool

	tMu     sync.Mutex
	session *Session
	view    string // the agent run the chat shows instead of the session, "" for none

	// trEpoch changes whenever the transcript is replaced (new, resume,
	// rewind), so the chat widget can cache the rows of settled messages.
	trEpoch atomic.Int64

	watches *watchReg
	// cfgPrev and cfgNow hold, per key, the values zemi.config.set wrote
	// during the previous boot and during this one, in order: a replay
	// writing the same sequence keeps a runtime pick made since (config.go)
	cfgMu    sync.Mutex
	cfgGen   Gen
	cfgPrev  map[string][]any
	cfgNow   map[string][]any
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
	initFail   atomic.Pointer[string] // headline of the user init.lua failure this boot recorded
	modelFn    func() (fantasy.LanguageModel, error)

	md        markdownCache
	lightTerm atomic.Bool // the terminal reported a light background

	// pendingMemory counts proposals awaiting review for the statusline
	// badge (refreshPending).
	pendingMemory atomic.Int32

	selectSeq atomic.Int64 // ids for overlays each call opens anew (select, input, /btw)

	errs  errLog   // recent runtime errors for /errors
	msgs  msgLog   // recent print and zemi.log lines for /messages
	evlog eventLog // recent events and notifications for Lua reflection

	pluginLoads atomic.Pointer[[]pluginLoad] // the last boot's enabled plugins, for /health and /plugins
	pluginRoots atomic.Pointer[[]string]     // the plugins that boot runs, for their skills, agents, commands and workflows

	// jobs owns the model's background processes (job_* tools).
	jobs  *tools.Jobs
	snaps snapshots

	// skillsReg caches discovery + enabled state (DESIGN §10.5); DB
	// rows in the global store are the durable state.
	skillsReg *skillReg

	// healthReg holds the /health checks Lua registered.
	healthReg healthReg
	// completions holds the zemi.composer.complete sources
	completions completeReg

	hostActions map[string]func(h *Host, args string) error

	// Version is the zemi build version for the splash screen.
	Version string
	// Stderr is where a run with no UI prints its warnings and errors.
	Stderr io.Writer

	composerMu     sync.Mutex
	composerText   string
	composerCursor int

	recentMu    sync.Mutex
	recent      []*store.Session
	recentStale atomic.Bool

	mcp   *mcpReg  // MCP servers and their tools
	tasks *taskReg // subagent runs
	lsp   *lspReg  // language servers

	todoMu     sync.Mutex
	todoSID    string
	todos      []store.Todo
	todosStale atomic.Bool

	// luaApproved holds the chunk names of Lua tool runs the user approved
	// (guardLua); luaRuns numbers them.
	luaApproved sync.Map
	luaRuns     atomic.Int64

	// turn is the scope of the turn runTurns is on: cancelling it stops
	// compaction, the model, tools and auto-mode screening.
	turn atomic.Pointer[turnScope]

	modes modeReg // permission modes in cycle order

	layout atomic.Pointer[layoutSet] // zemi.ui.layout.set's; nil draws DefaultLayout
	focus  atomic.Pointer[string]    // the layout node taking keys; nil or "" is the composer
	title  atomic.Pointer[titleSet]  // zemi.ui.title's; nil names the tab after the session

	// luaRules are the rules zemi.permission.rule.add and the project
	// permissions file registered: a reload replays the config that adds them.
	// PurgeGeneration moves them to staleRules, which stay in Perm until the
	// replay is done so no call runs without them; ForceReload then drops those
	// the replay did not add again, or on a failed replay only the allows.
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
	// and auto mode settles less without it.
	projectTrusted atomic.Bool
	trustDropped   atomic.Bool // trust went off: the next reload must run to drop project config
	// trustRoot is the repository the session belongs to (a worktree maps to
	// its main checkout), found once: it keys the store, the scratchpad and
	// the sandbox cache, and finding it walks the disk. project is the same
	// path ~-shortened, which widgets read every frame.
	trustRoot func() string
	project   func() string
	// pluginsRoot is Dirs.PluginsRoot resolved once, as chunk paths are:
	// PluginOf runs on every gated call and every overlay frame.
	pluginsRoot func() string
	// gitStatus is the working tree listing the environment prompt carries,
	// run once per process and warmed at boot: a large repository takes
	// seconds to answer, and on the send path that is time to first token.
	gitStatus func() string

	// sandbox is the startup probe's result (sandbox.go)
	sandbox atomic.Pointer[sandboxProbe]
	// guards holds, per running sandboxed command, the protected paths that
	// were missing when it started (guardSandbox)
	guardMu sync.Mutex
	guards  map[*sandboxGuard]bool
	// trustHeld keeps the project untrusted until /trust, after a sandboxed
	// command created a protected path
	trustHeld atomic.Bool
	// projectSkills is a trusted project's skills, found when the decision
	// is set so a command writing a skill mid-session cannot change the prompt
	projectSkills atomic.Pointer[[]skills.Skill]
	// recipes is the recipes the prompt lists, pinned with agentState.env
	recipes atomic.Pointer[[]skills.Skill]
}

// SetProjectTrusted records the trust decision and rescans the project's
// skills.
func (h *Host) SetProjectTrusted(ok bool) {
	prev := h.projectTrusted.Swap(ok)
	if prev && !ok {
		h.trustDropped.Store(true)
	}
	var found []skills.Skill
	if ok {
		found = h.discoverProjectSkills()
	}
	h.projectSkills.Store(&found)
	// the trust step runs on every reload, which must not move the prompt
	if prev != ok {
		h.pinRecipes()
	}
}

func (h *Host) pinRecipes() {
	found := h.discoverRecipes()
	h.recipes.Store(&found)
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

// projectNamespace is zemi.project: what a job-starting plugin checks first.
func (h *Host) projectNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("trusted", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LBool(h.ProjectTrusted()))
		return 1
	}))
	tb.RawSetString("root", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LString(h.Dirs.RepoRoot()))
		return 1
	}))
	return tb
}

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
		Stderr:    os.Stderr,
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
	}
	h.Perm.Default = h.fsDefault
	h.Keymaps.owner, h.Commands.owner, h.Widgets.owner, h.Themes.owner, h.luaTools.owner = h.PluginOf, h.PluginOf, h.PluginOf, h.PluginOf, h.PluginOf
	h.jobs.OnExit = h.refresh
	h.Perm.EnvKeep = func() string { return settings.Str("permissions.env_keep") }
	h.trustRoot = sync.OnceValue(h.Dirs.TrustRoot)
	h.project = sync.OnceValue(func() string { return TildePath(h.trustRoot()) })
	h.pluginsRoot = sync.OnceValue(func() string {
		root := dirs.PluginsRoot()
		if r, err := filepath.EvalSymlinks(root); err == nil {
			return r
		}
		return root
	})
	h.gitStatus = sync.OnceValue(func() string { return h.gitStatusSection(dirs.ProjectDir) })
	if dirs.StateHome != "" {
		// keyed on the repository, not the launch directory: a subdirectory
		// and a worktree are the same project, with the same sessions
		h.GlobalDB, h.DB, h.initErr = store.OpenForDirs(dirs.StateHome, h.trustRoot())
	}
	h.hostActions = map[string]func(h *Host, args string) error{
		"zemi.ui.help": func(h *Host, _ string) error { return h.OpenHelp() },
		"zemi.ui.reasoning.toggle": func(h *Host, _ string) error {
			h.ToggleReasoning()
			return nil
		},
		"zemi.ui.scroll.up":          func(h *Host, id string) error { h.scroll("up", id); return nil },
		"zemi.ui.scroll.down":        func(h *Host, id string) error { h.scroll("down", id); return nil },
		"zemi.ui.scroll.top":         func(h *Host, id string) error { h.scroll("top", id); return nil },
		"zemi.ui.scroll.bottom":      func(h *Host, id string) error { h.scroll("bottom", id); return nil },
		"zemi.ui.scroll.line_up":     func(h *Host, id string) error { h.scroll("line_up", id); return nil },
		"zemi.ui.scroll.line_down":   func(h *Host, id string) error { h.scroll("line_down", id); return nil },
		"zemi.model.pick":            func(h *Host, _ string) error { return h.OpenModelPicker("chat") },
		"zemi.composer.submit":       composerAction("submit"),
		"zemi.composer.insert":       composerAction("insert"),
		"zemi.composer.history_prev": composerAction("history_prev"),
		"zemi.composer.history_next": composerAction("history_next"),
		"zemi.composer.paste":        composerAction("paste"),
		"zemi.composer.edit":         composerAction("edit"),
		"zemi.ui.focus.set":          func(h *Host, id string) error { return h.focusNode(id) },
		"zemi.ui.focus.next":         func(h *Host, _ string) error { return h.cycleFocus(1) },
		"zemi.ui.focus.prev":         func(h *Host, _ string) error { return h.cycleFocus(-1) },
		"zemi.permission.mode.cycle": func(h *Host, _ string) error { return h.cyclePermMode(1) },
		"zemi.permission.mode.prev":  func(h *Host, _ string) error { return h.cyclePermMode(-1) },
		"zemi.agent.profile.cycle":   func(h *Host, _ string) error { return h.cycleProfile(1) },
		"zemi.agent.profile.prev":    func(h *Host, _ string) error { return h.cycleProfile(-1) },
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

// composerAction is a zemi.composer.* host action: op, with the text after
// the action's name for insert.
func composerAction(op string) func(h *Host, args string) error {
	return func(h *Host, args string) error {
		if ui := h.hooks(); ui != nil {
			ui.ComposerEdit(op, args, 0)
		}
		return nil
	}
}

func (h *Host) hooks() UIHooks {
	h.uiMu.RLock()
	defer h.uiMu.RUnlock()
	return h.ui
}

// Screen is the UI to open a screen on; headless it is ErrNeedsTUI.
func (h *Host) Screen() (UIHooks, error) {
	if ui := h.hooks(); ui != nil {
		return ui, nil
	}
	return nil, ErrNeedsTUI
}

func (h *Host) SetUIHooks(ui UIHooks) {
	h.uiMu.Lock()
	h.ui = ui
	h.uiMu.Unlock()
}

// Reflecting reports whether a reflector run is active; the UI keeps
// re-rendering widgets while it is, so the statusline badge animates.
func (h *Host) Reflecting() bool { return h.reflecting.Load() }

type titleSet struct {
	text string
	gen  Gen
}

// TerminalTitle is what the terminal tab shows: the title config set this
// generation, else the session's title with " · zemi" once it has one.
func (h *Host) TerminalTitle() string {
	if t := h.title.Load(); t != nil && t.gen == h.currentGen() {
		return t.text
	}
	if t := sanitize.String(h.SessionTitle()); t != "" && t != "zemi" {
		return t + " · zemi"
	}
	return "zemi"
}

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

// ToastTitle is a toast's title, Warning or Error for an untitled one.
func ToastTitle(level, title string) string {
	return cmp.Or(title, map[string]string{"warn": "Warning", "error": "Error"}[level])
}

// Notify shows a toast; with no UI a warning or an error goes to Stderr
// instead, so a headless run never swallows one.
func (h *Host) Notify(level, title, body string) {
	h.recordNotify(level, title, body)
	if ui := h.hooks(); ui != nil {
		ui.Notify(level, title, body)
		return
	}
	if level != "warn" && level != "error" {
		return
	}
	line := "zemi: " + ToastTitle(level, title)
	if body = strings.TrimSpace(ShortChunk(body)); body != "" {
		line += ": " + strings.ReplaceAll(body, "\n", "\n  ")
	}
	fmt.Fprintln(h.Stderr, line)
}

// scroll routes a zemi.ui.scroll.* action to the UI (no-op before the UI
// attaches, e.g. headless runs).
func (h *Host) scroll(kind, id string) {
	if ui := h.hooks(); ui != nil {
		ui.Scroll(kind, id)
	}
}

// Session returns a snapshot of the session state. The transcript is only
// appended to or replaced whole, never written in place, so the snapshot
// shares its rows: every widget render takes one.
func (h *Host) Session() Session {
	h.tMu.Lock()
	defer h.tMu.Unlock()
	return Session{
		Title:      h.session.Title,
		CWD:        h.session.CWD,
		Transcript: slices.Clip(h.session.Transcript),
	}
}

func (h *Host) AppendMessage(role, text string) {
	h.AppendChat(ChatMessage{Role: role, Text: text})
}

func (h *Host) AppendChat(m ChatMessage) {
	h.tMu.Lock()
	h.session.Transcript = append(h.session.Transcript, m)
	h.tMu.Unlock()
	h.refresh()
}

// goKeys are the bindings the key router owns (internal/ui/keys.go): they
// are not Lua keymaps, so /help would list none of them.
var goKeys = [][2]string{
	{"enter", "send the message"},
	{"shift+enter, alt+enter, ctrl+j", "newline"},
	{"esc", "back, or twice to interrupt"},
	{"ctrl+c", "interrupt, clear the draft, quit"},
	{"ctrl+g", "edit the draft in your editor"},
	{"ctrl+v", "paste, images included"},
	{"up, ctrl+p", "earlier prompts or queued ones"},
	{"down, ctrl+n", "later prompts"},
	{"ctrl+z", "undo in the composer"},
	{"ctrl+a, ctrl+e", "line start, line end"},
	{"alt+left, alt+right", "move by word"},
	{"ctrl+u, ctrl+k", "delete to line start, to line end"},
	{"ctrl+w", "delete the word before the cursor"},
	{"/", "command palette"},
	{"@", "attach a project file"},
	{"!cmd", "run a shell command with your next message"},
}

// listKeys are the keys every list and box takes (internal/ui/keys.go).
var listKeys = [][2]string{
	{"up, down, ctrl+p, ctrl+n", "move"},
	{"pgup, pgdown", "move a page"},
	{"home, end", "first row, last row"},
	{"/", "filter the rows"},
	{"space", "toggle"},
	{"y, Y", "copy the row, every row"},
	{"shift+up, shift+down", "scroll the text a line"},
}

// OpenHelp opens the help overlay listing keys and slash commands. A
// command row runs it, unless it needs an argument, which it fills in.
func (h *Host) OpenHelp() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	var items []any
	for _, k := range goKeys {
		items = append(items, map[string]any{"text": k[0], "desc": k[1], "section": "Keys"})
	}
	for _, b := range h.Keymaps.List() {
		desc := b.Desc
		if desc == "" {
			desc = describeRHS(b.RHS)
		}
		if b.Focus != "" {
			desc += " (in " + b.Focus + ")"
		}
		items = append(items, map[string]any{"text": b.Key, "desc": desc, "section": "Keys"})
	}
	for _, k := range listKeys {
		items = append(items, map[string]any{"text": k[0], "desc": k[1], "section": "Lists"})
	}
	// Hidden keeps a command out of the palette's default list, not out of
	// /help: this is the index the README points people at.
	for _, c := range h.Commands.List() {
		item := map[string]any{"text": "/" + c.Name, "desc": c.Desc, "section": "Commands", "run": c.Name}
		if c.NArgs == "+" {
			delete(item, "run")
			item["fill"] = "/" + c.Name + " "
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "help", Title: "Help", Origin: lua.OriginBuiltin, Items: items, Verb: "run"})
	return nil
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
	return h.Settings.Bool("ui.chat.reasoning")
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

// vetoDecision reads what a vetoable handler returned: nil is no opinion,
// a zemi.Veto table is its decision. Anything else is a handler that meant
// to decide and wrote the wrong shape, and bad says so, so the caller can
// fail closed instead of reading silence as consent (DESIGN §6.0).
func vetoDecision(ret glua.LValue, allowed ...string) (tb *glua.LTable, decision, bad string) {
	if ret == nil || ret == glua.LNil {
		return nil, "", ""
	}
	tb, ok := ret.(*glua.LTable)
	if !ok {
		return nil, "", fmt.Sprintf("a handler returned %s: return nil, or { decision = %q, reason = ... }",
			ret.Type().String(), allowed[0])
	}
	if decision = rawString(tb, "decision"); !slices.Contains(allowed, decision) {
		return nil, "", fmt.Sprintf("a handler returned decision %q: use nil or one of %s",
			decision, strings.Join(allowed, ", "))
	}
	return tb, decision, ""
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
					tb, decision, bad := vetoDecision(ret, "deny")
					switch {
					case bad != "":
						h.logHandlerError(hd, errors.New(bad))
						// a handler that meant to deny and misspelled it must
						// not read as consent where there is a closed side
						outcomes[i] = outcome{denied: name == "permission.request", reason: bad}
					case decision == "deny":
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

// eventTable sets name and from after the payload, so no emitter can pose
// as another event or origin.
func eventTable(name, from string, payload map[string]any) map[string]any {
	ev := maps.Clone(payload)
	if ev == nil {
		ev = map[string]any{}
	}
	ev["name"], ev["from"] = name, from
	return ev
}

// callEvent runs one handler fn(ev) under its origin with the deadline armed
// at start and returns its first result. A handler that ran past the
// deadline reports timedOut and no result, even if it returned.
func (h *Host) callEvent(L *glua.LState, hd *EventHandler, ev map[string]any, deadline time.Duration) (ret glua.LValue, timedOut bool, err error) {
	if hd.Once && !h.Events.drop(hd) {
		return glua.LNil, false, nil
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
	// Click is the id a click passes to the on_click of Widget; a Go
	// statusline pill sets it with no Widget
	Click  string
	Widget string
	Effect string // "shimmer": the UI animates the text, blending hl toward Normal
	Over   string // Go only: a hover group drawn over HL
	// NoCopy marks decoration (gutters, bullets, borders): a selection
	// neither lights nor copies it
	NoCopy bool
	// NoHover marks a clickable body row (a diff line, a tool's detail): the
	// pointer still says it acts, but hovering it restyles nothing
	NoHover bool
	// Soft ends a row that wraps onto the next: a copy joins the two rows
	// without a newline and drops the row's cells after this segment
	Soft bool
	Bar  bool // Go only: a viewport's scrollbar cell, kept to a box's right edge
}

// RenderWidget runs a widget's Lua render at the cells its layout node gives
// it and returns sanitized segment lines (DESIGN §6.6, §8). more is render's
// second result: the lines are what its time allowed and it wants to run
// again, which is how a long chat draws its latest rows first.
func (h *Host) RenderWidget(ctx context.Context, w *Widget, width, height int, state UIState) (lines [][]Segment, more bool, err error) {
	linesAny, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		// a frame queued before a reload holds the old generation's function
		if w.Gen != h.currentGen() {
			return nil, lua.ErrReloaded
		}
		// The purity guard is raised on the interpreter goroutine, only
		// while render runs: raised before Do, it also refused Lua that ran
		// while this render merely waited in the queue (boot code behind
		// the trust prompt).
		h.rendering.Store(true)
		defer h.rendering.Store(false)
		defer h.RT.PushFrame(L, w.Origin, w.Path)()
		base := L.GetTop()
		L.Push(w.Render)
		ctxTb, done := h.widgetCtxTable(L, width, height, state)
		defer done()
		L.Push(ctxTb)
		if err := L.PCall(1, 2, nil); err != nil {
			L.SetTop(base)
			return nil, err
		}
		ret, more := L.Get(-2), L.Get(-1) == glua.LTrue
		L.SetTop(base)
		if lines, ok := w.tableSegments(ret); ok {
			return []any{lines, more}, nil
		}
		v, err := lua.ToGo(ret)
		if err != nil {
			return nil, err
		}
		return []any{v, more}, nil
	})
	if err != nil || linesAny[0] == nil {
		return nil, false, err
	}
	more = linesAny[1].(bool)
	if lines, ok := linesAny[0].([][]Segment); ok {
		return lines, more, nil
	}
	lines, err = convertSegments(linesAny[0], w.ID)
	return lines, more, err
}

// isList reports whether t holds exactly the keys 1..n, the tables ToGo
// turns into a list, and n.
func isList(t *glua.LTable) (int, bool) {
	keys := 0
	t.ForEach(func(_, _ glua.LValue) { keys++ })
	return keys, keys == t.Len()
}

// tableSegments converts the usual render result, a list of lines of
// segments, without marshaling it through ToGo; ok is false for every other
// shape, which convertSegments handles and names in its errors. A line table
// seen in the last render keeps its segments: a widget must not change a
// line it already returned (a long chat returns the same thousands each
// frame). Interpreter goroutine only.
func (w *Widget) tableSegments(v glua.LValue) (out [][]Segment, ok bool) {
	top, isTable := v.(*glua.LTable)
	if !isTable {
		return nil, false
	}
	n, list := isList(top)
	if !list || n == 0 {
		return nil, false
	}
	if w.spare == nil {
		w.spare = make(map[*glua.LTable][]Segment, n)
	}
	seen := w.spare
	clear(seen)
	out = make([][]Segment, 0, n)
	for i := 1; i <= n; i++ {
		lt, isTable := top.RawGetInt(i).(*glua.LTable)
		if !isTable {
			return nil, false
		}
		line, hit := w.lines[lt]
		if !hit {
			m, list := isList(lt)
			if !list || m == 0 {
				return nil, false
			}
			line = make([]Segment, 0, m)
			for j := 1; j <= m; j++ {
				switch c := lt.RawGetInt(j).(type) {
				case glua.LString:
					line = append(line, Segment{Text: sanitize.String(string(c))})
				case *glua.LTable:
					if c.Len() > 0 {
						return nil, false
					}
					line = append(line, segment(func(k string) any {
						switch f := c.RawGetString(k).(type) {
						case glua.LString:
							return string(f)
						case glua.LBool:
							return bool(f)
						}
						return nil
					}, w.ID))
				default:
					return nil, false
				}
			}
		}
		seen[lt] = line
		out = append(out, line)
	}
	w.lines, w.spare = seen, w.lines
	return out, true
}

// segment builds one segment from a { text = ... } table's fields.
func segment(field func(string) any, widget string) Segment {
	seg := Segment{}
	if t, ok := field("text").(string); ok {
		seg.Text = sanitize.String(t)
	}
	if hl, ok := field("hl").(string); ok {
		seg.HL = UserHL(hl)
	}
	if e, ok := field("effect").(string); ok {
		seg.Effect = e
	}
	seg.NoCopy = field("copy") == false
	seg.NoHover = field("hover") == false
	seg.Soft = field("soft_wrap") == true
	if id, ok := field("click").(string); ok && id != "" {
		seg.Click, seg.Widget = id, widget
	}
	return seg
}

// ClickWidget runs the on_click of widget id with ev under the widget's
// origin, like a keymap. The handler is looked up at click time because the
// frame that drew the id may predate a reload.
func (h *Host) ClickWidget(ctx context.Context, id string, ev map[string]any) error {
	_, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		w := h.Widgets.Get(id)
		if w == nil || w.OnClick == nil || w.Gen != h.currentGen() {
			return nil, lua.ErrReloaded
		}
		defer h.RT.PushFrame(L, w.Origin, w.Path)()
		base := L.GetTop()
		L.Push(w.OnClick)
		L.Push(lua.FromGo(L, ev))
		err := L.PCall(1, 0, nil)
		L.SetTop(base)
		return nil, err
	})
	return err
}

// UserHL drops a "Host:*" style from a Lua segment: those are the fixed
// Go chrome styles (internal/ui/hooks.go), so a widget or overlay row
// could otherwise paint itself as a permission prompt.
func UserHL(hl string) string {
	if strings.HasPrefix(hl, "Host:") {
		return ""
	}
	return hl
}

// luaShape names a marshaled Lua value the way the author wrote it: a Go
// type name in a widget error tells them nothing (DESIGN §6.0).
func luaShape(v any) string {
	switch t := v.(type) {
	case nil:
		return "nil"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		if _, ok := t["text"]; ok {
			return "a single segment"
		}
		return "a table of keys"
	}
	return "an unexpected value"
}

// oneLine wraps the two returns that mean one line and used to be errors:
// a lone string, and a line handed back without its enclosing list. A list
// of several strings is both a line and a line each, so it stays an error.
func oneLine(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return []any{[]any{t}}, nil
	case []any:
		if len(t) == 0 {
			return v, nil
		}
		switch t[0].(type) {
		case map[string]any:
			return []any{t}, nil
		case string:
			if len(t) > 1 {
				return nil, errors.New(`widget render returned a list of strings: write { { "a", "b" } } for one line, { { "a" }, { "b" } } for two`)
			}
			return []any{t}, nil
		}
	}
	return v, nil
}

func convertSegments(v any, widget string) ([][]Segment, error) {
	v, err := oneLine(v)
	if err != nil {
		return nil, err
	}
	rows, ok := v.([]any)
	if m, isMap := v.(map[string]any); isMap && len(m) == 0 {
		ok = true // Lua's {} marshals as an empty map: no lines
	}
	if !ok {
		return nil, fmt.Errorf("widget render must return a list of lines, got %s", luaShape(v))
	}
	out := make([][]Segment, 0, len(rows))
	for _, row := range rows {
		cells, ok := row.([]any)
		if !ok {
			return nil, fmt.Errorf("widget line must be a list of segments, got %s", luaShape(row))
		}
		line := make([]Segment, 0, len(cells))
		for _, cell := range cells {
			switch c := cell.(type) {
			case string:
				line = append(line, Segment{Text: sanitize.String(c)})
			case map[string]any:
				line = append(line, segment(func(k string) any { return c[k] }, widget))
			default:
				return nil, fmt.Errorf("widget segment must be a string or a { text = ... } table, got %s", luaShape(cell))
			}
		}
		out = append(out, line)
	}
	return out, nil
}

// UIState is the context snapshot passed to widget renders.
type UIState struct {
	Time  string
	Frame int // the 50ms frame clock the spinner, eyes and shimmer step on; 0 with ui.animations off

	Ended          string // how the turn that ended a moment ago ended: "ok", "error" or "cancelled"; "" after that
	QuitArmed      bool   // a second ctrl+c quits
	InterruptArmed bool   // a second esc interrupts the turn
	KeySeq         string // keys of a bound sequence typed so far
	RowsBelow      int    // transcript rows under a scrolled-up viewport
	Cancelling     bool   // interrupt sent, turn not finished yet
	Editing        string // title of the file open in the editor
	Waiting        string // "approval" or "answer" while a prompt holds the turn
	Focus          string // the drawn layout node taking keys, "" for the composer
}

// TildePath shortens the home directory, or a path under it, to ~.
func TildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// widgetCtxTable builds a render's ctx. The fields that copy the transcript,
// the live turn, todos or recent sessions are built on first read, so a
// widget that never reads them does not pay for them (a statusline widget
// over a long session renders 50 times faster), and read nil once the
// render returned, so a ctx kept in an upvalue pins nothing. done ends it.
func (h *Host) widgetCtxTable(L *glua.LState, width, height int, state UIState) (tb *glua.LTable, done func()) {
	sess, stream := h.liveView()
	tb = L.CreateTable(0, 32)
	tb.RawSetString("width", glua.LNumber(width))
	tb.RawSetString("time", glua.LString(state.Time))
	tb.RawSetString("frame", glua.LNumber(state.Frame))
	tb.RawSetString("ended", glua.LString(state.Ended))
	tb.RawSetString("height", glua.LNumber(height))
	tb.RawSetString("quit_armed", glua.LBool(state.QuitArmed))
	tb.RawSetString("interrupt_armed", glua.LBool(state.InterruptArmed))
	tb.RawSetString("key_seq", glua.LString(state.KeySeq))
	tb.RawSetString("rows_below", glua.LNumber(state.RowsBelow))
	tb.RawSetString("cancelling", glua.LBool(state.Cancelling))
	tb.RawSetString("editing", glua.LString(state.Editing))
	tb.RawSetString("waiting", glua.LString(state.Waiting))
	tb.RawSetString("focus", glua.LString(state.Focus))
	tb.RawSetString("version", glua.LString(h.Version))
	tb.RawSetString("cwd", glua.LString(TildePath(sess.CWD)))
	tb.RawSetString("project", glua.LString(h.project()))
	tb.RawSetString("trusted", glua.LBool(h.ProjectTrusted()))
	tb.RawSetString("init_error", glua.LString(h.InitError()))
	tb.RawSetString("init_path", glua.LString(h.initPath()))
	tb.RawSetString("model_hint", glua.LString(h.noModelHint()))
	tb.RawSetString("profile", glua.LString(stream.Profile))
	tb.RawSetString("profile_read_only", glua.LBool(h.readOnly()))
	tb.RawSetString("model", glua.LString(stream.Model))
	tb.RawSetString("effort", glua.LString(h.Effort(stream.Model)))
	tb.RawSetString("agent_active", glua.LBool(stream.Active))
	mode := h.permModeDef()
	tb.RawSetString("permission_mode", glua.LString(mode.Name))
	tb.RawSetString("permission_mode_base", glua.LString(mode.Base))
	tb.RawSetString("show_reasoning", glua.LBool(h.ReasoningShown()))
	tb.RawSetString("reflecting", glua.LBool(h.reflecting.Load()))
	tb.RawSetString("pending_memory", glua.LNumber(h.pendingMemory.Load()))
	tb.RawSetString("agents_running", glua.LNumber(h.tasks.runningFor(h.AgentSessionID())))
	tb.RawSetString("jobs_running", glua.LNumber(h.jobsRunningCount()))
	// Showing an agent run, the chat renders its transcript alone: the
	// session's live turn and todos stay out of it.
	run := h.viewedRun()
	transcript := sess.Transcript
	if run != nil {
		status, _, _, _ := run.state()
		v := L.NewTable()
		v.RawSetString("id", glua.LString(run.id))
		v.RawSetString("agent", glua.LString(run.agent))
		v.RawSetString("description", glua.LString(sanitize.String(run.description)))
		v.RawSetString("status", glua.LString(status))
		tb.RawSetString("view", v)
		transcript = run.transcript()
		stream.Text, stream.Reasoning, stream.Tools, stream.Steering, stream.Reports = "", "", nil, run.queued(), 0
	}
	tb.RawSetString("usage", usageTable(L, stream))
	tb.RawSetString("context", contextTable(L, stream))
	tb.RawSetString("transcript_count", glua.LNumber(len(transcript)))
	// 0 while an agent run is shown: its rows mutate in place as the run
	// goes, so the chat widget must not cache them.
	epoch := h.trEpoch.Load()
	if run != nil {
		epoch = 0
	}
	tb.RawSetString("transcript_epoch", glua.LNumber(epoch))

	lazy := map[string]func(L *glua.LState) glua.LValue{
		"todos": func(L *glua.LState) glua.LValue {
			todos := L.NewTable()
			if run != nil {
				return todos
			}
			for _, td := range h.todoSnapshot() {
				row := L.NewTable()
				row.RawSetString("content", glua.LString(sanitize.String(td.Content)))
				row.RawSetString("status", glua.LString(td.Status))
				row.RawSetString("active_form", glua.LString(sanitize.String(td.ActiveForm)))
				todos.Append(row)
			}
			return todos
		},
		"agent":      func(L *glua.LState) glua.LValue { return agentTable(L, stream) },
		"transcript": func(L *glua.LState) glua.LValue { return transcriptTable(L, transcript) },
		// $HOME and / are nobody's project, so /init is not offered there
		"instructions_missing": func(L *glua.LState) glua.LValue {
			mine := filepath.Join(h.Dirs.ConfigHome, "AGENTS.md")
			return glua.LBool(h.ProjectTrusted() && !TrustUnsaved(h.Dirs.RepoRoot()) &&
				!slices.ContainsFunc(h.instructionFiles(), func(p string) bool { return p != mine }))
		},
		"recent": func(L *glua.LState) glua.LValue {
			if len(sess.Transcript) > 0 {
				return glua.LNil
			}
			recent := L.NewTable()
			for _, r := range h.recentSessions() {
				row := L.NewTable()
				row.RawSetString("id", glua.LString(r.ID))
				row.RawSetString("title", glua.LString(sanitize.String(r.Title)))
				row.RawSetString("ago", glua.LString(ago(r.UpdatedAt)))
				recent.Append(row)
			}
			return recent
		},
	}
	live := true
	mt := L.NewTable()
	mt.RawSetString("__index", L.NewFunction(func(L *glua.LState) int {
		key, _ := L.Get(2).(glua.LString)
		build := lazy[string(key)]
		if !live || build == nil {
			L.Push(glua.LNil)
			return 1
		}
		v := build(L)
		L.CheckTable(1).RawSetString(string(key), v)
		L.Push(v)
		return 1
	}))
	L.SetMetatable(tb, mt)
	return tb, func() { live = false }
}

// agentTable is ctx.agent: the live turn.
func agentTable(L *glua.LState, stream StreamSnapshot) *glua.LTable {
	at := L.CreateTable(0, 12)
	at.RawSetString("active", glua.LBool(stream.Active))
	at.RawSetString("model", glua.LString(stream.Model))
	at.RawSetString("text", glua.LString(stream.Text))
	at.RawSetString("reasoning", glua.LString(stream.Reasoning))
	steering := L.CreateTable(len(stream.Steering), 0)
	for _, s := range stream.Steering {
		steering.Append(glua.LString(s))
	}
	at.RawSetString("steering", steering)
	at.RawSetString("thought", glua.LNumber(stream.Thought.Seconds()))
	at.RawSetString("elapsed", glua.LNumber(stream.Elapsed.Seconds()))
	at.RawSetString("reports", glua.LNumber(stream.Reports))
	at.RawSetString("compacting", glua.LBool(stream.Compacting))
	if stream.Retrying {
		retry := L.CreateTable(0, 3)
		retry.RawSetString("attempt", glua.LNumber(stream.Retries+1))
		retry.RawSetString("why", glua.LString(sanitize.String(stream.RetryWhy)))
		retry.RawSetString("next_in", glua.LNumber(stream.RetryIn.Seconds()))
		at.RawSetString("retry", retry)
	}
	toolsTb := L.CreateTable(len(stream.Tools), 0)
	for _, t := range stream.Tools {
		row := L.CreateTable(0, 9)
		row.RawSetString("name", glua.LString(t.Name))
		row.RawSetString("input", glua.LString(t.Input))
		row.RawSetString("status", glua.LString(t.Status))
		row.RawSetString("detail", glua.LString(t.Detail))
		row.RawSetString("run", glua.LString(t.Run))
		row.RawSetString("label", glua.LString(sanitize.String(t.Label)))
		if !t.Started.IsZero() {
			row.RawSetString("elapsed", glua.LNumber(time.Since(t.Started).Seconds()))
		}
		row.RawSetString("diff", glua.LString(sanitize.String(t.Diff)))
		row.RawSetString("output", glua.LString(t.Output))
		toolsTb.Append(row)
	}
	at.RawSetString("tools", toolsTb)
	return at
}

// transcriptTable is ctx.transcript.
func transcriptTable(L *glua.LState, transcript []ChatMessage) *glua.LTable {
	tr := L.CreateTable(len(transcript), 0)
	for _, m := range transcript {
		row := L.CreateTable(0, 14)
		row.RawSetString("role", glua.LString(m.Role))
		row.RawSetString("kind", glua.LString(m.Kind))
		row.RawSetString("text", glua.LString(m.Text))
		row.RawSetString("tool", glua.LString(m.Tool))
		row.RawSetString("status", glua.LString(m.Status))
		row.RawSetString("detail", glua.LString(m.Detail))
		row.RawSetString("run", glua.LString(m.Run))
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
	return tr
}

// usageTable is the session token tally for widgets (DESIGN §8): totals
// across all turns of this session.
func usageTable(L *glua.LState, s StreamSnapshot) *glua.LTable {
	u := L.CreateTable(0, 4)
	u.RawSetString("in", glua.LNumber(s.SessionIn))
	u.RawSetString("out", glua.LNumber(s.SessionOut))
	u.RawSetString("total", glua.LNumber(s.SessionIn+s.SessionOut))
	u.RawSetString("cost", glua.LNumber(s.SessionCost))
	return u
}

// contextTable is the current-context estimate: the last prompt size
// against the model's context window, with the percent for the statusline.
func contextTable(L *glua.LState, s StreamSnapshot) *glua.LTable {
	c := L.CreateTable(0, 3)
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
	h.callOverlay(ctx, o, "on_select", o.OnSelect, item)
}

// OverlayAction runs on_action with the item and the action chosen in its
// detail view.
func (h *Host) OverlayAction(ctx context.Context, o Overlay, item, action any) {
	h.callOverlay(ctx, o, "on_action", o.OnAction, item, action)
}

// OverlayKey runs a list-level key's function with the selected item.
func (h *Host) OverlayKey(ctx context.Context, o Overlay, k OverlayKey, item any) {
	h.callOverlay(ctx, o, "key "+k.Key, k.Fn, item)
}

// CancelOverlay runs on_close if present.
func (h *Host) CancelOverlay(ctx context.Context, o Overlay) {
	h.callOverlay(ctx, o, "on_close", o.OnClose)
}

// callOverlay runs one of an overlay's callbacks as a task under the
// overlay's origin; a failure goes to /errors.
func (h *Host) callOverlay(ctx context.Context, o Overlay, what string, fn *glua.LFunction, args ...any) {
	if fn == nil || h.staleOverlay(o) {
		return
	}
	failed := func(err error) {
		if err != nil && !isQuietLuaErr(err) {
			h.Log.Error("ui", "overlay "+what+" failed", map[string]any{"id": o.ID, "error": err.Error()})
			h.RecordError("overlay "+o.ID+" "+what, err.Error())
		}
	}
	failed(h.RT.StartTask(ctx, h.luaTask(lua.ClassUI, luaCaller{gen: o.Gen, origin: o.Origin, path: o.Path}, fn, args...), func(err error) {
		h.refresh()
		failed(err)
	}))
}

// refuseBelowUser raises unless user config or shipped code is calling:
// the call acts with the user's authority, like command.run's Go commands.
// hint, when given, says what lower-trust code can do instead.
func (h *Host) refuseBelowUser(L *glua.LState, what string, hint ...string) {
	o := h.RT.CurrentOrigin(L)
	if o == lua.OriginUser || o == lua.OriginBuiltin {
		return
	}
	msg := fmt.Sprintf("zemi.%s: only user config may call this, not %s code", what, o)
	if len(hint) > 0 && hint[0] != "" {
		msg += "; " + hint[0]
	}
	L.RaiseError("%s", msg)
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
	h.Providers.Reset(h.Builtin)
	h.syncSkills()
	h.syncTypes()
	h.GoSafe("git status", func() { h.gitStatus() })

	g.SB.SetGuard(h.guardLua)
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
		{"zemi.project", h.projectNamespace(L), nil},
	}
	for _, p := range pairs {
		if err := register(p.name, p.tbl, p.call); err != nil {
			return err
		}
	}
	api := L.NewTable()
	api.RawSetString("version", glua.LString("0.84.0"))
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
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	h.GoSafe("reload", func() {
		// a reload cancels what the interpreter runs, which is the caller when
		// Lua asked for it: wait for it to return (a wedged one times out)
		_, _ = h.RT.Do(context.Background(), lua.ClassIO, 0, 0, func(*glua.LState) ([]any, error) { return nil, nil })
		ui.Reload()
	})
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
	// a reload after trust went off still runs, so the project config goes
	if !h.trustDropped.Swap(false) {
		if err := h.checkInits(); err != nil {
			return err
		}
	}
	err := h.RT.ForceReload(ctx, steps, h.PurgeGeneration)
	h.mcp.stopParked()
	if err == nil && h.InitError() != "" {
		err = errors.New(h.InitError())
	}
	// A failed replay keeps the old deny and ask rules and the settings it
	// did not write: part of a config never loosens what the whole one set.
	h.luaRulesMu.Lock()
	h.staleRules = slices.DeleteFunc(h.staleRules, func(r perm.Rule) bool {
		drop := err == nil || r.Decision == perm.Allow
		if drop {
			h.Perm.Remove(r)
		}
		return drop
	})
	h.luaRulesMu.Unlock()
	if err == nil {
		h.cfgSettle()
		h.dropStaleLayout()
	}
	return err
}

// checkInits parses the init files a reload runs, so a half-saved one
// fails the reload before it retires the working generation.
func (h *Host) checkInits() error {
	dirs := []string{h.Dirs.ConfigHome}
	if h.ProjectTrusted() {
		dirs = append(dirs, h.Dirs.ProjectConfigDir())
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, "init.lua")
		src, err := os.ReadFile(p)
		if err != nil {
			continue // the boot step skips or reports it
		}
		_, err = parse.Parse(bytes.NewReader(src), p)
		if pe, ok := errors.AsType[*parse.Error](err); ok {
			if pe.Pos.Line == parse.EOF {
				return fmt.Errorf("%s:%d: %s at end of file", TildePath(p), bytes.Count(bytes.TrimRight(src, "\n"), []byte("\n"))+1, pe.Message)
			}
			return fmt.Errorf("%s:%d: %s near '%s'", TildePath(p), pe.Pos.Line, pe.Message, pe.Token)
		}
	}
	return nil
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

// addPermissionRule registers a config rule (from zemi.permission.rule.add or
// the project permissions file) and keeps an equal "always" answer alive: a
// reload replays the config that adds these, PurgeGeneration moves them to
// staleRules, and claimStale drops each one the replay adds again.
func (h *Host) addPermissionRule(r perm.Rule) {
	if h.Perm.Add(r) || h.claimStale(r) {
		h.luaRulesMu.Lock()
		h.luaRules = append(h.luaRules, r)
		h.luaRulesMu.Unlock()
	}
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
	h.agents.mu.Lock()
	clear(h.agents.profiles)
	h.agents.mu.Unlock()
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
	h.completions.purge()
	h.luaTools.purge(h.currentGen())
	h.stops.purge()
	h.mcp.park()
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
