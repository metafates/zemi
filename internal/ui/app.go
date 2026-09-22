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
	"errors"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

const (
	frameInterval = time.Second / 30
	// Terminals hide the OSC 9;4 spinner after a few seconds unless the
	// program keeps sending it, so it is resent on this interval while running.
	progressInterval = 2 * time.Second
	frameMS          = 50              // one animation tick: the mascot's frames are multiples of it
	doneLinger       = 3 * time.Second // how long the mascot bows after a turn
	toastTTL         = 4 * time.Second
	hookQueueSize    = 64
	chatWheelStep    = 1
)

// App is the root bubbletea model. It implements hostapi.UIHooks.
type App struct {
	host *hostapi.Host
	log  *logging.Logger
	thm  themeResolver

	// termBG is the terminal's own background color as it answered an OSC
	// 11 query; nil means the terminal does not answer, so zemi leaves its
	// background alone. paintedTermBG is the last color zemi set it to.
	termBG, paintedTermBG color.Color

	width, height int

	composer composerState

	histLoaded bool // the stored messages already seeded the composer history

	// Scrolled regions by layout node id (DESIGN §8 scrollback); scrollDef
	// is the one scroll actions without an id and the wheel outside every
	// scrolled region move, scrollIDs the ones drawn on the last frame.
	scrolls   map[string]*scrollState
	scrollDef string
	scrollIDs []string
	// focusables are the focusable nodes the last frame drew, floats last
	focusables []string

	completion fileCompleter
	palette    cmdPalette

	overlays []overlayState
	trust    *trustPrompt
	toasts   []toast

	// Go-owned agent chrome (DESIGN §6.3, §8): the permission overlay
	// (with a queue for concurrent asks) and the memory review overlay.
	permAsk   *permAskPrompt
	permQueue []*permAskPrompt
	memReview *memReviewPrompt
	memEdited string // the row the next memory screen opens on
	planAsk   *planAskPrompt

	mouse          mouseSel
	press          mousePress
	hovering       bool
	hoverX, hoverY int
	hoverKey       string              // what the pointer is over, to redraw only on change
	pointer        string              // the OSC 22 shape last set
	lastRows       [][]hostapi.Segment // the last composed frame, for clicks and copy
	stage          uv.Rectangle        // where the last frame's stage was
	popRect        uv.Rectangle        // the cells a completion popup took on the last frame
	chromeRect     uv.Rectangle        // the cells the topmost modal box took
	barDrag        string              // the scrolled region whose scrollbar the pointer holds

	// work runs Lua-backed keymaps and commands in order off the key loop
	work     chan func()
	workOnce sync.Once
	question *questionPrompt
	editing  string // the editor handoff in progress, shown in the working row

	prefixes   map[string]string // styled segment prefixes, rebuilt each frame
	caches     map[string]*widgetCache
	pending    map[string]int64 // widget id -> generation of the render in flight
	frameGen   int64
	timeStr    string
	activeBump time.Time
	turnActive bool             // what onFrame last saw of the agent
	turnEnd    time.Time        // when it last stopped, so the mascot can bow
	lastState  hostapi.UIState  // what the widgets last rendered with, the tick aside
	now        func() time.Time // animation clock; tests pin it
	cancelling bool             // interrupt sent, turn not finished yet
	progress   bool             // the terminal was told a turn is running (OSC 9;4)
	progressAt time.Time        // when the running sequence was last sent (progressInterval)
	focused    bool             // terminal focus (true until the terminal reports a blur)
	keySeq     []string         // keys of a bound sequence typed so far
	keySeqAt   time.Time
	quitArmed  time.Time // a second idle ctrl+c before this quits
	colors     colorprofile.Profile

	bootErr        error
	streamPending  atomic.Bool // a stream agentStreamMsg is queued
	refreshPending atomic.Bool // a Refresh is queued

	// the last composed frame, published for Screenshot: reading a
	// snapshot is what lets any goroutine ask what the screen says
	// without waiting on the Update goroutine.
	frameMu   sync.Mutex
	lastFrame string
	rects     map[string]hostapi.Rect

	program   *tea.Program
	reload    func()
	hooks     chan tea.Msg
	testQueue chan tea.Msg // CaptureHooks (tests): hooks wait here for Pump
}

// widgetCache holds the last-good rendered lines of one widget keyed by
// (id); the frame they were rendered at gates reuse (DESIGN §4, §8).
type widgetCache struct {
	lines         [][]hostapi.Segment
	width, height int
	gen           int64
	err           string // last recorded render error; a repeat is not recorded again

	// wrapped holds the lines re-wrapped for wrapW cells: compose runs
	// every frame and wrapping the transcript is its biggest cost.
	wrapped [][]hostapi.Segment
	wrapW   int
	// lineRows is each line's rows at wrapW, by the address of its first
	// segment: a render hands back the lines it did not change
	// (Widget.tableSegments), so a new reply wraps only its own lines.
	lineRows map[*hostapi.Segment][][]hostapi.Segment
}

