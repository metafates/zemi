// Package ui implements the zemi TUI shell (DESIGN §8): the bubbletea
// program, region layout, widget line cache, theme resolution, slash
// palette, overlay stack, toasts, and the key router.
//
// Single-writer rule: all App state mutates on the bubbletea Update
// goroutine (or, in tests without a program, directly from the caller's
// goroutine). Work arriving from other goroutines (Lua callbacks, host
// hooks) is funneled through a hook channel drained by a persistent cmd.
package ui

import (
	"cmp"
	"context"
	"fmt"
	"image/color"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/status"
)

const (
	_frameInterval = _frameMS * time.Millisecond
	// Terminals hide the OSC 9;4 spinner after a few seconds unless the
	// program keeps sending it, so it is resent on this interval while running.
	_progressInterval = 2 * time.Second
	_idleInterval     = 250 * time.Millisecond // the tick with nothing moving on its own; a bump wakes it
	_frameMS          = 50                     // the frame clock: spinner, eyes and shimmer step once per tick
	_riseDuration     = 160 * time.Millisecond // how long a statusline pill's appear or disappear takes
	_riseTick         = 16 * time.Millisecond  // the tick while one runs, so the ramp reads smooth
	_doneLinger       = 3 * time.Second        // how long the mascot's eyes say how a turn ended
	// NOTE: 64 waiting hooks is the burst bound; past it dispatch drops a message, await waits.
	_hookQueueSize = 64
)

// App is the root bubbletea model. It implements hostapi.UIHooks.
type App struct {
	host *hostapi.Host
	log  *slog.Logger
	thm  themeResolver

	// copyTools are the local clipboard commands copyCmd tries in order.
	copyTools [][]string

	// termBG is the terminal's own background color as it answered an OSC
	// 11 query; nil means the terminal does not answer, so zemi leaves its
	// background alone.
	termBG color.Color

	width, height int

	composer composerState

	// composerW is the width the composer last drew its rows at, so the up
	// and down moves wrap at what the user sees; 0 before the first frame.
	composerW int

	// wrapKey is the draft, cursor and width wrapComposer last wrapped, held
	// so a frame's measure and draw share one wrap and a flood of keys that
	// edit nothing re-measures without one.
	wrapKey struct {
		text      string
		cursor, w int
		row, col  int
		rows      [][]rune
		soft      []bool
	}

	// published is the draft the host last heard, so a key that changed
	// nothing republishes neither.
	published composerSnap

	histLoaded bool // the stored messages already seeded the composer history

	// Scrolled regions by region (DESIGN §8 scrollback); scrollDef is the
	// one scroll actions without a region and the wheel outside every
	// scrolled region move, scrollIDs the ones drawn on the last frame.
	scrolls   map[region]*scrollState
	scrollDef region
	scrollIDs []region
	// The chat scrolls per view: chatScroll parks the session's state while
	// a run's transcript draws in its place.
	chatView   string
	chatScroll *scrollState
	// pendingScroll is an edge a region goes to at its next paint, when the
	// rows a scroll was issued for may not have arrived yet: "top" stays
	// parked there until the region is scrolled again, "bottom" applies once.
	pendingScroll map[region]hostapi.ScrollAction
	// pendingAnchor is the transcript row a region goes to at its next
	// paint, for a following list: the rows a jump was issued for may still
	// be filling in above.
	pendingAnchor map[region]int
	// highlight is the transcript row the open picker selects, whose rows
	// the paint bands; 0 when nothing follows. The row names a whole unit,
	// a run's drawing included.
	highlight int
	// chatFocus is tab's chat cursor: the composer handed the keys to the
	// chat, and chatSel is the message it walks (the transcript row the
	// paint bands, like highlight; 0 none).
	chatFocus bool
	chatSel   int
	// chatSelMemo is the message the cursor held when the chat last had the
	// keys: tab re-selects it rather than re-deriving one from the view.
	chatSelMemo int
	// byRow says an overlay is opening from a key on another list's row, so
	// it reopens where that list left it instead of following the chat
	byRow bool
	// stepFrom is the view a step (ctrl+j/k) held open; the walk's run posts
	// its landing (stepOpening marks the run on the worker) and the settle
	// swaps the two in one mutation. stepSettledSeq is the last served run.
	stepFrom       string
	stepOpening    atomic.Int64
	stepSeq        int64
	stepSettledSeq int64
	stepLanded     []hostapi.Overlay

	completion wordCompleter
	palette    cmdPalette

	overlays []overlayState
	trust    *trustPrompt
	toasts   []toast

	// the trust answer's guard: the box a boot opens right after the answer
	// (a first run lands on the model picker) must not act on the key that
	// answered, so the router drops the first key that reaches it
	answeredTrust time.Time
	guardOpen     int

	// Go-owned agent chrome (DESIGN §6.3, §8)
	permAsk  *permAskPrompt
	planAsk  *planAskPrompt
	question *questionPrompt

	mouse          mouseSel
	press          mousePress
	hovering       bool
	hoverX, hoverY int
	hoverSpans     []cellSpan          // what the pointer is over, to redraw only on change
	pointer        string              // the OSC 22 shape last set
	lastRows       [][]hostapi.Segment // the last composed frame, for clicks and copy
	panes          []uv.Rectangle      // the last frame's screenLayout.panes
	stage          uv.Rectangle        // where the last frame's stage was
	popRect        uv.Rectangle        // the cells a completion popup took on the last frame
	chromeRect     uv.Rectangle        // the cells the topmost modal box took
	arms           []uint8             // join's grid, kept so a frame allocates none
	barDrag        barHold             // the scrollbar the pointer holds, h 0 for none

	// work runs Lua-backed keymaps and commands in order off the key loop
	work     chan func()
	workOnce sync.Once
	editing  string // the editor handoff in progress, shown in the working row

	prefixes map[string]string  // styled segment prefixes, rebuilt each frame
	fadeKeys map[float64]string // a fade's key text, rebuilt each frame
	// widgets are the screen's own pieces, drawn from the frame's state
	widgets       []*widget
	caches        map[string]*widgetCache
	pending       map[string]renderKey // widget id -> the render in flight
	renderSeq     int64                // counts render dispatches, so the latest one wins
	frameGen      int64
	rises         map[string]*pillFade // statusline pills easing their appear and disappear
	spun          bool                 // the last frame drew a spinner, so the tick keeps stepping it
	timeStr       string
	turnActive    bool             // what onFrame last saw of the agent
	turnDone      *turnDoneMsg     // the last turn's end, waiting for a frame that reads the agent idle
	turnEnd       time.Time        // when the agent went idle after a turn, so the mascot can bow
	turnEnded     hostapi.TurnEnd  // how it ended
	looping       bool             // what onFrame last saw of the session's /loop
	unseen        hostapi.TurnEnd  // how the last turn ended while the window was away, "" once seen
	lastState     hostapi.UIState  // what the widgets last rendered with, the tick aside
	now           func() time.Time // animation clock; tests pin it
	cancelling    bool             // interrupt sent, turn not finished yet
	progress      bool             // the view reports a running turn (OSC 9;4)
	statusSent    status.Report    // the last OSC 7501 report the terminal holds
	statusSupport bool             // the terminal answered the OSC 7501 feature query
	turnErr       string           // the last failed turn's one-line cause, for the status report
	turnReply     string           // the last clean turn's reply, for the status report
	progressAt    time.Time        // when beat last flipped (progressInterval)
	beat          int              // the report's value, flipped so the renderer resends it
	focused       bool             // terminal focus (true until the terminal reports a blur)
	keySeq        []string         // keys of a bound sequence typed so far
	keySeqAt      time.Time
	armed         armed                // a key whose second press acts (ctrl+c quit, esc interrupt)
	burstSwallow  time.Time            // a danger key's fire: one burst key eats, the echo of the confirm
	askSwallow    time.Time            // a key that answered an ask: the paste's whole tail eats till the window ends
	burstKey      string               // the key whose echo the swallow waits for
	colors        colorprofile.Profile // what the terminal can show: segStyle quantizes cells to it, never OSC 11

	// an idle recap: when it is due (zero for none), and the stop of one in
	// flight
	recapAt time.Time
	// composerNote rides the placeholder after a "tell me instead" answer,
	// until the first printable key
	composerNote string
	recapStop    context.CancelFunc

	bootErr        error
	bootView       viewport          // the boot-failure box's error text
	streamPending  atomic.Bool       // a stream agentStreamMsg is queued
	wake           chan struct{}     // a bump while the frame tick sleeps idle
	refreshPending atomic.Bool       // a Refresh is queued
	lastSel        map[string]string // per overlay id, the selected row's key when it last closed
	capToastAt     time.Time         // when the draft-cap toast may push again
	sizedDraft     string            // the draft the last size-only render pass ran at

	// the last composed frame, published for Screenshot: reading a
	// snapshot is what lets any goroutine ask what the screen says
	// without waiting on the Update goroutine.
	frameMu   sync.Mutex
	lastFrame string
	// what lastFrame drew: a generation or size change recomposes, and the
	// changes that move neither (the draft, a widget's lines, the toasts)
	// set frameDirty, so a View with nothing behind it returns the frame
	// it has instead of composing again
	drawn      renderKey
	frameDirty bool

	program   *tea.Program
	relaunch  atomic.Pointer[relaunchTarget] // RelaunchDir
	hooks     chan tea.Msg
	testQueue chan tea.Msg // CaptureHooks (tests): hooks wait here for Pump
}

// widgetCache holds the last-good rendered lines of one widget keyed by
// (id); the frame they were rendered at gates reuse (DESIGN §4, §8).
type widgetCache struct {
	lines         [][]hostapi.Segment
	width, height int
	gen           int64
	seq           int64  // the dispatch the lines came from
	err           string // last recorded render error; a repeat is not recorded again
	partial       bool   // lines of a render that asked to run again: older rows are still to come

	// wrapped holds the lines re-wrapped for wrapW cells: compose runs
	// every frame and wrapping the transcript is its biggest cost.
	wrapped [][]hostapi.Segment
	wrapW   int
	// lineRows is each line's rows at wrapW, by the address of its first
	// segment: a render hands back the lines it did not change
	// (Widget.tableSegments), so a new reply wraps only its own lines.
	lineRows map[*hostapi.Segment][][]hostapi.Segment
	// spare is the lineRows map before it, cleared and reused, and wrapN the
	// last wrap's row count: a streaming frame allocates neither again
	spare map[*hostapi.Segment][][]hostapi.Segment
	wrapN int
}

