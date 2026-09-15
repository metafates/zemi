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
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
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
	// (with a queue for concurrent asks), the model picker and the memory
	// review overlay.
	permAsk   *permAskPrompt
	permQueue []*permAskPrompt
	modelPick *modelPickPrompt
	memReview *memReviewPrompt

	mouse          mouseSel
	hovering       bool
	hoverX, hoverY int
	hoverKey       string // what the pointer is over, to redraw only on change
	pointerHand    bool
	lastRows       [][]hostapi.Segment // the last composed frame, for clicks and copy
	lastChatBottom int

	// work runs Lua-backed keymaps and commands in order off the key loop
	work      chan func()
	workOnce  sync.Once
	cfgReview *cfgReviewPrompt
	question  *questionPrompt

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

	bootErr error

	program   *tea.Program
	reload    func()
	hooks     chan tea.Msg
	testQueue chan tea.Msg
}

// widgetCache holds the last-good rendered lines of one widget keyed by
// (id); the frame they were rendered at gates reuse (DESIGN §4, §8).
type widgetCache struct {
	lines [][]hostapi.Segment
	width int
	gen   int64

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
			a.Notify("warn", "quit", "blocked: "+res.Reason)
			return
		}
		if a.program != nil {
			a.program.Quit()
		}
	}
	if a.program != nil {
		go quit()
		return
	}
	quit()
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

type modelPick struct {
	id string
	ok bool
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
	return tea.Batch(a.frameTickCmd(), a.drainHooksCmd())
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
	case tea.FocusMsg:
		a.focused = true
	case tea.BlurMsg:
		a.focused = false
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.host.EmitAsync("ui.resize", map[string]any{"width": msg.Width, "height": msg.Height})
		a.frameGen++
		return a, a.frameTickCmd()
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
			a.scrollChatRows(step * chatWheelStep)
		}
		return a, nil
	case editorMsg:
		if msg.err != nil {
			a.pushToast("error", "editor", msg.err.Error())
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
			a.pushToast("warn", "paste", msg.err.Error())
			return a, nil
		}
		a.insertToken(msg.token)
		a.publishComposer()
		return a, nil
	case tea.PasteMsg:
		if a.pasteChrome(msg.Content) {
			return a, nil
		}
		// a dropped file arrives as a paste of its path
		if paths, ok := parseDrop(msg.Content); ok {
			a.insertAttachments(paths)
			a.publishComposer()
			return a, nil
		}
		s := msg.Content
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
	return v
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
	if a.host.AgentActive() && now.Sub(a.activeBump) >= 125*time.Millisecond {
		a.activeBump = now
		a.frameGen++
	}
	return tea.Batch(a.frameTickCmd(), a.renderStaleWidgetsCmd())
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
	delete(a.pending, msg.id)
	if msg.err != nil {
		if !isTolerableRenderErr(msg.err) {
			a.host.RecordError("widget "+msg.id, msg.err.Error())
		}
		a.log.Debug("ui", "widget render failed; keeping last good lines", map[string]any{
			"widget": msg.id, "error": msg.err.Error(),
		})
		return
	}
	a.caches[msg.id] = &widgetCache{lines: msg.lines, width: msg.width, gen: msg.gen}
}

// isTolerableRenderErr reports failures the UI expects under load (hot
// mode, reload in flight): they keep the cached lines without flagging.
func isTolerableRenderErr(err error) bool {
	return err == lua.ErrHot || err == lua.ErrReloaded || err == lua.ErrCanceled
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

func (a *App) widgetWidth() int {
	if a.width <= 0 {
		return 80
	}
	return a.width
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
			lines, err := a.host.RenderWidget(a.ctx(), w, width, st)
			return widgetRenderedMsg{id: w.ID, lines: lines, width: width, gen: gen, err: err}
		})
	}
	return tea.Batch(cmds...)
}

func (a *App) ctx() context.Context { return context.Background() }

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
}

func (a *App) pushToast(level, title, body string) {
	a.toasts = append(a.toasts, toast{level, title, body, time.Now().Add(toastTTL)})
	if len(a.toasts) > 3 {
		a.toasts = a.toasts[len(a.toasts)-3:]
	}
}

func (a *App) pruneToasts(now time.Time) {
	a.toasts = slices.DeleteFunc(a.toasts, func(t toast) bool { return !now.Before(t.expires) })
}