// NewApp builds the root model; host hooks are attached by the caller via
// host.SetUIHooks(app).
func NewApp(host *hostapi.Host, log *logging.Logger) *App {
	return &App{
		host:     host,
		log:      log,
		thm:      themeResolver{host: host, sgr: map[string]uv.Style{}},
		caches:   map[string]*widgetCache{},
		prefixes: map[string]string{},
		scrolls:  map[string]*scrollState{},
		timeStr:  "--:--",
		now:      time.Now,
		focused:  true,
		hooks:    make(chan tea.Msg, hookQueueSize),
	}
}

// SetProgram wires the running bubbletea program (for Quit/Send).
func (a *App) SetProgram(p *tea.Program) { a.program = p }

// SetReload installs the Go-reserved /reload dispatch (built by main).
func (a *App) SetReload(fn func()) { a.reload = fn }

// Reload runs that dispatch for callers outside the key loop (the /reload
// command, zemi.config.reload). It blocks on the interpreter, so it goes on
// its own goroutine, like the palette's path.
func (a *App) Reload() {
	a.do(func() {
		if a.reload != nil {
			go a.reload()
		}
	})
}

// SignalBoot reports the boot outcome from the boot goroutine (posted
// through the hook channel; safe from any goroutine).
func (a *App) SignalBoot(err error) {
	a.do(func() {
		a.bootErr = err
		if err != nil {
			a.log.Error("ui", "boot failed", map[string]any{"error": err.Error()})
		}
	})
}

// Quit asks the program to exit (/quit). The veto waits on the interpreter
// and program.Quit must not run inside Update, so both run on a goroutine.
func (a *App) Quit() {
	quit := func() {
		if res := a.host.EmitVetoable(context.Background(), "app.quit", map[string]any{}, lua.VetoDeadline); res.Denied {
			a.Notify("warn", "Quit blocked", res.Reason)
			return
		}
		a.leave()
	}
	if a.program != nil {
		go quit()
		return
	}
	quit()
}

// leave clears the running-turn progress report while the host is still on
// the alternate screen, then quits. It runs on a goroutine.
func (a *App) leave() {
	if a.program == nil {
		return
	}
	// restore the terminal's own background before leaving the alternate
	// screen; then clear the progress report
	var out string
	if a.termBG != nil && a.paintedTermBG != nil {
		out += termBytes(a.termBG)
	}
	a.program.Send(tea.RawMsg{Msg: out + "\x1b]9;4;0\x07"})
	// NOTE: raw output is written on the next frame and quitting leaves the
	// alternate screen first, so wait out a frame
	time.Sleep(50 * time.Millisecond)
	a.program.Quit()
}

// ---------------------------------------------------------------------------
// messages

type frameTickMsg struct{}

type widgetRenderedMsg struct {
	id            string
	lines         [][]hostapi.Segment
	more          bool // the render wants to run again
	width, height int
	gen           int64
	err           error
}

type (
	agentStreamMsg struct{}
	turnDoneMsg    struct{ err string }
)

// uiFunc is a state change posted from another goroutine; it runs on the
// Update goroutine.
type uiFunc func() tea.Cmd

// do posts fn to run on the Update goroutine.
func (a *App) do(fn func()) { a.dispatch(uiFunc(func() tea.Cmd { fn(); return nil })) }

type permAnswer struct {
	ok bool
	by string
}

// hookMsg wraps a message posted from a non-UI goroutine.
type hookMsg struct{ inner tea.Msg }

// ---------------------------------------------------------------------------
// tea.Model

