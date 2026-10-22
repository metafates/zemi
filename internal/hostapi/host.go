package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// UIHooks is what the host API needs from the UI layer. Implemented by
// internal/ui; hostapi never imports the UI.
type UIHooks interface {
	Notify(level slog.Level, title, body string)
	OpenOverlay(o Overlay)
	// RefreshOverlay replaces the open overlay id with what build returns,
	// calling build only while it is open: for a screen a clock refreshes.
	RefreshOverlay(id string, build func() Overlay)
	// CloseOverlay closes an overlay and runs its on_close.
	CloseOverlay(id string)
	// CloseLuaOverlays drops every overlay the interpreter owns (Gen != 0).
	CloseLuaOverlays()
	Refresh()
	// Scroll moves a scrolled region of the layout: the node id, or the
	// stage's when id is "".
	Scroll(kind ScrollAction, id string)
	// LayoutRects is where the last frame drew each node with an id, plus
	// "stage" and "composer"; safe from any goroutine.
	LayoutRects() map[string]Rect
	// KeyHelp is the keys the router owns and the keys every list takes, as
	// rows of key and what it does: they are not Lua keymaps, so the UI that
	// routes them owns the wording /help lists.
	KeyHelp() (keys, lists [][2]string)
	// AgentStream says a streaming update landed; the UI only invalidates
	// the frame, so these coalesce.
	AgentStream()
	// TurnDone ends a turn; err is "" when it finished cleanly, and stopped
	// says the user ended it by dismissing a question or plan.
	TurnDone(err string, stopped bool)
	AskPermission(req PermRequest) (allowed bool, decidedBy string)
	// Screenshot is the last drawn frame as plain text, "" before the
	// first one; safe from any goroutine.
	Screenshot() string
	// ChatRow is the transcript row the reader is at in the last drawn
	// frame, down to the call of a rail a row draws; 0 for none. Safe from
	// any goroutine.
	ChatRow() int
	AskQuestion(qs []tools.Question) (answers []tools.Answer, ok bool)
	// AskPlan shows a plan for approval (restore is the agent to return to on
	// approval, "" when none); ok is false when the user dismisses it.
	AskPlan(plan, restore string) (tools.PlanVerdict, bool)
	// ComposerEdit applies a zemi.composer edit on the UI goroutine; only
	// ComposerSet reads cursor (-1 = end).
	ComposerEdit(op ComposerOp, text string, cursor int)
	// EditFile opens a file in the user's editor and blocks until it exits.
	// Never call it on the UI goroutine.
	EditFile(ctx context.Context, req EditRequest) error
	// Copy puts text on the clipboard and says so.
	Copy(text string)
	// Relaunch leaves like /quit, its veto included, and has main start
	// zemi again in dir, resuming session resume unless it is "".
	Relaunch(dir, resume string)
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
	Items    []OverlayItem // every row's fields, Lua's parsed at the boundary
	OnSelect *glua.LFunction
	OnAction *glua.LFunction // on_action(item, key), for a row key with an id
	OnClose  *glua.LFunction
	Keys     []OverlayKey // list-level keys, named in the footer
	Gen      Gen
	Anchor   OverlayAnchor

	// Body is text above the items; with no items the overlay is a
	// read-only view. Empty is the line shown when there are no items and
	// no body. Selected is the item selected when it opens. Verb names what
	// enter does to a row, when the list's default word does not say it.
	Body     string
	BodyMD   bool        // render Body (and every detail view's) as markdown
	Parts    []BodyPart  // draw Body's place in parts, each in the form it is written in
	Fields   []Field     // set as a table above Body
	Mark     MarkKind    // ahead of the "status" field's value
	BodySegs [][]Segment // Body as styled rows, drawn in its place; Body is what y copies
	Empty    string
	Selected int
	Verb     Verb // what enter does to a row, where the list's default word does not say it
	Refresh  bool // only replaces the open overlay with this ID, so one the user closed stays closed
	// Confirm is the run of a row key to open armed: a destructive command
	// typed without --yes asks there
	Confirm string
	// Wide boxes take the widest a screen shows whatever opened them, so a
	// screen reached from a list or a command draws at one width
	Wide bool
	// Live marks a body its work rewrites while the view is open: a refresh
	// keeps where the reader scrolled, and one who did not moves with the
	// tail
	Live bool
	// Phases heads each section with its rows' combined mark: failed,
	// running, waiting, stopped or done
	Phases bool
	// Follow makes the list move the stage's scrolled region to the
	// transcript row each item names by its anchor, as the selection moves,
	// and band that message's rows while it is open: enter keeps the place,
	// esc puts the region back where it found it
	Follow bool
	// OpenAt is the transcript row the reader was at when the open was
	// made, pinned by a host that opens several levels of one list: the
	// levels the open itself puts up move the chat, and a level deriving
	// its selection from the moved chat would open on the parent's pick
	// instead of the reader's row. 0 derives from the chat as usual.
	OpenAt int
	// Rail is the transcript row opening the activity run the chat draws
	// whole while this list is up, 0 for none: a rail's own list walks its
	// calls, and the run under them has to be open to show them
	Rail int

	// Input makes the overlay a one-line text prompt (zemi.ui.input) that
	// starts with Default; OnSelect receives the text. A Go screen's prompt
	// sets Run instead, a command the text is appended to; Lua never sets it.
	// Placeholder shows dim in the empty line, what submitting it would mean.
	Input       bool
	Default     string
	Placeholder string
	Run         string

	// Reload makes a list a toggle list: enter and space run the row's "run"
	// command or on_select and rebuild the items, keeping the overlay open.
	// It runs on the UI worker, never on the key path.
	Reload func() []OverlayItem

	// Next and Prev are the commands that open the next and the previous
	// view on the same rail in this one's place (ctrl+j, ctrl+k), so walking
	// a turn's calls takes no trip back to the chat.
	Next, Prev string
}

// ServiceStatus is where an MCP server, a language server or a plugin
// stands: the status word of /mcp, /lsp, /plugins, /health and zemi.*.list.
type ServiceStatus string

const (
	StatusIdle     ServiceStatus = "idle" // a language server that starts on its first file
	StatusStarting ServiceStatus = "starting"
	StatusReady    ServiceStatus = "ready"
	StatusFailed   ServiceStatus = "failed"
	StatusSignIn   ServiceStatus = "needs sign-in"
	StatusStopped  ServiceStatus = "stopped"
	StatusOff      ServiceStatus = "off" // a plugin the user turned off
)

// serviceStatuses pins runtime/types/zemi.lua's zemi.ServiceStatus.
var serviceStatuses = []ServiceStatus{StatusIdle, StatusStarting, StatusReady, StatusFailed, StatusSignIn, StatusStopped, StatusOff}

// OverlayAnchor puts an overlay in line with the composer frame, by the
// pill that opened it; "" centers it.
type OverlayAnchor string

const (
	OverlayBottomLeft  OverlayAnchor = "bottom-left"
	OverlayBottomRight OverlayAnchor = "bottom-right"
)

var overlayAnchors = []OverlayAnchor{OverlayBottomLeft, OverlayBottomRight}

// ComposerOp is a zemi.composer edit.
type ComposerOp string

const (
	ComposerSet         ComposerOp = "set"
	ComposerInsert      ComposerOp = "insert"
	ComposerSubmit      ComposerOp = "submit"
	ComposerHistoryPrev ComposerOp = "history_prev"
	ComposerHistoryNext ComposerOp = "history_next"
	ComposerPaste       ComposerOp = "paste"
	ComposerEditor      ComposerOp = "edit" // the draft in the user's editor
)

// ScrollAction moves a scrolled region: a page with a one-row overlap, to
// an end, a row, or to the first row of a header (a run of sticky rows):
// the nearest one above the view, or the next one below it, else the tail.
type ScrollAction string

const (
	ScrollUp         ScrollAction = "up"
	ScrollDown       ScrollAction = "down"
	ScrollTop        ScrollAction = "top"
	ScrollBottom     ScrollAction = "bottom"
	ScrollLineUp     ScrollAction = "line_up"
	ScrollLineDown   ScrollAction = "line_down"
	ScrollStickyUp   ScrollAction = "sticky_up"
	ScrollStickyDown ScrollAction = "sticky_down"
)

var scrollActions = []ScrollAction{ScrollUp, ScrollDown, ScrollTop, ScrollBottom, ScrollLineUp, ScrollLineDown, ScrollStickyUp, ScrollStickyDown}

// RowKind is a transcript row's shape. RowReasoning is a finished thinking
// block the chat widget collapses to a one-line summary.
type RowKind string

const (
	RowMessage   RowKind = ""
	RowReasoning RowKind = "reasoning"
	RowTool      RowKind = "tool"
	RowShell     RowKind = "shell" // a !command and its output
	RowCompact   RowKind = "compact"
	RowNotice    RowKind = "notice"
	RowError     RowKind = "error"
)

var rowKinds = []RowKind{RowMessage, RowReasoning, RowTool, RowShell, RowCompact, RowNotice, RowError}

// CallStatus is where a tool call, or a !command, stands on its row.
type CallStatus string

