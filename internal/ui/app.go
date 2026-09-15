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
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
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
	toastSeq int

	// Go-owned agent chrome (DESIGN §6.3, §8): the permission overlay
	// (with a queue for concurrent asks), the model picker and the memory
	// review overlay.
	permAsk   *permAskPrompt
	permQueue []permAskMsg
	modelPick *modelPickPrompt
	memReview *memReviewPrompt
	cfgReview *cfgReviewPrompt
	question  *questionPrompt

	caches     map[string]*widgetCache
	frameGen   int64
	timeStr    string
	activeBump time.Time

	bootErr error
	errFlag bool

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
		chatFollow: true,
		hooks:      make(chan tea.Msg, hookQueueSize),
	}
}

// SetProgram wires the running bubbletea program (for Quit/Send).
func (a *App) SetProgram(p *tea.Program) { a.program = p }

// SetReload installs the Go-reserved :Reload dispatch (built by main).
func (a *App) SetReload(fn func()) { a.reload = fn }

// SignalBoot reports the boot outcome from the boot goroutine (posted
// through the hook channel; safe from any goroutine).
func (a *App) SignalBoot(err error) { a.dispatch(bootDoneMsg{err: err}) }

// Quit asks the program to exit (Go-reserved /q command). The send must
// not happen on the UI goroutine: the bubbletea event loop owns the only
// receive of the (unbuffered) msgs channel and is currently inside our
// Update, so a direct Quit() deadlocks the /q command.
func (a *App) Quit() {
	if a.program != nil {
		go a.program.Quit()
	}
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

type invalidateMsg struct{}

type chatScrollMsg struct{ kind string }

type bootDoneMsg struct{ err error }

type trustAskMsg struct {
	path   string
	hash   string
	answer chan bool
}

type agentEventMsg struct{ ev hostapi.AgentEvent }

type permAskMsg struct {
	req    hostapi.PermRequest
	answer chan permAnswer
}

type permAnswer struct {
	ok bool
	by string
}

type modelPick struct {
	id string
	ok bool
}

type modelPickMsg struct {
	items   []hostapi.ModelOption
	sel     int
	current string
	answer  chan modelPick
}

type memReviewMsg struct {
	items  []hostapi.MemoryRow
	answer chan []int
}

type cfgReviewMsg struct {
	items  []hostapi.ConfigRow
	answer chan cfgReviewVerdict
}

type questionMsg struct {
	q      tools.Question
	answer chan qAnswer
}

type toastMsg struct {
	level string
	title string
	body  string
}

type overlayOpenMsg struct{ o hostapi.Overlay }

type overlayCloseMsg struct{ id string }

// cfgBool reads a bool config key with a fallback.
func (a *App) cfgBool(key string, def bool) bool {
	if v, ok := a.host.Settings.Get(key); ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// cfgInt reads an int config key clamped to [min, max].
func (a *App) cfgInt(key string, def, min, max int) int {
	if v, ok := a.host.Settings.Get(key); ok {
		switch n := v.(type) {
		case int:
			return clampInt(n, min, max)
		case float64:
			return clampInt(int(n), min, max)
		}
	}
	return def
}

func clampInt(n, min, max int) int {
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
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
	case invalidateMsg:
		a.frameGen++
	case bootDoneMsg:
		a.bootErr = msg.err
		if msg.err != nil {
			a.log.Error("ui", "boot failed", map[string]any{"error": msg.err.Error()})
		}
		a.frameGen++
	case trustAskMsg:
		a.trust = &trustPrompt{path: msg.path, hash: msg.hash, answer: msg.answer}
		a.frameGen++
	case agentEventMsg:
		// Stream state lives in the host; just invalidate the frame.
		a.frameGen++
	case permAskMsg:
		if a.permAsk != nil {
			a.permQueue = append(a.permQueue, msg)
		} else {
			a.permAsk = &permAskPrompt{req: msg.req, answer: msg.answer}
		}
		a.frameGen++
	case modelPickMsg:
		a.modelPick = &modelPickPrompt{items: msg.items, sel: msg.sel, answer: msg.answer}
		a.frameGen++
	case memReviewMsg:
		a.memReview = &memReviewPrompt{items: msg.items, marked: map[int]bool{}, answer: msg.answer}
		a.frameGen++
	case cfgReviewMsg:
		a.cfgReview = &cfgReviewPrompt{items: msg.items, sel: 0, answer: msg.answer}
		a.frameGen++
	case questionMsg:
		a.question = &questionPrompt{q: msg.q, sel: 0, answer: msg.answer}
		a.frameGen++
	case toastMsg:
		a.pushToast(msg.level, msg.title, msg.body)
	case chatScrollMsg:
		a.applyChatScroll(msg.kind)
	case overlayOpenMsg:
		a.openOverlay(msg.o)
	case overlayCloseMsg:
		a.closeOverlayByID(msg.id)
		a.frameGen++
	case hookMsg:
		_, cmd := a.Update(msg.inner)
		// Re-arm the persistent drain: each delivered hookMsg re-arms
		// exactly one, so the chain never breaks.
		return a, tea.Batch(cmd, a.drainHooksCmd())
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.host.EmitAsync("ui.resize", map[string]any{"width": msg.Width, "height": msg.Height})
		a.frameGen++
		return a, a.frameTickCmd()
	case tea.KeyPressMsg:
		return a, a.handleKey(msg)
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			a.scrollChatRows(-chatWheelStep)
		case tea.MouseWheelDown:
			a.scrollChatRows(chatWheelStep)
		}
		return a, nil
	case tea.PasteMsg:
		s := msg.Content
		if !a.multiline() {
			s = strings.ReplaceAll(s, "\n", " ")
		}
		a.composer.insert(s)
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
	// Cell motion: the wheel scrolls the transcript. The altscreen buffer
	// has no native scrollback, so without capture the wheel does nothing.
	v.MouseMode = tea.MouseModeCellMotion
	return v
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
	cmds := []tea.Cmd{a.frameTickCmd()}
	// The clock plays timer.fire's role in M0: bumping the generation on
	// a minute change re-renders time-reading widgets (DESIGN §8).
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
	cmds = append(cmds, a.renderStaleWidgetsCmd())
	return tea.Batch(cmds...)
}

func (a *App) uiState() hostapi.UIState {
	return hostapi.UIState{Time: a.timeStr}
}

func (a *App) onWidgetRendered(msg widgetRenderedMsg) {
	if msg.err != nil {
		if !isTolerableRenderErr(msg.err) {
			a.errFlag = true
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
	return err == nil || err == lua.ErrHot || err == lua.ErrReloaded || err == lua.ErrCanceled
}

// visibleRegions lists the regions composed into the frame together with
// the width each widget renders at.
func (a *App) visibleWidgets() []*hostapi.Widget {
	var out []*hostapi.Widget
	out = append(out, a.host.Widgets.InRegion("header")...)
	out = append(out, a.host.Widgets.InRegion("chat")...)
	if a.host.SidebarOpen() {
		out = append(out, a.host.Widgets.InRegion("sidebar")...)
	}
	out = append(out, a.host.Widgets.InRegion("statusline.left")...)
	out = append(out, a.host.Widgets.InRegion("statusline.right")...)
	return out
}

func (a *App) widgetWidth(w *hostapi.Widget) int {
	if a.width <= 0 {
		return 80
	}
	switch {
	case strings.HasPrefix(w.Region, "statusline"):
		return a.width
	case w.Region == "sidebar":
		sw := a.sidebarWidth()
		if sw > a.width/2 {
			sw = a.width / 2
		}
		return sw
	case w.Region == "chat":
		chatW := a.width
		if a.host.SidebarOpen() {
			chatW = a.width - a.sidebarWidth() - 1
		}
		if chatW < 1 {
			chatW = 1
		}
		return chatW
	}
	return a.width
}

// renderStaleWidgetsCmd enqueues ui-class renders for widgets whose cache
// is missing or stale for the current frame generation.
func (a *App) renderStaleWidgetsCmd() tea.Cmd {
	gen := a.frameGen
	var cmds []tea.Cmd
	for _, w := range a.visibleWidgets() {
		c := a.caches[w.ID]
		width := a.widgetWidth(w)
		if c != nil && c.gen == gen && c.width == width {
			continue
		}
		st := a.uiState()
		w := w
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

func (a *App) pushToast(level, title, body string) {
	a.toastSeq++
	a.toasts = append(a.toasts, toast{
		id: a.toastSeq, level: level, title: title, body: body,
		expires: time.Now().Add(toastTTL),
	})
	if len(a.toasts) > 3 {
		a.toasts = a.toasts[len(a.toasts)-3:]
	}
}

func (a *App) pruneToasts(now time.Time) {
	keep := a.toasts[:0]
	for _, t := range a.toasts {
		if now.Before(t.expires) {
			keep = append(keep, t)
		}
	}
	a.toasts = keep
}

// ---------------------------------------------------------------------------
// hook plumbing (drained by a persistent cmd when a program is attached)

func (a *App) dispatch(msg tea.Msg) {
	if a.program != nil {
		select {
		case a.hooks <- hookMsg{inner: msg}:
		default:
			a.log.Warn("ui", "hook queue full; dropping message", nil)
		}
		return
	}
	if a.testQueue != nil {
		select {
		case a.testQueue <- msg:
		default:
			a.log.Warn("ui", "hook queue full; dropping message", nil)
		}
		return
	}
	// No program (headless boot): process inline on the caller's goroutine.
	a.Update(msg)
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
	// Header.
	header := a.regionLines("header", w)
	if len(header) == 0 {
		header = [][]hostapi.Segment{{
			{Text: " zemi ", HL: "StatusLineMode"},
			{Text: " " + a.host.Session().CWD, HL: "Comment"},
		}}
	}

	compRows := a.composerRows(w)
	compPopup := a.completionRows(w)
	palPopup := a.paletteRows(w)
	// + statusline + composer (+ its popups)
	reserved := len(header) + 1 + len(compRows) + len(compPopup) + len(palPopup)
	chatH := h - reserved
	if chatH < 1 {
		chatH = 1
	}

	// Chat area + optional sidebar column.
	chatW := w
	var sidebar [][]hostapi.Segment
	sw := 0
	if a.host.SidebarOpen() {
		sw = a.sidebarWidth()
		if sw > w/2 {
			sw = w / 2
		}
		if sw < 5 {
			sw = 5
		}
		sidebar = a.regionLines("sidebar", sw)
		chatW = w - sw - 1
		if chatW < 1 {
			chatW = 1
		}
	}
	chat := a.regionLines("chat", chatW)

	// Chat scrollback (§8): follow pins the window to the tail (chatTop
	// may go negative when content is shorter than the viewport, which
	// bottom-anchors it); otherwise chatTop is the absolute first visible
	// row, clamped so content growth or a narrower rewrap never shows
	// blank rows.
	a.chatRows = len(chat)
	a.chatViewH = chatH
	if a.chatFollow {
		a.chatTop = a.chatRows - chatH
	} else if maxTop := max0(a.chatRows - chatH); a.chatTop > maxTop {
		a.chatTop = maxTop
	}

	rows := make([][]hostapi.Segment, 0, h)
	rows = append(rows, header...)

	for i := 0; i < chatH; i++ {
		var row []hostapi.Segment
		if j := a.chatTop + i; j >= 0 && j < len(chat) {
			row = truncateSegs(chat[j], chatW)
		}
		row = padSegs(row, chatW)
		if sw > 0 {
			row = append(row, hostapi.Segment{Text: " "})
			var srow []hostapi.Segment
			if i < len(sidebar) {
				srow = sidebar[i]
			}
			row = append(row, truncateSegs(srow, sw)...)
		}
		rows = append(rows, row)
	}

	// Toasts stack up from the bottom of the chat area, newest lowest,
	// right-aligned over the chat rows they cover, one blank row apart.
	y := len(rows)
	for i := len(a.toasts) - 1; i >= 0; i-- {
		box := toastBox(a.toasts[i], w)
		for j := len(box) - 1; j >= 0 && y > len(header); j-- {
			y--
			x0 := max0(w - segsWidth(box[j]) - 1)
			rows[y] = append(padSegs(truncateSegs(rows[y], x0), x0), box[j]...)
		}
		y--
	}

	// Popups render above the composer, then the composer rows.
	rows = append(rows, compPopup...)
	rows = append(rows, palPopup...)
	rows = append(rows, compRows...)
	// Statusline.
	rows = append(rows, a.statusRow(w))

	// Lua overlays, then the Go-owned chrome on top: trust prompt, model
	// picker, permission ask (most urgent).
	for _, ov := range a.overlays {
		rows = a.spliceOverlay(rows, ov, w, len(header), chatH)
	}
	if a.trust != nil {
		rows = a.spliceTrust(rows, a.trust, w, len(header), chatH)
	}
	if a.modelPick != nil {
		rows = a.splicePick(rows, a.modelPick, w, len(header), chatH)
	}
	if a.question != nil {
		rows = a.spliceQuestion(rows, a.question, w, len(header), chatH)
	}
	if a.memReview != nil {
		rows = a.spliceMemReview(rows, a.memReview, w, len(header), chatH)
	}
	if a.cfgReview != nil {
		rows = a.spliceCfgReview(rows, a.cfgReview, w, len(header), chatH)
	}
	if a.permAsk != nil {
		rows = a.splicePerm(rows, a.permAsk, w, len(header), chatH)
	}

	var b strings.Builder
	for i, row := range rows {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(a.renderSegs(row, w))
	}
	return b.String()
}

// chatStep is one page of transcript scroll with a one-row overlap, from
// the last composed chat height (compose runs every frame, so the value is
// at most one frame stale).
func (a *App) chatStep() int {
	if step := a.chatViewH - 1; step > 0 {
		return step
	}
	return 1
}

// scrollChatRows moves the transcript viewport by n rows (negative =
// older). Scrolling down re-arms tail follow at the bottom; while
// scrolled up the view stays anchored as new lines arrive.
func (a *App) scrollChatRows(n int) {
	if n > 0 && a.chatFollow {
		return
	}
	if n < 0 && a.chatFollow {
		a.chatFollow = false
		a.chatTop = max0(a.chatRows - a.chatViewH)
	}
	a.chatTop += n
	if a.chatTop < 0 {
		a.chatTop = 0
	}
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
		lines := a.widgetLines(wgt.ID)
		if wgt.Wrap {
			// Word-wrap instead of truncating; the transcript shows the
			// wrapped tail (bottom-anchored).
			for _, line := range lines {
				out = append(out, wrapSegs(line, width)...)
			}
			continue
		}
		for _, line := range lines {
			out = append(out, truncateSegs(line, width))
		}
	}
	return out
}

func (a *App) sidebarWidth() int {
	if v, ok := a.host.Settings.Get("ui.sidebar.width"); ok {
		if n, ok := v.(int); ok && n > 0 {
			return n
		}
		if f, ok := v.(float64); ok && f > 0 {
			return int(f)
		}
	}
	return 30
}

// statusRow builds the one-row statusline: left widgets, Go chrome flags,
// then right widgets aligned to the right edge (DESIGN §8).
func (a *App) statusRow(w int) []hostapi.Segment {
	left := a.statuslineSegments("statusline.left")
	right := a.statuslineSegments("statusline.right")

	if a.bootErr != nil {
		left = append(left, hostapi.Segment{Text: " boot failed ", HL: "ToastError"})
	}
	if a.host.RT.IsHot() {
		left = append(left, hostapi.Segment{Text: " HOT :Reload ", HL: "ToastError"})
	}
	if a.errFlag {
		left = append(left, hostapi.Segment{Text: " !errors ", HL: "ToastError"})
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
	gap := w - leftW - rightW
	if gap < 0 {
		gap = 0
	}
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
			out = append(out, hostapi.Segment{Text: ansi.Truncate(s.Text, remaining, ""), HL: s.HL})
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
	if w < 1 {
		w = 1
	}
	if segsWidth(line) <= w {
		return [][]hostapi.Segment{line}
	}
	var rows [][]hostapi.Segment
	var row []hostapi.Segment
	width := 0
	flush := func() {
		rows = append(rows, row)
		row, width = nil, 0
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
			if width+ansi.StringWidth(word) > w && width > 0 {
				flush()
			}
			add(word, seg.HL)
		}
	}
	if width > 0 || len(rows) == 0 {
		flush()
	}
	return rows
}