func (a *App) Init() tea.Cmd {
	// the background query also picks the theme's light variant
	return tea.Batch(a.frameTickCmd(), a.drainHooksCmd(), tea.RequestBackgroundColor)
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case frameTickMsg:
		return a, a.onFrame()
	case widgetRenderedMsg:
		a.onWidgetRendered(msg)
	case uiFunc:
		a.frameGen++
		return a, msg()
	case agentStreamMsg:
		// Stream state lives in the host; just invalidate the frame.
		a.frameGen++
		a.streamPending.Store(false)
	case turnDoneMsg:
		a.frameGen++
		text := "zemi finished"
		if msg.err != "" {
			text = "zemi stopped: " + ansi.Truncate(msg.err, 80, "...")
		}
		return a, a.notifyIfAway(text)
	case hookMsg:
		_, cmd := a.Update(msg.inner)
		// Re-arm the persistent drain: each delivered hookMsg re-arms
		// exactly one, so the chain never breaks.
		return a, tea.Batch(cmd, a.drainHooksCmd())
	case tea.BackgroundColorMsg:
		// The answer picks the light variant and means the terminal replies
		// to OSC 11, so its own background can be repainted to match the theme.
		a.termBG = msg.Color
		a.thm.light = msg.Color != nil && luma(msg.Color) > 1<<15
	case tea.FocusMsg, tea.BlurMsg:
		_, a.focused = msg.(tea.FocusMsg)
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.host.EmitAsync("ui.resize", map[string]any{"width": msg.Width, "height": msg.Height})
		a.frameGen++
		// render now rather than start a tick: each tick schedules the next,
		// so every resize would add a frame loop that never stops
		return a, a.renderStaleWidgetsCmd()
	case tea.KeyPressMsg:
		cmd := a.handleKey(msg)
		a.publishComposer()
		return a, cmd
	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		return a, a.handleMouse(msg)
	case tea.MouseWheelMsg:
		a.mouse = mouseSel{}
		a.hoverKey = "" // the content under the pointer moves
		step := map[tea.MouseButton]int{tea.MouseWheelUp: -1, tea.MouseWheelDown: 1}[msg.Button]
		if step != 0 && !a.wheelChrome(step) {
			a.scrollRows(a.scrollAt(msg.X, msg.Y), step*chatWheelStep)
		}
		return a, nil
	case editorDoneMsg:
		a.editing = ""
		msg.errc <- msg.err
		return a, nil
	case editorMsg:
		if msg.err != nil {
			a.pushToast("error", "Editor failed", msg.err.Error())
			return a, nil
		}
		text := sanitize.String(msg.text)
		if !a.multiline() {
			text = strings.ReplaceAll(text, "\n", " ")
		}
		prev := composerSnap{a.composer.text, a.composer.cursor}
		a.composer.text = text
		a.composer.moveEnd()
		a.composer.snapshot(prev, false)
		a.publishComposer()
		return a, a.updateCompletion()
	case clipboardMsg:
		if msg.err != nil {
			a.pushToast("warn", "Paste failed", msg.err.Error())
			return a, nil
		}
		a.insertToken(msg.token)
		a.publishComposer()
		return a, nil
	case tea.PasteMsg:
		s := sanitize.String(strings.ReplaceAll(strings.ReplaceAll(msg.Content, "\r\n", "\n"), "\r", "\n"))
		if a.pasteChrome(s) {
			return a, nil
		}
		// a dropped file arrives as a paste of its path
		if paths, ok := parseDrop(s); ok {
			a.insertAttachments(paths)
			a.publishComposer()
			return a, nil
		}
		if !a.multiline() {
			s = strings.ReplaceAll(s, "\n", " ")
		}
		prev := composerSnap{a.composer.text, a.composer.cursor}
		a.composer.paste(s)
		a.composer.snapshot(prev, false)
		a.publishComposer()
		return a, a.updateCompletion()
	case filesScannedMsg:
		a.onFilesScanned(msg)
		a.frameGen++
	}
	return a, nil
}

func (a *App) View() tea.View {
	v := tea.NewView(a.Frame())
	v.AltScreen = true
	// Cell motion: the wheel scrolls the transcript (the altscreen buffer has
	// no native scrollback), drags select and clicks press. ui.mouse = false
	// leaves the mouse to the terminal.
	if a.mouseEnabled() {
		v.MouseMode = tea.MouseModeAllMotion // motion without a button drives hover
	}
	v.ReportFocus = true // notifyIfAway needs to know
	v.WindowTitle = a.host.TerminalTitle()
	return v
}

// progressCmd tells the terminal a turn is running with OSC 9;4, which
// ghostty, WezTerm and Windows Terminal draw on the tab or taskbar. It sends
// the running sequence when the turn starts and then every progressInterval,
// since those terminals hide the indicator once it stops being sent, and sends
// the clear once when the turn stops.
func (a *App) progressCmd(running bool) tea.Cmd {
	if running && !a.host.Settings.Bool("ui.progress") {
		running = false
	}
	if !running {
		if !a.progress {
			return nil
		}
		a.progress = false
		return tea.Raw("\x1b]9;4;0\x07")
	}
	if a.progress && a.now().Sub(a.progressAt) < progressInterval {
		return nil
	}
	a.progress, a.progressAt = true, a.now()
	return tea.Raw("\x1b]9;4;3\x07")
}

// notifyIfAway alerts through the terminal when zemi needs attention while
// its window is not focused: OSC 9 (a desktop notification in iTerm2,
// Ghostty, WezTerm, Windows Terminal) and a bell for other terminals.
func (a *App) notifyIfAway(text string) tea.Cmd {
	if a.focused || !a.host.Settings.Bool("ui.notify") {
		return nil
	}
	return tea.Raw("\x1b]9;" + sanitize.String(text) + "\x07\a")
}

// Frame renders the full screen as styled text; exported for the golden
// test harness (DESIGN §8 golden tests).
func (a *App) Frame() string {
	if a.width <= 0 || a.height <= 0 {
		return "zemi starting..."
	}
	return a.compose(a.width, a.height)
}

// ---------------------------------------------------------------------------
// frame loop and widget rendering

func (a *App) frameTickCmd() tea.Cmd {
	return func() tea.Msg {
		time.Sleep(frameInterval)
		return frameTickMsg{}
	}
}