// NewApp builds the root model; host hooks are attached by the caller via
// host.SetUIHooks(app).
func NewApp(host *hostapi.Host, log *slog.Logger) *App {
	return &App{
		host:     host,
		log:      log,
		thm:      themeResolver{sgr: map[string]uv.Style{}},
		caches:   map[string]*widgetCache{},
		wake:     make(chan struct{}, 1),
		prefixes: map[string]string{},
		fadeKeys: map[float64]string{},
		rises:    map[string]*pillFade{},
		scrolls:  map[region]*scrollState{},
		lastSel:  map[string]string{},
		widgets:  shippedWidgets(host),
		timeStr:  "--:--",
		now:      time.Now,
		focused:  true,
		hooks:    make(chan tea.Msg, _hookQueueSize),

		copyTools: [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}},

		pendingScroll: map[region]hostapi.ScrollAction{},
		pendingAnchor: map[region]int{},
	}
}

// SetProgram wires the running bubbletea program (for Quit/Send).
func (a *App) SetProgram(p *tea.Program) { a.program = p }

// SignalBoot reports the boot outcome from the boot goroutine (posted
// through the hook channel; safe from any goroutine).
func (a *App) SignalBoot(err error) {
	a.do(func() {
		a.bootErr = err
		if err != nil {
			a.log.Error("boot failed", "origin", "ui", "error", err)
			return
		}
		// a toggle list open across a reload rebuilds from the new state
		for _, st := range a.overlays {
			if st.o.Reload != nil {
				a.reloadOverlay(st.o, nil)
			}
		}
	})
}

// relaunchTarget is where main starts zemi again after a Relaunch: a
// directory.
type relaunchTarget struct{ dir string }

// Quit asks the program to exit (/quit).
func (a *App) Quit() { a.quitTo("") }

// Relaunch leaves like Quit, then main starts zemi again in dir.
func (a *App) Relaunch(dir, resume string) { a.quitTo(dir) }

// RelaunchDir is where a Relaunch that got past the quit veto restarts zemi:
// the directory, "" for none.
func (a *App) RelaunchDir() (dir, resume string) {
	if t := a.relaunch.Load(); t != nil {
		return t.dir, ""
	}
	return "", ""
}

// quitTo quits, then main starts zemi in dir unless it is "". The veto waits
// on the interpreter and program.Quit must not run inside Update, so both run
// on a goroutine.
func (a *App) quitTo(dir string) {
	quit := func() {
		if res := a.host.EmitVetoable(context.Background(), hostapi.EventAppQuit, map[string]any{}); res.Denied {
			what := "Switch blocked"
			if dir == "" {
				what = "Quit blocked"
			}
			a.Notify(slog.LevelWarn, what, res.Reason)
			return
		}
		a.relaunch.Store(&relaunchTarget{dir})
		a.leave()
	}
	if a.program != nil {
		a.host.GoSafe("quit", quit)
		return
	}
	quit()
}

// leave quits the program. It runs on a goroutine, since program.Quit waits
// on the Update loop.
func (a *App) leave() {
	if a.program != nil {
		// the record outlives the process otherwise: bubbletea resets the
		// title, background and progress bar on exit, not this, and a
		// queued Raw dies with the ticker that would flush it
		fmt.Fprint(os.Stdout, status.Report{State: status.Clear}.Sequence())
		a.program.Quit()
	}
}

// messages

type frameTickMsg struct{}

// widgetsRenderedMsg carries every stale widget's render at once: one
// message, one compose, where one per widget cost a compose each.
type widgetsRenderedMsg []widgetRender

// renderKey is what a widget render was for: a frame generation at a size.
type renderKey struct {
	gen           int64
	width, height int
}

type widgetRender struct {
	id            string
	lines         [][]hostapi.Segment
	more          bool // the render wants to run again
	width, height int
	gen, seq      int64
	err           error
}

type (
	agentStreamMsg struct{}
	turnDoneMsg    struct {
		err     string
		stopped bool
	}
)

// uiFunc is a state change posted from another goroutine; it runs on the
// Update goroutine.
type uiFunc func() tea.Cmd

// do posts fn to run on the Update goroutine.
func (a *App) do(fn func()) { a.dispatch(uiFunc(func() tea.Cmd { fn(); return nil })) }

// uiProbe is a posted look at UI state that changes nothing itself, so it
// wakes no frame; what it finds to change it posts as a uiFunc.
type uiProbe func()

type permAnswer struct {
	ok bool
	by string
}

// hookMsg wraps a message posted from a non-UI goroutine.
type hookMsg struct{ inner tea.Msg }

// tea.Model

func (a *App) Init() tea.Cmd {
	// the background query learns whether the terminal repaints its own bg
	return tea.Batch(a.frameTickCmd(_frameInterval), a.drainHooksCmd(), tea.RequestBackgroundColor,
		func() tea.Msg { return tea.RequestProgramStatusSupport() })
}

// Update re-renders at once the widgets a message resized, so no frame draws
// lines rendered for another size; the tick renders the rest. A keystroke
// that left the draft as it was (a refused one above the cap, a move) resized
// nothing: the size pass would solve the layout a key.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := a.update(msg)
	if _, tick := msg.(frameTickMsg); tick {
		return a, cmd
	}
	if _, ok := msg.(tea.KeyPressMsg); ok && a.composer.text == a.sizedDraft {
		return a, cmd
	}
	a.sizedDraft = a.composer.text
	return a, tea.Batch(cmd, a.renderStaleWidgetsCmd(true))
}

func (a *App) update(msg tea.Msg) tea.Cmd {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg, tea.MouseClickMsg, tea.MouseWheelMsg, tea.FocusMsg:
		// plain motion is not input: terminals report hover over an unfocused window
		a.recapAt = time.Time{}
		if a.recapStop != nil {
			a.recapStop()
		}
	}
	switch msg := msg.(type) {
	case frameTickMsg:
		return a.onFrame()
	case widgetsRenderedMsg:
		for _, r := range msg {
			a.onWidgetRendered(r)
		}
	case uiFunc:
		a.bump()
		return msg()
	case uiProbe:
		msg()
	case agentStreamMsg:
		// Stream state lives in the host; just invalidate the frame.
		a.bump()
		a.streamPending.Store(false)
	case turnDoneMsg:
		// onFrame applies it once the agent reads idle, so a chain of turns
		// (a queued follow-up, a goal) ends once, the way its last turn did
		a.bump()
		a.turnDone = &msg
	case hookMsg:
		cmd := a.update(msg.inner)
		// Re-arm the persistent drain: each delivered hookMsg re-arms
		// exactly one, so the chain never breaks.
		return tea.Batch(cmd, a.drainHooksCmd())
	case tea.ColorProfileMsg:
		a.colors = msg.Profile
	case tea.BackgroundColorMsg:
		// The answer means the terminal replies to OSC 11, so its own
		// background can be repainted to match whorl's.
		a.termBG = msg.Color
		a.bump()
	case tea.ProgramStatusSupportMsg:
		// the terminal keeps the program-status records, so they already
		// tell it what a record-carried alert would say
		a.statusSupport = true
	case tea.FocusMsg, tea.BlurMsg:
		if _, a.focused = msg.(tea.FocusMsg); a.focused {
			a.unseen = ""
		}
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.host.EmitAsync(hostapi.EventUIResize, map[string]any{"width": msg.Width, "height": msg.Height})
		a.bump()
	case tea.KeyPressMsg:
		if msg.String() == "alt+esc" {
			// the terminal merged the two escapes of a fast double-press
			// into one alt-combo (an ESC wait folds \x1b\x1b): run both
			// presses, the first arms, the second fires
			esc := tea.KeyPressMsg{Code: tea.KeyEscape}
			return tea.Batch(a.handleKey(esc), a.handleKey(esc))
		}
		cmd := a.handleKey(msg)
		a.publishComposer()
		return cmd
	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		_, click := msg.(tea.MouseClickMsg)
		defer a.disarmIfMoved(click)()
		return a.handleMouse(msg)
	case tea.MouseWheelMsg:
		defer a.disarmIfMoved(true)()
		a.mouse = mouseSel{}
		a.hoverSpans = nil // the content under the pointer moves
		step := 0
		switch msg.Button {
		case tea.MouseWheelUp:
			step = -1
		case tea.MouseWheelDown:
			step = 1
		}
		if step != 0 {
			a.wheel(msg.X, msg.Y, step)
		}
		return nil
	case editorDoneMsg:
		a.editing = ""
		msg.errc <- msg.err
		return nil
	case editorMsg:
		if msg.err != nil {
			a.pushToast(slog.LevelError, "Could not open the editor", msg.err.Error())
			return nil
		}
		text := sanitize.String(msg.text)
		if utf8.RuneCountInString(text) > _maxDraftChars {
			a.draftCapToast()
			return nil
		}
		prev := composerSnap{text: a.composer.text, cursor: a.composer.cursor}
		a.composer.text = text
		a.composer.moveEnd()
		a.composer.snapshot(prev, false)
		a.publishComposer()
		return a.updateCompletion()
	case clipboardMsg:
		if msg.err != nil {
			a.pushToast(slog.LevelError, "Could not paste", msg.err.Error())
			return nil
		}
		prev := composerSnap{text: a.composer.text, cursor: a.composer.cursor}
		cmd := a.insertToken(msg.token)
		a.composer.snapshot(prev, false)
		a.publishComposer()
		return cmd
	case tea.PasteMsg:
		s := sanitize.String(strings.ReplaceAll(strings.ReplaceAll(msg.Content, "\r\n", "\n"), "\r", "\n"))
		if a.pasteChrome(s) {
			return nil
		}
		// a dropped file arrives as a paste of its path
		if paths, ok := parseDrop(s); ok {
			cmd := a.insertAttachments(paths)
			a.publishComposer()
			return cmd
		}
		prev := composerSnap{text: a.composer.text, cursor: a.composer.cursor}
		if !a.composer.paste(s) {
			a.draftCapToast()
			return nil
		}
		a.composer.snapshot(prev, false)
		a.publishComposer()
		return a.updateCompletion()
	case filesScannedMsg:
		a.onFilesScanned(msg)
		a.bump()
	}
	return nil
}

func (a *App) View() tea.View {
	v := tea.NewView(a.viewFrame())
	v.AltScreen = true
	// Cell motion: the wheel scrolls the transcript (the altscreen buffer has
	// no native scrollback), drags select and clicks press. ui.mouse = false
	// leaves the mouse to the terminal.
	if a.host.Settings.Bool(config.KeyUIMouse) {
		v.MouseMode = tea.MouseModeAllMotion // motion without a button drives hover
	}
	v.ReportFocus = true // notifyIfAway needs to know
	// bubbletea resets it (OSC 111) on every exit path, a signal included
	v.BackgroundColor = a.termBackground()
	v.WindowTitle = a.windowTitle()
	if a.progress {
		v.ProgressBar = &tea.ProgressBar{State: tea.ProgressBarIndeterminate, Value: a.beat}
	}
	return v
}

