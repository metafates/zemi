package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// UIHooks is what the host API needs from the UI layer. Implemented by
// internal/ui; hostapi never imports the UI.
type UIHooks interface {
	Notify(level slog.Level, title, body string)
	// Alert raises what Notify does plus, while the window is unfocused, a
	// one-time notification (Host.Alert).
	Alert(level slog.Level, title, body string)
	OpenOverlay(o Overlay)
	// RefreshOverlay replaces the open overlay id with what build returns,
	// calling build only while it is open: for a screen a clock refreshes.
	RefreshOverlay(id string, build func() Overlay)
	// CloseOverlay closes an overlay and runs its on_close. by is who asks:
	// builtin for Go code, the running origin for zemi.ui.overlay.close.
	CloseOverlay(id string, by lua.Origin, byPath string)
	// CloseLuaOverlays drops every overlay the interpreter owns (Gen != 0).
	CloseLuaOverlays()
	Refresh()
	// Scroll moves the chat, as the zemi.ui.scroll.* host actions do.
	Scroll(kind ScrollAction)
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
	// DismissAsk answers the shown permission ask as nobody: the chat it
	// belonged to is gone (the writer's reset), the waiter unblocks at once.
	DismissAsk()
	// Screenshot is the last drawn frame as plain text, "" before the
	// first one; safe from any goroutine.
	Screenshot() string
	AskQuestion(qs []tools.Question) (answers []tools.Answer, ok bool)
	// AskPlan shows a plan for approval; ok is false when the user dismisses it.
	AskPlan(plan string) (tools.PlanVerdict, bool)
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
	// Answered is the Go-side hook a blocking ask (zemi.ui.select/input)
	// waits on: the host calls it with the item or the text, nil on cancel,
	// when the overlay is answered. Lua's own callbacks are separate.
	Answered func(v any)
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
	// KeepOrder marks a list whose order is the meaning (a transcript
	// browser): a filter keeps Match's row order instead of ranking it, and
	// matches the rows, not the section metadata above them
	KeepOrder bool
	// Phases heads each section with its rows' combined mark: failed,
	// running, waiting, stopped or done
	Phases bool
	// Follow makes the list move the stage's scrolled region to the
	// transcript row each item names by its anchor, as the selection moves,
	// and band that message's rows while it is open: enter keeps the place,
	// esc puts the region back where it found it
	Follow bool
	// CloseOnCopy makes a copy act (enter on a copy row, y) close the
	// overlay after it copies: a picker whose rows are only values, where
	// copying is the pick (/copy). Other lists stay open to copy again.
	CloseOnCopy bool

	// Input makes the overlay a one-line text prompt (zemi.ui.input) that
	// starts with Default; OnSelect receives the text. A Go screen's prompt
	// sets Run instead, a command the text is appended to; Lua never sets
	// it, and a Go screen holding a secret sets OnInput, which receives the
	// text off the command path. Placeholder shows dim in the empty line,
	// what submitting it would mean; Secret draws a bullet per rune, the
	// only kind of input whose text may never echo.
	Input       bool
	Default     string
	Placeholder string
	Run         string
	Secret      bool
	OnInput     func(text string) error // Go-only: what a secret's submit runs, the box reopened with Default on its error
	Note        string                  // dim under an input's line, where submitting it puts the text

	// Reload makes a list a toggle list: enter and space run the row's "run"
	// command or on_select and rebuild the items, keeping the overlay open.
	// It runs on the UI worker, never on the key path.
	Reload func() []OverlayItem

	// More loads a browsing list's older rows when the reader pages up past
	// its first one; ok false when nothing older is left. It runs on the UI
	// worker, never on the key path, and the list is replaced with what it
	// returns.
	More func() (Overlay, bool)

	// Foot is the stats line a Go screen draws ahead of its footer's keys:
	// live numbers a rebuild refreshes, never keys.
	Foot string

	// Next and Prev are the commands that open the next and the previous
	// view on the same rail in this one's place (ctrl+j, ctrl+k), so walking
	// a turn's calls takes no trip back to the chat.
	Next, Prev string
}

// ReadsOnly reports a reading box - an answer or a report drawn from its
// body, with no rows to act on and no input of its own. Its keys are esc and
// the scroll keys; typing belongs to the composer under it.
func (o Overlay) ReadsOnly() bool { return o.Body != "" && len(o.Items) == 0 && o.Run == "" }

// ServiceStatus is where an MCP server, a language server or a plugin
// stands: the status word of /mcp, /lsp, /plugin, /health and zemi.*.list.
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

// _serviceStatuses pins runtime/types/zemi.lua's zemi.ServiceStatus.
var _serviceStatuses = []ServiceStatus{StatusIdle, StatusStarting, StatusReady, StatusFailed, StatusSignIn, StatusStopped, StatusOff}

// OverlayAnchor puts an overlay in line with the composer frame, by the
// pill that opened it; "" centers it.
type OverlayAnchor string

const (
	OverlayBottomLeft  OverlayAnchor = "bottom-left"
	OverlayBottomRight OverlayAnchor = "bottom-right"
)

var _overlayAnchors = []OverlayAnchor{OverlayBottomLeft, OverlayBottomRight}

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

var _scrollActions = []ScrollAction{ScrollUp, ScrollDown, ScrollTop, ScrollBottom, ScrollLineUp, ScrollLineDown, ScrollStickyUp, ScrollStickyDown}

// RowKind is a transcript row's shape. RowReasoning is a finished thinking
// block the chat widget collapses to a one-line summary.
type RowKind string

const (
	RowMessage   RowKind = ""
	RowReasoning RowKind = "reasoning"
	RowTool      RowKind = "tool"
	RowShell     RowKind = "shell" // a shell command row and its output
	RowNotice    RowKind = "notice"
	RowError     RowKind = "error"
)

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

// Mark is a row's state mark: a glyph one cell wide and its theme group.
type Mark struct{ Glyph, HL string }

// MarkKind names a state mark: a key of Marks, and the word a row or Lua
// passes to ask for one. Job.Mark speaks it too, so the words live where its
// jobs are made (internal/tools).
type MarkKind = tools.MarkKind