func (a *App) onFrame() tea.Cmd {
	now := time.Now()
	a.pruneToasts(now)
	// A minute change re-renders time-reading widgets.
	if ts := now.Format("15:04"); ts != a.timeStr {
		a.timeStr = ts
		a.frameGen++
	}
	// While a turn runs, the thought/elapsed readouts must tick even when
	// no stream events arrive (long reasoning pauses, slow tools). The
	// snapshot already computes the live durations; the widgets just need
	// re-rendering: every other frame (~15/s), so the mascot's 150ms spinner
	// steps evenly.
	active := a.host.AgentActive()
	if (active || a.host.Reflecting()) && now.Sub(a.activeBump) >= 60*time.Millisecond {
		a.activeBump = now
		a.frameGen++
	}
	if active != a.turnActive {
		a.turnActive, a.turnEnd = active, now
	}
	// widgets read these flags, so a change re-renders them; the animation
	// tick alone does not, it only matters while a turn bumps the frame
	// anyway, and Done turning on and off bumps at both ends by itself
	st := a.uiState()
	if st.Frame = 0; st != a.lastState {
		a.lastState = st
		a.frameGen++
	}
	cmds := []tea.Cmd{a.frameTickCmd(), a.renderStaleWidgetsCmd()}
	if a.termBG != nil { // the terminal answers OSC 11: keep its bg the theme's own
		if bg := a.syncedTermBG(); !sameColor(bg, a.paintedTermBG) {
			a.paintedTermBG = bg
			cmds = append(cmds, setTermBG(bg))
		}
	}
	if c := a.progressCmd(active); c != nil {
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

// syncedTermBG is the theme's canvas color when it has one; a theme with no
// bg (the terminal shows its own) keeps the terminal's color, not ours.
func (a *App) syncedTermBG() color.Color {
	if theme := a.thm.active(); theme != nil {
		if spec, ok := a.host.Themes.Group(theme, "Normal"); ok && spec.Bg != "" {
			return appBG(a.thm, theme)
		}
	}
	return a.termBG
}

// setTermBG paints the terminal's own background (OSC 11) so everything
// outside the painted cells - margins, scrollback - matches the theme too.
func setTermBG(c color.Color) tea.Cmd {
	return tea.Raw(termBytes(c))
}

func termBytes(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("\x1b]11;#%02x%02x%02x\x07", r>>8, g>>8, b>>8)
}

func (a *App) uiState() hostapi.UIState {
	st := hostapi.UIState{
		Time: a.timeStr, Frame: a.frame(), Done: !a.turnActive && a.now().Sub(a.turnEnd) < doneLinger,
		QuitArmed: a.now().Before(a.quitArmed), RowsBelow: a.rowsBelow(),
		Cancelling: a.cancelling, Editing: a.editing, Focus: a.focusedNode(),
	}
	if len(a.keySeq) > 0 && a.now().Sub(a.keySeqAt) <= keySeqTimeout {
		st.KeySeq = strings.Join(a.keySeq, " ")
	}
	switch {
	case a.permAsk != nil:
		st.Waiting = "approval"
	case a.planAsk != nil:
		st.Waiting = "plan"
	case a.question != nil:
		st.Waiting = "answer"
	}
	return st
}

// frame is the animation tick every widget animates on: the mascot's own
// 500ms cadence, blinking markers included. Always 0 with animations off.
func (a *App) frame() int {
	if !a.host.Settings.Bool("ui.animations") {
		return 0
	}
	return int(a.now().UnixMilli() / frameMS)
}

func (a *App) onWidgetRendered(msg widgetRenderedMsg) {
	if a.pending[msg.id] == msg.gen {
		delete(a.pending, msg.id)
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
		c.gen, c.width, c.height = msg.gen, msg.width, msg.height
		if !isTolerableRenderErr(msg.err) && c.err != msg.err.Error() {
			c.err = msg.err.Error()
			a.host.RecordError("widget "+msg.id, c.err)
		}
		a.log.Debug("ui", "widget render failed; keeping last good lines", map[string]any{
			"widget": msg.id, "error": msg.err.Error(),
		})
		return
	}
	// renders of different generations run concurrently: an older one that
	// finishes last must not replace newer lines
	if c := a.caches[msg.id]; c != nil && c.gen > msg.gen {
		return
	}
	c := &widgetCache{lines: msg.lines, width: msg.width, height: msg.height, gen: msg.gen}
	old := a.caches[msg.id]
	if old != nil {
		c.wrapW, c.lineRows = old.wrapW, old.lineRows
	}
	if msg.more {
		c.gen-- // stale, so the next frame renders this widget alone again
		// A reader scrolled up keeps the last whole render until the new one
		// is whole: anchoring on partial rows would lose their place.
		if old != nil && len(old.lines) > 0 && a.scrolledUp(msg.id) {
			c.lines, c.wrapped = old.lines, old.wrapped
		}
	}
	a.caches[msg.id] = c
}

// isTolerableRenderErr reports failures the UI expects under load (hot
// mode, reload in flight): they keep the cached lines without flagging.
func isTolerableRenderErr(err error) bool {
	return errors.Is(err, lua.ErrHot) || errors.Is(err, lua.ErrReloaded) || errors.Is(err, lua.ErrCanceled)
}

// renderStaleWidgetsCmd enqueues ui-class renders for widgets whose cache
// is missing or stale for the current frame generation.
func (a *App) renderStaleWidgetsCmd() tea.Cmd {
	gen, st := a.frameGen, a.uiState()
	var cmds []tea.Cmd
	for _, w := range a.shownWidgets() {
		if c := a.caches[w.ID]; c != nil && c.gen == gen && c.width == w.width && c.height == w.height {
			continue
		}
		// One render per widget in flight: at 30 fps a slow widget would
		// otherwise pile up a queue of renders of the same generation.
		if a.pending[w.ID] == gen {
			continue
		}
		if a.pending == nil {
			a.pending = map[string]int64{}
		}
		a.pending[w.ID] = gen
		cmds = append(cmds, func() tea.Msg {
			lines, more, err := a.host.RenderWidget(context.Background(), w.Widget, w.width, w.height, st)
			return widgetRenderedMsg{id: w.ID, lines: lines, more: more, width: w.width, height: w.height, gen: gen, err: err}
		})
	}
	return tea.Batch(cmds...)
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

// ---------------------------------------------------------------------------
// toasts

// toast is a transient notification (zemi.notify, DESIGN §6.1).
type toast struct {
	level, title, body string
	expires            time.Time
	recorded           bool // the whole error is in /errors
}

// toastRows caps a toast's body: a long error or a stack trace would cover
// the transcript.
const toastRows = 3

func (a *App) pushToast(level, title, body string) {
	t := toast{level: level, title: title, body: body, expires: time.Now().Add(toastTTL)}
	short, _, trace := strings.Cut(body, "stack traceback:")
	t.body = hostapi.ShortChunk(strings.TrimRight(short, " \t\n"))
	if level == "error" && (trace || strings.Count(t.body, "\n") >= toastRows || len(t.body) > 150) {
		a.host.RecordError(title, body)
		t.recorded = true
	}
	a.toasts = append(a.toasts, t)
	if len(a.toasts) > 3 {
		a.toasts = a.toasts[len(a.toasts)-3:]
	}
}

func (a *App) pruneToasts(now time.Time) {
	a.toasts = slices.DeleteFunc(a.toasts, func(t toast) bool { return !now.Before(t.expires) })
}

// ---------------------------------------------------------------------------
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
		a.log.Warn("ui", "hook queue full; dropping message", nil)
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

// ---------------------------------------------------------------------------
// compose (pure: reads caches and Go-side state only)

func (a *App) compose(w, h int) string {
	if !a.host.AgentActive() {
		a.cancelling = false
	}
	clear(a.prefixes) // a theme cannot change inside one frame

	lay := a.layout(w, h)
	stage := lay.stage
	a.scrollIDs, a.scrollDef = a.scrollIDs[:0], lay.scroll
	rows := a.paint(lay.root)
	a.paintFloats(rows, &lay, w, h)
	a.focusables = slices.DeleteFunc(lay.focusable, func(id string) bool { _, ok := lay.rects[id]; return !ok })

	var pop uv.Rectangle
	var popup [][]hostapi.Segment
	var popX int
	if a.focusedNode() == "" { // the composer's popups follow its keys
		popup, popX, pop = a.floatPopup(append(a.completionRows(), a.paletteRows()...), lay, len(rows), w)
	}
	for i, line := range popup {
		placeBox(rows, [][]hostapi.Segment{line}, popX, pop.Min.Y+i, w)
	}
	if pills := a.pills(); !lay.bar && len(pills) > 0 {
		// without a statusline the alarms still show
		rows = placeBox(rows, [][]hostapi.Segment{pills}, 0, h-1, w)
	}

	// Modal chrome goes over everything: Lua overlays, then the Go-owned
	// prompts, bottom to top (topChrome must match this order so input
	// reaches the box drawn on top). What lies under the top box is dimmed,
	// so no text beside it can pass for its chrome (DESIGN §8).
	// It centers over the stage, or the screen when the stage is too small
	// for a prompt.
	area := stage
	if area.Dx() < 40 || area.Dy() < 12 {
		area = uv.Rect(0, 0, w, h)
	}
	var modals []func()
	a.chromeRect = uv.Rectangle{}
	splice := func(box [][]hostapi.Segment) {
		modals = append(modals, func() { rows, a.chromeRect = spliceBox(rows, box, area, w) })
	}
	for i := range a.overlays {
		if i+1 < len(a.overlays) && a.overlays[i+1].back {
			continue // a detail view replaces the list it opened from
		}
		if ov := &a.overlays[i]; ov.o.Anchor == "bottom" {
			box := a.overlayBox(ov, area.Dx(), area.Dy())
			modals = append(modals, func() { rows, a.chromeRect = anchorBottom(rows, box, area, w) })
		} else {
			splice(a.overlayBox(ov, area.Dx(), area.Dy()))
		}
	}
	if a.trust != nil {
		splice(trustBox(a.trust, area.Dx(), area.Dy()))
	}
	if a.planAsk != nil {
		splice(a.planBox(a.planAsk, area.Dx(), area.Dy()))
	}
	if a.question != nil {
		splice(a.questionBox(a.question, area.Dx(), area.Dy()))
	}
	if a.memReview != nil {
		splice(a.memReviewBox(area.Dx(), area.Dy()))
	}
	if a.permAsk != nil {
		splice(a.permBox(a.permAsk, area.Dx(), area.Dy()))
	}
	// Toasts stack down from the top of the stage, newest highest,
	// right-aligned over the rows they cover, one blank row apart. They go
	// over the dimmed rows but under the top box: a toast is not dimmed, and
	// its text never covers the chrome the user answers.
	toasts := func() {
		y := stage.Min.Y
		for _, v := range slices.Backward(a.toasts) {
			for _, line := range toastBox(v, stage.Dx()) {
				if y >= stage.Max.Y {
					break
				}
				x0 := max(stage.Min.X, stage.Max.X-segsWidth(line)-2)
				rows = placeBox(rows, [][]hostapi.Segment{margined(line)}, x0, y, w)
				y++
			}
			y++
		}
	}
	for i, place := range modals {
		if i == len(modals)-1 {
			dimRows(rows)
			toasts()
		}
		place()
	}
	if len(modals) == 0 {
		toasts()
	}

	a.lastRows, a.stage, a.popRect = rows, stage, pop
	a.highlightSelection(rows, w)
	a.highlightHover(rows)

	var b strings.Builder
	for i, row := range rows {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(a.renderSegs(row, w))
	}
	out := b.String()
	rects := make(map[string]hostapi.Rect, len(lay.rects))
	for id, r := range lay.rects {
		if id != "screen" {
			rects[id] = hostapi.Rect{X: r.Min.X, Y: r.Min.Y, Width: r.Dx(), Height: r.Dy()}
		}
	}
	a.frameMu.Lock()
	a.lastFrame, a.rects = out, rects
	a.frameMu.Unlock()
	return out
}

// LayoutRects implements hostapi.UIHooks from the last composed frame.
func (a *App) LayoutRects() map[string]hostapi.Rect {
	a.frameMu.Lock()
	defer a.frameMu.Unlock()
	return a.rects
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

// dimRows repaints rows in one fixed color. It copies each row: rows share
// slices with widget caches. Image placeholders keep their color, which
// names the image.
func dimRows(rows [][]hostapi.Segment) {
	for i, r := range rows {
		out := make([]hostapi.Segment, len(r))
		for j, s := range r {
			out[j] = hostapi.Segment{Text: s.Text, HL: "Host:Dim"}
			if strings.ContainsRune(s.Text, kitty.Placeholder) {
				out[j] = s
			}
		}
		rows[i] = out
	}
}

// floatPopup frames a completion popup for the bottom rows of the stage,
// left-aligned with it, when the stage is above the composer; otherwise
// beside the composer on the side with more rows, aligned with it. It covers
// only its own cells; a popup taller than its room is clipped. It returns the rows
// to draw, their left column and the cells they cover.
func (a *App) floatPopup(popup [][]hostapi.Segment, lay screenLayout, h, w int) ([][]hostapi.Segment, int, uv.Rectangle) {
	if len(popup) == 0 {
		return nil, 0, uv.Rectangle{}
	}
	stage, c := lay.stage, lay.rects["composer"]
	x, width, top, bottom := stage.Min.X, stage.Dx(), stage.Min.Y, min(stage.Max.Y, h)
	up := true
	if stage.Max.Y > c.Min.Y {
		x, width, top, bottom = c.Min.X, c.Dx(), 0, c.Min.Y
		if up = c.Min.Y >= h-c.Max.Y; !up {
			top, bottom = c.Max.Y, h
		}
	}
	box := hostBox("", "", "", popup, width)
	if up {
		top = max(top, bottom-len(box))
	} else {
		bottom = min(bottom, top+len(box))
	}
	out := box[:max(bottom-top, 0)]
	bw := 0
	for _, row := range out {
		bw = max(bw, segsWidth(row))
	}
	return out, x, uv.Rect(x, top, bw, len(out))
}

// rowsBelow counts the rows under the default scrolled region's viewport
// while it is off the tail. The altscreen has no scrollbar, so without it
// nothing says how far up the reader is, or that a running turn is still
// filling the bottom.
func (a *App) rowsBelow() int {
	st := a.scrolls[a.scrollDef]
	if st == nil || st.follow {
		return 0
	}
	return max(0, st.rows-st.top-st.viewH)
}

// scrolledUp reports whether the region a widget draws in is off its tail.
func (a *App) scrolledUp(widget string) bool {
	w := a.host.Widgets.Get(widget)
	for _, st := range a.scrolls {
		if w != nil && st.region == w.Region && !st.follow {
			return true
		}
	}
	return false
}

// scrollAt is the scrolled region drawn under (x, y), else the default one.
func (a *App) scrollAt(x, y int) string {
	for _, id := range a.scrollIDs {
		if st := a.scrolls[id]; st != nil && (uv.Position{X: x, Y: y}).In(st.rect) {
			return id
		}
	}
	return a.scrollDef
}

// scrollRows moves a scrolled region's viewport by n rows (negative =
// older). Scrolling down re-arms tail follow at the bottom of a "bottom"
// region; while scrolled up the view stays anchored as new lines arrive.
func (a *App) scrollRows(id string, n int) {
	st := a.scrolls[id]
	if st == nil || n > 0 && st.follow {
		return
	}
	if n < 0 && st.follow {
		st.follow = false
		st.top = max(0, st.rows-st.viewH)
	}
	st.top = max(0, min(st.top+n, st.rows-st.viewH))
	st.follow = st.bottom && st.top+st.viewH >= st.rows
	a.frameGen++
}

// followStage pins the default scrolled region back to its tail (a submit).
func (a *App) followStage() {
	if st := a.scrolls[a.scrollDef]; st != nil && st.bottom {
		st.follow = true
	}
}

// applyScroll maps the zemi.ui.scroll.* host actions onto a scrolled
// region, the default one when id is "": up and down move a page with a
// one-row overlap, top and bottom go to the ends.
func (a *App) applyScroll(kind, id string) {
	id = cmp.Or(id, a.scrollDef)
	st := a.scrolls[id]
	if st == nil {
		return
	}
	page := max(st.viewH-1, 1)
	switch kind {
	case "up":
		a.scrollRows(id, -page)
	case "down":
		a.scrollRows(id, page)
	case "top":
		a.scrollRows(id, -st.rows)
	case "bottom":
		a.scrollRows(id, st.rows)
	}
}

func (a *App) regionLines(region string, width int) [][]hostapi.Segment {
	var out [][]hostapi.Segment
	for _, wgt := range a.host.Widgets.InRegion(region) {
		c := a.caches[wgt.ID]
		if c == nil {
			continue
		}
		lines := c.lines
		if !wgt.Wrap {
			for _, line := range expandTabs(lines) {
				out = append(out, truncateSegs(line, width))
			}
			continue
		}
		// Word-wrap instead of truncating; the transcript shows the
		// wrapped tail (bottom-anchored). The rows change only when the
		// widget renders again or the width does.
		if c.wrapped == nil || c.wrapW != width {
			if c.wrapW != width {
				c.lineRows = nil
			}
			var rows [][]hostapi.Segment
			next := make(map[*hostapi.Segment][][]hostapi.Segment, len(lines))
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
			c.wrapped, c.wrapW, c.lineRows = rows, width, next
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
// (DESIGN §8), as plain text after a dot like the rest of the rule.
func (a *App) pills() []hostapi.Segment {
	var out []hostapi.Segment
	pill := func(text, click string) {
		out = append(out, hostapi.Segment{Text: " · ", HL: "Comment"}, hostapi.Segment{Text: text, HL: "Danger", Click: click})
	}
	// a failed boot step also leaves the runtime hot: one pill is enough
	if a.bootErr != nil {
		pill("boot failed", "errors")
	} else if a.host.RT.IsHot() {
		pill("stuck: /reload", "reload")
	}
	if a.host.HasUnseenErrors() {
		pill("errors: /errors", "errors")
	}
	return out
}

// statusRow builds the one-row statusline: the left cluster, then the right
// one aligned to the right edge (DESIGN §8).
func (a *App) statusRow(w int, leftRegion, rightRegion string) []hostapi.Segment {
	left, right := a.statusClusters(w, leftRegion, rightRegion)
	out := append([]hostapi.Segment{}, left...)
	if gap := max(0, w-segsWidth(left)-segsWidth(right)); gap > 0 {
		out = append(out, hostapi.Segment{Text: strings.Repeat(" ", gap)})
	}
	return append(out, right...)
}

// statusClusters are the statusline's two sides fitted into w cells: the
// focused window and the left region's widgets followed by the pills that
// report a broken Lua layer, then the right region's widgets.
func (a *App) statusClusters(w int, leftRegion, rightRegion string) (left, right []hostapi.Segment) {
	left, right = a.statuslineSegments(leftRegion), a.statuslineSegments(rightRegion)
	pills := a.pills()
	if len(pills) > 0 && pills[0].HL == "Host:BtnSel" {
		if len(left) > 0 {
			left = append([]hostapi.Segment{statusDot}, left...)
		}
		left = append([]hostapi.Segment{pills[0]}, left...)
		pills = pills[1:]
	}
	left = append(left, pills...)
	if segsWidth(left)+segsWidth(right) < w {
		return left, right
	}
	// Overflow truncates the right cluster first (lowest priority of the
	// row's real estate goes last, §8); under four cells it would show a
	// stray letter or two, so it goes whole.
	if room := w - segsWidth(left) - 1; room >= 4 {
		return left, fitSegs(right, room)
	}
	return truncateSegs(left, w), nil
}

var statusDot = hostapi.Segment{Text: " · ", HL: "Comment", NoCopy: true}

func (a *App) statuslineSegments(region string) []hostapi.Segment {
	var out []hostapi.Segment
	for _, wgt := range a.host.Widgets.InRegion(region) {
		lines := a.widgetLines(wgt.ID)
		if len(lines) == 0 || segsWidth(lines[0]) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, statusDot)
		}
		out = append(out, lines[0]...)
	}
	return out
}

// toastBox lays a toast out in the frame every other box uses, the title in
// the top rule in the level's color, the body word-wrapped under it.
func toastBox(t toast, w int) [][]hostapi.Segment {
	level := map[string]string{"warn": "ToastWarn", "error": "ToastErr"}[t.level]
	if level == "" {
		level = "ToastInfo"
	}
	inner := max(min(w-2, 60)-4, 1)
	var rows [][]hostapi.Segment
	if t.body != "" {
		rows = wrapText(t.body, "Normal", inner)
	}
	if len(rows) > toastRows {
		more := map[bool]string{true: "... full error in /errors", false: "..."}[t.recorded]
		rows = append(rows[:toastRows-1], []hostapi.Segment{{Text: more, HL: "Comment"}})
	}
	title := ""
	if t.title != "" {
		title = " " + ansi.Truncate(t.title, inner, "…") + " "
	}
	return hostBoxW(title, level, "", rows, w, 60)
}

// ---------------------------------------------------------------------------
// segment helpers

// hlStyle resolves a highlight name. "Host:*" names are Go-owned security
// chrome (DESIGN §8): they resolve to fixed styles and never consult the theme.
func (a *App) hlStyle(hl string) uv.Style {
	if st, ok := fixedStyle(hl, a.thm.light); ok {
		if st.Bg == nil { // chrome sits on the theme's canvas like any group
			st.Bg = appBG(a.thm, a.thm.active())
		}
		return st
	}
	return a.thm.style(hl)
}

func (a *App) segStyle(hl, over string) uv.Style {
	if over == "" {
		return a.hlStyle(hl)
	}
	return a.thm.hover(a.hlStyle(hl), over)
}

// sgrPrefix is the ANSI sequence a segment's style opens with, "" for the
// terminal's own. It is cached for the frame: compose styles thousands of
// segments and building the sequence for each is the biggest cost of
// drawing one.
func (a *App) sgrPrefix(hl, over string) string {
	key := hl // no highlight name holds a NUL, and hover is the rare case
	if over != "" {
		key = hl + "\x00" + over
	}
	if p, ok := a.prefixes[key]; ok {
		return p
	}
	st := a.segStyle(hl, over)
	p := ""
	if !st.IsZero() {
		p = st.String()
	}
	a.prefixes[key] = p
	return p
}

// renderSegs styles and joins segments, truncating to w visible cells.
func (a *App) renderSegs(segs []hostapi.Segment, w int) string {
	var b strings.Builder
	write := func(text, hl, over string) {
		p := a.sgrPrefix(hl, over)
		if p == "" {
			b.WriteString(text)
			return
		}
		b.WriteString(p)
		b.WriteString(text)
		b.WriteString(ansi.ResetStyle)
	}
	for _, s := range segs {
		if s.Text == "" {
			continue
		}
		if s.Effect == "shimmer" {
			for _, r := range a.shimmer(s, a.now(), a.host.Settings.Bool("ui.animations")) {
				write(r.Text, r.HL, s.Over)
			}
			continue
		}
		write(s.Text, s.HL, s.Over)
	}
	out := b.String()
	if ansi.StringWidth(out) > w {
		out = ansi.Truncate(out, w, "")
	}
	return out
}

// fitSegs drops whole segments from the end, and a " · " left dangling,
// until segs fit w cells; a first segment alone too wide ends in "…".
func fitSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	for len(segs) > 1 && segsWidth(segs) > w {
		segs = segs[:len(segs)-1]
		if n := len(segs); n > 1 && strings.TrimSpace(segs[n-1].Text) == "·" {
			segs = segs[:n-1]
		}
	}
	if segsWidth(segs) > w {
		return ellipsizeSegs(segs, w)
	}
	return segs
}

func truncateSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	if segsWidth(segs) <= w {
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

func padSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	pad := w - segsWidth(segs)
	if pad <= 0 {
		return segs
	}
	// Normal carries the app background (theme.lua), so a fill cell paints
	// the canvas instead of leaving the terminal's background showing.
	return append(append([]hostapi.Segment{}, segs...), hostapi.Segment{Text: strings.Repeat(" ", pad), HL: "Normal"})
}

// the same measurement the markdown renderer uses
var segsWidth = hostapi.SegsWidth