// progressTick keeps the view's OSC 9;4 report, which ghostty, WezTerm and
// Windows Terminal draw on the tab or taskbar, on while a turn runs. Those
// terminals hide it once it stops being sent and the renderer writes it only
// when it changes, so the value (which the indeterminate state never sends)
// flips every progressInterval.
func (a *App) progressTick(running bool) {
	a.progress = running
	if now := a.now(); a.progress && now.Sub(a.progressAt) >= _progressInterval {
		a.progressAt, a.beat = now, 1-a.beat
	}
}

// notifyIfAway alerts in the ui.notify style (Host.NotifyStyle) when zemi
// needs attention while its window is not focused.
func (a *App) notifyIfAway(text string) tea.Cmd {
	if a.focused {
		return nil
	}
	style, _, _ := a.host.NotifyStyle()
	return a.notifyAs(style, text)
}

// notifyUnlessReported alerts an event the program-status record already
// carries (an ask, a turn's end): a terminal that answered the OSC 7501
// feature query shows the record its own way, so this alert would double it.
func (a *App) notifyUnlessReported(text string) tea.Cmd {
	if a.statusSupport {
		return nil
	}
	return a.notifyIfAway(text)
}

// notifyAs sends text in the ui.notify style; a style this terminal has no
// channel for sends nothing.
func (a *App) notifyAs(style, text string) tea.Cmd {
	text = sanitize.String(text)
	switch style {
	case config.NotifyOSC:
		return tea.Raw(ansi.Notify(text))
	case config.NotifyBell:
		return tea.Raw("\a")
	case config.NotifyNative:
		// the native channels title the banner "zemi" themselves; the
		// prefix is for the bare OSC 9 line
		args := hostapi.NotifyArgv(strings.TrimPrefix(text, "zemi: "))
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = proc.Run(proc.CommandContext(ctx, args[0], args[1:]...))
			return nil
		}
	default:
		return nil
	}
}

// Frame renders the full screen as styled text; exported for the golden
// test harness (DESIGN §8 golden tests), which pins the composed screen and
// so always composes.
func (a *App) Frame() string {
	if a.width <= 0 || a.height <= 0 {
		return "zemi starting…"
	}
	// a reload's window has the old generation's widgets and theme purged,
	// and a frame drawn there flashes without them: keep the last one until
	// the new generation draws, like a reload keeps the old layout
	if a.host.RT.Reloading() {
		a.frameMu.Lock()
		defer a.frameMu.Unlock()
		if a.lastFrame != "" {
			return a.lastFrame
		}
	}
	return a.compose(a.width, a.height)
}

// viewFrame is Frame behind the drawn-frame cache: while nothing changed
// since the last paint (the generation and size it ran at, and no draft,
// widget line or toast that marked itself in), the View returns the frame
// it has instead of composing again.
func (a *App) viewFrame() string {
	if a.width <= 0 || a.height <= 0 {
		return "zemi starting…"
	}
	// a frame that drew something moving by itself (a.spun) is never clean:
	// its effects step at the tick with nothing else bumping
	if !a.host.RT.Reloading() && !a.frameDirty && !a.spun && a.drawn == (renderKey{gen: a.frameGen, width: a.width, height: a.height}) {
		a.frameMu.Lock()
		defer a.frameMu.Unlock()
		return a.lastFrame
	}
	return a.Frame()
}

// frame loop and widget rendering

// bump invalidates the frame. It also wakes the tick, so a change never
// waits out idleInterval; onFrame's own bumps need no waking.
func (a *App) bump() {
	a.frameGen++
	a.wakeTick()
}

func (a *App) wakeTick() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// frameTickCmd ticks on the last frame boundary within d, or the next one
// after a wake: frame() then advances by exactly one per tick. A tick faster
// than the frame clock (a rise) re-renders only and keeps no grid.
func (a *App) frameTickCmd(d time.Duration) tea.Cmd {
	if d < _frameInterval {
		return func() tea.Msg {
			select {
			case <-a.wake:
			case <-time.After(d):
			}
			return frameTickMsg{}
		}
	}
	at := time.Now().Add(d).Truncate(_frameInterval)
	return func() tea.Msg {
		select {
		case <-a.wake:
			at = time.Now().Add(_frameInterval).Truncate(_frameInterval)
		case <-time.After(time.Until(at)):
		}
		time.Sleep(time.Until(at))
		return frameTickMsg{}
	}
}

// frameWork is the work that redraws by itself: a turn, a shown run or a
// busy job. turnRunning differs per caller: tickInterval reads last frame's
// a.turnActive, onFrame the fresh AgentActive.
func (a *App) frameWork(turnRunning bool) bool {
	busy, _ := a.host.BusyNow()
	return turnRunning || a.host.ViewedRunRunning() || busy != ""
}

func (a *App) frameAwake(st hostapi.UIState) bool {
	return a.frameWork(a.turnActive) || st.Ended != "" || st.QuitArmed || st.InterruptArmed || st.KeySeq != "" || len(a.toasts) > 0 || a.spun || a.statusRising()
}

// tickInterval is idleInterval unless something on screen moves by itself:
// a turn's readouts, a shown run's clock, a spinner, a toast's or the done
// frame's expiry, a statusline widget's rise. Everything else bumps.
func (a *App) tickInterval() time.Duration {
	if a.statusRising() {
		return _riseTick
	}
	if a.frameAwake(a.lastState) {
		return _frameInterval
	}
	return _idleInterval
}

func (a *App) onFrame() tea.Cmd {
	now := time.Now()
	a.pruneToasts(now)
	// A minute change re-renders time-reading widgets.
	if ts := now.Format(hostapi.ClockLayout); ts != a.timeStr {
		a.timeStr = ts
		a.frameGen++
	}
	// While a turn, a shown run or a busy job runs every tick re-renders, so
	// the eyes and the elapsed time step even when no stream event arrives.
	active := a.host.AgentActive()
	if a.frameWork(active) || a.statusRising() {
		a.frameGen++
	}
	a.turnActive = active
	var ended tea.Cmd
	if !active && a.turnDone != nil {
		ended = a.endTurn(*a.turnDone)
		a.turnDone = nil
	}
	// a loop's runs end quietly (endTurn); the loop's end is worth an alert,
	// naming the why the host stopped it with
	if _, on := a.host.Loop(); on != a.looping {
		if a.looping {
			if why := a.host.LoopEndedWhy(); why != "" {
				text := ansi.Truncate(sanitize.Line(why), 200, "…")
				ended = tea.Batch(ended, a.notifyUnlessReported("zemi: "+lowerFirst(text)))
			} else {
				ended = tea.Batch(ended, a.notifyUnlessReported("zemi: the loop ended"))
			}
		}
		a.looping = on
	}
	if !active && !a.recapAt.IsZero() && !a.now().Before(a.recapAt) {
		var ctx context.Context
		ctx, a.recapStop = context.WithCancel(context.Background())
		a.recapAt = time.Time{}
		a.host.Recap(ctx)
	}
	// widgets read these flags, so a change re-renders them; the animation
	// tick alone does not, it only matters while a turn bumps the frame
	// anyway, and Ended turning on and off bumps at both ends by itself
	var replay tea.Cmd
	if len(a.keySeq) > 0 && a.now().Sub(a.keySeqAt) > _keySeqTimeout {
		replay = a.replayKeys(a.keySeq)
		a.keySeq = nil
	}
	st := a.uiState()
	if st.Frame = 0; st != a.lastState {
		a.lastState = st
		a.frameGen++
	}
	a.progressTick(active)
	return tea.Batch(a.frameTickCmd(a.tickInterval()), a.renderStaleWidgetsCmd(false), replay, ended, a.statusTick())
}

// endTurn stamps how the last turn ended for the eyes and the tab title,
// arms the recap, and alerts an unfocused terminal; a chain a loop run
// began arms no recap and alerts nobody, since no one waits on it.
func (a *App) endTurn(msg turnDoneMsg) tea.Cmd {
	byLoop := a.host.AgentStream().ByLoop
	a.turnEnd, a.turnEnded = a.now(), hostapi.TurnOK
	if !byLoop {
		if d := a.host.Settings.Duration(config.KeySessionRecap); d > 0 {
			a.recapAt = a.now().Add(d)
		}
	}
	if msg.stopped {
		a.turnEnded, a.recapAt = hostapi.TurnCancelled, time.Time{}
	} else if msg.err != "" {
		a.turnEnded, a.recapAt = hostapi.TurnError, time.Time{}
		a.turnErr = ansi.Truncate(sanitize.Line(msg.err), 200, "…")
	} else {
		a.turnReply = ansi.Truncate(sanitize.Line(stripMarkdown(a.host.LastReply())), 200, "…")
	}
	a.unseen = ""
	if !a.focused {
		a.unseen = a.turnEnded
	}
	text := "zemi finished"
	switch {
	case msg.stopped:
		text = "zemi stopped"
	case msg.err != "":
		if line := ansi.Truncate(sanitize.Line(msg.err), 80, "…"); line != "" {
			text = "zemi failed: " + line
		} else {
			text = "zemi failed"
		}
	case a.turnReply != "":
		text = "zemi finished: " + a.turnReply
	}
	if byLoop {
		return nil
	}
	return a.notifyUnlessReported(text)
}

// termBackground is what the terminal's own background is set to (OSC 11)
// so margins and scrollback match the theme's canvas: only on a terminal
// that answered the query and shows the color, nil (its own) otherwise or
// for a theme with no bg.
func (a *App) termBackground() color.Color {
	if a.termBG == nil || a.colors < colorprofile.ANSI256 {
		return nil
	}
	return a.thm.canvas()
}

func (a *App) uiState() hostapi.UIState {
	st := hostapi.UIState{
		Time: a.timeStr, Frame: a.frame(),
		// the arm outlives the state it armed in (a paste fills the draft, a
		// background report starts a turn): promise a quit only when the next
		// ctrl+c reaches the arm branch
		QuitArmed:      a.isArmed("ctrl+c") && a.composer.empty() && !a.host.AgentActive(),
		InterruptArmed: a.isArmed("esc") && a.armed.what == "interrupt",
		RowsBelow:      a.rowsBelow(),
		Cancelling:     a.cancelling, Editing: a.editing,
	}
	if !a.turnActive && a.now().Sub(a.turnEnd) < _doneLinger {
		st.Ended = a.turnEnded
	}
	if len(a.keySeq) > 0 && a.now().Sub(a.keySeqAt) <= _keySeqTimeout {
		st.KeySeq = strings.Join(a.keySeq, " ")
	}
	st.Waiting = a.waiting()
	return st
}

// waiting is the prompt holding the turn, "" for none. The trust overlay is
// a permission ask: it holds the keys before any turn, and the title mark is
// what a tmux-side observer reads (the OSC report already carries it).
func (a *App) waiting() hostapi.Waiting {
	switch {
	case a.trust != nil:
		return hostapi.WaitApproval
	case a.permAsk != nil:
		return hostapi.WaitApproval
	case a.planAsk != nil:
		return hostapi.WaitPlan
	case a.question != nil:
		return hostapi.WaitAnswer
	default:
		return ""
	}
}