// Marks are the state marks every row shares, the chat's calls and the
// lists' runs alike (zemi.ui.marks), one glyph and one meaning each:
// running, finished, finished with a failure inside, failed, stopped by the
// user, going on in the background, waiting, reasoning and a notice.
var Marks = map[MarkKind]Mark{
	tools.MarkSpin: {Spinner[0], "Dim"}, tools.MarkOK: {"✓", "Ok"}, tools.MarkWarn: {"!", "Warn"}, tools.MarkErr: {"✗", "Danger"}, tools.MarkStop: {"■", "Dim"},
	tools.MarkBG: {"⧗", "Dim"}, tools.MarkWait: {"…", "Dim"}, tools.MarkThought: {"✻", "ThinkingMark"}, tools.MarkNote: {"◦", "Dim"},
}

// Spinner is the running mark's frames, one per frame tick.
var Spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// _projectEmoji pools the emoji a project's marker is picked from: one
// codepoint, emoji presentation (colored without VS16), East Asian Width W
// by UAX #11 (so zemi.ui.width and the terminal agree on 2 cells), Emoji 11.0
// or older so every font that ships emoji has them, and distinct and
// neutral: no faces, hands, people, flags or symbols the UI draws. The pool
// test pins the rest; the entries were curated against Unicode's
// emoji-variants data.
var _projectEmoji = []string{
	"🌊", "💧", "☔", "⛅", "🌈", "🌙", "⭐", "🌍", "🌵", "🌱",
	"🍀", "🍄", "🌻", "🦋", "🐢", "🐙", "🐳", "🐧", "🐚", "📎",
	"📌", "🔖", "📚", "📦", "🔑", "🔨", "🧰", "🧩", "🧭", "💡",
	"💎", "🔮", "🎲", "🎁", "🎨", "🚀", "⚓", "☕", "🍕", "💾",
	"🦊", "🦉", "🐝", "🐬", "🦀", "🦑", "🦎", "🦕", "🐘", "🦔",
	"🦜", "🐠", "🌲", "🌳", "🍁", "🌷", "🌹", "🌸", "🌺", "🍒",
	"🍋", "🥑", "🥕", "🍞", "🧀", "🍩", "🍿", "🔭", "🔬", "🧪",
	"🧬", "🧲", "🛸", "🎸", "🧸", "🧵", "🎯", "🗻", "🏰", "🎡",
	"🌠", "🌌", "🌄", "🌋", "🌴", "🌾", "🌿", "🍂", "🍃", "🌼",
	"💐", "🐇", "🐹", "🐄", "🐑", "🐐", "🐎", "🐅", "🐍", "🦓",
	"🦒", "🦌", "🦖", "🦆", "🦅", "🦢", "🦚", "🦈", "🦘", "🐡",
	"🍇", "🍈", "🍉", "🍊", "🍎", "🍐", "🍓", "🌽", "🌰", "🍅",
	"🥝", "🥔", "🥥", "🥭", "🥜", "🥐", "🥨", "🥖", "🥞", "🥟",
	"🥯", "🥮", "🍪", "🍰", "🧁", "🍫", "🍬", "🍭", "🍯", "🍵",
	"📐", "📏", "🧮", "🧺", "🧶", "🧷", "🧿", "🧱", "🏮", "🎒",
	"🎹", "🥁", "🎪", "🎋", "🎈", "🎠", "🎢", "🚂", "🛶", "🚁",
	"🔦", "🗼", "🐾",
}

// ProjectMark derives a project's marker emoji from its root: stable across
// restart, zero storage, and not user-changeable. An empty root (no
// repository) has no marker.
func ProjectMark(root string) string {
	if root == "" {
		return ""
	}
	h := uint32(2166136261)
	for i := range len(root) {
		h = (h ^ uint32(root[i])) * 16777619
	}
	return _projectEmoji[h%uint32(len(_projectEmoji))]
}