// ---------------------------------------------------------------------------
// hook plumbing (drained by a persistent cmd when a program is attached)

func (a *App) dispatch(msg tea.Msg) {
	q := a.testQueue
	if a.program != nil {
		q = a.hooks
	}
	if q == nil {
		// No program (headless boot): process inline on the caller's goroutine.
		a.Update(msg)
		return
	}
	select {
	case q <- msg:
	default:
		a.log.Warn("ui", "hook queue full; dropping message", nil)
	}
}

// CaptureHooks redirects hook dispatch into a queue drained by Pump
// instead of inline processing. For tests that answer hooks from other
// goroutines: with capture on, all state mutation happens on the single
// goroutine that calls Pump, so no locks are needed.
func (a *App) CaptureHooks() {
	if a.testQueue == nil {
		a.testQueue = make(chan tea.Msg, 64)
	}
}

// Pump delivers one queued hook message; it reports whether one was
// processed. Call from the owning goroutine until it returns false.
func (a *App) Pump() bool {
	if a.testQueue == nil {
		return false
	}
	select {
	case msg := <-a.testQueue:
		a.Update(msg)
		return true
	default:
		return false
	}
}

func (a *App) drainHooksCmd() tea.Cmd {
	return func() tea.Msg { return hookMsg{inner: <-a.hooks} }
}

// ---------------------------------------------------------------------------
// compose (pure: reads caches and Go-side state only)

func (a *App) compose(w, h int) string {
	// The default UI registers no header; the region stays for Lua.
	header := a.regionLines("header", w)

	compRows := a.composerRows(w)
	compPopup := a.completionRows(w)
	palPopup := a.paletteRows(w)
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

	chat := a.regionLines("chat", w)

	// Chat scrollback (§8): follow pins the window to the tail (chatTop
	// may go negative when content is shorter than the viewport, which
	// bottom-anchors it); otherwise chatTop is the absolute first visible
	// row, clamped so content growth or a narrower rewrap never shows
	// blank rows.
	a.chatRows = len(chat)
	if a.chatViewH != chatH {
		a.frameGen++ // widgets read the height (the splash centers on it)
	}
	a.chatViewH = chatH
	if a.chatFollow {
		a.chatTop = a.chatRows - chatH
	} else {
		a.chatTop = min(a.chatTop, max(0, a.chatRows-chatH))
	}

	rows := make([][]hostapi.Segment, 0, h)
	rows = append(rows, header...)

	for i := 0; i < chatH; i++ {
		var row []hostapi.Segment
		if j := a.chatTop + i; j >= 0 && j < len(chat) {
			row = truncateSegs(chat[j], w)
		}
		rows = append(rows, padSegs(row, w))
	}

	// Toasts stack up from the bottom of the chat area, newest lowest,
	// right-aligned over the chat rows they cover, one blank row apart.
	y := len(rows)
	for i := len(a.toasts) - 1; i >= 0; i-- {
		box := toastBox(a.toasts[i], w)
		for j := len(box) - 1; j >= 0 && y > len(header); j-- {
			y--
			x0 := max(0, w-segsWidth(box[j])-1)
			rows[y] = append(padSegs(truncateSegs(rows[y], x0), x0), box[j]...)
		}
		y--
	}

	rows = floatPopup(rows, append(compPopup, palPopup...), w, len(header), chatH)

	rows = append(rows, padSegs(nil, w))
	// The working row renders above the composer.
	rows = append(rows, working...)
	rows = append(rows, compRows...)
	rows = append(rows, padSegs(nil, w), a.statusRow(w))

	// Lua overlays, then the Go-owned chrome, bottom to top; topChrome must
	// match this order so input reaches the box drawn on top.
	splice := func(box [][]hostapi.Segment) { rows = spliceBox(rows, box, w, len(header), chatH) }
	for _, ov := range a.overlays {
		if ov.o.Anchor == "bottom" {
			rows = anchorBottom(rows, overlayBox(ov, w), w, len(header), chatH)
			continue
		}
		splice(overlayBox(ov, w))
	}
	if a.trust != nil {
		splice(trustBox(a.trust, w))
	}
	if a.modelPick != nil {
		splice(pickBox(a.modelPick, w))
	}
	if a.question != nil {
		splice(questionBox(a.question, w))
	}
	if a.memReview != nil {
		splice(memReviewBox(a.memReview, w))
	}
	if a.cfgReview != nil {
		splice(cfgReviewBox(a.cfgReview, w))
	}
	if a.permAsk != nil {
		splice(a.permBox(a.permAsk, w, chatH))
	}

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
	return b.String()
}

