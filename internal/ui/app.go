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
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
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
	"github.com/metafates/zemi/internal/win"
)

const (
	frameInterval = time.Second / 30
	toastTTL      = 4 * time.Second
	hookQueueSize = 64
	chatWheelStep = 3
)

// App is the root bubbletea model. It implements hostapi.UIHooks.
type App struct {
	host *hostapi.Host
	log  *logging.Logger
	thm  themeResolver

	width, height int

	composer composerState

	// the session whose stored messages seeded the composer history
	histSID    string
	histLoaded bool
	histSeed   []string // the session's own messages, dropped when it changes

	// Chat scrollback (DESIGN §8): chatFollow pins the transcript to the
	// tail; scrolling up freezes chatTop (the absolute first visible row
	// of the wrapped chat region) until the user scrolls back or submits.
	chatFollow bool
	chatTop    int
	chatRows   int
	chatViewH  int

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

	mouse          mouseSel
	press          mousePress
	hovering       bool
	hoverX, hoverY int
	hoverKey       string              // what the pointer is over, to redraw only on change
	pointer        string              // the OSC 22 shape last set
	lastRows       [][]hostapi.Segment // the last composed frame, for clicks and copy
	lastChatBottom int

	// work runs Lua-backed keymaps and commands in order off the key loop
	work     chan func()
	workOnce sync.Once
	question *questionPrompt
	editing  string    // the editor handoff in progress, shown in the working row
	focusWin int       // window taking keys, 0 = none
	winRects []winRect // where windows were drawn on the last frame
	// cursor is the host cursor: the focused window's, nil hides it. windowRows
	// sets it in window cells and compose places it on the frame.
	cursor    *tea.Cursor
	winDrag   *winDrag // a drag on window chrome in progress
	chatAreaH int      // the chat area's height on the last frame (splits resize against it)

	caches     map[string]*widgetCache
	pending    map[string]int64 // widget id -> generation of the render in flight
	frameGen   int64
	timeStr    string
	activeBump time.Time
	now        func() time.Time // animation clock; tests pin it
	cancelling bool             // interrupt sent, turn not finished yet
	focused    bool             // terminal focus (true until the terminal reports a blur)
	keySeq     []string         // keys of a bound sequence typed so far
	keySeqAt   time.Time
	quitArmed  time.Time // a second idle ctrl+c before this quits
	kittyTerm  bool      // the terminal named itself one that draws kitty placeholders
	colors     colorprofile.Profile

	bootErr       error
	streamPending atomic.Bool // a stream agentEventMsg is queued

	// the last composed frame, published for Screenshot: reading a
	// snapshot is what lets any goroutine ask what the screen says
	// without waiting on the Update goroutine.
	frameMu   sync.Mutex
	lastFrame string
	private   []winRect // editor windows' cells, blank in the screenshot

	program   *tea.Program
	reload    func()
	hooks     chan tea.Msg
	testQueue chan tea.Msg // CaptureHooks (tests): hooks wait here for Pump
}

// widgetCache holds the last-good rendered lines of one widget keyed by
// (id); the frame they were rendered at gates reuse (DESIGN §4, §8).
type widgetCache struct {
	lines [][]hostapi.Segment
	width int
	gen   int64
	err   string // last recorded render error; a repeat is not recorded again

	// wrapped holds the lines re-wrapped for wrapW cells: compose runs
	// every frame and wrapping the transcript is its biggest cost.
	wrapped [][]hostapi.Segment
	wrapW   int
}