// windowTitle is the tab title: the host's, behind a mark for a prompt that
// waits on the user or a turn that ended while the window was away. A
// running turn leaves the title alone — the program status reports it.
func (a *App) windowTitle() string {
	t := a.host.TerminalTitle()
	switch {
	case a.waiting() != "":
		return "? " + t
	case a.unseen == hostapi.TurnOK:
		return "✓ " + t
	case a.unseen == hostapi.TurnError:
		return "✗ " + t
	default:
		return t
	}
}

// frameClock is the frame clock's tick, frameMS each: what the spinner, the
// eyes and the shimmer step on.
func frameClock(now func() time.Time) int {
	return int(now().UnixMilli() / _frameMS)
}

// frame is the app's tick of the frame clock.
func (a *App) frame() int { return frameClock(a.now) }

// renderMissingWidgets renders the shown widgets that have never drawn
// straight into the cache, so the frame this compose paints is whole: a
// first paint without them is chrome only, and the queued render would land
// a visible flash later. The queue renders everything after this frame.
func (a *App) renderMissingWidgets() {
	for _, w := range a.shownWidgets() {
		if a.caches[w.ID] != nil {
			continue
		}
		a.renderSeq++
		// a widget that fills over frames draws its first pass, the tail,
		// unless a reader is off it: their place is found again by matching
		// rows, so the rows have to be whole before they are painted.
		// NOTE: the whole fill runs inside this frame, so switching back into
		// a long transcript pays for it; a cache per view would keep it free
		anchored := a.scrolledUp(w.ID)
		for {
			st := a.host.ScreenState(w.width, w.height, a.uiState())
			lines, more, err := a.renderWidget(w, st)
			a.onWidgetRendered(widgetRender{
				id: w.ID, lines: lines, more: more, width: w.width, height: w.height,
				gen: a.frameGen, seq: a.renderSeq, err: err,
			})
			if !more || !anchored || err != nil {
				break
			}
		}
	}
}

func (a *App) onWidgetRendered(msg widgetRender) {
	if a.pending[msg.id] == (renderKey{gen: msg.gen, width: msg.width, height: msg.height}) {
		delete(a.pending, msg.id)
	}
	// renders of one generation at two sizes run concurrently: one dispatched
	// earlier that finishes last must not replace newer lines
	if c := a.caches[msg.id]; c != nil && c.seq > msg.seq {
		return
	}
	if msg.err != nil {
		// The failure marks the generation rendered so the widget is not
		// retried every tick, and a repeat of the same error is not recorded
		// again: RecordError bumps the frame, which would loop.
		c := a.caches[msg.id]
		if c == nil {
			c = &widgetCache{}
			a.caches[msg.id] = c
		}
		c.gen, c.seq, c.width, c.height = msg.gen, msg.seq, msg.width, msg.height
		// a dropped request (hot mode, a reload) keeps the lines unflagged,
		// and the repeat test reads the panic's first line only: the stack
		// under it differs every render
		head, _, _ := strings.Cut(msg.err.Error(), "\n")
		if !lua.Dropped(msg.err) && c.err != head {
			c.err = head
			a.host.RecordError("widget", msg.id, msg.err.Error())
		}
		a.log.Debug("widget render failed; keeping last good lines", "origin", "ui",
			"widget", msg.id, "error", msg.err,
		)
		return
	}
	c := &widgetCache{lines: a.statusFaded(msg), width: msg.width, height: msg.height, gen: msg.gen, seq: msg.seq, partial: msg.more}
	old := a.caches[msg.id]
	if old != nil {
		c.wrapW, c.lineRows, c.spare, c.wrapN = old.wrapW, old.lineRows, old.spare, old.wrapN
	}
	if msg.more {
		c.gen-- // stale, so the next frame renders this widget alone again
		a.wakeTick()
		// A reader scrolled up keeps the last whole render until the new one
		// is whole: anchoring on partial rows would lose their place.
		if old != nil && len(old.lines) > 0 && a.scrolledUp(msg.id) {
			c.lines, c.wrapped, c.partial = old.lines, old.wrapped, old.partial
		}
	}
	a.caches[msg.id] = c
	a.frameDirty = true // the new lines join the next View though the generation stood still
}

// pillFade is one statusline pill's place in its appear or disappear: shown
// says what its last render drew, from the alpha the flip started from, and
// lines what a disappearing pill keeps drawing while it sinks.
type pillFade struct {
	shown bool
	at    time.Time
	from  float64
	lines [][]hostapi.Segment
}

// pillAlpha is a pill's current alpha, its last flip's from easing toward
// shown 1 or hidden 0 over _riseDuration.
func pillAlpha(f *pillFade, now time.Time) float64 {
	target := 0.0
	if f.shown {
		target = 1
	}
	p := float64(now.Sub(f.at)) / float64(_riseDuration)
	if p >= 1 {
		return target
	}
	return f.from + (target-f.from)*smoothstep(max(0, p))
}

// statusFaded is a statusline pill's lines settled through its appear and
// disappear: a pill that starts drawing rises out of the surface, one that
// stops keeps drawing what it drew, sinking back in, until the ramp ends.
func (a *App) statusFaded(msg widgetRender) [][]hostapi.Segment {
	w := a.widgetByID(msg.id)
	if w == nil || (w.region != _regionStatusLeft && w.region != _regionStatusRight) {
		return msg.lines
	}
	f := a.rises[msg.id]
	shown := len(msg.lines) > 0
	if f == nil {
		f = &pillFade{}
		a.rises[msg.id] = f
	}
	if f.shown != shown {
		if shown {
			f.lines = msg.lines
		}
		f.from = pillAlpha(f, a.now())
		f.shown = shown
		f.at = a.now()
		a.wakeTick()
	} else if shown {
		f.lines = msg.lines
	}
	switch alpha := pillAlpha(f, a.now()); {
	case f.shown && alpha >= 1:
		return msg.lines
	case f.shown:
		return sunkCopy(msg.lines, 1-alpha)
	case alpha <= 0:
		return nil
	default:
		return sunkCopy(f.lines, 1-alpha)
	}
}

// statusRising reports whether a pill's appear or disappear is still on, so
// the tick keeps redrawing until the ramp is done.
func (a *App) statusRising() bool {
	now := a.now()
	for _, f := range a.rises {
		if d := now.Sub(f.at); d >= 0 && d < _riseDuration {
			return true
		}
	}
	return false
}

// settlePills marks every statusline pill settled shown: a test's pinned
// clock would freeze a pill's first fade mid-ramp.
func (a *App) settlePills() {
	for _, w := range a.widgets {
		if w.region == _regionStatusLeft || w.region == _regionStatusRight {
			if a.rises[w.id] == nil {
				a.rises[w.id] = &pillFade{shown: true, at: a.now().Add(-_riseDuration), from: 1}
			}
		}
	}
}

// sunkCopy is rows with every segment sunk toward the surface it sits on.
func sunkCopy(rows [][]hostapi.Segment, fade float64) [][]hostapi.Segment {
	out := make([][]hostapi.Segment, len(rows))
	for i, row := range rows {
		out[i] = make([]hostapi.Segment, len(row))
		for j, s := range row {
			s.Fade = max(s.Fade, fade)
			out[i][j] = s
		}
	}
	return out
}

// smoothstep is the pill fades' easing.
func smoothstep(p float64) float64 { return p * p * (3 - 2*p) }

// widgetRegion is the region a widget draws in.
func (a *App) widgetRegion(id string) (region, bool) {
	w := a.widgetByID(id)
	if w == nil {
		return _regionNone, false
	}
	return w.region, true
}

// renderStaleWidgetsCmd renders, as ui-class requests, the widgets rendered
// at another size and, unless sizeOnly, those never rendered or stale for the
// current frame generation.
func (a *App) renderStaleWidgetsCmd(sizeOnly bool) tea.Cmd {
	gen := a.frameGen
	var stale []shownWidget
	for _, w := range a.shownWidgets() {
		c := a.caches[w.ID]
		if c == nil && sizeOnly || c != nil && c.width == w.width && c.height == w.height && (sizeOnly || c.gen == gen) {
			continue
		}
		// One render per widget in flight: at 20 fps a slow widget would
		// otherwise pile up a queue of renders of the same generation.
		key := renderKey{gen: gen, width: w.width, height: w.height}
		if a.pending[w.ID] == key {
			continue
		}
		if a.pending == nil {
			a.pending = map[string]renderKey{}
		}
		a.pending[w.ID] = key
		stale = append(stale, w)
	}
	if len(stale) == 0 {
		return nil
	}
	st := a.uiState()
	a.renderSeq++
	seq := a.renderSeq
	return func() tea.Msg {
		out := make(widgetsRenderedMsg, 0, len(stale))
		for _, w := range stale {
			screen := a.host.ScreenState(w.width, w.height, st)
			lines, more, err := a.renderWidget(w, screen)
			out = append(out, widgetRender{id: w.ID, lines: lines, more: more, width: w.width, height: w.height, gen: gen, seq: seq, err: err})
		}
		return out
	}
}

// widgetLines returns the last-good cached lines of a widget (the render
// itself happens on the queue, never inside View, DESIGN §4).
func (a *App) widgetLines(id string) [][]hostapi.Segment {
	c := a.caches[id]
	if c == nil {
		return nil
	}
	return c.lines
}

// toasts

// toast is a transient notification (zemi.notify, DESIGN §6.1).
type toast struct {
	level       slog.Level
	title, body string
	expires     time.Time
	recorded    bool // its caller recorded the whole error in /errors
}

// _toastRows caps a toast's body and _toastsLive the toasts shown at once: a
// long error, a stack trace or a burst of them would cover the transcript.
const (
	_toastRows  = 3
	_toastsLive = 5
)

// boxArea is what a modal box centers over: the stage (the shipped layout's
// holds the chat, so a box centers on the chat's column), or the
// w by h screen when the stage is too small for a prompt.
func boxArea(stage uv.Rectangle, w, h int) uv.Rectangle {
	if stage.Dx() < 40 || stage.Dy() < 8 {
		// NOTE: no box fits a stage under 8 rows, so it covers the screen, composer included
		return uv.Rect(0, 0, w, h)
	}
	return stage
}