// markedTitle is the marker-prefixed title the project surfaces draw: the
// same shape everywhere a project row names itself, clipped as a row shows it.
func markedTitle(root, title string) string {
	return ProjectMark(root) + " " + clipRunes(title, 60)
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

// NoSeq marks a transcript row the log holds no message for: a live-only
// notice, a run row, a live row the stamping has not named yet. The log's
// own first message is seq 0, so 0 is a real seq like any other.
const NoSeq int64 = -1

// ChatMessage is a transcript entry (composer submissions); assistant
// replies carry the turn stats the chat widget renders.
type ChatMessage struct {
	Role   fantasy.MessageRole
	Kind   RowKind
	Text   string // tool rows: the input summary
	Label  string // tool rows: the model's description of a command
	Tool   tools.ToolName
	Status CallStatus // tool and shell rows; an error row an interrupt left is CallCancelled
	Detail string     // tool rows: subagent or workflow progress summary; notice rows: a hint drawn after the text, never copied
	Tail   string     // a running shell row: its output tail, which its view reads as it grows
	Job    string     // shell and job_start rows: the job the call handed the work to
	Run    string     // agent tool rows: the run id
	Prompt string     // agent tool rows: the message a send_to sent a run, whole (the row's summary clips it)
	Diff   string     // file tool rows: the applied change as "- "/"+ " lines
	Output string     // tool rows: the last lines of the result (shortOutput)
	Stat   *TurnStat
	CallID string // tool rows: the stream's call id, so a report names a call once
	// Seq is the row's log message: the /memories jump names it, and a user
	// row's is where a reset cuts. NoSeq for a row the log holds no
	// message for (a live-only notice, a run row).
	Seq    int64
	ID     string // a run row's stored message id (its transcript rebuilds from the store)
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

// readMoreLines reads that marker back: the count it names, 0 when the line
// is not one.
func readMoreLines(line string) int {
	rest, ok := strings.CutPrefix(line, "… ")
	if !ok {
		return 0
	}
	rest, ok = strings.CutSuffix(rest, " more lines")
	if !ok {
		if rest, ok = strings.CutSuffix(rest, " more line"); !ok {
			return 0
		}
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// rowOutput is a tool result as its transcript row shows it, else
// shortOutput: a plan's verdict and an answered question call's
// "question\tanswer\tnote" lines, since their results are written for the model.
func rowOutput(tool, out string) string {
	switch {
	case tools.ToolName(tool) == tools.ToolPlan:
		verdict, path := tools.PlanOutcome(out)
		if out = verdict; path != "" {
			out += ", saved to " + perm.TildePath(path)
		}
	case tools.ToolName(tool) == tools.ToolQuestion:
		if qa := tools.QuestionAnswers(out); qa != nil {
			line := func(s string) string { return clipLine(s, 300) }
			rows := make([]string, len(qa))
			for i, p := range qa {
				rows[i] = line(p[0]) + "\t" + cmp.Or(line(p[1]), "not answered") + "\t" + line(p[2])
			}
			return strings.Join(rows, "\n")
		}
	case tools.ToolName(tool) == tools.ToolDate:
		// the model reads the full RFC3339Nano stamp; the row reads the
		// compact local one. Already-compacted text fails the parse and
		// passes through, so the rebuild is idempotent.
		if at, err := time.Parse(time.RFC3339Nano, out); err == nil {
			out = at.Local().Format(_previewStamp)
		}
	}
	return shortOutput(out)
}

// shortOutput keeps 10 lines of a tool result for the transcript row, each
// capped at 300 runes and sanitized: the first three, which say what went
// wrong, and the last six, with a row counting what fell out between them.
func shortOutput(s string) string {
	s = tools.StripReminder(s) // the reminder is the model's, the row never shows it
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
// time (thought = time spent in reasoning blocks). Its JSON names are the
// reply row's stored readout: the one shape a finished turn rides out and a
// rebuilt chat reads back.
type TurnStat struct {
	In        int64  `json:"-"` // not on the wire: the log's readout carries no input count
	Out       int64  `json:"out"`
	ThoughtMS int64  `json:"thought_ms"`
	ElapsedMS int64  `json:"turn_ms"`
	Ended     int64  `json:"ended"` // unix seconds the turn ended, 0 when unknown
	Model     string `json:"model"` // the model id that wrote the reply, "" elsewhere
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
	Builtin atomic.Pointer[agent.Catalog]
	// storedKeysPass marks a stored-key pass in flight, so stacked reloads
	// do not exec the keychain twice over.
	storedKeysPass atomic.Bool
	DB             *store.DB
	GlobalDB       *store.DB

	Keymaps  *KeymapRegistry
	Commands *CommandRegistry

	Events *EventRegistry

	// initErr surfaces store-open failures at the namespaces boot step.
	initErr error

	// chat is the eternal log the main conversation writes and reads: the
	// appends and the builder worker over the project store, ended by Close.
	chat *chatLog

	// resumeStop cancels the queued reports' idle drain nextTurn armed;
	// guarded by agents.mu with the queue it works on.
	resumeStop func()

	uiMu sync.RWMutex
	ui   UIHooks

	gen atomic.Int64

	tMu      sync.RWMutex // transcript lock; agents.mu is always taken first, never under this one
	session  *Session
	view     *agentRun // the agent run the chat shows instead of the session, nil for none
	viewBack string    // the command that reopens the screen view came from
	viewMenu string    // the overlay left open under view, whose esc goes to the view

	// levelMu guards the overlay levels a transcript that grows rebuilds
	// where they are open (the memories browser), the call view on top of
	// them, and mcp's open group level: a server's state rebuilds its level
	levelMu   sync.Mutex
	tracked   []string
	mcpLevel  string // the group level /mcp opened last, "" for none
	callRow   int    // the row the open call view shows
	callID    string // that call's id, which names the row it settles at
	callServe int    // the row the live call view's watcher serves, so an
	// opened view supersedes the last watcher instead of adding one

	// stampFrom is the log seq the transcript's seq stamping pairs from, and
	// stampRow the window row it pairs at: rows before it are claimed
	// (stampLogSeqs). Both move only under tMu.
	stampFrom int64
	stampRow  int

	// trEpoch changes whenever the transcript is replaced (new, resume,
	// reset), so the chat widget can cache the rows of settled messages.
	trEpoch atomic.Int64
	// trEdit changes when a settled row's text grew in place (a joined
	// reasoning chunk), which re-renders every cached unit: trEpoch would also
	// drop the rows the reader opened.
	trEdit atomic.Int64
	// trCut counts the rows the window's head has shed since boot. A trim
	// moves every row down by one delta, which a key shift absorbs, so it
	// must not bump trEpoch: that would re-render the whole window per flush
	// for the rest of a long chat. A whole replacement leaves it; the chat
	// re-adopts it when the epoch hands it a new window.
	trCut atomic.Int64
	// trHeadNote says the log opens with the reset's own note: the transcript
	// drops it, and the head count must not name it an earlier message.
	trHeadNote atomic.Bool

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
	// KeepPicks lets a runtime pick outlive the process; launch turns it on
	// once the TUI's flags applied, so nothing else keeps one.
	KeepPicks atomic.Bool
	// sessionStartDone: the plugins' SessionStart hooks capture once, at the
	// first send.
	sessionStartDone atomic.Bool
	// agentsView is the agents view's one ticker claim (watchAgents).
	agentsView atomic.Bool
	agents     *agentHost
	luaTools   *toolReg
	stops      *stopReg
	// test is set, before boot, only in a zemi plugin test zemi
	test *PluginTest
	// goes counts the host goroutines in flight, which Close joins
	goes goes
	// closed ends the overlay tickers, which otherwise tick until the thing
	// they watch stops moving; a closed host runs no service
	closed    chan struct{}
	closeOnce sync.Once
	// cutMu orders a reset's wipe against a detached run's writes, so a run
	// never stores what it read from rows the wipe removed.
	cutMu    sync.Mutex
	initFail atomic.Pointer[string]                    // headline of the user init.lua failure this boot recorded
	modelFn  func() (fantasy.LanguageModel, error)     // stubs every model in tests
	authFn   func(context.Context, func(string)) error // stubs a sign-in flow in tests
	browser  func(string)                              // stubs the sign-in browser in tests

	busyMu sync.RWMutex
	busy   []*busyJob // user-visible jobs that are not a turn, oldest first

	md markdownCache

	selectSeq atomic.Int64 // ids for overlays each call opens anew (select, input, /btw)

	errs  errLog   // recent runtime errors for /errors
	msgs  msgLog   // recent print and zemi.log lines, for /logs
	evlog eventLog // recent events and notifications for Lua reflection

	pluginLoads atomic.Pointer[[]pluginLoad] // the last boot's enabled plugins, for /health and /plugin
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
	// builtinSkillsOK says the embedded skills are on disk; they never
	// change within a process, so they are written once no matter how
	// often skills sync, and retried when the write failed
	builtinSkillsOK atomic.Bool

	// healthReg holds the /health checks Lua registered.
	healthReg healthReg
	// completions holds the zemi.composer.add sources
	completions completeReg

	hostActions map[string]func(h *Host, args string) error

	// luaCeiling bounds each slice of a Lua tool run on the interpreter and
	// toolPostCeiling one tool.post handler's slice: both measure wall time,
	// so a loaded machine can trip them and a test moves them to reach the
	// bound without waiting.
	luaCeiling      time.Duration
	toolPostCeiling time.Duration

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

	recentStale atomic.Bool

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

	title atomic.Pointer[titleSet] // zemi.ui.title's; nil names the tab after the session

	// reloadDone is non-nil and open while reloads run; reloadSeq counts
	// reloads started (see awaitReload). reloadRun keeps one reload's sweep
	// of the dead generation's registrations from racing the next one's replay.
	reloadMu   sync.RWMutex
	reloadRun  sync.Mutex
	reloads    int
	reloadSeq  uint64
	reloadDone chan struct{}
	// reload and changed are main's (SetReload): a reload that moves the
	// baseline changed compares against, set before boot and never after;
	// configMark is the baseline reset itself (SetConfigMark)
	reload      func() error
	changed     func() bool
	configMark  func()
	reloadCheck sync.Mutex

	// projectTrusted is the trust boot step's decision (DESIGN §5): project
	// instructions and language servers load only when it is set,
	// and reads reach less without it.
	projectTrusted atomic.Bool
	trustDropped   atomic.Bool // trust went off: the next reload must run to drop the project's files
	// trustRoot is the repository the session belongs to (a worktree maps to
	// its main checkout), found once: it keys the store and the scratchpad,
	// and finding it walks the disk. project is the same
	// path ~-shortened, which widgets read every frame. repoRoot is the
	// un-resolved checkout, which the HEAD and prompt paths read; the walk
	// behind it stats every ancestor, so the statusline's render cannot pay it.
	trustRoot func() string
	project   func() string
	repoRoot  func() string
	// branchMu guards the HEAD cache the statusline reads every render; a
	// branch switch rewrites HEAD, whose stat keys it
	branchMu  sync.Mutex
	branchKey headKey
	branch    string
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

// NewHost wires the registries and opens the stores (best effort:
// failures surface at the namespaces boot step).
func NewHost(rt *lua.Runtime, log *slog.Logger, settings *config.Store, dirs config.Dirs) *Host {
	h := &Host{
		RT:        rt,
		Log:       log,
		Settings:  settings,
		Dirs:      dirs,
		Stderr:    os.Stderr,
		Keymaps:   &KeymapRegistry{},
		Commands:  &CommandRegistry{},
		Events:    &EventRegistry{},
		session:   &Session{CWD: dirs.ProjectDir},
		watches:   &watchReg{},
		hookReg:   &hookReg{},
		Providers: &agent.Registry{},
		agents:    newAgentHost(rt.Clock()),
		luaTools:  &toolReg{},
		stops:     &stopReg{},
		jobs:      &tools.Jobs{},
		skillsReg: &skillReg{},
		mcp:       &mcpReg{},
		tasks:     newTaskReg(settings),
		lsp:       &lspReg{},
		closed:    make(chan struct{}),

		luaCeiling:      10 * time.Second,
		toolPostCeiling: lua.VetoDeadline,
	}
	if dirs.StateHome != "" {
		// the rate limiters share their buckets with every other zemi
		// process of the user, so parallel windows pace one quota
		h.Providers.PaceDir = filepath.Join(dirs.StateHome, "pace")
	}
	h.Builtin.Store(&agent.Catalog{})
	h.Keymaps.owner, h.Commands.owner, h.Events.owner, h.luaTools.owner, h.watches.owner = h.PluginOf, h.PluginOf, h.PluginOf, h.PluginOf, h.PluginOf
	h.jobs.OnChange = func() {
		h.refresh()
		h.watchJobs()
	}
	settings.SetChoices(config.KeyAgentEffort, append(slices.Clone(agent.Efforts), ""))
	h.trustRoot = sync.OnceValue(h.Dirs.TrustRoot)
	h.project = sync.OnceValue(func() string { return perm.TildePath(h.trustRoot()) })
	h.repoRoot = sync.OnceValue(h.Dirs.RepoRoot)
	h.pluginsRoot = sync.OnceValue(func() string {
		root := dirs.PluginsRoot()
		// RealPath, not EvalSymlinks: the memo may run before the root exists
		// (a first-run tree, a test), and a raw memo would then never match
		// the resolved paths chunks record
		if r := perm.RealPath(root); r != "" {
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
	if h.DB != nil {
		// the window names itself in its log rows, so a restart tells its rows
		// from the ones an older boot wrote
		h.DB.Origin = originID()
		h.chat = newChatLog(context.Background(), h)
		// the window opens on the log's tail: the chat that never ends has
		// no resume, only more of itself. The tail is read with slack so
		// its head can sit on a turn start, not an answer's shed remains.
		if rows, err := h.DB.LogTail(_chatWindowRows * 3); err == nil {
			h.trHeadNote.Store(len(rows) > 0 && systemNote(rows[0]))
			h.session.Transcript = alignTrim(logRows(h.relPath, rows))
			h.stampReset(h.session.Transcript)
			h.markDeadTail()
		}
		h.loadGoal()
		// the meter rides the stored context of the latest step: a resume
		// shows it before the first new turn bills its own
		if s, err := h.DB.GetSession(store.MainSessionID); err == nil {
			h.agents.mu.Lock()
			h.agents.lastIn = s.LastIn
			h.agents.mu.Unlock()
		}
	}
	h.picked, h.pluginKeys = map[config.Key]any{}, map[config.Key]Gen{}
	if h.GlobalDB != nil {
		h.loadPicks()
	}
	h.hostActions = map[string]func(h *Host, args string) error{
		"zemi.ui.help":    func(h *Host, _ string) error { return h.OpenHelp() },
		"zemi.model.pick": func(h *Host, _ string) error { return h.OpenModelPicker(_defaultRole) },
		"zemi.reload":     nil, // Go-reserved; never dispatched here
	}
	for _, a := range _scrollActions {
		h.hostActions["zemi.ui.scroll."+string(a)] = func(h *Host, _ string) error { h.scroll(a); return nil }
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
	h.recentStale.Store(true)
	h.trEpoch.Store(1) // 0 means "an agent run's transcript; do not cache"
	// the saved decision lands before the first frame draws: the boot's trust
	// step still asks when none is saved, but a decided project never flashes
	// the statusline's untrusted pill
	if v, ok := h.Dirs.ReadTrust()[h.trustRoot()]; ok {
		h.SetProjectTrusted(v == config.Trusted)
	}
	return h
}

// SetProjectTrusted records the trust decision and rescans the project's
// skills.
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
		L.Push(glua.LString(h.repoRoot()))
		return 1
	}))
	return tb
}

// Session is the session state (cwd, the transcript window).
type Session struct {
	CWD        string
	Transcript []ChatMessage
}

// _chatWindowRows is how many settled rows the live transcript holds: the
// log is eternal, the chat's working set is not. Older rows stay reachable
// through zoom and the paged store reads.
const _chatWindowRows = 1000

// _stampBatch bounds one read of the log rows the seq stamping pairs.
const _stampBatch = 256

// appendTranscript appends rows to the window and trims its head once it
// passes chatWindowRows. Replaced whole, like setTranscript: readers share
// the old array and the chat widget caches rows by index. Rows appended
// live never carry a log seq; the stamping names them from the log.
func (h *Host) appendTranscript(rows ...ChatMessage) {
	if len(rows) == 0 {
		return
	}
	for i := range rows {
		if rows[i].Seq == 0 { // the zero value names nothing, like NoSeq
			rows[i].Seq = NoSeq
		}
	}
	h.tMu.Lock()
	h.session.Transcript = h.trimTranscript(append(h.session.Transcript, rows...))
	h.tMu.Unlock()
}

// trimTranscript clips a window grown past chatWindowRows to the bound,
// tMu held: the shed head advances trCut, the delta the chat's key shift
// absorbs so no wipe is needed, and the pairing's row moves down with it.
// NOTE: a shed deeper than the clip (alignTrim's turn start) can cut past
// the pairing cursor - rows shed unpaired between stampRow and the cut,
// a text-coincidence later would stamp one with another row's seq; that
// needs a thousand unpaired rows between stamps, a pathological window.
func (h *Host) trimTranscript(tr []ChatMessage) []ChatMessage {
	kept := alignTrim(tr)
	if cut := len(tr) - len(kept); cut > 0 {
		tr = slices.Clone(kept)
		h.trCut.Add(int64(cut))
		h.stampCursor(cut)
	}
	return tr
}

// alignTrim clips tr to its last chatWindowRows rows, then sheds further
// until the head sits on a row that opens a turn, so the window's oldest
// row never reads as an answer whose question was shed. Standalone notices
// and errors after a turn's reply hang to that turn and go with it. A run
// with no turn start after the clip keeps the plain clip instead: the shed
// never empties the window, and a turn longer than the clip's slack does
// the same by starting at its own first row.
func alignTrim(tr []ChatMessage) []ChatMessage {
	if len(tr) <= _chatWindowRows {
		return tr
	}
	cut := len(tr) - _chatWindowRows
	for cut < len(tr) && !turnStart(tr[cut]) {
		cut++
	}
	if cut >= len(tr) { // no turn start in the kept span: the plain clip wins
		cut = len(tr) - _chatWindowRows
	}
	return tr[cut:]
}

// turnStart reports whether row opens a turn: a user message or a shell
// command. Assistant rows, tool rows and notices hang under one.
func turnStart(row ChatMessage) bool {
	return row.Kind == RowShell || row.Kind == RowMessage && row.Role == fantasy.MessageRoleUser
}

// markDeadTail is the boot read of the dangling check: the run before this
// one may have died inside the very turn the tail shows dangling.
func (h *Host) markDeadTail() {
	h.markDanglingTail()
}

// markDanglingTail terminates the tail a window died inside: a turn whose rows
// hold no reply and no error row never ended — the window closed on it
// (a kill, a quit, a crash) — and the view says so, the way an interrupt's
// "■ Interrupted" does. The log stays as it is, and the next turn's rows make
// the check pass.
func (h *Host) markDanglingTail() {
	h.tMu.Lock()
	tr := h.session.Transcript
	start := -1
	for i, m := range slices.Backward(tr) {
		if turnStart(m) {
			start = i
			break
		}
	}
	dangling := false
	if start >= 0 {
		dangling = true
		for _, m := range tr[start+1:] {
			if m.Kind == RowError || m.Kind == RowMessage && m.Role == fantasy.MessageRoleAssistant {
				dangling = false
				break
			}
		}
	}
	if dangling {
		h.session.Transcript = append(tr, ChatMessage{
			Role: fantasy.MessageRoleAssistant, Kind: RowError,
			Text: "the window closed mid-turn", Status: CallCancelled, Seq: NoSeq,
		})
		h.trEpoch.Add(1)
	}
	h.tMu.Unlock()
	if !dangling {
		return
	}
	// the turn died without a release: the item it took up goes back to pending
	h.GoSafe("settle todos", h.settleTodos)
}

// TempDir is a directory for the host's own scratch files (the editor's
// draft file, a warm diff index).
func (h *Host) TempDir() (string, error) {
	if h.Dirs.StateHome == "" {
		return "", errors.New("no state dir for temp files")
	}
	dir := filepath.Join(h.Dirs.StateHome, "tmp")
	return dir, os.MkdirAll(dir, 0o700)
}

// originID is a window's writer id for the log rows it writes: two windows
// on one repository pair their live transcripts against the same log, and
// each must tell the other's rows from its own.
func originID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("w%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// now is the time Lua sees (lua.Clock): a toast's, a log line's, a render's.
func (h *Host) now() time.Time { return h.RT.Clock().Now() }

func (h *Host) hooks() UIHooks {
	h.uiMu.RLock()
	defer h.uiMu.RUnlock()
	return h.ui
}

// Screen is the UI to open a screen on; before the UI attaches it is
// ErrNeedsTUI.
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
	h.busyMu.RLock()
	defer h.busyMu.RUnlock()
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
	h.busyMu.RLock()
	defer h.busyMu.RUnlock()
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
// generation, else the project's marker and base name, which is how windows
// on different repositories tell themselves apart at a glance.
func (h *Host) TerminalTitle() string {
	if t := h.title.Load(); t != nil && t.gen == h.currentGen() {
		return t.text
	}
	if root := h.trustRoot(); root != "" {
		return ProjectMark(root) + " " + filepath.Base(root)
	}
	return "zemi"
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
	default:
		return "Error"
	}
}

// Notify shows a toast; before the UI attaches, a warning or an error goes
// to Stderr instead, so an early failure is never swallowed. The title says
// what happened, in sentence case with no period: "<Kind> <past participle>
// [object]", "Could not <verb> <object>" with the cause in the body,
// "Nothing to <verb> [yet]" with a reason only when it is not obvious.
func (h *Host) Notify(level slog.Level, title, body string) {
	h.recordNotify(level, title, body)
	if ui := h.hooks(); ui != nil {
		ui.Notify(level, title, body)
		return
	}
	h.notifyStderr(level, title, body)
}

// Alert raises what Notify does plus, while the window is unfocused, a
// one-time notification: for completions no status record carries, which a
// terminal that follows OSC 7501 itself would never surface. Headless, it is
// Notify.
func (h *Host) Alert(level slog.Level, title, body string) {
	h.recordNotify(level, title, body)
	if ui := h.hooks(); ui != nil {
		ui.Alert(level, title, body)
		return
	}
	h.notifyStderr(level, title, body)
}

// notifyStderr is the headless Notify: a warning or an error on Stderr, so
// an early failure is never swallowed.
func (h *Host) notifyStderr(level slog.Level, title, body string) {
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
// attaches).
func (h *Host) scroll(kind ScrollAction) {
	if ui := h.hooks(); ui != nil {
		ui.Scroll(kind)
	}
}

// Session returns a snapshot of the session state. The transcript is only
// appended to or replaced whole, never written in place, so the snapshot
// shares its rows: every widget render takes one.
func (h *Host) Session() Session {
	h.tMu.RLock()
	defer h.tMu.RUnlock()
	return Session{
		CWD:        h.session.CWD,
		Transcript: slices.Clip(h.session.Transcript),
	}
}

func (h *Host) AppendMessage(role fantasy.MessageRole, text string) {
	h.AppendChat(ChatMessage{Role: role, Text: text})
}

func (h *Host) AppendChat(m ChatMessage) {
	h.appendTranscript(m)
	h.refresh()
	h.refreshLevels()
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
		// y copies the line whole: a long desc truncates in the list, and
		// its tail is the part a reader was after
		items = append(items, OverlayItem{Text: k[0], Desc: k[1], Copy: k[0] + "   " + k[1], Section: "Keymaps"})
	}
	var aliases []OverlayItem
	for _, b := range h.Keymaps.List() {
		desc := b.Desc
		if desc == "" {
			desc = describeRHS(b.RHS)
		}
		if b.List {
			aliases = append(aliases, OverlayItem{Text: b.Key, Desc: cmp.Or(b.Desc, "same as "+b.RHS.Move), Section: "Lists"})
			continue
		}
		// a Lua keymap row copies whole, the Go ones' way: a plugin's long
		// desc truncates in the list and its tail is what a reader wanted
		items = append(items, OverlayItem{Text: b.Key, Desc: desc, Copy: b.Key + "   " + desc, Section: "Keymaps"})
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
	// Text is a composer.submit handler's { decision = "rewrite", text = ... }:
	// what the user typed is replaced with it, pre-send.
	Text string
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
		reason string
		timed  bool
		text   string
	}
	outcomes := make([]outcome, len(handlers))
	allowed := []string{"deny"}
	if name == EventComposerSubmit {
		allowed = append(allowed, "rewrite")
	}
	ev := eventTable(name, "host", payload)
	_, err := h.RT.Do(ctx, lua.ClassUI, lua.VetoCeiling(len(handlers), 1), 0, "hook "+name,
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
					case decision == "deny":
						outcomes[i] = outcome{denied: true, reason: glua.LVAsString(tb.RawGetString("reason"))}
					case decision == "rewrite":
						outcomes[i] = outcome{text: glua.LVAsString(tb.RawGetString("text"))}
					}
				}
			}
			return nil, nil
		})
	if errors.Is(err, lua.ErrAbandoned) {
		return VetoResult{TimedOut: true}
	}
	var res VetoResult
	for _, o := range outcomes {
		if o.timed {
			res.TimedOut = true
		}
		if o.denied && !res.Denied {
			res.Denied, res.Reason = true, o.reason
		}
		if res.Text == "" {
			res.Text = o.text
		}
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
	// the caller listed the handlers before this Do ran; a reload in between
	// purged them, and a purged handler never runs on the fresh state
	if hd.Gen != h.currentGen() {
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
	h.RecordError("event", hd.Event, err.Error())
}

// Effect is an animation the UI applies to a segment's text.
type Effect string

const (
	EffectShimmer Effect = "shimmer" // a bright band sweeps the text, blending its hl toward Normal
	EffectSpin    Effect = "spin"    // its first glyph steps through Spinner
)

// Segment is one styled run of text.
type Segment struct {
	Text string
	HL   string
	Hit  int // Go chrome only: 1-based index of the clickable item this text belongs to
	// Click is the id a click on the text passes to the widget that drew it;
	// text drawn outside a widget names a command instead
	Click string
	// Widget is the Go widget that drew the text, set as the frame is
	// painted: a click on the text goes to it. Go chrome only.
	Widget any
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
	// From is Go only: the highlight group a segment's Rise runs out of
	From string
	// Rise is how far (0 to 1) the segment's colors still stand in its From
	// group: a user row lands styled queued and its colors run into their own
	Rise float64
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
// the canvas but the user's prompt lifts to the panel, every box on the
// panel, a box's selected row on the band.
type Fill uint8

const (
	FillAuto   Fill = iota // the canvas, or the panel inside a box
	FillPanel              // a box: overlays, prompts, toasts, popups, the composer
	FillBand               // a list's or a prompt's selected row, over any background
	FillCanvas             // the canvas even inside a box: a well, such as a prompt's command
	FillWell               // a well's lifted half, over the panel: what a command answered
)

// segment builds one segment from a { text = ... } table's fields.
func segment(field func(string) any) Segment {
	var seg Segment
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
		seg.Click = id
	}
	if l, ok := field("link").(string); ok {
		seg.Link = linkOf(l)
	}
	return seg
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
// type name in a Lua error tells them nothing (DESIGN §6.0).
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
	default:
		return "an unexpected value"
	}
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
				return nil, errors.New(`a body of strings and lines: write { { "a", "b" } } for one line, { { "a" }, { "b" } } for two`)
			}
			return []any{t}, nil
		}
	}
	return v, nil
}