const (
	CallWait      CallStatus = "wait" // arguments streaming, or queued behind a serial call
	CallRun       CallStatus = "run"
	CallOK        CallStatus = "ok"
	CallWarn      CallStatus = "warn" // it finished, but something it ran failed
	CallErr       CallStatus = "err"
	CallStopped   CallStatus = "stopped"   // the user denied, dismissed or interrupted it
	CallBG        CallStatus = "bg"        // its work goes on in the background
	CallCancelled CallStatus = "cancelled" // an error row: the turn was interrupted
)

var callStatuses = []CallStatus{CallWait, CallRun, CallOK, CallWarn, CallErr, CallStopped, CallBG, CallCancelled}

// Mark is a row's state mark: a glyph one cell wide and its theme group.
type Mark struct{ Glyph, HL string }

// MarkKind names a state mark: a key of Marks, and the word a row or Lua
// passes to ask for one.
type MarkKind string

const (
	MarkSpin    MarkKind = "spin" // drawn with the "spin" effect, which steps its glyph through Spinner
	MarkOK      MarkKind = "ok"
	MarkWarn    MarkKind = "warn"
	MarkErr     MarkKind = "err"
	MarkStop    MarkKind = "stop"
	MarkBG      MarkKind = "bg"
	MarkWait    MarkKind = "wait"
	MarkThought MarkKind = "thought"
	MarkNote    MarkKind = "note"
)

// Marks are the state marks every row shares, the chat's calls and the
// lists' runs alike (zemi.ui.marks), one glyph and one meaning each:
// running, finished, finished with a failure inside, failed, stopped by the
// user, going on in the background, waiting, reasoning and a notice.
var Marks = map[MarkKind]Mark{
	MarkSpin: {Spinner[0], "Dim"}, MarkOK: {"✓", "Ok"}, MarkWarn: {"!", "Warn"}, MarkErr: {"✗", "Danger"}, MarkStop: {"■", "Dim"},
	MarkBG: {"⧗", "Dim"}, MarkWait: {"·", "Dim"}, MarkThought: {"✻", "ThinkingMark"}, MarkNote: {"◦", "Dim"},
}

// Spinner is the running mark's frames, one per frame tick.
var Spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// sessionEmoji pools the emoji a session's marker is picked from: one
// codepoint, emoji presentation (colored without VS16), East Asian Width W
// by UAX #11 (so zemi.ui.width and the terminal agree on 2 cells), Emoji 11.0
// or older so every font that ships emoji has them, and distinct and
// neutral: no faces, hands, people, flags or symbols the UI draws. The pool
// test pins the rest; the entries were curated against Unicode's
// emoji-variants data.
var sessionEmoji = []string{
	"🌊", "💧", "☔", "⛅", "🌈", "🌙", "⭐", "🌍", "🌵", "🌱",
	"🍀", "🍄", "🌻", "🦋", "🐢", "🐙", "🐳", "🐧", "🐚", "📎",
	"📌", "🔖", "📚", "📦", "🔑", "🔨", "🧰", "🧩", "🧭", "💡",
	"💎", "🔮", "🎲", "🎁", "🎨", "🚀", "⚓", "☕", "🍕", "💾",
	"🦊", "🦉", "🐝", "🐬", "🦀", "🦑", "🦎", "🦕", "🐘", "🦔",
	"🦜", "🐠", "🌲", "🌳", "🍁", "🌷", "🌹", "🌸", "🌺", "🍒",
	"🍋", "🥑", "🥕", "🍞", "🧀", "🍩", "🍿", "🔭", "🔬", "🧪",
	"🧬", "🧲", "🛸", "🎸", "🧸", "🧵", "🎯", "🗻", "🏰", "🎡",
}

// SessionMark derives a top-level session's marker emoji from its id: stable
// across resume and restart, zero storage, and not user-changeable. An empty
// id (no session yet) has no marker.
func SessionMark(sid string) string {
	if sid == "" {
		return ""
	}
	var h uint32 = 2166136261
	for i := range len(sid) {
		h = (h ^ uint32(sid[i])) * 16777619
	}
	return sessionEmoji[h%uint32(len(sessionEmoji))]
}

// markedTitle is the marker-prefixed title the session surfaces draw: the
// same shape everywhere a session row names itself, clipped as a row shows it.
func markedTitle(sid, title string) string {
	return SessionMark(sid) + " " + clipRunes(title, 60)
}

// OverlayKey is one character that runs Fn(selected item) on a list, or on
// a Go screen's list the command Run. A Danger key acts on its second press,
// the footer saying so after the first, as a danger row does. A Go screen's
// key with neither names the row action of that hotkey its rows offer, so the
// footer shows it whatever row is selected; on a row without it, the key
// toasts the row's detail, which says why.
type OverlayKey struct {
	Key, Desc string
	Fn        *glua.LFunction
	Run       string
	Danger    bool
	// Act is the Go screen's handler: it runs with the selected row, where
	// Run names a command that takes none. Applies narrows the key and its
	// hint to the rows it acts on, nil for every row.
	Act     func(OverlayItem)
	Applies func(OverlayItem) bool
}

// rowMeta is what a transcript row knows beyond its text: the facts a
// resumed session rebuilds it from, kept beside the message that drew it.
type rowMeta struct {
	Status CallStatus `json:"status,omitempty"`
	Run    string     `json:"run,omitempty"`
	Job    string     `json:"job,omitempty"`
}

// ChatMessage is a transcript entry (composer submissions); assistant
// replies carry the turn stats the chat widget renders.
type ChatMessage struct {
	Role   fantasy.MessageRole
	Kind   RowKind
	Text   string // tool rows: the input summary
	Label  string // tool rows: the model's description of a command
	Tool   string
	Status CallStatus // tool and shell rows; an error row an interrupt left is CallCancelled
	Detail string     // tool rows: subagent or workflow progress summary; notice rows: a hint drawn after the text, never copied
	Tail   string     // a running shell row: its output tail, which its view reads as it grows
	Job    string     // shell and job_start rows: the job the call handed the work to
	Run    string     // agent tool rows: the run id
	Diff   string     // file tool rows: the applied change as "- "/"+ " lines
	Output string     // tool rows: the last lines of the result (shortOutput)
	Stat   *TurnStat
	CallID string // tool rows: the stream's call id, so zemi run reports a call once
	ID     string // the stored message's id once it is stored; a rewind names it
	SentAt int64  // a user row: when the prompt was sent, unix ms, 0 elsewhere
}

// Plural is one when n is 1, else many.
func Plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// MoreLines is the one wording of what a cut left out: "… 3 more lines".
func MoreLines(n int) string {
	return fmt.Sprintf("… %d more %s", n, Plural(n, "line", "lines"))
}

// rowOutput is a tool result as its transcript row shows it, else
// shortOutput: a plan's verdict and an answered question call's
// "question\tanswer\tnote" lines, since their results are written for the model.
func rowOutput(tool, out string) string {
	switch tool {
	case tools.ToolPlan:
		verdict, path := tools.PlanOutcome(out)
		if out = verdict; path != "" {
			out += ", saved to " + perm.TildePath(path)
		}
	case tools.ToolQuestion:
		if qa := tools.QuestionAnswers(out); qa != nil {
			line := func(s string) string { return clipLine(s, 300) }
			rows := make([]string, len(qa))
			for i, p := range qa {
				rows[i] = line(p[0]) + "\t" + cmp.Or(line(p[1]), "not answered") + "\t" + line(p[2])
			}
			return strings.Join(rows, "\n")
		}
	}
	return shortOutput(out)
}