func (a *App) pushToast(level slog.Level, title, body string) {
	title = hostapi.ToastTitle(level, title)
	// news goes quickly, what needs reading or acting on stays
	ttl := 8 * time.Second
	if level < slog.LevelWarn {
		ttl = 4 * time.Second
	}
	t := toast{level: level, title: title, body: body, expires: time.Now().Add(ttl)}
	short, _, _ := strings.Cut(body, "stack traceback:")
	t.body = hostapi.ShortChunk(strings.TrimRight(short, " \t\n"))
	t.recorded = level == slog.LevelError && a.host.Recorded(body)
	// the same notice again (a second send with no model) restarts its row
	// instead of adding a twin
	a.toasts = slices.DeleteFunc(a.toasts, func(o toast) bool { return o.level == level && o.title == title && o.body == t.body })
	a.toasts = append(a.toasts, t)
	if len(a.toasts) > _toastsLive {
		a.toasts = a.toasts[len(a.toasts)-_toastsLive:]
	}
	a.frameDirty = true
}

func (a *App) pruneToasts(now time.Time) {
	live := len(a.toasts)
	a.toasts = slices.DeleteFunc(a.toasts, func(t toast) bool { return !now.Before(t.expires) })
	if len(a.toasts) != live {
		a.frameDirty = true
	}
}

// hook plumbing (drained by a persistent cmd when a program is attached)

// dispatch drops msg when the queue is full, so a caller on the Update
// goroutine never waits on itself; it reports whether msg was delivered.
func (a *App) dispatch(msg tea.Msg) bool {
	q := a.hookQueue()
	if q == nil {
		a.Update(msg)
		return true
	}
	select {
	case q <- msg:
		return true
	default:
		a.log.Warn("hook queue full; dropping message", "origin", "ui")
		return false
	}
}

// await posts fn for a blocking hook (AskPermission and the like), which
// never runs on the Update goroutine: its caller waits on the answer, so a
// dropped prompt would hang it. It waits for room instead.
func (a *App) await(fn func() tea.Cmd) {
	if q := a.hookQueue(); q != nil {
		q <- uiFunc(fn)
		return
	}
	a.Update(uiFunc(fn))
}

// hookQueue is where posted messages wait for the Update goroutine; nil
// without a program or captured hooks, where they run inline.
func (a *App) hookQueue() chan tea.Msg {
	if a.program != nil {
		return a.hooks
	}
	return a.testQueue
}

func (a *App) drainHooksCmd() tea.Cmd {
	return func() tea.Msg { return hookMsg{inner: <-a.hooks} }
}

// compose (pure: reads caches and Go-side state only)

func (a *App) compose(w, h int) string {
	if !a.host.AgentActive() {
		a.cancelling = false
	}
	clear(a.prefixes)
	clear(a.fadeKeys)

	a.syncChatScroll()
	lay := a.layout(w, h)
	stage := lay.stage
	a.scrollIDs, a.scrollDef = a.scrollIDs[:0], lay.scroll
	a.syncFollow()
	a.renderMissingWidgets()
	rows := a.paint(lay.root)

	pop := a.floatPopup(rows, lay, w)
	if pills := a.pills(); !lay.bar && len(pills) > 0 {
		// without a statusline the alarms still show
		placeBox(rows, [][]hostapi.Segment{joinItems(pills, false)}, 0, h-1, w)
	}

	// Modal chrome goes over everything: Lua overlays, then the Go-owned
	// prompts, bottom to top (topChrome must match this order so input
	// reaches the box drawn on top). What lies under the top box sinks toward
	// the canvas, so nothing beside it passes for its chrome (DESIGN §8).
	area := boxArea(stage, w, h)
	frame := lay.composer
	if frame.Empty() {
		frame = area
	}
	framed := ruleAt(rows, frame)
	var modals []func()
	a.chromeRect = uv.Rectangle{}
	splice := func(box [][]hostapi.Segment) {
		modals = append(modals, func() { a.chromeRect = spliceBox(rows, box, area, w) })
	}
	for i := range a.overlays {
		if i+1 < len(a.overlays) && (a.overlays[i+1].back || a.overlays[i].o.Follow) {
			continue // a view drawn over a browse replaces the level below it
		}
		ov := &a.overlays[i]
		top := i == len(a.overlays)-1
		if ov.o.Anchor != "" {
			box, right := a.overlayBox(ov, area.Dx(), area.Dy(), top), ov.o.Anchor == hostapi.OverlayBottomRight
			modals = append(modals, func() { a.chromeRect = anchorBottom(rows, box, area, frame, framed, right, hostapi.ChromeRule, w) })
		} else {
			splice(a.overlayBox(ov, area.Dx(), area.Dy(), top))
		}
	}
	if a.trust != nil {
		splice(trustBox(a.trust, area.Dx(), area.Dy()))
	}
	if a.bootErr != nil {
		splice(a.bootBox(area.Dx(), area.Dy()))
	}
	if a.question != nil {
		splice(a.questionBox(a.question, area.Dx(), area.Dy()))
	}
	if a.planAsk != nil {
		splice(a.planBox(a.planAsk, area.Dx(), area.Dy()))
	}
	if a.permAsk != nil {
		splice(a.permBox(a.permAsk, area.Dx(), area.Dy()))
	}
	// The toasts are one tray at the top of the stage, over every window: a
	// notification is the one thing that must never sit behind a box, so it
	// draws last and stays undimmed. It hugs the stage's right edge, three cells
	// over it, the rest in the margin, and stays inside the screen. A toast
	// covers its own cells, so no box border takes a gap where one stands on it.
	toasts := func() {
		tray := toastTray(a.toasts, stage.Dx(), stage.Dy())
		if len(tray) == 0 {
			return
		}
		x := max(min(stage.Max.X-3, w-hostapi.SegsWidth(tray[0])-1), 0)
		for i, line := range tray {
			placeBox(rows, [][]hostapi.Segment{line}, x, stage.Min.Y+i, w)
		}
	}
	for i, place := range modals {
		if i == len(modals)-1 {
			// the backdrop joins before it fades, so no faded line joins the top box
			a.join(rows, w)
			rows = a.fadeRows(rows)
		}
		place()
	}
	// after the join: a box the tray stands beside must not bend its border
	// into it, and the tray must not be joined into the box's
	a.join(rows, w)
	toasts()

	a.lastRows, a.stage, a.popRect, a.panes = rows, stage, pop, lay.panes
	a.highlightSelection(rows, w)
	a.highlightHover(rows)

	var b strings.Builder
	a.spun = false
	for i, row := range rows {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(a.renderSegs(row, w))
	}
	out := b.String()
	a.frameMu.Lock()
	a.lastFrame = out
	a.frameMu.Unlock()
	a.drawn, a.frameDirty = renderKey{gen: a.frameGen, width: w, height: h}, false
	return out
}

// chatRow is the transcript row the reader is at: the message a jump placed
// the view at while it still sits there, the last one the view draws at the
// transcript's end, else the row over its middle.
func (a *App) chatRow() int {
	chat := a.scrolls[a.scrollDef]
	switch {
	case chat == nil || len(chat.lines) == 0:
		return 0
	case chat.focus > 0 && chat.top == chat.placed:
		return chat.focus
	case chat.follow || chat.top+chat.viewH >= chat.rows:
		return drawnRow(chat.lines, len(chat.lines)-1, -1, -1)
	}
	mid := chat.top + max(chat.viewH-1, 0)/2
	if mid < 0 || mid >= len(chat.lines) {
		return 0
	}
	if row := drawnRow(chat.lines, mid, -1, 1); row > 0 {
		return row
	}
	// nothing at or below the middle names a row (the view's end is blanks,
	// a live rail, an unanchored tail): the nearest one above it is the place
	return drawnRow(chat.lines, mid-1, -1, -1)
}

// drawnRow is the transcript row of the line at from walking by step until
// one names the reader's place: a call of a rail, else a row that names a
// unit (a message, a run's digest, a step's group row). A rail's air rows
// carry the run's first anchor but stand between calls, and a live call's row
// is no place yet, so neither stops the walk.
func drawnRow(lines [][]hostapi.Segment, from, stop, step int) int {
	for i := from; i != stop && i >= 0 && i < len(lines); i += step {
		row := lines[i]
		if bareBar(row) {
			continue
		}
		if c := rowCall(row); c != 0 {
			if c > 0 {
				return c
			}
			continue // a live call's row: not the reader's place
		}
		if anchor := rowAnchor(row); anchor > 0 {
			return anchor
		}
	}
	return 0
}

// Screenshot implements hostapi.UIHooks: the screen as plain text, one
// line per row with the styling and trailing padding dropped. It reads the
// frame compose published, so the interpreter can ask what zemi looks like
// without the UI goroutine waiting on it or the other way round.
func (a *App) Screenshot() string {
	a.frameMu.Lock()
	frame := a.lastFrame
	a.frameMu.Unlock()
	if frame == "" {
		return ""
	}
	var b strings.Builder
	for i, row := range strings.Split(frame, "\r\n") {
		if i > 0 {
			b.WriteString("\n")
		}
		line := ansi.Strip(row)
		b.WriteString(strings.TrimRight(line, " "))
	}
	return b.String()
}

// mapRows copies rows through f: rows share slices with widget caches. An
// image placeholder keeps its color, which names the image.
func mapRows(rows [][]hostapi.Segment, f func(hostapi.Segment) hostapi.Segment) {
	for i, r := range rows {
		out := make([]hostapi.Segment, len(r))
		for j, s := range r {
			if strings.ContainsRune(s.Text, kitty.Placeholder) {
				out[j] = s
				continue
			}
			out[j] = f(s)
		}
		rows[i] = out
	}
}

// dimRows repaints rows in the one Host:Dim style, as text that joins no line.
// A dim segment keeps the edge fade the row came with, so the backdrop under
// a modal keeps its depth.
func dimRows(rows [][]hostapi.Segment) {
	mapRows(rows, func(s hostapi.Segment) hostapi.Segment {
		return hostapi.Segment{Text: s.Text, HL: hostapi.ChromeDim, Fade: s.Fade}
	})
}

// _modalFade is how far the backdrop under a box sinks toward the canvas: its
// content stays readable as a shape, and no color of it competes with the box.
const _modalFade = 0.7

// fadeRows sinks rows toward the canvas for the backdrop of a modal box: a
// cell keeps its text, style and effect, so a spinner behind the box keeps
// stepping, and nothing that acts or joins, so no backdrop row answers the
// box's prompt, no backdrop line joins its border and no backdrop bar takes a
// drag. A row a box drew on its panel sinks to the canvas with the rest: the
// backdrop is one surface. The message a following list picked stays lit: the
// map dims the chat around what it is looking at, which is why it can be seen.
// A rail level's pick is one call and the run's first action carries the run's
// own anchor, so there the pick matches by call alone; any other pick lights
// the whole unit its anchor draws. It returns the rows: a region's rows share
// slices with the widget cache, so a faded row is a copy, never written in
// place.
func (a *App) fadeRows(rows [][]hostapi.Segment) [][]hostapi.Segment {
	out := make([][]hostapi.Segment, len(rows))
	for i, row := range rows {
		if a.highlight != 0 && (rowCall(row) == a.highlight || rowAnchor(row) == a.highlight) {
			out[i] = row
			continue
		}
		faded := make([]hostapi.Segment, len(row))
		for k, s := range row {
			faded[k] = hostapi.Segment{
				Text: s.Text, HL: s.HL, Over: s.Over, Effect: s.Effect,
				NoCopy: s.NoCopy, NoHover: s.NoHover, Soft: s.Soft, Pin: s.Pin,
				Fill: s.Fill, Fade: max(s.Fade, _modalFade),
			}
		}
		out[i] = faded
	}
	return out
}