// floatPopup frames a completion popup and lays it over the bottom rows of
// the chat area, left-aligned with the composer. The rows it covers are
// blanked to the right of the frame (word fragments beside a dropdown read
// as garbage); a popup taller than the chat is clipped.
func floatPopup(rows, popup [][]hostapi.Segment, w, headerH, chatH int) [][]hostapi.Segment {
	if len(popup) == 0 {
		return rows
	}
	box := hostBox("", "", "", popup, w)
	bottom := min(headerH+chatH, len(rows))
	y0 := max(headerH, bottom-len(box))
	for i := y0; i < bottom; i++ {
		rows[i] = padSegs(box[i-y0], w)
	}
	return rows
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
		a.chatFollow = false
		a.chatTop = 0
		a.frameGen++
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

// expandTabs turns tabs into four spaces: a tab has no cell width, so
// layout and drawing would both lose it.
func expandTabs(lines [][]hostapi.Segment) [][]hostapi.Segment {
	for i, line := range lines {
		for j, s := range line {
			if strings.Contains(s.Text, "\t") {
				if j == 0 {
					line = append([]hostapi.Segment(nil), line...)
					lines[i] = line
				}
				line[j].Text = strings.ReplaceAll(s.Text, "\t", "    ")
			}
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
	if len(a.keySeq) > 0 && a.now().Sub(a.keySeqAt) <= keySeqTimeout {
		left = append(left, hostapi.Segment{Text: " " + strings.Join(a.keySeq, " ") + " ... ", HL: "Title"})
	}
	pill := func(text string) {
		left = append(left, hostapi.Segment{Text: "  "}, hostapi.Segment{Text: text, HL: "ToastError"})
	}
	if a.bootErr != nil {
		pill(" boot failed ")
	}
	if a.host.RT.IsHot() {
		pill(" stuck: /reload ")
	}
	if a.host.HasUnseenErrors() {
		pill(" ! errors /errors ")
	}

	leftW := segsWidth(left)
	rightW := segsWidth(right)
	if leftW+rightW > w {
		// Overflow truncates the right cluster first (lowest priority of
		// the row's real estate goes last, §8).
		if w-leftW > 0 {
			right = truncateSegs(right, w-leftW)
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
	if t.body != "" {
		for _, l := range strings.Split(t.body, "\n") {
			body = append(body, wrapSegs([]hostapi.Segment{{Text: l, HL: "Toast"}}, inner)...)
		}
	}
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

// renderSegs styles and joins segments, truncating to w visible cells.
// "Host:*" highlight names are Go-owned security chrome (DESIGN §8): they
// resolve to fixed styles and never consult the theme.
func (a *App) renderSegs(segs []hostapi.Segment, w int) string {
	var b strings.Builder
	for _, s := range segs {
		if s.Text == "" {
			continue
		}
		if fs, ok := fixedStyle(s.HL); ok {
			b.WriteString(fs.Styled(s.Text))
			continue
		}
		st := a.thm.style(s.HL)
		b.WriteString(st.Styled(s.Text))
	}
	out := b.String()
	if ansi.StringWidth(out) > w {
		out = ansi.Truncate(out, w, "")
	}
	return out
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
			out = append(out, hostapi.Segment{Text: ansi.Truncate(s.Text, remaining, ""), HL: s.HL, Hit: s.Hit})
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
	add := func(text, hl string) {
		for {
			if width >= w {
				flush()
			}
			tw := ansi.StringWidth(text)
			if tw <= w-width {
				row = append(row, hostapi.Segment{Text: text, HL: hl})
				width += tw
				return
			}
			cut := ansi.Truncate(text, w-width, "")
			if cut == "" {
				if width <= indent { // a wide rune that fits nowhere: place it anyway
					row = append(row, hostapi.Segment{Text: text, HL: hl})
					width += tw
					return
				}
				flush()
				continue
			}
			row = append(row, hostapi.Segment{Text: cut, HL: hl})
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
			add(word, seg.HL)
		}
	}
	if width > indent || len(rows) == 0 {
		rows = append(rows, row)
	}
	return rows
}