// shortOutput keeps 10 lines of a tool result for the transcript row, each
// capped at 300 runes and sanitized: the first three, which say what went
// wrong, and the last six, with a row counting what fell out between them.
func shortOutput(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if n := len(lines) - 9; n > 1 {
		// a tools.TruncOut marker stands for the lines it cut
		for _, l := range lines[3 : len(lines)-6] {
			if a, b, ok := tools.CutLines(l); ok {
				n += b - a
			}
		}
		cut := append([]string{MoreLines(n)}, lines[len(lines)-6:]...)
		lines = append(lines[:3:3], cut...)
	}
	for i, l := range lines {
		lines[i] = sanitize.String(clipRunes(l, 300))
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
	Ended     int64 // unix seconds the turn ended, 0 when unknown
}

// Host bundles the core services the zemi.* namespaces adapt.
type Host struct {
	RT       *lua.Runtime
	Log      *slog.Logger
	Settings *config.Store
	Dirs     config.Dirs
	// logLevel is the file log's level, moved live by the log.level key;
	// nil in hosts that own no log file. logPath is that file, for /logs.
	logLevel *slog.LevelVar
	logPath  string

	// Agent core (DESIGN §6.3). DB is the project store, GlobalDB
	// the state-dir store.
	Providers *agent.Registry
	// Builtin seeds Providers on every generation: the catalog with the
	// env keys resolved at boot, since the environment cannot change under
	// a running zemi, and again by /model update.
	Builtin  atomic.Pointer[agent.Catalog]
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

	tMu      sync.Mutex
	session  *Session
	view     *agentRun // the agent run the chat shows instead of the session, nil for none
	viewBack string    // the command that reopens the screen view came from
	viewMenu string    // the overlay left open under view, whose esc goes to the view

	// msgMu guards the messages picker's levels opened in this session, and
	// the call view on top of them: a transcript that grows rebuilds each
	// where it is open
	msgMu     sync.Mutex
	msgLevels []string
	callRow   int    // the row the open call view shows
	callID    string // that call's id, which names the row it settles at
	callServe int    // the row the live call view's watcher serves, so an
	// opened view supersedes the last watcher instead of adding one

	// trEpoch changes whenever the transcript is replaced (new, resume,
	// rewind), so the chat widget can cache the rows of settled messages.
	trEpoch atomic.Int64
	// trEdit changes when a settled row's text grew in place (a joined
	// reasoning chunk), which re-renders every cached unit: trEpoch would also
	// drop the rows the reader opened.
	trEdit atomic.Int64
	// findSeq tags a session search: a scan whose tag moved on lands nowhere,
	// so an older answer never replaces a newer box.
	findSeq atomic.Int64

	watches *watchReg
	hookReg *hookReg
	// cfgPrev and cfgNow hold, per key, the values zemi.config.set wrote
	// during the previous boot and during this one, in order: a replay
	// writing the same sequence keeps a runtime pick made since (config.go)
	cfgMu   sync.Mutex
	cfgGen  Gen
	cfgPrev map[config.Key][]any
	cfgNow  map[config.Key][]any
	// picked mirrors the picks kept in the global store as config:<path>,
	// unchecked for a plugin key not registered yet
	picked map[config.Key]any
	// pluginKeys is the generation that last registered each key a
	// registration owns (a plugin setting, a model target's models.<name>);
	// dropPluginKeys drops the ones a replay did not register again, never one
	// the schema owns
	pluginKeys map[config.Key]Gen
	// KeepPicks lets a runtime pick outlive the process; main turns it on
	// once the TUI's flags applied, so a flag, zemi run and zemi health keep none.
	KeepPicks atomic.Bool
	// swap records that closeSession swapped the session (/new, /resume):
	// a headless run reads and clears it, so only a swap made during the
	// run counts.
	swap atomic.Bool
	// agentsView is the agents view's one ticker claim (watchAgents).
	agentsView atomic.Bool
	agents     *agentHost
	luaTools   *toolReg
	stops      *stopReg
	// test is set, before boot, only in a zemi plugins test zemi
	test *PluginTest

	// memUndo reverses the /memory list's actions, newest last; opening the
	// list empties it, so an undo reaches no further than the list shows.
	memUndo struct {
		mu    sync.Mutex
		steps []func() error
	}

	// reflectMu serializes reflector runs (one at a time, DESIGN §10.3)
	reflectMu sync.Mutex
	// sweptMu guards swept: the message count each session held when this
	// process's startup sweep listed it, so one sitting reads a session once.
	sweptMu sync.Mutex
	swept   map[string]int
	// cutMu orders a rewind's cut against a reflector run's writes, so a run
	// never stores what it read from rows a rewind removed.
	cutMu    sync.Mutex
	initFail atomic.Pointer[string]                // headline of the user init.lua failure this boot recorded
	modelFn  func() (fantasy.LanguageModel, error) // stubs every model in tests
	browser  func(string)                          // stubs the sign-in browser in tests

	busyMu sync.Mutex
	busy   []*busyJob // user-visible jobs that are not a turn, oldest first

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
	pluginGrant atomic.Pointer[pluginGrant]  // the plugins the last boot loaded: their roots and what each manifest declared
	// askMu holds the prompt stage of an ask (ask); asksWaiting counts the
	// asks in it, and askCut moves when the user denies every waiting one
	askMu       sync.Mutex
	asksWaiting atomic.Int32
	askCut      atomic.Uint64

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
	// ResumeDir is the -C dir of this run, which ctx.resume names; "" for none.
	ResumeDir string
	// Inert, set before boot, makes a boot that only reads config (zemi
	// models, completion): no model call, no Lua IO but reads, no MCP start,
	// no file sync.
	Inert bool
	// Stderr is where a run with no UI prints its warnings and errors.
	Stderr io.Writer

	composerMu     sync.Mutex
	composerText   string
	composerCursor int

	recentMu    sync.Mutex
	recent      []*store.Session
	recentStale atomic.Bool
	splashMu    sync.Mutex // guards splash, splashProjects' cache
	splash      []Project
	splashStale atomic.Bool

	instrMu    sync.Mutex // guards instr, instructionsCached's cache
	instr      []string
	instrStale atomic.Bool

	mcp   *mcpReg  // MCP servers and their tools
	tasks *taskReg // subagent runs
	lsp   *lspReg  // language servers

	todoMu     sync.Mutex
	todoSID    string
	todos      []store.Todo
	todosStale atomic.Bool

	// luaApproved holds the chunk names of Lua tool runs the user approved
	// (guardLua); luaRuns numbers every run. scripts holds what a running
	// script's zemi.tool.call reaches: run chunk name -> *luaScript.
	luaApproved sync.Map
	luaRuns     atomic.Int64
	scripts     sync.Map

	// turn is the scope of the turn runTurns is on: cancelling it stops
	// compaction, the model and tools.
	turn atomic.Pointer[turnScope]

	plans planReg // the read-only restore point and headless denials

	layout atomic.Pointer[layoutSet] // zemi.ui.layout.set's; nil draws DefaultLayout
	focus  atomic.Pointer[string]    // the layout node taking keys; nil or "" is the composer
	title  atomic.Pointer[titleSet]  // zemi.ui.title's; nil names the tab after the session

	// reloadDone is non-nil and open while reloads run; reloadSeq counts
	// reloads started (see awaitReload). reloadRun keeps one reload's sweep
	// of the dead generation's registrations from racing the next one's replay.
	reloadMu   sync.Mutex
	reloadRun  sync.Mutex
	reloads    int
	reloadSeq  uint64
	reloadDone chan struct{}
	// reload and changed are main's (SetReload): a reload that moves the
	// baseline changed compares against, set before boot and never after
	reload      func() error
	changed     func() bool
	reloadCheck sync.Mutex

	// projectTrusted is the trust boot step's decision (DESIGN §5): project
	// config, instructions and language servers load only when it is set,
	// and reads reach less without it.
	projectTrusted atomic.Bool
	trustDropped   atomic.Bool // trust went off: the next reload must run to drop project config
	// trustRoot is the repository the session belongs to (a worktree maps to
	// its main checkout), found once: it keys the store and the scratchpad,
	// and finding it walks the disk. project is the same
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

	// projectSkills is a trusted project's skills, found when the decision
	// is set, which every reload does
	projectSkills atomic.Pointer[[]skill.Skill]
}

// SetProjectTrusted records the trust decision and rescans the project's
// skills. A flip also rebuilds the brain render the session carries: the pin
// key moves with trust, and the next send would otherwise re-pin the stale
// render, keeping the private catalog in an untrusted session (DESIGN §10.7) or
// holding it back a compaction.
func (h *Host) SetProjectTrusted(ok bool) {
	prev := h.projectTrusted.Swap(ok)
	if prev && !ok {
		h.trustDropped.Store(true)
	}
	var found []skill.Skill
	if ok {
		found = h.discoverProjectSkills()
	}
	h.projectSkills.Store(&found)
	h.instrStale.Store(true)
	ag := h.agents
	ag.mu.Lock()
	stale := prev != ok && ag.brain != ""
	ag.mu.Unlock()
	if stale {
		brain, globs := h.brainFor()
		ag.mu.Lock()
		ag.brain, ag.brainGlobs = brain, globs
		ag.mu.Unlock()
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

// TakeSwap reports and clears the session-swap fact (closeSession).
func (h *Host) TakeSwap() bool { return h.swap.Swap(false) }

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
func NewHost(rt *lua.Runtime, log *slog.Logger, settings *config.Store, dirs config.Dirs) *Host {
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
		hookReg:   newHookReg(),
		Providers: agent.NewRegistry(),
		agents:    newAgentHost(rt.Clock()),
		luaTools:  newToolReg(),
		stops:     newStopReg(),
		jobs:      tools.NewJobs(),
		skillsReg: &skillReg{},
		mcp:       newMCPReg(),
		tasks:     newTaskReg(settings),
		lsp:       newLSPReg(),
	}
	h.Builtin.Store(&agent.Catalog{})
	h.Keymaps.owner, h.Commands.owner, h.Widgets.owner, h.Themes.owner, h.Events.owner, h.luaTools.owner, h.watches.owner = h.PluginOf, h.PluginOf, h.PluginOf, h.PluginOf, h.PluginOf, h.PluginOf, h.PluginOf
	h.jobs.OnChange = func() {
		h.refresh()
		h.watchJobs()
	}
	settings.SetChoices("agent.effort", append(slices.Clone(agent.Efforts), ""))
	h.trustRoot = sync.OnceValue(h.Dirs.TrustRoot)
	h.project = sync.OnceValue(func() string { return perm.TildePath(h.trustRoot()) })
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
	h.picked, h.pluginKeys = map[config.Key]any{}, map[config.Key]Gen{}
	if h.GlobalDB != nil {
		h.loadPicks()
		// the global store's partition reconciles at startup; a project
		// store's waits for its project session to open (DESIGN §7)
		h.reconcileBrain(h.partition(store.ScopeUser))
	}
	h.hostActions = map[string]func(h *Host, args string) error{
		"zemi.ui.help":            func(h *Host, _ string) error { return h.OpenHelp() },
		"zemi.model.pick":         func(h *Host, _ string) error { return h.OpenModelPicker("chat") },
		"zemi.ui.focus.set":       func(h *Host, id string) error { return h.focusNode(id) },
		"zemi.ui.focus.next":      func(h *Host, _ string) error { return h.cycleFocus(1) },
		"zemi.ui.focus.prev":      func(h *Host, _ string) error { return h.cycleFocus(-1) },
		"zemi.agent.profile.next": func(h *Host, _ string) error { return h.cycleProfile(1) },
		"zemi.agent.profile.prev": func(h *Host, _ string) error { return h.cycleProfile(-1) },
		"zemi.reload":             nil, // Go-reserved; never dispatched here
	}
	for _, a := range scrollActions {
		h.hostActions["zemi.ui.scroll."+string(a)] = func(h *Host, id string) error { h.scroll(a, id); return nil }
	}
	// insert takes the text after the action's name
	for _, op := range []ComposerOp{ComposerSubmit, ComposerInsert, ComposerHistoryPrev, ComposerHistoryNext, ComposerPaste, ComposerEditor} {
		h.hostActions["zemi.composer."+string(op)] = func(h *Host, args string) error {
			if ui := h.hooks(); ui != nil {
				ui.ComposerEdit(op, args, 0)
			}
			return nil
		}
	}
	h.refreshPending()
	h.recentStale.Store(true)
	h.splashStale.Store(true)
	h.instrStale.Store(true)
	h.trEpoch.Store(1) // 0 means "an agent run's transcript; do not cache"
	return h
}

// TempDir is a directory for the host's own scratch files (a memory the
// editor opens, a warm diff index).
func (h *Host) TempDir() (string, error) {
	if h.Dirs.StateHome == "" {
		return "", errors.New("no state dir for temp files")
	}
	dir := filepath.Join(h.Dirs.StateHome, "tmp")
	return dir, os.MkdirAll(dir, 0o700)
}

// now is the time Lua sees (lua.Clock): a toast's, a log line's, a render's.
func (h *Host) now() time.Time { return h.RT.Clock().Now() }

func (h *Host) hooks() UIHooks {
	h.uiMu.RLock()
	defer h.uiMu.RUnlock()
	return h.ui
}

// trustHint names what trusts the project: /trust in the TUI, the flag of
// a headless run.
func (h *Host) trustHint() string {
	if h.hooks() == nil {
		return "zemi run --trust"
	}
	return "/trust"
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

type busyJob struct {
	label  string
	start  time.Time
	cancel context.CancelFunc
}

// Busy shows label on the working rule until done is called, for a job the
// user waits on that is not a turn; a non-nil cancel lets a double esc
// (CancelBusy) stop it.
func (h *Host) Busy(label string, cancel context.CancelFunc) (done func()) {
	j := &busyJob{label: label, start: h.now(), cancel: cancel}
	h.busyMu.Lock()
	h.busy = append(h.busy, j)
	h.busyMu.Unlock()
	h.refresh()
	return func() {
		h.busyMu.Lock()
		h.busy = slices.DeleteFunc(h.busy, func(b *busyJob) bool { return b == j })
		h.busyMu.Unlock()
		h.refresh()
	}
}

// CancelBusy stops every busy job that has a cancel func. Only the user's
// esc calls it: zemi.agent.cancel and a permission answer stop the turn alone.
func (h *Host) CancelBusy() {
	h.busyMu.Lock()
	defer h.busyMu.Unlock()
	for _, j := range h.busy {
		if j.cancel != nil {
			j.cancel()
		}
	}
}

// BusyNow is the newest busy job's label ("" when none runs) and whether esc
// can stop any busy job; the UI re-renders every frame while one runs.
// NOTE: only the newest label shows; a "+N" part would name the others.
func (h *Host) BusyNow() (label string, cancellable bool) {
	h.busyMu.Lock()
	defer h.busyMu.Unlock()
	if len(h.busy) == 0 {
		return "", false
	}
	j := h.busy[len(h.busy)-1]
	return j.label, slices.ContainsFunc(h.busy, func(b *busyJob) bool { return b.cancel != nil })
}

// BusyEntry is one busy job as ctx.busy carries it: what the job is, how long
// it has run, and whether esc stops it.
type BusyEntry struct {
	Label       string
	Elapsed     time.Duration
	Cancellable bool
}

// BusyList is every busy job, newest first, so a widget can name each of them
// instead of the newest alone.
func (h *Host) BusyList() []BusyEntry {
	h.busyMu.Lock()
	defer h.busyMu.Unlock()
	out := make([]BusyEntry, 0, len(h.busy))
	for _, j := range slices.Backward(h.busy) {
		out = append(out, BusyEntry{Label: j.label, Elapsed: h.now().Sub(j.start), Cancellable: j.cancel != nil})
	}
	return out
}

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
		return markedTitle(h.AgentSessionID(), t) + " · zemi"
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
func ToastTitle(level slog.Level, title string) string {
	switch {
	case title != "" || level < slog.LevelWarn:
		return title
	case level == slog.LevelWarn:
		return "Warning"
	}
	return "Error"
}

// Notify shows a toast; with no UI a warning or an error goes to Stderr
// instead, so a headless run never swallows one. The title says what
// happened, in sentence case with no period: "<Kind> <past participle>" with
// the name in the body, "Could not <verb> <object>" with the cause in the
// body, "Nothing to <verb> [yet]" with a reason only when it is not obvious.
func (h *Host) Notify(level slog.Level, title, body string) {
	h.recordNotify(level, title, body)
	if ui := h.hooks(); ui != nil {
		ui.Notify(level, title, body)
		return
	}
	if level < slog.LevelWarn {
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
func (h *Host) scroll(kind ScrollAction, id string) {
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

func (h *Host) AppendMessage(role fantasy.MessageRole, text string) {
	h.AppendChat(ChatMessage{Role: role, Text: text})
}

func (h *Host) AppendChat(m ChatMessage) {
	h.tMu.Lock()
	h.session.Transcript = append(h.session.Transcript, m)
	h.tMu.Unlock()
	h.refresh()
	h.refreshMessages()
}

// OpenHelp opens the help overlay listing keys and slash commands. A
// command row runs it, unless it needs an argument, which it fills in.
func (h *Host) OpenHelp() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	var items []OverlayItem
	keys, lists := ui.KeyHelp()
	for _, k := range keys {
		items = append(items, OverlayItem{Text: k[0], Desc: k[1], Section: "Keys"})
	}
	var aliases []OverlayItem
	for _, b := range h.Keymaps.List() {
		desc := b.Desc
		if desc == "" {
			desc = describeRHS(b.RHS)
		}
		if b.Focus != "" {
			desc += " (in " + b.Focus + ")"
		}
		if b.List {
			aliases = append(aliases, OverlayItem{Text: b.Key, Desc: cmp.Or(b.Desc, "same as "+b.RHS.Move), Section: "Lists"})
			continue
		}
		items = append(items, OverlayItem{Text: b.Key, Desc: desc, Section: "Keys"})
	}
	for _, k := range lists {
		items = append(items, OverlayItem{Text: k[0], Desc: k[1], Section: "Lists"})
	}
	items = append(items, aliases...)
	// Hidden keeps a command out of the palette's default list, not out of
	// /help: this is the index the README points people at.
	for _, c := range h.Commands.List() {
		item := OverlayItem{Text: "/" + c.Name, Desc: c.Desc, Section: "Commands", Run: c.Name}
		if c.NArgs == NArgsSome {
			item.Run, item.Fill = "", "/"+c.Name+" "
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "help", Title: "Help", Origin: lua.OriginBuiltin, Items: items})
	return nil
}

func describeRHS(rhs RHS) string {
	switch {
	case rhs.Command != "":
		return "/" + rhs.Command
	case rhs.API != "":
		return rhs.API
	case rhs.Move != "":
		return rhs.Move
	default:
		return "<function>"
	}
}

// host -> Lua event plumbing

// emitSync runs the handlers synchronously on the given interpreter state;
// callers are boot steps and the Lua-side emit, already on the interpreter.
func (h *Host) emitSync(L *glua.LState, handlers []*EventHandler, name string, payload map[string]any, from string) {
	ev := eventTable(name, from, payload)
	for _, hd := range handlers {
		if _, _, err := h.callEvent(L, hd, ev); err != nil {
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
func vetoDecision[T ~string](ret glua.LValue, allowed ...T) (tb *glua.LTable, decision T, bad string) {
	if ret == nil || ret == glua.LNil {
		return nil, "", ""
	}
	tb, ok := ret.(*glua.LTable)
	if !ok {
		return nil, "", fmt.Sprintf("a handler returned %s: return nil, or { decision = %q, reason = ... }",
			ret.Type().String(), allowed[0])
	}
	if decision = T(glua.LVAsString(tb.RawGetString("decision"))); !slices.Contains(allowed, decision) {
		return nil, "", fmt.Sprintf("a handler returned decision %q: use nil or one of %s",
			decision, strings.Join(strs(allowed), ", "))
	}
	return tb, decision, ""
}

// VetoResult is the aggregated outcome of a vetoable event.
type VetoResult struct {
	Denied   bool
	Reason   string
	TimedOut bool
	// Ask is a handler's { decision = "ask" }: the gate runs the ask path
	// for it. A denial outranks it.
	Ask bool
}

// EmitVetoable runs handlers synchronously on the interpreter, each bounded
// by the veto deadline (armed at handler start); a timed-out handler
// contributes nothing and the aggregated decision or the default wins
// (DESIGN §4, §6.2).
func (h *Host) EmitVetoable(ctx context.Context, name string, payload map[string]any) VetoResult {
	return h.emitVetoable(ctx, name, h.Events.Handlers(name), payload)
}

func (h *Host) emitVetoable(ctx context.Context, name string, handlers []*EventHandler, payload map[string]any) VetoResult {
	if len(handlers) == 0 {
		// counted only: the gate's veto event with nobody listening must
		// not displace the recent log on every model action
		h.countEvent(name)
		return VetoResult{}
	}
	h.recordEvent(name, "host", payload)
	type outcome struct {
		denied bool
		ask    bool
		reason string
		timed  bool
	}
	outcomes := make([]outcome, len(handlers))
	allowed := []string{"deny"}
	if name == EventPermissionRequest {
		allowed = append(allowed, "ask")
	}
	ev := eventTable(name, "host", payload)
	_, err := h.RT.Do(ctx, lua.ClassUI, lua.VetoCeiling(len(handlers), 1), 0,
		func(L *glua.LState) ([]any, error) {
			for i, hd := range handlers {
				ret, timedOut, err := h.callEvent(L, hd, ev)
				switch {
				case timedOut:
					outcomes[i].timed = true
				case err != nil:
					h.logHandlerError(hd, err)
				default:
					tb, decision, bad := vetoDecision(ret, allowed...)
					switch {
					case bad != "":
						h.logHandlerError(hd, errors.New(bad))
						// a handler that meant to deny and misspelled it must
						// not read as consent where there is a closed side
						outcomes[i] = outcome{denied: name == EventPermissionRequest, reason: bad}
					case decision == "deny":
						outcomes[i] = outcome{denied: true, reason: glua.LVAsString(tb.RawGetString("reason"))}
					case decision == "ask":
						outcomes[i] = outcome{ask: true, reason: glua.LVAsString(tb.RawGetString("reason"))}
					}
				}
			}
			return nil, nil
		})
	if errors.Is(err, lua.ErrAbandoned) {
		return VetoResult{TimedOut: true}
	}
	if err != nil && name == EventPermissionRequest {
		// a hot runtime cannot run the deny hooks: fail closed
		return VetoResult{Denied: true, Reason: "permission.request hooks could not run (lua runtime unavailable, /reload): " + err.Error()}
	}
	res := VetoResult{}
	for _, o := range outcomes {
		if o.timed {
			res.TimedOut = true
		}
		if o.denied && !res.Denied {
			res.Denied, res.Reason = true, o.reason
		}
		if o.ask {
			res.Ask = true
		}
	}
	if res.Denied {
		res.Ask = false // deny outranks ask
	}
	if res.Denied {
		h.Log.Info("vetoable event denied", "origin", "events",
			"event", name, "reason", res.Reason,
		)
	} else if res.TimedOut {
		h.Log.Warn("vetoable event timed out; host default applies", "origin", "events", "event", name)
	}
	return res
}

// setHandlerContext arms a per-handler deadline. gopher-lua's RemoveContext
// also nils the context polled by any still-running VM loop, so a handler
// invoked from a live Lua frame (e.g. zemi.event.emit under a chunk) must
// restore the enclosing request context instead of removing it: the outer
// mainLoopWithContext would otherwise dereference a nil ctx on its next
// instruction.
func setHandlerContext(L *glua.LState) (context.Context, context.CancelFunc) {
	prev := L.Context()
	handlerCtx, cancel := context.WithTimeout(context.Background(), lua.VetoDeadline)
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
func (h *Host) callEvent(L *glua.LState, hd *EventHandler, ev map[string]any) (glua.LValue, bool, error) {
	if hd.Once && !h.Events.drop(hd) {
		return glua.LNil, false, nil
	}
	handlerCtx, restore := setHandlerContext(L)
	defer restore()
	rets, err := h.callFn(L, luaCaller{gen: h.currentGen(), origin: hd.Origin, path: hd.Path}, hd.Fn, 1, lua.FromGo(L, ev))
	timedOut := handlerCtx.Err() != nil
	if timedOut || err != nil {
		return glua.LNil, timedOut, err
	}
	return rets[0], false, nil
}

func (h *Host) logHandlerError(hd *EventHandler, err error) {
	h.Log.Error("event handler failed", "origin", "events",
		"event", hd.Event, "id", hd.ID, "from", string(hd.Origin), "error", err,
	)
	h.RecordError("event "+hd.Event, err.Error())
}

// widget rendering

// Effect is an animation the UI applies to a segment's text.
type Effect string

const (
	EffectNone    Effect = ""        // still
	EffectShimmer Effect = "shimmer" // a bright band sweeps the text, blending its hl toward Normal
	EffectSpin    Effect = "spin"    // its first glyph steps through Spinner
)

// Segment is one styled run of text.
type Segment struct {
	Text string
	HL   string
	Hit  int // Go chrome only: 1-based index of the clickable item this text belongs to
	// Click is the id a click passes to the on_click of Widget; a Go
	// statusline pill sets it with no Widget
	Click  string
	Widget string
	// Link is the URL the text stands for, drawn as a terminal hyperlink and
	// opened by a click: a markdown link or a bare URL, and nothing else
	Link   string
	Effect Effect // the animation the UI applies to the text
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
	// Pin on a row's first segment is its part in the header stack a
	// scrolled region pins over its first rows (Lua's sticky)
	Pin Pin
	// Anchor is the transcript row a rendered row draws: a following
	// overlay's row names the same number to scroll the region here
	Anchor int
	// Call is the transcript row of the call or thought a rail's row draws,
	// so a list naming one call bands its rows and no other's
	Call int
	Bar  bool // Go only: a viewport's scrollbar cell, drawn in the viewport's right border, a separator column behind it standing down
	// Fill is Go only: the surface under the segment where its style paints
	// the canvas. A field, since hover rewrites Over and HL.
	Fill Fill
	// OnPanel is Go only: a box drew the segment on its panel, so its Fade
	// sinks toward the panel and not the canvas. Fill cannot say it: a band
	// lies over either surface.
	OnPanel bool
	// Line is Go only: a chrome border, rule or separator, its runes light box
	// lines or blanks one cell each; lines that touch join (ui.join)
	Line bool
	// Fade is how far (0 to 1) the segment's colors sink toward the surface it
	// sits on: a scrolled view's rows at an edge content lies past, or a
	// widget's own clipped block, which sets it on the segments it cuts
	Fade float64
}

// Pin is a row's part in a scrolled region's pinned header stack.
type Pin uint8

const (
	PinNone    Pin = iota
	PinTurn        // sticky = true: heads a turn and starts the stack anew
	PinSection     // sticky = "section": heads a part of the turn, pinned under it
	PinEnd         // sticky = "end": the last row of the nearest open section
)

// Fill is a surface of the active palette (DESIGN §6.6): the chat sits on
// the canvas, every box on the panel, a box's selected row on the band.
type Fill uint8

const (
	FillAuto   Fill = iota // the canvas, or the panel inside a box
	FillPanel              // a box: overlays, prompts, toasts, popups, the composer
	FillBand               // a list's or a prompt's selected row, over any background
	FillCanvas             // the canvas even inside a box: a well, such as a prompt's command
	FillWell               // a well's lifted half, over the panel: what a command answered
)

// RenderWidget runs a widget's Lua render at the cells its layout node gives
// it and returns sanitized segment lines (DESIGN §6.6, §8). more is render's
// second result: the lines are what its time allowed and it wants to run
// again, which is how a long chat draws its latest rows first.
func (h *Host) RenderWidget(ctx context.Context, w *Widget, width, height int, state UIState) (lines [][]Segment, more bool, err error) {
	return h.renderWidget(ctx, w, width, height, state, nil)
}

// renderWidget is RenderWidget with over's fields set over the ctx render
// gets, which is how test.render supplies what a session would.
func (h *Host) renderWidget(ctx context.Context, w *Widget, width, height int, state UIState, over *glua.LTable) (lines [][]Segment, more bool, err error) {
	linesAny, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		// a frame queued before a reload holds the old generation's function
		if w.Gen != h.currentGen() {
			return nil, lua.ErrReloaded
		}
		// the purity guard holds only while render runs, not while this
		// request waits in the queue behind other Lua (boot code behind the
		// trust prompt)
		h.rendering.Store(true)
		defer h.rendering.Store(false)
		ctxTb, done := h.widgetCtxTable(L, width, height, state)
		defer done()
		if over != nil {
			over.ForEach(ctxTb.RawSet)
		}
		rets, err := h.callFn(L, luaCaller{gen: w.Gen, origin: w.Origin, path: w.Path}, w.Render, 2, ctxTb)
		if err != nil {
			return nil, err
		}
		ret, more := rets[0], rets[1] == glua.LTrue
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
					line = append(line, Segment{Text: sanitize.Row(string(c))})
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
						case glua.LNumber:
							return float64(f)
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
		seg.Text = sanitize.Row(t)
	}
	if hl, ok := field("hl").(string); ok {
		seg.HL = UserHL(hl)
	}
	if e, ok := field("effect").(string); ok {
		seg.Effect = Effect(e)
	}
	if f, ok := field("fade").(float64); ok {
		seg.Fade = min(max(f, 0), 1)
	}
	seg.NoCopy = field("copy") == false
	seg.NoHover = field("hover") == false
	seg.Soft = field("soft_wrap") == true
	if n, ok := field("anchor").(float64); ok {
		seg.Anchor = int(n)
	}
	if n, ok := field("call").(float64); ok {
		seg.Call = int(n)
	}
	switch field("sticky") {
	case true:
		seg.Pin = PinTurn
	case "section":
		seg.Pin = PinSection
	case "end":
		seg.Pin = PinEnd
	}
	if id, ok := field("click").(string); ok && id != "" {
		seg.Click, seg.Widget = id, widget
	}
	if l, ok := field("link").(string); ok {
		seg.Link = linkOf(l)
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
		_, err := h.callFn(L, luaCaller{gen: w.Gen, origin: w.Origin, path: w.Path}, w.OnClick, 0, lua.FromGo(L, ev))
		return nil, err
	})
	return err
}

// Chrome groups are the Go styles of prompts and other host chrome
// (ui.chromeStyles), in the active palette's roles: no group or hl.set
// reaches them and UserHL drops them from Lua.
const (
	ChromePrefix     = "Host:"
	ChromeBold       = ChromePrefix + "Bold"
	ChromeTitle      = ChromePrefix + "Title"
	ChromeRule       = ChromePrefix + "Rule"
	ChromeCursor     = ChromePrefix + "Cursor"
	ChromeValue      = ChromePrefix + "Value"
	ChromeHint       = ChromePrefix + "Hint"
	ChromeHintKey    = ChromePrefix + "HintKey"
	ChromeWarn       = ChromePrefix + "Warn"
	ChromeOk         = ChromePrefix + "Ok"
	ChromeDanger     = ChromePrefix + "Danger"
	ChromeDiffAdd    = ChromePrefix + "DiffAdd"
	ChromeDiffAddNum = ChromePrefix + "DiffAddNum"
	ChromeDiffDel    = ChromePrefix + "DiffDel"
	ChromeDiffDelNum = ChromePrefix + "DiffDelNum"
	ChromeDiffCtx    = ChromePrefix + "DiffCtx"
	ChromeDiffCtxNum = ChromePrefix + "DiffCtxNum"
	ChromeSelection  = ChromePrefix + "Selection"
	ChromeHover      = ChromePrefix + "Hover"
	ChromeDim        = ChromePrefix + "Dim"
)

// UserHL drops a "Host:*" style from a Lua segment: those are the Go
// chrome styles (internal/ui/theme.go), so a widget or overlay row
// could otherwise paint itself as a permission prompt.
func UserHL(hl string) string {
	if strings.HasPrefix(hl, ChromePrefix) {
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

// oneLine wraps the two returns that mean one line:
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
				line = append(line, Segment{Text: sanitize.Row(c)})
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

// TurnEnd is how a turn ended.
type TurnEnd string

const (
	TurnOK        TurnEnd = "ok"
	TurnError     TurnEnd = "error"
	TurnCancelled TurnEnd = "cancelled"
)

var turnEnds = []TurnEnd{TurnOK, TurnError, TurnCancelled}

// Waiting is the prompt holding a turn: a permission ask, a plan to
// approve or a question to answer.
type Waiting string

const (
	WaitApproval Waiting = "approval"
	WaitPlan     Waiting = "plan"
	WaitAnswer   Waiting = "answer"
)

var waitings = []Waiting{WaitApproval, WaitPlan, WaitAnswer}

// ClockLayout formats UIState.Time, the wall clock widgets show; clock in
// zemi.tui.text spells it for Lua.
const ClockLayout = "15:04"

// StampLayout dates a session in an export and in zemi sessions.
const StampLayout = time.DateOnly + " " + ClockLayout

// UIState is the context snapshot passed to widget renders.
type UIState struct {
	Time  string // the wall clock in ClockLayout, stepped once a minute
	Frame int    // the 50ms frame clock the spinner, eyes and shimmer step on

	Ended          TurnEnd // how the turn that ended a moment ago ended; "" after that
	QuitArmed      bool    // a second ctrl+c quits
	InterruptArmed bool    // a second esc interrupts the turn
	KeySeq         string  // keys of a bound sequence typed so far
	RowsBelow      int     // transcript rows under a scrolled-up viewport
	Cancelling     bool    // interrupt sent, turn not finished yet
	Editing        string  // title of the file open in the editor
	Waiting        Waiting // while a prompt holds the turn
	Focus          string  // the drawn layout node taking keys, "" for the composer
	Rail           int     // the activity run the chat draws whole, 0 for none
	Printed        *Printed
	// Regions are the regions the layout draws this frame, replaced whole
	// when the set moves, so comparing two states sees it
	Regions *set.Set[string]
}

// Printed is set when zemi run prints the frame to a shell: Rows stand in
// for the transcript, never cached, and Live for the live turn (nil: none,
// and no todos). Prev is the last row printed above them, which the chat
// seeds its first block gap from; Tail marks the live tail's own render,
// which leaves the pending row to the rows it draws under.
type Printed struct {
	Rows      []ChatMessage
	Live      *StreamSnapshot
	Reasoning bool // zemi run --reasoning: every thought and call drawn whole
	Prev      *ChatMessage
	Tail      bool
}

// widgetCtxTable builds a render's ctx. The fields that copy the transcript,
// the live turn, todos, recent sessions or projects are built on first read, so a
// widget that never reads them does not pay for them (a statusline widget
// over a long session renders 50 times faster), and read nil once the
// render returned, so a ctx kept in an upvalue pins nothing. done ends it.
func (h *Host) widgetCtxTable(L *glua.LState, width, height int, state UIState) (tb *glua.LTable, done func()) {
	// the edit counter before the rows: a join landing between them would move it
	// past a snapshot without the chunk, and nothing moves it again
	edit := h.trEdit.Load()
	sess, stream := h.liveView()
	printed := state.Printed
	if printed != nil {
		sess.Transcript = printed.Rows
		if printed.Live != nil {
			stream = *printed.Live
		} else {
			stream.Active, stream.Text, stream.Reasoning, stream.Tools, stream.Steering = false, "", "", nil, nil
		}
	}
	// Showing an agent run, the chat renders its transcript alone and the run
	// reports its own state: the session's turn, todos and stream stay out of
	// it. Before the ctx writes below, which read stream.
	run := h.viewedRun()
	transcript := sess.Transcript
	var view *glua.LTable
	var rev uint64
	if run != nil {
		status, rows, live, vrev := run.view(stream)
		transcript, stream, rev = rows, live, vrev
		v := L.NewTable()
		v.RawSetString("id", glua.LString(run.id))
		v.RawSetString("agent", glua.LString(sanitize.String(run.label())))
		v.RawSetString("description", glua.LString(sanitize.String(run.description)))
		v.RawSetString("status", glua.LString(status))
		view = v
	}
	tb = L.CreateTable(0, 33)
	tb.RawSetString("headless", glua.LBool(printed != nil))
	if printed != nil && printed.Prev != nil {
		tb.RawSetString("printed_prev", chatRow(L, *printed.Prev))
	}
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
	tb.RawSetString("cwd", glua.LString(perm.TildePath(sess.CWD)))
	home, _ := os.UserHomeDir()
	tb.RawSetString("home", glua.LString(home))
	tb.RawSetString("project", glua.LString(h.project()))
	tb.RawSetString("trusted", glua.LBool(h.ProjectTrusted()))
	tb.RawSetString("init_error", glua.LString(h.InitError()))
	tb.RawSetString("init_path", glua.LString(h.initPath()))
	tb.RawSetString("model_hint", glua.LString(h.NoModelHint()))
	tb.RawSetString("profile", glua.LString(stream.Profile))
	tb.RawSetString("profile_read_only", glua.LBool(h.readOnly()))
	tb.RawSetString("model", glua.LString(stream.Model))
	tb.RawSetString("effort", glua.LString(h.Effort(stream.Model)))
	tb.RawSetString("agent_active", glua.LBool(stream.Active))
	tb.RawSetString("show_reasoning", glua.LBool(printed != nil && printed.Reasoning))
	if state.Rail > 0 {
		tb.RawSetString("rail", glua.LNumber(state.Rail))
	}
	tb.RawSetString("emoji", glua.LString(SessionMark(h.AgentSessionID())))
	// the tail's render leaves the pending row's inputs out: the rows printed
	// above it drew it once, and the tail draws it under them
	tail := printed != nil && printed.Tail
	if jobs := h.BusyList(); len(jobs) > 0 && !tail {
		b := L.CreateTable(len(jobs), 0)
		for _, j := range jobs {
			e := L.CreateTable(0, 3)
			e.RawSetString("label", glua.LString(j.Label))
			e.RawSetString("elapsed", glua.LNumber(j.Elapsed.Seconds()))
			e.RawSetString("cancellable", glua.LBool(j.Cancellable))
			b.Append(e)
		}
		tb.RawSetString("busy", b)
	}
	tb.RawSetString("pending_memory", glua.LNumber(h.pendingMemory.Load()))
	if !tail {
		sid := h.AgentSessionID()
		agents, flows := h.tasks.live(func(p string) bool { return p == sid })
		tb.RawSetString("agents_running", glua.LNumber(agents))
		tb.RawSetString("workflows_running", glua.LNumber(flows))
		tb.RawSetString("jobs_running", glua.LNumber(h.jobsRunningCount()))
	}
	if g, ok := h.Goal(); ok {
		gt := L.CreateTable(0, 3)
		gt.RawSetString("condition", glua.LString(sanitize.String(g.Condition)))
		gt.RawSetString("turns", glua.LNumber(g.Turns))
		gt.RawSetString("reason", glua.LString(sanitize.String(g.Reason)))
		tb.RawSetString("goal", gt)
	}
	if l, ok := h.Loop(); ok {
		lt := L.CreateTable(0, 4)
		lt.RawSetString("prompt", glua.LString(sanitize.String(l.Prompt)))
		lt.RawSetString("every", glua.LNumber(l.Every.Seconds()))
		lt.RawSetString("next", glua.LNumber(l.Next.Unix()))
		lt.RawSetString("runs", glua.LNumber(l.Runs))
		tb.RawSetString("loop", lt)
	}
	tb.RawSetString("usage", usageTable(L, stream))
	tb.RawSetString("context", contextTable(L, stream))
	tb.RawSetString("transcript_count", glua.LNumber(len(transcript)))
	if view != nil { // absent while the session shows
		tb.RawSetString("view", view)
	}
	// 0 while an agent run or a printed run is shown: the session's epoch
	// counts replacements of its own rows, and neither shows those. A shown
	// run reports the revision of its own rows beside it.
	epoch := h.trEpoch.Load()
	if run != nil || printed != nil {
		epoch = 0
	}
	tb.RawSetString("transcript_epoch", glua.LNumber(epoch))
	tb.RawSetString("view_revision", glua.LNumber(rev))
	tb.RawSetString("transcript_edit", glua.LNumber(edit))

	lazy := map[string]func(L *glua.LState) glua.LValue{
		"todos": func(L *glua.LState) glua.LValue {
			todos := L.NewTable()
			if run != nil || printed != nil && printed.Live == nil {
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
		"regions": func(L *glua.LState) glua.LValue {
			out := L.NewTable()
			if state.Regions != nil {
				for r := range *state.Regions {
					out.RawSetString(r, glua.LTrue)
				}
			}
			return out
		},
		"agent":      func(L *glua.LState) glua.LValue { return agentTable(L, stream, h.now()) },
		"transcript": func(L *glua.LState) glua.LValue { return transcriptTable(L, transcript) },
		"instructions": func(L *glua.LState) glua.LValue {
			files := h.instructionsCached()
			if files == nil {
				return glua.LNil
			}
			tb := L.CreateTable(len(files), 0)
			for _, p := range files {
				tb.Append(glua.LString(p))
			}
			return tb
		},
		"trustable": func(L *glua.LState) glua.LValue { return glua.LBool(!TrustUnsaved(h.trustRoot())) },
		"resume": func(L *glua.LState) glua.LValue {
			if id := h.AgentSessionID(); id != "" {
				return glua.LString(ResumeCommand(h.ResumeDir, id))
			}
			return glua.LString("")
		},
		"recent": func(L *glua.LState) glua.LValue {
			if len(sess.Transcript) > 0 {
				return glua.LNil
			}
			recent := L.NewTable()
			for _, r := range h.recentSessions() {
				row := L.NewTable()
				row.RawSetString("id", glua.LString(r.ID))
				title := sanitize.String(r.Title)
				if title == "" {
					title = "(untitled)" // as the splash names a session without one
				}
				row.RawSetString("title", glua.LString(markedTitle(r.ID, title)))
				row.RawSetString("ago", glua.LString(ago(r.UpdatedAt)))
				recent.Append(row)
			}
			return recent
		},
		"projects": func(L *glua.LState) glua.LValue {
			ps := h.splashProjects()
			if len(sess.Transcript) > 0 || len(ps) == 0 {
				return glua.LNil
			}
			out := L.NewTable()
			for _, p := range ps {
				row := L.NewTable()
				row.RawSetString("path", glua.LString(sanitize.String(perm.TildePath(p.Root))))
				row.RawSetString("root", glua.LString(p.Root))
				row.RawSetString("ago", glua.LString(ago(p.Used.UnixMilli())))
				out.Append(row)
			}
			return out
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

// instructionsCached is ctx.instructions: the project's instruction files,
// repo-relative, nil where project files cannot load (untrusted, or $HOME and
// /, which are nobody's project). Cached, since the splash renders every
// frame while a job runs: rebuilt after a reload, a trust change and a
// session close.
// NOTE: a file written outside zemi waits for one of those.
func (h *Host) instructionsCached() []string {
	h.instrMu.Lock()
	defer h.instrMu.Unlock()
	if !h.instrStale.Swap(false) {
		return h.instr
	}
	h.instr = nil
	if root := h.Dirs.RepoRoot(); h.ProjectTrusted() && !TrustUnsaved(root) {
		h.instr = []string{}
		mine := filepath.Join(h.Dirs.ConfigHome, "AGENTS.md")
		for _, p := range h.instructionFiles() {
			if p == mine {
				continue
			}
			if rel, err := filepath.Rel(root, p); err == nil {
				p = rel
			}
			h.instr = append(h.instr, p)
		}
	}
	return h.instr
}

// agentTable is ctx.agent: the live turn. now is the host's clock, which
// the tool stamps were taken on.
func agentTable(L *glua.LState, stream StreamSnapshot, now time.Time) *glua.LTable {
	at := L.CreateTable(0, 12)
	at.RawSetString("active", glua.LBool(stream.Active))
	at.RawSetString("model", glua.LString(stream.Model))
	at.RawSetString("text", glua.LString(stream.Text))
	at.RawSetString("reasoning", glua.LString(stream.Reasoning))
	at.RawSetString("thinking", glua.LBool(stream.Thinking))
	steering := L.CreateTable(len(stream.Steering), 0)
	for _, s := range stream.Steering {
		q := L.CreateTable(0, 3)
		q.RawSetString("text", glua.LString(s.Text))
		q.RawSetString("notice", glua.LBool(s.Notice))
		q.RawSetString("run", glua.LString(s.Run))
		steering.Append(q)
	}
	at.RawSetString("steering", steering)
	at.RawSetString("thought", glua.LNumber(stream.Thought.Seconds()))
	at.RawSetString("elapsed", glua.LNumber(stream.Elapsed.Seconds()))
	at.RawSetString("compacting", glua.LBool(stream.Compacting))
	at.RawSetString("checking_goal", glua.LBool(stream.CheckingGoal))
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
		row.RawSetString("job", glua.LString(t.Job))
		row.RawSetString("label", glua.LString(sanitize.String(t.Label)))
		if !t.Started.IsZero() {
			row.RawSetString("elapsed", glua.LNumber(now.Sub(t.Started).Seconds()))
		}
		row.RawSetString("diff", glua.LString(sanitize.String(t.Diff)))
		row.RawSetString("output", glua.LString(t.Output))
		toolsTb.Append(row)
	}
	at.RawSetString("tools", toolsTb)
	return at
}

// chatRow is one transcript row as ctx.transcript and ctx.printed_prev
// carry it.
func chatRow(L *glua.LState, m ChatMessage) *glua.LTable {
	row := L.CreateTable(0, 16)
	row.RawSetString("role", glua.LString(m.Role))
	row.RawSetString("kind", glua.LString(m.Kind))
	row.RawSetString("text", glua.LString(m.Text))
	row.RawSetString("tool", glua.LString(m.Tool))
	row.RawSetString("status", glua.LString(m.Status))
	row.RawSetString("detail", glua.LString(m.Detail))
	row.RawSetString("run", glua.LString(m.Run))
	row.RawSetString("job", glua.LString(m.Job))
	row.RawSetString("label", glua.LString(sanitize.String(m.Label)))
	row.RawSetString("diff", glua.LString(sanitize.String(m.Diff)))
	row.RawSetString("output", glua.LString(m.Output))
	if m.Stat != nil {
		row.RawSetString("in", glua.LNumber(m.Stat.In))
		row.RawSetString("out", glua.LNumber(m.Stat.Out))
		row.RawSetString("thought", glua.LNumber(float64(m.Stat.ThoughtMS)/1000))
		row.RawSetString("elapsed", glua.LNumber(float64(m.Stat.ElapsedMS)/1000))
		row.RawSetString("ended", glua.LNumber(m.Stat.Ended))
	}
	return row
}

// transcriptTable is ctx.transcript.
func transcriptTable(L *glua.LState, transcript []ChatMessage) *glua.LTable {
	tr := L.CreateTable(len(transcript), 0)
	for _, m := range transcript {
		tr.Append(chatRow(L, m))
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

// contextPct is how full the model's context window is, in percent; 0 when
// the window is unknown. Always a number, never a division.
func contextPct(lastIn, window int64) float64 {
	if window <= 0 {
		return 0
	}
	return float64(lastIn) / float64(window) * 100
}

// contextTable is the current-context estimate: the last prompt size
// against the model's context window, with the percent for the statusline.
func contextTable(L *glua.LState, s StreamSnapshot) *glua.LTable {
	c := L.CreateTable(0, 3)
	c.RawSetString("tokens", glua.LNumber(s.LastIn))
	c.RawSetString("window", glua.LNumber(s.CtxWindow))
	c.RawSetString("pct", glua.LNumber(contextPct(s.LastIn, s.CtxWindow)))
	return c
}

// overlay actions from the UI

// SelectOverlay runs the overlay's on_select with the chosen item. The UI
// closes the overlay first, so on_select may reopen one with the same id.
func (h *Host) SelectOverlay(ctx context.Context, o Overlay, index int) {
	var item any
	if index >= 0 && index < len(o.Items) {
		item = o.Items[index].raw
	}
	_ = h.callOverlay(ctx, o, "on_select", o.OnSelect, item)
}

// SubmitOverlayInput hands an input overlay's text to on_select.
func (h *Host) SubmitOverlayInput(ctx context.Context, o Overlay, text string) {
	_ = h.callOverlay(ctx, o, "on_select", o.OnSelect, text)
}

// staleOverlay reports an overlay left open across a reload: its callbacks
// belong to a retired LState, so they are dropped rather than called
// (DESIGN §4). Go-built overlays carry no generation.
func (h *Host) staleOverlay(o Overlay) bool {
	return o.Gen != 0 && o.Gen != h.currentGen()
}

// OverlayAction runs on_action with the item and the action chosen in its
// detail view. Lua gets the row back as the table it wrote.
func (h *Host) OverlayAction(ctx context.Context, o Overlay, item OverlayItem, action any) {
	_ = h.callOverlay(ctx, o, "on_action", o.OnAction, item.raw, action)
}

// OverlayKey runs a list-level key's function with the selected item.
func (h *Host) OverlayKey(ctx context.Context, o Overlay, k OverlayKey, item OverlayItem) {
	_ = h.callOverlay(ctx, o, "key "+k.Key, k.Fn, item.raw)
}

// CancelOverlay runs on_close if present.
func (h *Host) CancelOverlay(ctx context.Context, o Overlay) {
	_ = h.callOverlay(ctx, o, "on_close", o.OnClose)
}

// callOverlay runs one of an overlay's callbacks as a task under the
// overlay's origin; a failure goes to /errors, and the first slice's to the
// caller too.
func (h *Host) callOverlay(ctx context.Context, o Overlay, what string, fn *glua.LFunction, args ...any) error {
	if fn == nil || h.staleOverlay(o) {
		return nil
	}
	failed := func(err error) error {
		if err != nil && !lua.Dropped(err) {
			h.Log.Error("overlay "+what+" failed", "origin", "ui", "id", o.ID, "error", err)
			h.RecordError("overlay "+o.ID+" "+what, err.Error())
		}
		return err
	}
	return failed(h.RT.StartTask(ctx, h.luaTask(lua.ClassUI, luaCaller{gen: o.Gen, origin: o.Origin, path: o.Path}, fn, args...), func(err error) {
		h.refresh()
		_ = failed(err)
	}))
}

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
	h.Providers.Reset(*h.Builtin.Load())
	if !h.Inert {
		h.syncSkills()
		h.syncTypes()
		h.GoSafe("git status", func() { h.gitStatus() })
	}

	g.SB.SetGuard(h.guardLua)
	pairs := []struct {
		name string
		tbl  *glua.LTable
		call *glua.LFunction
	}{
		{"zemi.config", h.configNamespace(L), nil},
		{"zemi.keymap", h.keymapNamespace(L), nil},
		{"zemi.command", h.commandNamespace(L), nil},
		{"zemi.event", h.eventNamespace(L), nil},
		{"zemi.log", h.logNamespace(L), callable(L, h.logFn(slog.LevelInfo))},
		{"zemi.notify", h.notifyNamespace(L), callable(L, h.notifyFn(slog.LevelInfo))},
		{"zemi.ui", h.uiNamespace(L), nil},
		{"zemi.env", h.envNamespace(L), nil},
		{"zemi.state", h.stateNamespace(L), nil},
		{"zemi.timer", h.timerNamespace(L), nil},
		{"zemi.fs", h.fsNamespace(L), nil},
		{"zemi.http", h.httpNamespace(L), nil},
		{"zemi.job", h.jobNamespace(L), nil},
		{"zemi.tool", h.toolNamespace(L), nil},
		{"zemi.perm", h.permNamespace(L), nil},
		{"zemi.provider", h.providerNamespace(L), nil},
		{"zemi.model", h.modelNamespace(L), nil},
		{"zemi.classify", h.classifyNamespace(L), nil},
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
		if err := g.SB.RegisterNamespace(p.name, p.tbl, p.call); err != nil {
			return err
		}
	}
	api := L.NewTable()
	api.RawSetString("version", glua.LString("0.132.0"))
	return g.SB.RegisterProtectedValue("zemi.api", api)
}

func (h *Host) currentGen() Gen { return Gen(h.gen.Load()) }

// SetReload installs main's reload and its check for config that moved on
// disk since the last one. Call it before boot.
func (h *Host) SetReload(reload func() error, changed func() bool) {
	h.reload, h.changed = reload, changed
}

// Reload runs the reload now, for callers off the interpreter (the typed
// /reload, the stuck pill), which must not wait on a wedged one.
func (h *Host) Reload() {
	if h.reload != nil {
		_ = h.reload()
	}
}

// Retrust sets the trust /trust decided and reloads when it changed. It
// holds the watcher's lock, so the watcher cannot reload first for the
// config trees the change moved; set before the reload, a boot that fails
// before the trust step does not leave the old decision in force.
func (h *Host) Retrust(ok bool) {
	h.reloadCheck.Lock()
	defer h.reloadCheck.Unlock()
	was := h.ProjectTrusted()
	h.SetProjectTrusted(ok)
	if ok != was && h.reload != nil {
		_ = h.reload()
	}
}

// ReloadConfig runs the /reload command (palette, keymaps,
// zemi.command.run), so config zemi wrote for itself takes effect without a
// restart.
func (h *Host) ReloadConfig() error {
	// config that reloads while it is loading would replay itself forever
	if h.RT.Booting() {
		return errors.New("config is still loading; a reload from config would repeat forever")
	}
	// headless, the process would end before the reload ran
	if _, err := h.Screen(); err != nil {
		return err
	}
	if h.reload == nil {
		return errors.New("nothing to reload in this run")
	}
	h.GoSafe("reload", func() {
		// a reload cancels what the interpreter runs, which is the caller when
		// Lua asked for it: wait for it to return (a wedged one times out)
		_, _ = h.RT.Do(context.Background(), lua.ClassIO, 0, 0, func(*glua.LState) ([]any, error) { return nil, nil })
		_ = h.reload()
	})
	return nil
}

// ReloadIfChanged reloads when config on disk moved since the last reload
// and says how it went: "" when nothing ran, else a first line and one
// error line per boot step that failed.
func (h *Host) ReloadIfChanged() string {
	h.reloadCheck.Lock()
	defer h.reloadCheck.Unlock()
	if h.reload == nil || h.RT.Booting() || !h.changed() {
		return ""
	}
	t, gen := h.now(), h.currentGen()
	err := h.reload()
	if err != nil && h.currentGen() == gen {
		return "(the changed config did not load; the old one still runs)\nerror: " + Headline(err.Error())
	}
	errs := h.errorsSince(t, "boot ")
	// a failed step records itself; a replay that stopped may name no step
	if err != nil && !slices.ContainsFunc(errs, func(e string) bool { return strings.HasSuffix(e, ": "+Headline(err.Error())) }) {
		errs = append(errs, "error: "+Headline(err.Error()))
	}
	if len(errs) == 0 {
		return "(zemi reloaded the changed config)"
	}
	return fmt.Sprintf("(zemi reloaded the changed config; %d %s)\n%s", len(errs), Plural(len(errs), "error", "errors"), strings.Join(errs, "\n"))
}

// ForceReload replays the boot steps on a fresh generation (DESIGN §4).
// Until it returns, tool vetoes wait (awaitReload) and the old generation's
// veto handlers stay in force, so a running turn is never checked against half a config.
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
	h.lspStopChanged()
	if err == nil && h.InitError() != "" {
		err = errors.New(h.InitError())
	}
	if err == nil {
		h.dropPluginKeys()
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
			return lua.SyntaxErr(perm.TildePath(p), string(src), pe)
		}
	}
	return nil
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

// outsideReload runs a tool veto, or a read of the config's modes, until no
// reload overlapped it. Mid-reload the handlers and modes are purged or not
// yet replayed: a veto with no handler is no objection and the mode falls
// back to auto, so the user's denials would be skipped.
func (h *Host) outsideReload(fn func()) {
	seq := h.awaitReload()
	for {
		fn()
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
	h.agents.targetsMu.Lock()
	clear(h.agents.targets)
	h.agents.targetsMu.Unlock()
	h.Keymaps.Purge()
	h.Commands.Purge()
	h.Widgets.Purge()
	h.Events.Purge()
	h.Themes.Purge()
	h.purgeWatches()
	h.hookReg.purge(h.currentGen())
	h.healthReg.purge()
	h.completions.purge()
	h.luaTools.purge(h.currentGen())
	h.stops.purge()
	h.mcp.park()
	h.lsp.forget()
	h.instrStale.Store(true)
	// An overlay left open would keep drawing with callbacks that are now
	// dropped (staleOverlay), which reads as a frozen picker.
	if ui := h.hooks(); ui != nil {
		ui.CloseLuaOverlays()
	}
}

// Close ends the host: the runtime, what its generation started (timers,
// jobs, requests, servers) and the stores, so it holds no file or goroutine.
func (h *Host) Close() {
	h.RT.Close()
	h.PurgeGeneration()
	h.mcp.stopParked()
	h.lsp.stopAll()
	for _, db := range []*store.DB{h.DB, h.GlobalDB} {
		if db != nil {
			db.Close()
		}
	}
}

type turnScope struct {
	ctx    context.Context
	cancel context.CancelFunc
}