// floatPopup draws the open composer popup, the palette or the completion
// list, over rows, in line with the composer: standing on its frame like an
// anchored overlay (anchorBottom) in the draft's rule tint, in the bottom
// rows of the stage when the stage is above the composer, else beside the
// composer on the side with more rows. It covers only its own cells and ends
// in its hint row; each list refits every frame it has rows. It returns the
// cells it covers.
func (a *App) floatPopup(rows [][]hostapi.Segment, lay screenLayout, w int) uv.Rectangle {
	h := len(rows)
	var render func(maxW, room, minW int) [][]hostapi.Segment
	var size *boxSize
	var hints []hintPair
	var title string
	switch {
	case a.palette.open:
		if a.palette.count() > 0 {
			a.palette.size = boxSize{} // the list refits every frame: erasing a letter narrows the popup back
		}
		render, size, title = a.paletteRows, &a.palette.size, "Commands"
		hints = []hintPair{{key: "enter", verb: "run"}, {key: "tab", verb: "complete"}, {key: "esc", verb: "close"}}
		if a.palette.count() == 0 { // enter still runs the typed line
			hints = slices.Delete(hints, 1, 2)
		}
	case a.completion.shown():
		if a.completion.count() > 0 {
			a.completion.size = boxSize{} // the list refits every frame: erasing a token narrows the popup back
		}
		render, size, title = a.completionRows, &a.completion.size, a.completion.title()
		hints = []hintPair{{key: "enter", verb: "pick"}, {key: "esc", verb: "close"}}
		if a.completion.count() == 0 {
			hints = hints[1:]
		}
	default:
		return uv.Rectangle{}
	}
	stage, c := lay.stage, lay.composer
	x, width, top, bottom := c.Min.X, c.Dx(), stage.Min.Y, min(stage.Max.Y, h)
	up := true
	if stage.Max.Y > c.Min.Y {
		top, bottom = 0, c.Min.Y
		if up = c.Min.Y >= h-c.Max.Y; !up {
			top, bottom = c.Max.Y, h
		}
	}
	// the borders, and the blank row and hint under the list
	if bottom-top < 5 {
		return uv.Rectangle{}
	}
	maxW := boxInner(width)
	size.w = max(size.w, _listBoxW)
	body := append(render(maxW, bottom-top-4, size.w), nil, hint(maxW, hints...))
	box := promptBox(title, "", body, width, bottom-top, 100, size)
	if up {
		_, _, rule := a.draftMode()
		return anchorBottom(rows, box, uv.Rect(x, top, width, bottom-top), c, ruleAt(rows, c), false, cmp.Or(rule, hostapi.ChromeRule), w)
	}
	// NOTE: below the composer the popup covers the statusline row: there is
	// no rule under the frame for it to stand on
	placeBox(rows, box, x, top, w)
	return uv.Rect(x, top, hostapi.SegsWidth(box[0]), len(box))
}

// rowsBelow counts the rows under the default scrolled region's viewport
// while it is off the tail: the bar shows where the reader is, not how many
// rows a running turn has added below or which key goes back.
func (a *App) rowsBelow() int {
	st := a.scrolls[a.scrollDef]
	if st == nil || st.follow {
		return 0
	}
	return max(0, st.rows-st.top-st.viewH)
}

// scrolledUp reports whether the region a widget draws in is off its tail.
func (a *App) scrolledUp(widget string) bool {
	r, ok := a.widgetRegion(widget)
	for _, st := range a.scrolls {
		if ok && st.region == r && !st.follow {
			return true
		}
	}
	return false
}

// leaveTail takes the scrolled region a widget draws in off its tail, so
// the rows a click has it redraw (a digest opening over its rail, a thought
// unrolling) grow under the reader's place instead of scrolling it away.
func (a *App) leaveTail(w *widget) {
	if w != nil {
		for _, st := range a.scrolls {
			if st.region == w.region {
				st.follow, st.focus = false, 0
			}
		}
	}
}

// scrollAt is the scrolled region drawn under (x, y), else the default one. A
// bar one cell past a region's rect is that region's own.
func (a *App) scrollAt(x, y int) region {
	for _, id := range a.scrollIDs {
		st := a.scrolls[id]
		if st == nil {
			continue
		}
		if (uv.Position{X: x, Y: y}).In(st.rect) ||
			x == st.rect.Max.X && y >= st.rect.Min.Y && y < st.rect.Max.Y {
			return id
		}
	}
	return a.scrollDef
}

// scrollRows moves a scrolled region's viewport by n rows (negative =
// older). Scrolling down re-arms tail follow at the bottom of a "bottom"
// region; while scrolled up the view stays anchored as new lines arrive.
func (a *App) scrollRows(id region, n int) {
	delete(a.pendingScroll, id)
	st := a.scrolls[id]
	if st == nil {
		return
	}
	// scrolling to the end says the reader is at the newest content, so the
	// place a jump left is no more; short of it the place holds while its
	// message stays on screen, and the list finds it there
	if n > 0 && st.follow {
		st.focus = 0
		return
	}
	st.scroll(n)
	st.follow = st.top+st.viewH >= st.rows
	if st.follow {
		st.focus = 0
	}
	a.bump()
}

// followStage pins the default scrolled region back to its tail (a submit).
func (a *App) followStage() {
	delete(a.pendingScroll, a.scrollDef)
	if st := a.scrolls[a.scrollDef]; st != nil {
		st.follow, st.focus = true, 0
	}
}

// syncChatScroll keeps the chat's rows and scroll state with the view that
// drew them: the session's place parks while a run's transcript draws in its
// place, a transcript starts at its tail, and the rows cached are always the
// view on screen's. Every way in or out of a run's view ends in a scroll
// action or a repaint, so those two are the only sync points.
func (a *App) syncChatScroll() {
	v := a.host.ViewedAgent()
	if v == a.chatView {
		return
	}
	chat := _regionNone
	for _, id := range a.scrollIDs { // last paint's order, so the pick is stable
		if st := a.scrolls[id]; st != nil && st.region == _regionChat {
			chat = id
			break
		}
	}
	if chat == _regionNone {
		return // no chat region drew yet: the sync waits for the frame that does
	}
	fromSession := a.chatView == ""
	a.chatView = v
	// the cached rows are the other view's: dropping them draws this one whole
	// in the frame the switch lands on, as a first frame does, instead of
	// painting a transcript the reader is not on and anchoring the parked
	// place against its rows
	delete(a.caches, "chat")
	// an edge or a jump parked for the outgoing view must not fire into the
	// incoming one
	delete(a.pendingScroll, chat)
	delete(a.pendingAnchor, chat)
	if v != "" {
		if fromSession { // one view to another keeps parking the session's
			a.chatScroll = a.scrolls[chat]
		}
		delete(a.scrolls, chat)
		return
	}
	if a.chatScroll != nil {
		a.scrolls[chat] = a.chatScroll
		a.chatScroll = nil
	} else {
		delete(a.scrolls, chat) // no place was left: the session follows its tail
	}
}

// applyScroll maps the zemi.ui.scroll.* host actions onto a scrolled
// region, the default one for regionNone: up and down move a page with a
// one-row overlap, top and bottom go to the ends, line_up and line_down a
// row. An edge also holds as the region's pending target, so it lands on
// rows that arrive after it or a region not drawn yet.
func (a *App) applyScroll(kind hostapi.ScrollAction, id region) {
	a.syncChatScroll()
	if id == _regionNone {
		id = a.scrollDef
	}
	st := a.scrolls[id]
	if kind == hostapi.ScrollTop || kind == hostapi.ScrollBottom {
		defer func() { a.pendingScroll[id] = kind }()
	}
	if st == nil {
		return
	}
	page := max(st.viewH-1, 1)
	switch kind {
	case hostapi.ScrollUp:
		a.scrollRows(id, -page)
	case hostapi.ScrollDown:
		a.scrollRows(id, page)
	case hostapi.ScrollTop:
		a.scrollRows(id, -max(st.rows, 1))
	case hostapi.ScrollBottom:
		a.scrollRows(id, max(st.rows, 1))
	case hostapi.ScrollLineUp:
		a.scrollRows(id, -1)
	case hostapi.ScrollLineDown:
		a.scrollRows(id, 1)
	case hostapi.ScrollStickyUp:
		for i := min(st.top, len(st.lines)) - 1; i >= 0; i-- {
			if headerAt(st.lines, i) {
				a.scrollRows(id, i-st.top)
				return
			}
		}
	case hostapi.ScrollStickyDown:
		for i := st.top + 1; i < len(st.lines); i++ {
			if headerAt(st.lines, i) {
				a.scrollRows(id, i-st.top)
				return
			}
		}
		a.scrollRows(id, max(st.rows, 1))
	}
}

// headerAt says row i starts a header: a turn's or a section's, which the
// row above does not continue.
func headerAt(lines [][]hostapi.Segment, i int) bool {
	p := pinOf(lines[i])
	return (p == hostapi.PinTurn || p == hostapi.PinSection) && (i == 0 || pinOf(lines[i-1]) != p)
}

// chatFocusEnter gives the chat the keys (tab on a bare composer): the
// cursor takes the message it held last time, or the one at the viewport's
// bottom row a screen-tenth up when it never had the keys.
func (a *App) chatFocusEnter() tea.Cmd {
	units := a.chatUnits()
	if len(units) == 0 {
		return nil
	}
	sel := 0
	if a.chatSelMemo > 0 {
		if at, ok := slices.BinarySearch(units, a.chatSelMemo-1); ok {
			sel = a.chatSelMemo
		} else if at > 0 { // the memo's row merged into an earlier unit (a /reset)
			sel = units[at-1] + 1
		}
	}
	// the memo comes back only while the reader still sees it: scrolled off,
	// tab takes what the view ends on and the old place is forgotten
	if sel > 0 && !inView(a.scrolls[a.scrollDef], sel) {
		a.chatSelMemo, sel = 0, 0
	}
	if sel < 1 {
		if s := a.scrolls[a.scrollDef]; s != nil && len(s.lines) > 0 {
			// start a screen-tenth above the bottom so j still walks down
			// and the band does not sit against the composer
			bottom := min(s.top+s.viewH, len(s.lines)) - 1 - s.viewH/10
			for i := bottom; i >= max(s.top, 0) && sel == 0; i-- {
				sel = rowAnchor(s.lines[i])
			}
		}
		if sel > 0 {
			// the row's anchor names a transcript row the cursor groups into a
			// block with its run or burst: take the block the anchor falls in
			at, ok := slices.BinarySearch(units, sel-1)
			if !ok {
				at--
			}
			if at >= 0 {
				sel = units[at] + 1
			}
		}
		if sel < 1 {
			sel = units[len(units)-1] + 1
		}
	}
	a.chatFocus, a.chatSel = true, sel
	a.bump()
	return nil
}