// NewApp builds the root model; host hooks are attached by the caller via
// host.SetUIHooks(app).
func NewApp(host *hostapi.Host, log *logging.Logger) *App {
	return &App{
		host:       host,
		log:        log,
		thm:        themeResolver{host: host, sgr: map[string]uv.Style{}},
		caches:     map[string]*widgetCache{},
		timeStr:    "--:--",
		now:        time.Now,
		chatFollow: true,
		focused:    true,
		hooks:      make(chan tea.Msg, hookQueueSize),
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

// leave deletes the windows' images while the host is still on the
// alternate screen, where ghostty keeps them past the exit, then quits. It
// runs on a goroutine.
func (a *App) leave() {
	if a.program == nil {
		return
	}
	a.host.Wins.CloseAll()
	if g := a.host.Wins.TakeGraphics(); g != "" {
		a.program.Send(tea.RawMsg{Msg: g})
		// NOTE: raw output is written on the next frame and quitting leaves the
		// alternate screen first, so wait out a frame
		time.Sleep(50 * time.Millisecond)
	}
	a.program.Quit()
}

// ---------------------------------------------------------------------------
// messages

type frameTickMsg struct{}

type widgetRenderedMsg struct {
	id    string
	lines [][]hostapi.Segment
	width int
	gen   int64
	err   error
}

type agentEventMsg struct{ ev hostapi.AgentEvent }

// uiFunc is a state change posted from another goroutine; it runs on the
// Update goroutine.
type uiFunc func() tea.Cmd

// do posts fn to run on the Update goroutine.
func (a *App) do(fn func()) { a.dispatch(uiFunc(func() tea.Cmd { fn(); return nil })) }

type permAnswer struct {
	ok bool
	by string
}

// cfgBool reads a bool config key with a fallback.
func (a *App) cfgBool(key string, def bool) bool {
	if v, ok := a.host.Settings.Get(key); ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// cfgInt reads an int config key clamped to [lo, hi].
func (a *App) cfgInt(key string, def, lo, hi int) int {
	if v, ok := a.host.Settings.Get(key); ok {
		switch n := v.(type) {
		case int:
			return min(max(n, lo), hi)
		case float64:
			return min(max(int(n), lo), hi)
		}
	}
	return def
}

// hookMsg wraps a message posted from a non-UI goroutine.
type hookMsg struct{ inner tea.Msg }

// ---------------------------------------------------------------------------
// tea.Model

func (a *App) Init() tea.Cmd {
	// windows answer color queries with the host's colors, so an editor picks a matching theme
	cmds := []tea.Cmd{a.frameTickCmd(), a.drainHooksCmd(), tea.RequestForegroundColor, tea.RequestBackgroundColor, tea.RequestCursorColor}
	// the terminal's name says whether windows get kitty images; Terminal.app
	// prints the query instead of answering it
	if os.Getenv("TERM_PROGRAM") != "Apple_Terminal" {
		cmds = append(cmds, tea.RequestTerminalVersion)
	}
	return tea.Batch(cmds...)
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
	case agentEventMsg:
		// Stream state lives in the host; just invalidate the frame.
		a.frameGen++
		if msg.ev.Kind != "turn_done" {
			a.streamPending.Store(false)
		}
		if msg.ev.Kind == "turn_done" {
			text := "zemi finished"
			if msg.ev.Err != "" {
				text = "zemi stopped: " + ansi.Truncate(msg.ev.Err, 80, "...")
			}
			return a, a.notifyIfAway(text)
		}
	case hookMsg:
		_, cmd := a.Update(msg.inner)
		// Re-arm the persistent drain: each delivered hookMsg re-arms
		// exactly one, so the chain never breaks.
		return a, tea.Batch(cmd, a.drainHooksCmd())
	case tea.TerminalVersionMsg:
		// NOTE: an allowlist of terminals known to draw kitty placeholders;
		// windows opened before the reply get symbols
		name := strings.ToLower(msg.Name)
		a.kittyTerm = strings.HasPrefix(name, "ghostty") || strings.HasPrefix(name, "kitty")
		a.host.Wins.SetKitty(a.kittyTerm && a.colors >= colorprofile.ANSI256)
	case tea.ColorProfileMsg:
		// a placeholder names its image by color, which NO_COLOR drops
		a.colors = msg.Profile
		a.host.Wins.SetKitty(a.kittyTerm && a.colors >= colorprofile.ANSI256)
	case tea.ForegroundColorMsg:
		a.host.Wins.SetHostColor(10, msg.Color)
	case tea.BackgroundColorMsg:
		a.host.Wins.SetHostColor(11, msg.Color)
	case tea.CursorColorMsg:
		a.host.Wins.SetHostColor(12, msg.Color)
	case tea.FocusMsg, tea.BlurMsg:
		_, a.focused = msg.(tea.FocusMsg)
		// the focused window's program hears it, so nvim reloads files changed meanwhile
		if wn := a.focusedWin(); wn != nil {
			wn.Focus(a.focused)
		}
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
		if a.topChrome() == "" && a.windowMouse(msg) {
			if _, motion := msg.(tea.MouseMotionMsg); motion && a.winDrag == nil {
				return a, a.setPointer("") // over a window's cells
			}
			return a, nil
		}
		return a, a.handleMouse(msg)
	case tea.MouseWheelMsg:
		if a.topChrome() == "" && a.windowMouse(msg) {
			return a, nil
		}
		a.mouse = mouseSel{}
		a.hoverKey = "" // the content under the pointer moves
		step := map[tea.MouseButton]int{tea.MouseWheelUp: -1, tea.MouseWheelDown: 1}[msg.Button]
		if step != 0 && !a.wheelChrome(step) {
			a.scrollChatRows(step * chatWheelStep)
		}
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
		// terminals paste line breaks as CR; escapes in a clipboard would reach
		// the terminal on every frame, so only a window's program gets them raw
		s := sanitize.String(strings.ReplaceAll(strings.ReplaceAll(msg.Content, "\r\n", "\n"), "\r", "\n"))
		if a.pasteChrome(s) {
			return a, nil
		}
		if wn := a.focusedWin(); wn != nil {
			wn.Paste(msg.Content)
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
	v.WindowTitle = a.windowTitle()
	v.Cursor = a.cursor
	return v
}

// windowTitle names the terminal tab after the session once it has a
// title of its own.
func (a *App) windowTitle() string {
	t := sanitize.String(a.host.SessionTitle())
	if t == "" || t == "zemi" {
		return "zemi"
	}
	return t + " · zemi"
}

// notifyIfAway alerts through the terminal when zemi needs attention while
// its window is not focused: OSC 9 (a desktop notification in iTerm2,
// Ghostty, WezTerm, Windows Terminal) and a bell for other terminals.
func (a *App) notifyIfAway(text string) tea.Cmd {
	if a.focused || !a.cfgBool("ui.notify", true) {
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
	// re-rendering (one every 4 frames, ~8/s).
	if (a.host.AgentActive() || a.host.Reflecting()) && now.Sub(a.activeBump) >= 125*time.Millisecond {
		a.activeBump = now
		a.frameGen++
	}
	cmds := []tea.Cmd{a.frameTickCmd(), a.renderStaleWidgetsCmd()}
	if g := a.host.Wins.TakeGraphics(); g != "" {
		cmds = append(cmds, tea.Raw(g))
	}
	if c := a.host.Wins.TakeClipboard(); c != "" {
		cmds = append(cmds, copyCmd(c))
	}
	return tea.Batch(cmds...)
}

func (a *App) uiState() hostapi.UIState {
	return hostapi.UIState{Time: a.timeStr, Pulse: a.pulse(), Height: a.chatViewH}
}

// pulse is the 600ms blink phase shared by the working row and running
// tool markers; always false with animations off.
func (a *App) pulse() bool {
	return a.cfgBool("ui.animations", true) && a.now().UnixMilli()/600%2 == 1
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
		c.gen, c.width = msg.gen, msg.width
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
	a.caches[msg.id] = &widgetCache{lines: msg.lines, width: msg.width, gen: msg.gen}
}

// isTolerableRenderErr reports failures the UI expects under load (hot
// mode, reload in flight): they keep the cached lines without flagging.
func isTolerableRenderErr(err error) bool {
	return errors.Is(err, lua.ErrHot) || errors.Is(err, lua.ErrReloaded) || errors.Is(err, lua.ErrCanceled)
}

// visibleWidgets lists the widgets of every region composed into the frame.
func (a *App) visibleWidgets() []*hostapi.Widget {
	var out []*hostapi.Widget
	out = append(out, a.host.Widgets.InRegion("header")...)
	out = append(out, a.host.Widgets.InRegion("chat")...)
	out = append(out, a.host.Widgets.InRegion("statusline.left")...)
	out = append(out, a.host.Widgets.InRegion("statusline.right")...)
	return out
}

// widgetWidth is the main column's width: compose draws widgets beside any
// vsplits, so rendering at the terminal width would wrap every line twice.
func (a *App) widgetWidth() int {
	if a.width <= 0 {
		return 80
	}
	_, _, w := a.winColumns(a.width)
	return w
}

// renderStaleWidgetsCmd enqueues ui-class renders for widgets whose cache
// is missing or stale for the current frame generation.
func (a *App) renderStaleWidgetsCmd() tea.Cmd {
	gen, width, st := a.frameGen, a.widgetWidth(), a.uiState()
	var cmds []tea.Cmd
	for _, w := range a.visibleWidgets() {
		if c := a.caches[w.ID]; c != nil && c.gen == gen && c.width == width {
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
			lines, err := a.host.RenderWidget(context.Background(), w, width, st)
			return widgetRenderedMsg{id: w.ID, lines: lines, width: width, gen: gen, err: err}
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

// chunkPath matches a Lua chunk name ("@user:/home/me/.config/zemi/init.lua:"),
// which a toast shortens to the file name.
var chunkPath = regexp.MustCompile(`@\w+:/\S*/([^/:\s]+):`)

// toastRows caps a toast's body: a long error or a stack trace would cover
// the transcript.
const toastRows = 3

func (a *App) pushToast(level, title, body string) {
	t := toast{level: level, title: title, body: body, expires: time.Now().Add(toastTTL)}
	short, _, trace := strings.Cut(body, "stack traceback:")
	t.body = chunkPath.ReplaceAllString(strings.TrimRight(short, " \t\n"), "$1:")
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
	// vsplits are full-height columns beside the main column; everything
	// below lays the main column out at mainW and the columns join at the
	// end, floats over all of it.
	vsplits, vwidths, mainW := a.winColumns(w)
	full := w
	w = mainW
	// The default UI registers no header; the region stays for Lua.
	header := a.regionLines("header", w)

	compRows := a.composerRows(w)
	compPopup := a.completionRows()
	palPopup := a.paletteRows()
	var working [][]hostapi.Segment
	if !a.host.AgentActive() {
		a.cancelling = false
	} else if row := a.workingRow(); row != nil {
		working = [][]hostapi.Segment{row, padSegs(nil, w)}
	}
	// + statusline + composer, the working row with a blank row under it, a
	// blank row that keeps the input off the conversation and one above the
	// statusline; popups float over the chat (floatPopup), so opening one
	// never moves the transcript
	chatH := max(h-len(header)-1-len(compRows)-len(working)-2, 1)

	// Splits take rows at the bottom of the chat area; the viewport is
	// what is left.
	splits, sheights, chatViewH := a.winRowsLayout(chatH)
	a.chatAreaH = chatH
	chat := a.regionLines("chat", w)

	// Chat scrollback (§8): follow pins the window to the tail (chatTop
	// may go negative when content is shorter than the viewport, which
	// bottom-anchors it); otherwise chatTop is the absolute first visible
	// row, clamped so content growth or a narrower rewrap never shows
	// blank rows.
	a.chatRows = len(chat)
	if a.chatViewH != chatViewH {
		a.frameGen++ // widgets read the height (the splash centers on it)
	}
	a.chatViewH = chatViewH
	if a.chatFollow {
		a.chatTop = a.chatRows - chatViewH
	} else {
		a.chatTop = min(a.chatTop, max(0, a.chatRows-chatViewH))
	}

	rows := make([][]hostapi.Segment, 0, h)
	rows = append(rows, header...)

	a.winRects, a.cursor = a.winRects[:0], nil
	for i := range chatViewH {
		var row []hostapi.Segment
		if j := a.chatTop + i; j >= 0 && j < len(chat) {
			row = truncateSegs(chat[j], w)
		}
		rows = append(rows, padSegs(row, w))
	}
	y := len(header) + chatViewH
	for i, wn := range splits {
		sh := sheights[i]
		wn.Resize(w, sh-1)
		a.winRects = append(a.winRects, winRect{id: wn.ID, x: 0, y: y + 1, w: w, h: sh - 1, titleY: y, sepX: -1})
		rows = append(rows, a.windowRows(wn, w, sh-1)...)
		y += sh
	}

	toastBottom := len(rows)

	// the popup takes the mouse on the whole rows it covers, windows under it included
	popTop := floatPopup(rows, append(compPopup, palPopup...), w, len(header), chatH)
	for i := range a.winRects {
		r := &a.winRects[i]
		r.h = max(0, min(r.h, popTop-r.y))
		if r.titleY >= popTop {
			r.titleY = -1
		}
	}

	rows = append(rows, padSegs(nil, w))
	// The working row renders above the composer.
	rows = append(rows, working...)
	rows = append(rows, compRows...)
	rows = append(rows, padSegs(nil, w), a.statusRow(w))

	// join the vsplit columns: a separator, then the window, full height
	x := w
	for i, wn := range vsplits {
		vw := vwidths[i]
		wn.Resize(vw, h-1)
		a.winRects = append(a.winRects, winRect{id: wn.ID, x: x + 1, y: 1, w: vw, h: h - 1, titleY: 0, sepX: x})
		col := a.windowRows(wn, vw, h-1)
		for j := range rows {
			line := append(padSegs(rows[j], x), hostapi.Segment{Text: "│", HL: "Comment"})
			if j < len(col) {
				line = append(line, col[j]...)
			}
			rows[j] = line
		}
		x += vw + 1
	}
	chatW, w := w, full
	for _, wn := range a.shownWins(win.KindFloat) {
		box, x0, y0, r := a.floatBox(wn, w, h, len(header), chatH)
		a.winRects = append(a.winRects, r)
		rows = placeBox(rows, box, x0, y0, w)
	}

	// Modal chrome goes over everything, windows included: Lua overlays,
	// then the Go-owned prompts, bottom to top (topChrome must match this
	// order so input reaches the box drawn on top). What lies under the top
	// box is dimmed, so no text beside it can pass for its chrome (DESIGN §8).
	var modals []func()
	splice := func(box [][]hostapi.Segment) {
		modals = append(modals, func() { rows = spliceBox(rows, box, w, len(header), chatH) })
	}
	for i := range a.overlays {
		if i+1 < len(a.overlays) && a.overlays[i+1].back {
			continue // a detail view replaces the list it opened from
		}
		if ov := &a.overlays[i]; ov.o.Anchor == "bottom" {
			box := overlayBox(ov, w, chatH)
			modals = append(modals, func() { rows = anchorBottom(rows, box, w, len(header), chatH) })
		} else {
			splice(overlayBox(ov, w, chatH))
		}
	}
	if a.trust != nil {
		splice(trustBox(a.trust, w, chatH))
	}
	if a.question != nil {
		splice(questionBox(a.question, w, chatH))
	}
	if a.memReview != nil {
		splice(a.memReviewBox(w, chatH))
	}
	if a.permAsk != nil {
		splice(a.permBox(a.permAsk, w, chatH))
	}
	// Toasts stack down from the top of the chat area, newest highest,
	// right-aligned over the chat rows they cover, one blank row apart. They
	// go over the dimmed rows but under the top box: a toast is not dimmed,
	// and its text never covers the chrome the user answers.
	toasts := func() {
		y := len(header)
		for _, v := range slices.Backward(a.toasts) {
			for _, line := range toastBox(v, chatW) {
				if y >= toastBottom {
					break
				}
				x0 := max(0, chatW-segsWidth(line)-2)
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
	a.cursor = a.placeCursor(a.cursor, len(modals) > 0)

	a.lastRows, a.lastChatBottom = rows, len(header)+chatH
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
	var private []winRect
	for _, r := range a.winRects {
		if wn, ok := a.host.Wins.Get(r.id); ok && wn.Owner == "editor" {
			private = append(private, r)
		}
	}
	a.frameMu.Lock()
	a.lastFrame, a.private = out, private
	a.frameMu.Unlock()
	return out
}

// Screenshot implements hostapi.UIHooks: the screen as plain text, one
// line per row with the styling and trailing padding dropped. It reads the
// frame compose published, so the interpreter can ask what zemi looks like
// without the UI goroutine waiting on it or the other way round. An editor
// window holds what the user types, which the Lua tool must not read.
// NOTE: a float or modal drawn over an editor is blanked where it overlaps.
func (a *App) Screenshot() string {
	a.frameMu.Lock()
	frame, private := a.lastFrame, a.private
	a.frameMu.Unlock()
	if frame == "" {
		return ""
	}
	var b strings.Builder
	for i, row := range strings.Split(frame, "\r\n") {
		if i > 0 {
			b.WriteString("\n")
		}
		line := win.StripPlaceholders(ansi.Strip(row))
		for _, r := range private {
			if i >= r.titleY && i < r.y+r.h { // the title names the file too
				left := ansi.Truncate(line, r.x, "")
				line = left + strings.Repeat(" ", r.x-ansi.StringWidth(left)+r.w) + ansi.TruncateLeft(line, r.x+r.w, "")
			}
		}
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

// floatPopup frames a completion popup and lays it over the bottom rows of
// the chat area, left-aligned with the composer. The rows it covers are
// blanked to the right of the frame (word fragments beside a dropdown read
// as garbage); a popup taller than the chat is clipped. It returns the
// first row the popup covers.
func floatPopup(rows, popup [][]hostapi.Segment, w, headerH, chatH int) int {
	bottom := min(headerH+chatH, len(rows))
	if len(popup) == 0 {
		return bottom
	}
	box := hostBox("", "", "", popup, w)
	y0 := max(headerH, bottom-len(box))
	for i := y0; i < bottom; i++ {
		rows[i] = padSegs(box[i-y0], w)
	}
	return y0
}

// rowsBelow counts the transcript rows under the viewport while it is off
// the tail. The altscreen has no scrollbar, so without it nothing says how
// far up the reader is, or that a running turn is still filling the bottom.
func (a *App) rowsBelow() int {
	if a.chatFollow {
		return 0
	}
	return max(0, a.chatRows-a.chatTop-a.chatViewH)
}

// chatStep is one page of transcript scroll with a one-row overlap, from
// the last composed chat height (compose runs every frame, so the value is
// at most one frame stale).
func (a *App) chatStep() int { return max(a.chatViewH-1, 1) }

// scrollChatRows moves the transcript viewport by n rows (negative =
// older). Scrolling down re-arms tail follow at the bottom; while
// scrolled up the view stays anchored as new lines arrive.
func (a *App) scrollChatRows(n int) {
	if n > 0 && a.chatFollow {
		return
	}
	if n < 0 && a.chatFollow {
		a.chatFollow = false
		a.chatTop = max(0, a.chatRows-a.chatViewH)
	}
	a.chatTop = max(0, a.chatTop+n)
	if a.chatTop+a.chatViewH >= a.chatRows {
		a.chatFollow = true
	}
	a.frameGen++
}

// applyChatScroll maps the zemi.ui.scroll.* host actions onto the
// viewport (up/down = one page, top/bottom = the ends).
func (a *App) applyChatScroll(kind string) {
	switch kind {
	case "up":
		a.scrollChatRows(-a.chatStep())
	case "down":
		a.scrollChatRows(a.chatStep())
	case "top":
		a.scrollChatRows(-a.chatRows)
	case "bottom":
		a.chatFollow = true
		a.frameGen++
	}
}

func (a *App) regionLines(region string, width int) [][]hostapi.Segment {
	var out [][]hostapi.Segment
	for _, wgt := range a.host.Widgets.InRegion(region) {
		c := a.caches[wgt.ID]
		if c == nil {
			continue
		}
		lines := expandTabs(c.lines)
		if !wgt.Wrap {
			for _, line := range lines {
				out = append(out, truncateSegs(line, width))
			}
			continue
		}
		// Word-wrap instead of truncating; the transcript shows the
		// wrapped tail (bottom-anchored). The rows change only when the
		// widget renders again or the width does.
		if c.wrapped == nil || c.wrapW != width {
			var rows [][]hostapi.Segment
			for _, line := range lines {
				rows = append(rows, wrapSegs(line, width)...)
			}
			c.wrapped, c.wrapW = rows, width
		}
		out = append(out, c.wrapped...)
	}
	return out
}

// expandTabs turns tabs into four spaces, in place (every render hands the
// cache fresh lines): a tab has no cell width, so layout and drawing would
// both lose it.
func expandTabs(lines [][]hostapi.Segment) [][]hostapi.Segment {
	for _, line := range lines {
		for j := range line {
			line[j].Text = strings.ReplaceAll(line[j].Text, "\t", "    ")
		}
	}
	return lines
}

// statusRow builds the one-row statusline: left widgets, Go chrome flags,
// then right widgets aligned to the right edge (DESIGN §8).
func (a *App) statusRow(w int) []hostapi.Segment {
	left := a.statuslineSegments("statusline.left")
	right := a.statuslineSegments("statusline.right")

	if a.now().Before(a.quitArmed) {
		left = append([]hostapi.Segment{{Text: "ctrl+c again to quit ", HL: "Title"}}, left...)
	}
	if wn := a.focusedWin(); wn != nil {
		left = append([]hostapi.Segment{{Text: fmt.Sprintf(" window %d · %s to leave ", wn.ID, a.unfocusKey()), HL: "Host:BtnSel"}, {Text: " "}}, left...)
	}
	if len(a.keySeq) > 0 && a.now().Sub(a.keySeqAt) <= keySeqTimeout {
		left = append(left, hostapi.Segment{Text: " " + strings.Join(a.keySeq, " ") + " ... ", HL: "Title"})
	}
	pill := func(text, click string) {
		left = append(left, hostapi.Segment{Text: "  "}, hostapi.Segment{Text: text, HL: "ToastError", Click: click})
	}
	// a failed boot step also leaves the runtime hot: one pill is enough
	if a.bootErr != nil {
		pill(" boot failed ", "errors")
	} else if a.host.RT.IsHot() {
		pill(" stuck: /reload ", "reload")
	}
	if a.host.HasUnseenErrors() {
		pill(" errors: /errors ", "errors")
	}

	if n := a.rowsBelow(); n > 0 {
		left = append(left, hostapi.Segment{Text: fmt.Sprintf("  ↓ %d · ctrl+end", n), HL: "Title"})
	}

	leftW := segsWidth(left)
	rightW := segsWidth(right)
	// The clusters need a blank cell between them or they read as one word.
	if leftW+rightW >= w {
		// Overflow truncates the right cluster first (lowest priority of
		// the row's real estate goes last, §8); under four cells it would
		// show a stray letter or two, so it goes whole.
		if room := w - leftW - 1; room >= 4 {
			right = fitSegs(right, room)
			rightW = segsWidth(right)
		} else {
			left = truncateSegs(left, w)
			right, rightW = nil, 0
		}
	}
	gap := max(0, w-leftW-rightW)
	out := append([]hostapi.Segment{}, left...)
	if gap > 0 {
		out = append(out, hostapi.Segment{Text: strings.Repeat(" ", gap)})
	}
	return append(out, right...)
}

func (a *App) statuslineSegments(region string) []hostapi.Segment {
	var out []hostapi.Segment
	for _, wgt := range a.host.Widgets.InRegion(region) {
		lines := a.widgetLines(wgt.ID)
		if len(lines) == 0 || segsWidth(lines[0]) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, hostapi.Segment{Text: " "})
		}
		out = append(out, lines[0]...)
	}
	return out
}

// toastBox lays a toast out as a panel: a level-colored bar, the bold
// title, then the body word-wrapped, every row padded to one width.
func toastBox(t toast, w int) [][]hostapi.Segment {
	bar := map[string]string{"warn": "ToastWarn", "error": "ToastErr"}[t.level]
	if bar == "" {
		bar = "ToastInfo"
	}
	inner := max(min(w-2, 60)-3, 1)
	var body [][]hostapi.Segment
	if t.title != "" {
		body = append(body, truncateSegs([]hostapi.Segment{{Text: t.title, HL: "ToastTitle"}}, inner))
	}
	var rows [][]hostapi.Segment
	if t.body != "" {
		for l := range strings.SplitSeq(t.body, "\n") {
			rows = append(rows, wrapSegs([]hostapi.Segment{{Text: l, HL: "Toast"}}, inner)...)
		}
	}
	if len(rows) > toastRows {
		more := map[bool]string{true: "... full error in /errors", false: "..."}[t.recorded]
		rows = append(rows[:toastRows-1], []hostapi.Segment{{Text: more, HL: "Toast"}})
	}
	body = append(body, rows...)
	bw := 0
	for _, r := range body {
		bw = max(bw, segsWidth(r))
	}
	box := make([][]hostapi.Segment, 0, len(body))
	for _, r := range body {
		row := append([]hostapi.Segment{{Text: "▌ ", HL: bar}}, r...)
		box = append(box, append(row, hostapi.Segment{Text: strings.Repeat(" ", bw-segsWidth(r)+1), HL: "Toast"}))
	}
	return box
}

// ---------------------------------------------------------------------------
// segment helpers

// hlStyle resolves a highlight name. "Host:*" names are Go-owned security
// chrome (DESIGN §8): they resolve to fixed styles and never consult the theme.
func (a *App) hlStyle(hl string) uv.Style {
	if st, ok := fixedStyle(hl); ok {
		return st
	}
	return a.thm.style(hl)
}

// renderSegs styles and joins segments, truncating to w visible cells.
func (a *App) renderSegs(segs []hostapi.Segment, w int) string {
	var b strings.Builder
	for _, s := range segs {
		if s.Text == "" {
			continue
		}
		st := a.hlStyle(s.HL)
		b.WriteString(st.Styled(s.Text))
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
	return append(append([]hostapi.Segment{}, segs...), hostapi.Segment{Text: strings.Repeat(" ", pad)})
}

func segsWidth(segs []hostapi.Segment) int {
	n := 0
	for _, s := range segs {
		n += ansi.StringWidth(s.Text)
	}
	return n
}

// wrapSegs word-wraps one logical line into rows of at most w cells,
// preserving each chunk's style; words longer than w are hard-split. The
// transcript is bottom-anchored, so callers concatenate the rows and take
// the tail.
func wrapSegs(line []hostapi.Segment, w int) [][]hostapi.Segment {
	w = max(w, 1)
	if segsWidth(line) <= w {
		return [][]hostapi.Segment{line}
	}
	// Continuation rows hang under the text, past a short gutter segment
	// ("  ✗ ", "▌ ", "    └ ") or leading indentation.
	indent := 0
	if len(line) > 0 {
		if g := line[0].Text; strings.HasSuffix(g, " ") && ansi.StringWidth(g) <= 8 && ansi.StringWidth(g) < w/2 {
			indent = ansi.StringWidth(g)
		}
	}
	var rows [][]hostapi.Segment
	var row []hostapi.Segment
	width := 0
	flush := func() {
		rows = append(rows, row)
		row, width = nil, 0
		if indent > 0 {
			row, width = []hostapi.Segment{{Text: strings.Repeat(" ", indent)}}, indent
		}
	}
	// add keeps every field of seg on each piece of text
	add := func(text string, seg hostapi.Segment) {
		piece := func(t string) hostapi.Segment { seg.Text = t; return seg }
		for {
			if width >= w {
				flush()
			}
			tw := ansi.StringWidth(text)
			if tw <= w-width {
				row = append(row, piece(text))
				width += tw
				return
			}
			cut := ansi.Truncate(text, w-width, "")
			if cut == "" {
				if width <= indent { // a wide rune that fits nowhere: place it anyway
					row = append(row, piece(text))
					width += tw
					return
				}
				flush()
				continue
			}
			row = append(row, piece(cut))
			text = strings.TrimPrefix(text, cut)
			flush()
		}
	}
	for _, seg := range line {
		for _, word := range strings.SplitAfter(seg.Text, " ") {
			if word == "" {
				continue
			}
			if width+ansi.StringWidth(word) > w && width > indent {
				flush()
			}
			add(word, seg)
		}
	}
	if width > indent || len(rows) == 0 {
		rows = append(rows, row)
	}
	return rows
}