func convertSegments(v any) ([][]Segment, error) {
	v, err := oneLine(v)
	if err != nil {
		return nil, err
	}
	rows, ok := v.([]any)
	if m, isMap := v.(map[string]any); isMap && len(m) == 0 {
		ok = true // Lua's {} marshals as an empty map: no lines
	}
	if !ok {
		return nil, fmt.Errorf("an overlay body must be a list of lines, got %s", luaShape(v))
	}
	out := make([][]Segment, 0, len(rows))
	for _, row := range rows {
		cells, ok := row.([]any)
		if !ok {
			return nil, fmt.Errorf("an overlay line must be a list of segments, got %s", luaShape(row))
		}
		line := make([]Segment, 0, len(cells))
		for _, cell := range cells {
			switch c := cell.(type) {
			case string:
				line = append(line, Segment{Text: sanitize.Row(c)})
			case map[string]any:
				line = append(line, segment(func(k string) any { return c[k] }))
			default:
				return nil, fmt.Errorf("an overlay segment must be a string or a { text = ... } table, got %s", luaShape(cell))
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

var _turnEnds = []TurnEnd{TurnOK, TurnError, TurnCancelled}

// Waiting is the prompt holding a turn: a permission ask, a plan to
// approve or a question to answer.
type Waiting string

const (
	WaitApproval Waiting = "approval"
	WaitPlan     Waiting = "plan"
	WaitAnswer   Waiting = "answer"
)

var _waitings = []Waiting{WaitApproval, WaitPlan, WaitAnswer}

// ClockLayout formats UIState.Time, the wall clock the screen shows.
const ClockLayout = "15:04"

// StampLayout dates a session in an export and in zemi sessions.
const StampLayout = time.DateOnly + " " + ClockLayout

// UIState is the frame's state, passed in with every render.
type UIState struct {
	Time  string // the wall clock in ClockLayout, stepped once a minute
	Frame int    // the 50ms frame clock the snail's spinner and shimmer step on

	QuitArmed      bool    // a second ctrl+c quits
	InterruptArmed bool    // a second esc interrupts the turn
	KeySeq         string  // keys of a bound sequence typed so far
	RowsBelow      int     // transcript rows under a scrolled-up viewport
	Cancelling     bool    // interrupt sent, turn not finished yet
	Editing        string  // title of the file open in the editor
	Waiting        Waiting // while a prompt holds the turn
}

// usageTable is the session token tally for widgets (DESIGN §8): totals
// across all turns of this session. priced says the conversation's model
// declares prices, however the tally gathered it: the statusline's cost
// hides without it, a side call on a priced model is not a price the
// conversation's own shows.
func usageTable(L *glua.LState, s StreamSnapshot, priced bool) *glua.LTable {
	u := L.CreateTable(0, 5)
	u.RawSetString("in", glua.LNumber(s.SessionIn))
	u.RawSetString("out", glua.LNumber(s.SessionOut))
	u.RawSetString("total", glua.LNumber(s.SessionIn+s.SessionOut))
	u.RawSetString("cost", glua.LNumber(s.SessionCost))
	u.RawSetString("priced", glua.LBool(priced))
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
	h.overlayAnswered(o, item)
	_ = h.callOverlay(ctx, o, "on_select", o.OnSelect, item)
}

// SubmitOverlayInput hands an input overlay's text to on_select, or to the
// Go-side OnInput a secret's box sets: the save closes the box, and an
// error reopens it with the text back, so enter retries without re-pasting.
// The reopen lands only while the box is still shown: an esc during the
// save stays a cancel.
func (h *Host) SubmitOverlayInput(ctx context.Context, o Overlay, text string) {
	if o.OnInput != nil {
		if err := o.OnInput(strings.TrimSpace(text)); err != nil {
			o.Default = strings.TrimSpace(text)
			if ui := h.hooks(); ui != nil {
				ui.RefreshOverlay(o.ID, func() Overlay { return o })
			}
			h.Notify(slog.LevelWarn, "Could not save the key", err.Error())
			return
		}
		if ui := h.hooks(); ui != nil {
			ui.CloseOverlay(o.ID, o.Origin, o.Path)
		}
		return
	}
	h.overlayAnswered(o, text)
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
	h.overlayAnswered(o, nil)
	_ = h.callOverlay(ctx, o, "on_close", o.OnClose)
}

// overlayAnswered reports the ask's answer to its waiting task, once: the
// same value the Lua callback gets, nil on cancel.
func (h *Host) overlayAnswered(o Overlay, v any) {
	if o.Answered != nil {
		o.Answered(v)
	}
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
			h.RecordError("overlay", o.ID+" "+what, err.Error())
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
		return h.initErr
	}
	L := g.L
	h.gen.Store(g.ID)
	if err := h.installDefaultKeymap(); err != nil {
		return err
	}
	h.Providers.Reset(*h.Builtin.Load())
	if !h.Inert {
		h.syncSkills()
		h.syncTypes()
		h.GoSafe("git status", func() { h.gitStatus() })
		// the keychain is a subprocess per entry; the pass never holds boot
		h.GoSafe("stored keys", func() { h.loadStoredKeys() })
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
		{"zemi.provider", h.providerNamespace(L), nil},
		{"zemi.model", h.modelNamespace(L), nil},
		{"zemi.classify", h.classifyNamespace(L), nil},
		{"zemi.agent", h.agentNamespace(L), nil},
		{"zemi.workflow", h.workflowNamespace(L), nil},
		{"zemi.json", h.jsonNamespace(L), nil},
		{"zemi.csv", h.csvNamespace(L), nil},
		{"zemi.yaml", h.yamlNamespace(L), nil},
		{"zemi.composer", h.composerNamespace(L), nil},
		{"zemi.mcp", h.mcpNamespace(L), nil},
		{"zemi.lsp", h.lspNamespace(L), nil},
		{"zemi.session", h.sessionNamespace(L), nil},
		{"zemi.skills", h.skillsNamespace(L), nil},
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
	api.RawSetString("version", glua.LString("0.159.0"))
	return g.SB.RegisterProtectedValue("zemi.api", api)
}

func (h *Host) currentGen() Gen { return Gen(h.gen.Load()) }

// SetReload installs main's reload and its check for config that moved on
// disk since the last one. Call it before boot.
func (h *Host) SetReload(reload func() error, changed func() bool) {
	h.reload, h.changed = reload, changed
}

// SetConfigMark hands the host the watcher's baseline reset, for a caller
// outside the package that rewrites watched files without meaning a change.
func (h *Host) SetConfigMark(mark func()) { h.configMark = mark }

// MarkConfig makes the current config trees the baseline the watcher
// compares against: the plugin sweep's fetch rewrites files the watcher
// reads whether anything moved or not.
func (h *Host) MarkConfig() {
	if h.configMark != nil {
		h.configMark()
	}
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
	// before the UI attaches, the process would end before a reload ran
	if _, err := h.Screen(); err != nil {
		return err
	}
	if h.reload == nil {
		return errors.New("nothing to reload in this run")
	}
	h.GoSafe("reload", func() {
		// a reload cancels what the interpreter runs, which is the caller when
		// Lua asked for it: wait for it to return (a wedged one times out)
		_, _ = h.RT.Do(context.Background(), lua.ClassIO, 0, 0, "reload", func(*glua.LState) ([]any, error) { return nil, nil })
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
	// a reload after trust went off still runs, so the project's files go
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
	}
	return err
}

// checkInits parses the init file a reload runs, so a half-saved one
// fails the reload before it retires the working generation.
func (h *Host) checkInits() error {
	dir := h.Dirs.ConfigHome
	if dir == "" {
		return nil
	}
	p := filepath.Join(dir, "init.lua")
	src, err := os.ReadFile(p)
	if err != nil {
		return nil // the boot step skips or reports it
	}
	_, err = parse.Parse(bytes.NewReader(src), p)
	if pe, ok := errors.AsType[*parse.Error](err); ok {
		return lua.SyntaxErr(perm.TildePath(p), string(src), pe)
	}
	return nil
}

// awaitReload waits out running reloads and returns how many have started.
func (h *Host) awaitReload() uint64 {
	for {
		h.reloadMu.RLock()
		done, seq := h.reloadDone, h.reloadSeq
		h.reloadMu.RUnlock()
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
	h.Events.Purge()
	h.purgeWatches()
	h.hookReg.purge(h.currentGen())
	h.healthReg.purge()
	h.completions.purge()
	h.luaTools.purge(h.currentGen())
	h.stops.purge()
	h.mcp.park()
	h.lsp.forget()
	// An overlay left open would keep drawing with callbacks that are now
	// dropped (staleOverlay), which reads as a frozen picker.
	if ui := h.hooks(); ui != nil {
		ui.CloseLuaOverlays()
	}
}

// Close ends the host: the runtime, what its generation started (timers,
// jobs, requests, servers) and the stores, so it holds no file or goroutine:
// it joins the goroutines GoSafe started, waiting closeWait for the last.
func (h *Host) Close() {
	h.closeOnce.Do(func() { close(h.closed) })
	// under the agents lock: a live turn's armQueueDrain rewrites resumeStop
	ag := h.agents
	ag.mu.Lock()
	if h.resumeStop != nil {
		h.resumeStop()
	}
	ag.mu.Unlock()
	h.RT.Close()
	h.PurgeGeneration()
	h.mcp.stopParked()
	h.lsp.stopAll()
	if h.chat != nil {
		h.chat.Close()
	}
	// joined after the chat: the builder's worker ends on the chat's close,
	// so waiting first would wait out the whole cap on it every time
	ctx, cancel := context.WithTimeout(context.Background(), _closeWait)
	defer cancel()
	if err := h.goes.wait(ctx); err != nil {
		h.Log.Warn("goroutines outlived the close", "origin", "host", "error", err)
	}
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

// pluralCount is "1 line", "2 code blocks".
func pluralCount(n int, one, many string) string {
	return fmt.Sprintf("%d %s", n, Plural(n, one, many))
}