// inView says the selection's rows meet the chat's viewport: a unit taller
// than the view still counts while any of it shows.
func inView(st *scrollState, sel int) bool {
	if st == nil || st.viewH < 1 {
		return false
	}
	from, to := unitSpan(st.lines, sel)
	return from >= 0 && to >= from && to >= st.top && from < st.top+st.viewH
}

// chatUnits lists the transcript rows the cursor stops on, ascending: each
// drawing unit's first row, a run of activity rows or a burst of notices
// being one block, every other row its own. The live turn's own run draws
// with its run's anchor, not a transcript row's, so the cursor cannot take
// it while the turn runs.
func (a *App) chatUnits() []int {
	st := a.host.ScreenState(a.width, a.height, a.uiState())
	var out []int
	for i := 0; i < len(st.Transcript); {
		out = append(out, i)
		k := chatKind(st.Transcript[i])
		for i++; i < len(st.Transcript) && k != 0 && chatKind(st.Transcript[i]) == k; i++ {
		}
	}
	if n := len(out); n > 0 && st.AgentActive && st.Agent.Text == "" &&
		chatKind(st.Transcript[out[n-1]]) == 1 {
		out = out[:n-1]
	}
	return out
}

// chatKind groups the transcript rows into units the way the chat draws
// them: 1 a run of activity rows, 2 a burst of notices, 0 a row alone.
func chatKind(m hostapi.ChatMessage) int {
	switch {
	case activityKind(m):
		return 1
	case m.Kind == hostapi.RowNotice:
		return 2
	default:
		return 0
	}
}

// chatCursorKey is one key on the focused chat: j, k and the arrows walk the
// messages, y copies, e opens the editor, tab and esc hand the keys back to
// the composer. A key the cursor does not take is ignored.
func (a *App) chatCursorKey(key string) (tea.Cmd, bool) {
	units := a.chatUnits()
	at := slices.Index(units, a.chatSel-1)
	moved := false
	switch key {
	case "j", "down":
		if at >= 0 && at+1 < len(units) {
			a.chatSel, moved = units[at+1]+1, true
		}
	case "k", "up":
		if at > 0 {
			a.chatSel, moved = units[at-1]+1, true
		}
	case "g", "home":
		if len(units) > 0 {
			a.chatSel, moved = units[0]+1, true
		}
	case "G", "end":
		if len(units) > 0 {
			a.chatSel, moved = units[len(units)-1]+1, true
		}
	case "y", "e":
		row := chatCopyText(a.host.ScreenState(a.width, a.height, a.uiState()).Transcript[a.chatSel-1])
		if key == "y" {
			return a.copied(row), true
		}
		return a.editInEditor(row, "the message", false), true
	case "enter", "space", "l", "h":
		a.chatFoldKey(key)
	case "tab", "esc":
		a.chatSelMemo = a.chatSel
		a.chatFocus = false
	case "shift+up":
		a.applyScroll(hostapi.ScrollLineUp, _regionNone)
	case "shift+down":
		a.applyScroll(hostapi.ScrollLineDown, _regionNone)
	case "pgdown":
		a.applyScroll(hostapi.ScrollDown, _regionNone)
	case "pgup":
		a.applyScroll(hostapi.ScrollUp, _regionNone)
	case "ctrl+d", "ctrl+u":
		// the pair walks the cursor a view down or up, the view following it,
		// as it does on the composer where the same keys scroll
		dir, page, lines := 1, 1, [][]hostapi.Segment(nil)
		if key == "ctrl+u" {
			dir = -1
		}
		if st := a.scrolls[a.scrollDef]; st != nil {
			page, lines = max(st.viewH, 1), st.lines
		}
		if next, ok := chatPage(units, at, dir, page, lines); ok {
			a.chatSel, moved = next, true
		}
	default:
		return nil, false
	}
	if moved {
		a.followChatSel()
	}
	a.bump()
	return nil, true
}

// chatPage is the unit a page of scrolling lands the cursor on: it walks the
// units in dir until a viewport's worth of their rows is spent, so the cursor
// goes with the view instead of leaving it.
func chatPage(units []int, at, dir, page int, lines [][]hostapi.Segment) (int, bool) {
	if at < 0 {
		return 0, false
	}
	land := at
	for n := at + dir; n >= 0 && n < len(units) && page > 0; n += dir {
		land = n
		if from, to := unitSpan(lines, units[n]+1); from >= 0 && to >= from {
			page -= to - from + 1
		} else {
			page-- // a unit the fill has not built yet counts as one row
		}
	}
	return units[land] + 1, land != at
}

// chatFoldKey works the fold the selected message carries, what its click
// does: enter and space toggle it, l opens it, h folds it back. A message that
// folds nothing takes none of them.
func (a *App) chatFoldKey(key string) {
	w := a.widgetByID("chat")
	if w == nil || w.onClick == nil {
		return
	}
	st := a.scrolls[a.scrollDef]
	if st == nil {
		return
	}
	from, to := unitSpan(st.lines, a.chatSel)
	if from < 0 {
		return
	}
	for _, row := range st.lines[from : to+1] {
		for _, seg := range row {
			// a run's digest and a long prompt carry their fold the same way
			if !strings.HasPrefix(seg.Click, "turn:") && !strings.HasPrefix(seg.Click, "prompt:") {
				continue
			}
			// the glyph says the fold's state: ▸ folded, ▾ open. A prompt's
			// head rows carry the click without one
			open := strings.HasPrefix(seg.Text, "▾")
			if !open && !strings.HasPrefix(seg.Text, "▸") {
				continue
			}
			if key == "h" && !open || key == "l" && open {
				return
			}
			w.onClick(a, map[string]any{"id": seg.Click})
			return
		}
	}
}

// chatCopyText is what y copies: the message's text, a tool row's output
// tail under it; a notice's hint never copies, the way a drag does not.
func chatCopyText(m hostapi.ChatMessage) string {
	if m.Output != "" {
		return strings.TrimSpace(m.Text) + "\n" + m.Output
	}
	return m.Text
}

// followChatSel puts the selected message in view: the paint's jump lands it
// once its rows are drawn, and a page's scroll drags the window toward one
// the chat has not built yet, so the next move lands.
func (a *App) followChatSel() {
	a.syncChatScroll()
	st := a.scrolls[a.scrollDef]
	if st == nil {
		return
	}
	if from, _ := unitRows(st.lines, a.chatSel); from >= 0 {
		a.pendingAnchor[a.scrollDef] = a.chatSel
		return
	}
	page := max(st.viewH-1, 1)
	drawn := 0
	for _, row := range st.lines {
		if at := rowAnchor(row); at > drawn {
			drawn = at
		}
	}
	if a.chatSel > drawn {
		a.scrollRows(a.scrollDef, page)
	} else {
		a.scrollRows(a.scrollDef, -page)
	}
}

func (a *App) regionLines(r region, width, clip int) [][]hostapi.Segment {
	var out [][]hostapi.Segment
	for _, wgt := range a.regionWidgets(r) {
		c := a.caches[wgt.ID]
		if c == nil {
			continue
		}
		lines := c.lines
		if !wgt.Wrap {
			rows := widgetRows(lines, false, width)
			// a widget that pads to its region height (the splash) holds rows
			// laid out for an older, taller one until its re-render lands: cut
			// them, so the stale rows are not overflow for the view to scroll
			if clip > 0 && c.height > clip && len(rows) == c.height {
				rows = rows[:clip]
			}
			out = append(out, rows...)
			continue
		}
		// Word-wrap instead of truncating; the transcript shows the
		// wrapped tail. The rows change only when the widget renders again
		// or the width does.
		if c.wrapped == nil || c.wrapW != width {
			if c.wrapW != width {
				c.lineRows = nil
			}
			rows := make([][]hostapi.Segment, 0, c.wrapN)
			next := c.spare
			if next == nil {
				next = make(map[*hostapi.Segment][][]hostapi.Segment, len(lines))
			}
			clear(next)
			for _, line := range lines {
				if len(line) == 0 {
					rows = append(rows, hostapi.WrapSegs(line, width)...)
					continue
				}
				wr, ok := c.lineRows[&line[0]]
				if !ok {
					wr = hostapi.WrapSegs(line, width)
				}
				next[&line[0]] = wr
				rows = append(rows, wr...)
			}
			c.wrapped, c.wrapW, c.lineRows, c.spare, c.wrapN = rows, width, next, c.lineRows, len(rows)
		}
		// the chat is alone in its region: its rows are not copied per frame
		if out == nil {
			out = slices.Clip(c.wrapped)
			continue
		}
		out = append(out, c.wrapped...)
	}
	return out
}

// widgetRows maps one widget's lines to the rows the layout draws them as: a
// wrapping widget word-wrapped at width, the rest tab-expanded and cut to it.
func widgetRows(lines [][]hostapi.Segment, wrap bool, width int) [][]hostapi.Segment {
	var out [][]hostapi.Segment
	if wrap {
		for _, line := range lines {
			out = append(out, hostapi.WrapSegs(line, width)...)
		}
		return out
	}
	for _, line := range expandTabs(lines) {
		out = append(out, truncateSegs(line, width))
	}
	return out
}

// expandTabs turns tabs into four spaces, in place: a tab has no cell width,
// so layout and drawing would both lose it. A wrapped widget needs none,
// WrapSegs expands its own.
func expandTabs(lines [][]hostapi.Segment) [][]hostapi.Segment {
	for _, line := range lines {
		for j := range line {
			line[j].Text = strings.ReplaceAll(line[j].Text, "\t", "    ")
		}
	}
	return lines
}

// pills are the Go-drawn statusline notes: what reports a broken Lua layer
// (DESIGN §8), each a pointer to the command that deals with it. They sit
// innermost in the left cluster and go last when the row is short.
func (a *App) pills() []statusItem {
	// a failed boot leaves no composer to stand them on, and no Lua to run
	// the command a pill names: the boot box says it instead
	if a.bootErr != nil {
		return nil
	}
	var out []statusItem
	pill := func(noun, cmd string) {
		out = append(out, statusItem{segs: []hostapi.Segment{{Text: noun, HL: hostapi.GroupDanger, Click: cmd}, {Text: " /" + cmd, HL: hostapi.GroupDim, Click: cmd}}})
	}
	if a.host.RT.IsHot() {
		pill("stuck", "reload")
	}
	if a.host.HasUnseenErrors() {
		pill("errors", "errors")
	}
	return out
}

// statusRow builds the one-row statusline: the left cluster, then the right
// one aligned to the right edge (DESIGN §8).
func (a *App) statusRow(w int) []hostapi.Segment {
	left, right := a.statusClusters(w)
	out := append([]hostapi.Segment{}, left...)
	if gap := max(0, w-hostapi.SegsWidth(left)-hostapi.SegsWidth(right)); gap > 0 {
		out = append(out, hostapi.Segment{Text: strings.Repeat(" ", gap)})
	}
	return append(out, right...)
}

// statusItem is one statusline widget's row and its priority.
type statusItem struct {
	segs []hostapi.Segment
	pri  float64
}

// statusClusters are the statusline's two sides fitted into w cells, each
// drawn from its outer edge in priority order, so a lasting widget sits
// outermost and one that comes and goes never moves it: the left widgets and
// the pills left to right, the right widgets right to left.
func (a *App) statusClusters(w int) (left, right []hostapi.Segment) {
	leftItems := append(a.statusItems(_regionStatusLeft), a.pills()...)
	return fitClusters(leftItems, a.statusItems(_regionStatusRight), w)
}

// fitClusters joins the clusters, dropping the highest priority widget (the
// inner one of a tie) until they and a cell between them fit w. A last
// widget still too wide ends in "…".
func fitClusters(left, right []statusItem, w int) ([]hostapi.Segment, []hostapi.Segment) {
	for {
		l, r := joinItems(left, false), joinItems(right, true)
		if hostapi.SegsWidth(l)+hostapi.SegsWidth(r) < w || len(left)+len(right) == 1 {
			return ellipsizeSegs(l, w), ellipsizeSegs(r, w)
		}
		side, at := &left, -1
		for _, c := range []*[]statusItem{&left, &right} {
			for i := range slices.Backward(*c) {
				if at < 0 || (*c)[i].pri > (*side)[at].pri {
					side, at = c, i
				}
			}
		}
		*side = slices.Delete(slices.Clone(*side), at, at+1)
	}
}

// joinItems puts statusDot between items, in reverse for the right cluster.
func joinItems(items []statusItem, reverse bool) []hostapi.Segment {
	var out []hostapi.Segment
	for i := range items {
		if reverse {
			i = len(items) - 1 - i
		}
		if len(out) > 0 {
			out = append(out, _statusDot)
		}
		out = append(out, items[i].segs...)
	}
	return out
}

var _statusDot = hostapi.Segment{Text: "   ", HL: hostapi.GroupDim, NoCopy: true}

// statusItems are the widgets of r that draw, by priority.
func (a *App) statusItems(r region) []statusItem {
	var out []statusItem
	for _, wgt := range a.regionWidgets(r) {
		if lines := a.widgetLines(wgt.ID); len(lines) > 0 && hostapi.SegsWidth(lines[0]) > 0 {
			out = append(out, statusItem{lines[0], wgt.Priority})
		}
	}
	return out
}

// toastTray lays the toasts out in one box of a fixed width at most h rows
// tall, newest first: toasts of one level and title are one section, the
// title in the rule that opens it and each body word-wrapped under it. A
// lone title or body is a row in the level's color under an untitled rule.
func toastTray(ts []toast, w, h int) [][]hostapi.Segment {
	type section struct {
		level slog.Level
		title string
		rows  [][]hostapi.Segment
	}
	inner := max(min(44, w-6), 12)
	var secs []section
	for _, t := range slices.Backward(ts) {
		title, text, hl := t.title, t.body, "Normal"
		if (t.title == "") != (t.body == "") {
			title, text, hl = "", t.title+t.body, toastHL(t.level)
		}
		rows := wrapText(text, hl, inner)
		if len(rows) > _toastRows {
			more := []hostapi.Segment{{Text: hostapi.MoreLines(len(rows) - _toastRows + 1), HL: hostapi.ChromeHint}}
			if t.recorded {
				more = append(more, hostapi.Segment{Text: " /errors", HL: hostapi.ChromeHint})
			}
			rows = append(rows[:_toastRows-1], more)
		}
		i := slices.IndexFunc(secs, func(s section) bool { return s.level == t.level && s.title == title })
		if i < 0 {
			i, secs = len(secs), append(secs, section{level: t.level, title: title})
		}
		secs[i].rows = append(secs[i].rows, rows...)
	}
	var tray [][]hostapi.Segment
	for i, s := range secs {
		box := hostBox(s.title, toastHL(s.level), "", s.rows, inner+8, inner+4, inner)
		// a section that does not fit drops whole: a row-wise cut would leave
		// an orphaned bottom border under dropped rows
		if len(tray)+len(box) > h {
			break
		}
		if i > 0 {
			// the section's rule closes the one above
			top := box[0]
			top[0].Text, top[len(top)-1].Text = "├", "┤"
			tray = tray[:len(tray)-1]
		}
		tray = append(tray, box...)
	}
	// a toast sits over a box the user may be reading, so a drag across it
	// copies the box's own text and never two boxes at once
	for _, line := range tray {
		for i := range line {
			line[i].NoCopy = true
		}
	}
	return tray
}

func toastHL(level slog.Level) string {
	switch level {
	case slog.LevelWarn:
		return "ToastWarn"
	case slog.LevelError:
		return "ToastErr"
	default:
		return "ToastInfo"
	}
}

// segment helpers

// hlStyle resolves a highlight name. "Host:*" names are Go-owned security
// chrome (DESIGN §8): they resolve in the palette's roles, never through a group.
func (a *App) hlStyle(hl string) uv.Style {
	if st, ok := a.thm.chromeStyle(hl); ok {
		return st
	}
	return a.thm.style(hl)
}

func (a *App) segStyle(hl, over string, fill hostapi.Fill, fade float64, panel bool) uv.Style {
	st := a.hlStyle(hl)
	if over != "" {
		st = a.thm.hover(st, over)
	}
	if pal := hostapi.Whorl; pal != nil {
		switch fill {
		case hostapi.FillBand:
			st.Bg = pal.Band
		case hostapi.FillWell:
			st.Bg = pal.Well
		case hostapi.FillPanel:
			if sameColor(st.Bg, pal.Bg) {
				st.Bg = pal.Panel
			}
		case hostapi.FillAuto, hostapi.FillCanvas:
		}
		// a faded segment sinks toward the surface it sits on, its fill
		// included, before the colors are quantized
		if fade > 0 {
			to := pal.Bg
			if panel {
				to = pal.Panel
			}
			st.Fg = hostapi.Mix(cmp.Or(st.Fg, pal.Fg), to, fade)
			if st.Bg != nil {
				st.Bg = hostapi.Mix(st.Bg, to, fade)
			}
			if st.UnderlineColor != nil {
				st.UnderlineColor = hostapi.Mix(st.UnderlineColor, to, fade)
			}
		}
	}
	return toProfile(st, a.colors)
}

// sgrPrefix is the ANSI sequence a segment's style opens with, "" for the
// terminal's own. It is cached for the frame: compose styles thousands of
// segments and building the sequence for each is the biggest cost of
// drawing one.
func (a *App) sgrPrefix(hl, over string, fill hostapi.Fill, fade float64, panel bool) string {
	key := hl // no highlight name holds a NUL, and hover, a fill and a fade are the rare cases
	if over != "" {
		key = hl + "\x00" + over
	}
	if fill != hostapi.FillAuto {
		key += "\x00" + string(rune('0'+fill))
	}
	if fade > 0 {
		fk, ok := a.fadeKeys[fade]
		if !ok {
			// the same handful of fades (the modal's, the edge ramps') style
			// thousands of segments a frame under a box: format each once
			fk = strconv.FormatFloat(fade, 'f', 3, 64)
			a.fadeKeys[fade] = fk
		}
		key += "\x00" + fk
		if panel {
			key += "p"
		}
	}
	if p, ok := a.prefixes[key]; ok {
		return p
	}
	st := a.segStyle(hl, over, fill, fade, panel)
	p := ""
	if !st.IsZero() {
		p = st.String()
	}
	a.prefixes[key] = p
	return p
}

// renderSegs styles and joins segments, truncating to w visible cells.
func (a *App) renderSegs(segs []hostapi.Segment, w int) string {
	segs, spun := segEffects(a.thm, a.frame(), segs)
	a.spun = a.spun || spun
	// truncated before styling, so a link never leaves its escape sequence open
	if hostapi.SegsWidth(segs) > w {
		segs = truncateSegs(segs, w)
	}
	var b strings.Builder
	for _, s := range segs {
		if s.Text == "" {
			continue
		}
		p := a.sgrPrefix(s.HL, s.Over, s.Fill, s.Fade, s.OnPanel)
		if p != "" {
			b.WriteString(p)
		}
		if s.Link != "" {
			b.WriteString(ansi.SetHyperlink(s.Link))
		}
		b.WriteString(s.Text)
		if s.Link != "" {
			b.WriteString(ansi.ResetHyperlink())
		}
		if p != "" {
			b.WriteString(ansi.ResetStyle)
		}
	}
	return b.String()
}

func truncateSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	if hostapi.SegsWidth(segs) <= w {
		return segs
	}
	var out []hostapi.Segment
	used := 0
	for _, s := range segs {
		sw := ansi.StringWidth(s.Text)
		if used+sw <= w {
			out = append(out, s)
			used += sw
			continue
		}
		remaining := w - used
		if remaining > 0 {
			s.Text = ansi.Truncate(s.Text, remaining, "")
			out = append(out, s)
		}
		break
	}
	return out
}

// _blank is the run of spaces a padded row slices: a strings.Repeat per row
// costed a few hundred small allocations a frame.
var _blank = strings.Repeat(" ", 4096)

// padSeg is the segment padding a row out to w cells, or false when the row
// already fills them.
func padSeg(segs []hostapi.Segment, w int) (hostapi.Segment, bool) {
	pad := w - hostapi.SegsWidth(segs)
	if pad <= 0 {
		return hostapi.Segment{}, false
	}
	text := _blank[:min(pad, len(_blank))]
	if pad > len(_blank) { // not a width a terminal reports
		text = strings.Repeat(" ", pad)
	}
	// a Normal fill cell sits on the canvas, the palette's bg, instead of
	// leaving the terminal's background showing; a row that ends on a fill
	// (the prompt's slab, a band) keeps it out to the edge
	fill := hostapi.FillAuto
	if n := len(segs); n > 0 {
		fill = segs[n-1].Fill
	}
	return hostapi.Segment{Text: text, HL: "Normal", Fill: fill}, true
}

func padSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	p, ok := padSeg(segs, w)
	if !ok {
		return segs
	}
	out := make([]hostapi.Segment, 0, len(segs)+1)
	out = append(out, segs...)
	return append(out, p)
}
