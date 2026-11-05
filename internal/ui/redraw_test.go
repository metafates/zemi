package ui

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/store"
)

// TestProgressReport: the view reports OSC 9;4 while a turn runs, changes it
// after the interval so the renderer resends it, and drops it when the turn
// ends.
func TestProgressReport(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 10)
	app.progressTick(true)
	first := app.View().ProgressBar
	app.progressTick(true)
	if first == nil || *app.View().ProgressBar != *first {
		t.Fatalf("a running turn should report, unchanged before the interval: %+v", first)
	}
	app.now = func() time.Time { return time.Now().Add(_progressInterval + time.Second) }
	if app.progressTick(true); *app.View().ProgressBar == *first {
		t.Fatal("a running turn should change the report after the interval")
	}
	if app.progressTick(false); app.View().ProgressBar != nil {
		t.Fatal("the end of a turn should clear the report")
	}
}

// TestRedrawAfterCallbacks: a one-shot timer's change reaches the screen
// with no other redraw, zemi.ui.redraw asks for one, and a burst of
// refreshes waits in the hook queue as one.
func TestRedrawAfterCallbacks(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 10)
	runLua(t, app, `zemi.event.on("user.tick", function() zemi.timer.after(0, function() zemi.notify.info("ran", "Tick") end) end)`)
	// Read before the timer exists: its refresh can land while runLua still pumps.
	gen := app.frameGen
	runLua(t, app, `zemi.event.emit("user.tick")`)
	for deadline := time.Now().Add(3 * time.Second); app.frameGen == gen; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the timer callback did not redraw")
		}
		for app.Pump() {
		}
	}
	if frame := app.screen(t); !strings.Contains(frame, "Tick") {
		t.Fatalf("frame after the timer:\n%s", frame)
	}

	gen = app.frameGen
	runLua(t, app, `zemi.ui.redraw()`)
	if app.frameGen == gen {
		t.Fatal("zemi.ui.redraw did not bump the frame")
	}
	for range 200 {
		app.Refresh()
	}
	if n := len(app.testQueue); n != 1 {
		t.Fatalf("200 refreshes queued %d hooks, want 1", n)
	}
}

// TestIdleDrawsNothing: the 50 ms frame clock re-renders nothing while zemi
// sits idle, and the animation tick alone never does. A running turn ticks
// the readouts, and the mascot's done frame comes and goes once each.
func TestIdleDrawsNothing(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	app.Update(frameTickMsg{}) // the first tick settles the clock and the flags
	gen := app.frameGen
	for ms := range int64(20) {
		app.now = func() time.Time { return time.UnixMilli(ms * 500) }
		app.Update(frameTickMsg{})
	}
	if app.frameGen != gen {
		t.Fatalf("idle: %d redraws over 10s of ticks", app.frameGen-gen)
	}
	if app.tickInterval() != _idleInterval {
		t.Fatal("idle should tick slowly")
	}
	host.SeedStream(hostapi.StreamSnapshot{Active: true})
	app.Update(frameTickMsg{})
	if app.frameGen == gen {
		t.Fatal("a running turn does not tick the working row")
	}
	if app.tickInterval() != _frameInterval {
		t.Fatal("a running turn should tick at frame rate")
	}

	host.SeedStream(hostapi.StreamSnapshot{})
	app.Update(turnDoneMsg{})
	app.Update(frameTickMsg{}) // the turn ends: the mascot bows
	gen = app.frameGen
	app.now = func() time.Time { return app.turnEnd.Add(_doneLinger + time.Second) }
	app.Update(frameTickMsg{}) // it stops bowing
	app.Update(frameTickMsg{})
	if app.frameGen != gen+1 {
		t.Fatalf("the done frame cost %d redraws, want 1", app.frameGen-gen)
	}

	// a run left 'running' in the store by a crash is one this process does
	// not hold: viewing it reads as cancelled and ticks nothing
	host.SeedStream(hostapi.StreamSnapshot{})
	testStore(t, host)
	id := store.ShortID()
	if err := host.DB.CreateSession(id, store.ShortID(), "run", "explore"); err != nil {
		t.Fatal(err)
	}
	if err := host.DB.SetSessionRunStatus(id, "running"); err != nil {
		t.Fatal(err)
	}
	if err := host.ViewAgent(id); err != nil {
		t.Fatal(err)
	}
	// the view's own bumps settle before the frame is measured: a gen they
	// land after the snapshot is the load race, not a tick
	for app.Pump() {
	}
	gen = app.frameGen
	app.Update(frameTickMsg{})
	if app.frameGen != gen {
		t.Fatal("a stored 'running' row viewed ticks the frame")
	}
	if app.tickInterval() != _idleInterval {
		t.Fatal("a stored 'running' row viewed should tick idly")
	}
}

// TestBumpWakesIdleTick: a change while the tick sleeps idle wakes it, and
// every stale widget comes back in one message.
func TestBumpWakesIdleTick(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	woke := func() bool {
		select {
		case <-app.wake:
			return true
		default:
			return false
		}
	}
	woke()
	if app.bump(); !woke() {
		t.Fatal("the bump left the idle tick asleep")
	}
	if msg, ok := app.renderStaleWidgetsCmd(false)().(widgetsRenderedMsg); !ok || len(msg) < 2 {
		t.Fatalf("stale widgets should render as one message, got %T %v", msg, msg)
	}
	if app.onWidgetRendered(widgetRender{id: "chat", more: true}); !woke() {
		t.Fatal("a render asking to run again left the idle tick asleep")
	}
}

// TestFrameClock: a tick lands on the next frame boundary, from the timer
// and from a wake that cuts an idle sleep short alike, so the spinner and the
// eyes step one glyph per tick.
func TestFrameClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &App{wake: make(chan struct{}, 1)}
		time.Sleep(_frameInterval / 3) // off a boundary
		last := time.Now().UnixMilli() / _frameMS
		for i := range 6 {
			d := _frameInterval
			if i%2 == 1 {
				d = _idleInterval
				a.wakeTick()
			}
			a.frameTickCmd(d)()
			f := time.Now().UnixMilli() / _frameMS
			if f != last+1 || time.Now().UnixMilli()%_frameMS != 0 {
				t.Fatalf("tick %d ran at %v, after one in frame %d", i, time.Now(), last)
			}
			last = f
		}
	})
}

// TestFrameCache: a View with nothing behind it returns the frame it has,
// and every change that moves neither the generation nor the size - a
// composer key, a widget's lines, a toast coming and going - still repaints.
func TestFrameCache(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 14)
	app.renderSync(t)
	first := app.View().Content

	app.composer.text, app.composer.cursor = "changed under the cache", 23
	if app.View().Content != first {
		t.Fatal("a View with nothing behind it recomposed")
	}

	app.Update(tea.KeyPressMsg{Text: "x"})
	if frame := ansi.Strip(app.View().Content); !strings.Contains(frame, "changed under the cachex") {
		t.Fatalf("the draft did not repaint:\n%s", frame)
	}

	app.onWidgetRendered(widgetRender{id: "statusline.model", lines: [][]hostapi.Segment{{{Text: "fresh lines"}}}, width: 78, height: 12, gen: app.frameGen})
	if frame := ansi.Strip(app.View().Content); !strings.Contains(frame, "fresh lines") {
		t.Fatalf("a widget's lines did not repaint:\n%s", frame)
	}

	app.pushToast(slog.LevelInfo, "saved", "")
	app.View()        // the toast's rise starts
	app.settleRamps() // land it, a real clock would ride the 160ms
	if frame := ansi.Strip(app.View().Content); !strings.Contains(frame, "saved") {
		t.Fatalf("a toast did not show:\n%s", frame)
	}
	app.toasts[0].expires = time.Now().Add(-time.Second) // expired for the tray's clock too
	app.pruneToasts(time.Now().Add(time.Hour))
	app.View()        // the leave starts its sink
	app.settleRamps() // land it, a real clock would ride the 160ms
	if frame := ansi.Strip(app.View().Content); strings.Contains(frame, "saved") {
		t.Fatalf("an expired toast stayed:\n%s", frame)
	}

	// a frame that drew something moving by itself is never clean: its
	// shimmer steps with the frame clock though nothing else changed
	app.onWidgetRendered(widgetRender{id: "statusline.model", lines: [][]hostapi.Segment{{{Text: "breathing", HL: "Dim", Effect: hostapi.EffectShimmer}}}, width: 78, height: 12, gen: app.frameGen})
	app.View()
	app.now = func() time.Time { return time.UnixMilli(50) }
	slow := app.View().Content
	app.now = func() time.Time { return time.UnixMilli(100) }
	if fast := app.View().Content; fast == slow {
		t.Fatal("a shimmering frame stopped moving")
	}
}

// TestComposerGrowResizesSplash: the frames drawn between the composer
// growing and the splash's re-render at the chat's new height draw the stale
// splash cut to the region, no scrollbar, and the re-render lands at the new
// height.
func TestComposerGrowResizesSplash(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	app.screen(t)
	_, cmd := app.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	// the stale frame, drawn before the re-render at the new height lands
	for _, row := range app.lastRows {
		for _, seg := range row {
			if seg.Bar {
				t.Fatalf("a scrollbar is painted before the splash re-renders:\n%s", ansi.Strip(app.Frame()))
			}
		}
	}
	for _, msg := range runCmd(cmd) {
		if r, ok := msg.(widgetsRenderedMsg); ok {
			app.Update(r)
		}
	}
	app.Frame()
	i := slices.IndexFunc(app.shownWidgets(), func(w shownWidget) bool { return w.ID == "splash" })
	if i < 0 {
		t.Fatal("no splash is shown")
	}
	if w, c := app.shownWidgets()[i], app.caches["splash"]; c.height != w.height {
		t.Fatalf("splash rendered for %d rows, the chat has %d", c.height, w.height)
	}
}

// runCmd runs cmd and every command of the batches it returns, and returns
// their messages.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	b, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, c := range b {
		out = append(out, runCmd(c)...)
	}
	return out
}

// TestRailRise: a rail action the live turn starts rises out of the surface
// over the rise, one ramp per call, an action the earlier frames drew riding
// the one it started.
func TestRailRise(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	f := app.chatFolds()
	at := time.UnixMilli(1000)
	f.now = func() time.Time { return at }
	tools := []hostapi.StreamTool{{ID: "t1", Name: "read", Label: "read go.mod", Status: hostapi.CallRun}}

	fades := func() map[int]float64 {
		host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: tools})
		st := app.uiState()
		lines, _ := renderChat(t, app, 98, 20, st)
		out := map[int]float64{}
		for _, row := range lines {
			if c := rowCall(row); c != 0 {
				for _, s := range row {
					out[c] = max(out[c], s.Fade)
				}
			}
		}
		return out
	}

	got := fades()
	if got[-1] < 0.5 {
		t.Fatalf("a fresh call's row is not sunk: fade %v", got[-1])
	}
	// the border stands from the first frame: only the row's inside fades
	for _, row := range linesOf(t, app, -1) {
		if row[0].HL == "ThinkingBar" {
			if row[0].Fade != 0 {
				t.Fatalf("the rail's bar faded: %v", row[0])
			}
			if len(row) > 1 && row[1].Fade == 0 {
				t.Fatalf("the action inside did not fade: %v", row)
			}
		}
	}
	// the ramp passes: the call rides it risen
	at = at.Add(2 * _riseDuration)
	if got = fades(); got[-1] != 0 {
		t.Fatalf("the call stayed sunk after the ramp: fade %v", got[-1])
	}
	// a second call starts: it sinks, the first stays risen
	tools = []hostapi.StreamTool{
		{ID: "t1", Name: "read", Label: "read go.mod", Status: hostapi.CallOK},
		{ID: "t2", Name: "shell", Label: "run the tests", Status: hostapi.CallRun},
	}
	if got = fades(); got[-2] < 0.5 || got[-1] != 0 {
		t.Fatalf("the second call did not rise alone: %v", got)
	}
	// the output under a call's command appears when the result does, after
	// the call's own ramp: it sinks and rises on a ramp of its own
	tools[1].Status = hostapi.CallErr
	tools[1].Output = "no game-over line\nexit status 255"
	if got = fades(); got[-2] < 0.5 {
		t.Fatalf("the output the result brought did not sink: %v", got)
	}
	at = at.Add(2 * _riseDuration)
	if got = fades(); got[-2] != 0 {
		t.Fatalf("the output stayed sunk after its ramp: %v", got)
	}
	// a rail longer than it keeps folds its head away: once the kept calls
	// have risen, a new one still sinks though it adds no height
	tools = tools[:1]
	for i := 2; i <= _railKeep+2; i++ {
		tools = append(tools, hostapi.StreamTool{ID: fmt.Sprintf("t%d", i), Name: "shell", Label: "call", Status: hostapi.CallOK})
	}
	fades()
	at = at.Add(2 * _riseDuration)
	if got = fades(); got[-6] != 0 {
		t.Fatalf("the kept calls stayed sunk after their ramp: %v", got)
	}
	tools = append(tools, hostapi.StreamTool{ID: "t-new", Name: "shell", Label: "next", Status: hostapi.CallRun})
	if got = fades(); got[-len(tools)] < 0.5 || got[-6] != 0 {
		t.Fatalf("a call the folded rail scrolled in did not sink alone: %v", got)
	}

	// the thinking row the same turn draws rises like a call: the spinner row
	// sinks on its first draw and rides the ramp risen
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Thinking: true, Reasoning: "a thought"})
	lines, _ := renderChat(t, app, 98, 20, app.uiState())
	spin := 0.0
	for _, row := range lines {
		if len(row) > 2 && row[2].Text == "thinking" {
			spin = max(spin, row[2].Fade)
		}
	}
	if spin < 0.5 {
		t.Fatalf("the thinking row is not sunk: fade %v", spin)
	}
	at = at.Add(2 * _riseDuration)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Thinking: true, Reasoning: "more"})
	lines, _ = renderChat(t, app, 98, 20, app.uiState())
	for _, row := range lines {
		if len(row) > 2 && row[2].Text == "thinking" && row[2].Fade != 0 {
			t.Fatalf("the thinking row stayed sunk after the ramp: %v", row[2])
		}
	}
	// the row back after frames away rises again
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Text: "a reply"})
	renderChat(t, app, 98, 20, app.uiState())
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Thinking: true, Reasoning: "again"})
	lines, _ = renderChat(t, app, 98, 20, app.uiState())
	for _, row := range lines {
		if len(row) > 2 && row[2].Text == "thinking" && row[2].Fade < 0.5 {
			t.Fatalf("the thinking row back after frames away did not rise: %v", row[2])
		}
	}
}

// TestResetSinksTheChatIntoTheSplash: a reset's wipe reaches the chat as an
// empty transcript under a new epoch, and the screen sinks the chat it
// showed toward the surface before the knot rises in its place.
func TestResetSinksTheChatIntoTheSplash(t *testing.T) {
	app, _ := bootAgentApp(t, 100, 30)
	f := app.chatFolds()
	at := time.UnixMilli(1000)
	f.now = func() time.Time { return at }
	app.now = func() time.Time { return at }

	st := func(count int, epoch int64) *hostapi.ScreenState {
		s := &hostapi.ScreenState{Width: 98, Height: 20, TranscriptCount: count, TranscriptEpoch: epoch}
		if count > 0 {
			s.Transcript = []hostapi.ChatMessage{{Role: "assistant", Text: "the answer"}}
		}
		return s
	}
	draw := func(id string, s *hostapi.ScreenState) [][]hostapi.Segment {
		lines, more := app.widgetByID(id).render(s)
		app.onWidgetRendered(widgetRender{id: id, lines: lines, more: more, gen: 1, seq: 1, width: 98, height: 20})
		return app.caches[id].lines
	}
	// the chat's rows appear whole: only the reset's sink moves them as one
	if lines := draw("chat", st(1, 1)); len(lines) == 0 || maxFade(lines) != 0 {
		t.Fatalf("the chat's first rows did not draw clean: fade %v", maxFade(lines))
	}
	// the first message took the region: the knot vanishes whole
	if rows := draw("splash", st(1, 1)); rows != nil {
		t.Fatal("the knot drew over the chat")
	}
	// the reset: the chat sinks on the rows it kept, the knot waits
	lines := draw("chat", st(0, 2))
	if len(lines) == 0 {
		t.Fatal("the reset emptied the chat in one frame")
	}
	at = at.Add(_riseDuration / 2)
	if lines = draw("chat", st(0, 2)); len(lines) == 0 || maxFade(lines) < 0.4 || maxFade(lines) > 0.6 {
		t.Fatalf("the sink did not ride mid-ramp: fade %v", maxFade(lines))
	}
	if rows := draw("splash", st(0, 2)); rows != nil {
		t.Fatal("the knot rose while the chat still sank")
	}
	at = at.Add(2 * _riseDuration)
	if lines := draw("chat", st(0, 2)); len(lines) != 0 {
		t.Fatal("the chat stayed after its sink")
	}
	// the knot rises in the region the chat gave up
	if rows := draw("splash", st(0, 2)); len(rows) == 0 || maxFade(rows) == 0 {
		t.Fatalf("the knot did not rise after the chat sank: fade %v", maxFade(rows))
	}
	at = at.Add(2 * _riseDuration)
	if rows := draw("splash", st(0, 2)); len(rows) != 0 && maxFade(rows) != 0 {
		t.Fatalf("the knot stayed sunk after its ramp: fade %v", maxFade(rows))
	}
}

// TestFirstMessageSinksTheKnot: the first message takes the region, and the
// knot sinks on the rows it kept before the chat's rows draw whole.
func TestFirstMessageSinksTheKnot(t *testing.T) {
	app, _ := bootAgentApp(t, 100, 30)
	at := time.UnixMilli(1000)
	app.now = func() time.Time { return at }

	st := func(count int) *hostapi.ScreenState {
		s := &hostapi.ScreenState{Width: 98, Height: 20, TranscriptCount: count, TranscriptEpoch: 1}
		if count > 0 {
			s.Transcript = []hostapi.ChatMessage{{Role: "user", Text: "hello"}}
		}
		return s
	}
	draw := func(id string, s *hostapi.ScreenState) [][]hostapi.Segment {
		lines, more := app.widgetByID(id).render(s)
		app.onWidgetRendered(widgetRender{id: id, lines: lines, more: more, gen: 1, seq: 1, width: 98, height: 20})
		return app.caches[id].lines
	}
	// the boot's knot stands settled
	if rows := draw("splash", st(0)); len(rows) == 0 {
		t.Fatal("the knot did not draw")
	}
	app.settleRamps()

	// the first message: the splash renders none, its curtain sinks the knot,
	// and the chat waits while the kept rows still draw
	if rows := draw("splash", st(1)); rows == nil {
		t.Fatal("the knot vanished whole on the first message")
	}
	if lines := draw("chat", st(1)); len(lines) != 0 {
		t.Fatal("the chat drew over the knot's sink")
	}
	at = at.Add(_riseDuration / 2)
	if rows := draw("splash", st(1)); len(rows) == 0 || maxFade(rows) < 0.4 || maxFade(rows) > 0.6 {
		t.Fatalf("the knot's sink did not ride mid-ramp: fade %v", maxFade(rows))
	}
	at = at.Add(2 * _riseDuration)
	if rows := draw("splash", st(1)); len(rows) != 0 {
		t.Fatal("the knot stayed after its sink")
	}
	// the knot rode out: the chat takes the region whole
	if lines := draw("chat", st(1)); len(lines) == 0 || maxFade(lines) != 0 {
		t.Fatalf("the chat's first rows did not draw clean: fade %v", maxFade(lines))
	}
}

// maxFade is the deepest sink in rows, 0 when they draw clean.
func maxFade(rows [][]hostapi.Segment) float64 {
	fade := 0.0
	for _, row := range rows {
		for _, s := range row {
			fade = max(fade, s.Fade)
		}
	}
	return fade
}

// TestReadoutRise: the stats line under a reply appears only once the
// message landed whole, rising out of the surface; a reply a session opens
// with never rises.
func TestReadoutRise(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	f := app.chatFolds()
	at := time.UnixMilli(1000)
	f.now = func() time.Time { return at }

	readoutFade := func(app *App) (float64, bool) {
		st := app.uiState()
		lines, _ := renderChat(t, app, 98, 20, st)
		fade, found := 0.0, false
		for _, row := range lines {
			if len(row) > 1 && row[0].NoCopy && row[0].Text == "  " && strings.HasSuffix(hostapi.SegsText(row), " out") {
				found = true
				for _, s := range row {
					fade = max(fade, s.Fade)
				}
			}
		}
		return fade, found
	}

	// the reply streams
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Text: "streaming"})
	renderChat(t, app, 98, 20, app.uiState())
	// it settles whole: its readout appears sunk and rises
	host.SeedStream(hostapi.StreamSnapshot{})
	appendReply(host, "the answer", hostapi.TurnStat{Model: "fake/m", ElapsedMS: 600, Out: 11})
	if fade, ok := readoutFade(app); !ok || fade < 0.5 {
		t.Fatalf("the fresh readout did not sink: fade %v found %v", fade, ok)
	}
	at = at.Add(2 * _riseDuration)
	if fade, ok := readoutFade(app); !ok || fade != 0 {
		t.Fatalf("the readout stayed sunk after the ramp: fade %v found %v", fade, ok)
	}

	// a reply a session opens with (a resume) is read as it is
	app2, host2 := bootAgentApp(t, 100, 30)
	app2.chatFolds().now = func() time.Time { return at }
	appendReply(host2, "old answer", hostapi.TurnStat{Model: "fake/m", ElapsedMS: 600, Out: 11})
	if fade, ok := readoutFade(app2); !ok || fade != 0 {
		t.Fatalf("a resumed readout rose: fade %v found %v", fade, ok)
	}
}

// TestReadoutRiseSurvivesTheSeqNaming: the reply's row lands unstamped and
// the log names it a moment later; the rise keys by the row's place in the
// window, so the naming neither freezes the readout sunk nor pops it clean.
func TestReadoutRiseSurvivesTheSeqNaming(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	f := app.chatFolds()
	at := time.UnixMilli(1000)
	f.now = func() time.Time { return at }
	f.epoch, f.rows = 1, 1 // the user row ahead of the reply is drawn already
	st := &hostapi.ScreenState{
		Width: 98, Height: 20, TranscriptEpoch: 1, TranscriptCount: 2,
		Transcript: []hostapi.ChatMessage{
			{Role: fantasy.MessageRoleUser, Text: "q", Seq: 1},
			{Role: fantasy.MessageRoleAssistant, Text: "a", Stat: &hostapi.TurnStat{Model: "fake/m", Out: 11}, Seq: hostapi.NoSeq},
		},
	}
	f.render(host, st) // the reply lands unstamped: its readout sinks

	// the log names the row between frames, as the flush's stamping does
	st.Transcript[1].Seq = 2
	fade := func() float64 {
		lines, _ := f.render(host, st)
		out := 0.0
		for _, row := range lines {
			if len(row) > 1 && row[0].NoCopy && row[0].Text == "  " && strings.HasSuffix(hostapi.SegsText(row), " out") {
				for _, s := range row {
					out = max(out, s.Fade)
				}
			}
		}
		return out
	}
	if fade() == 0 {
		t.Fatal("the readout drew clean before its rise")
	}
	at = at.Add(2 * _riseDuration)
	if fade := fade(); fade != 0 {
		t.Fatalf("the readout stayed sunk after the log named its row: fade %v", fade)
	}
}

// TestAppendKeepsTheSettledRows: a transcript row appended (a rail action
// landing) rebuilds from the tail but keeps every unchanged unit's rows, so
// the scrollbar's total never dips for the frames the fill would need.
func TestAppendKeepsTheSettledRows(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	for range 40 {
		host.AppendChat(hostapi.ChatMessage{Role: "user", Text: "a question"})
		appendReply(host, "a reply of a few words", hostapi.TurnStat{Model: "fake/m", Out: 11})
	}
	for range 200 {
		renderChat(t, app, 98, 20, app.uiState())
		if app.chatFolds().fillUnit == 0 {
			break
		}
	}
	before := len(app.chatFolds().settled)
	if before < 20 {
		t.Fatalf("the transcript did not fill a view: %d rows", before)
	}
	appendReply(host, "one more", hostapi.TurnStat{Model: "fake/m", Out: 11})
	renderChat(t, app, 98, 20, app.uiState())
	if after := len(app.chatFolds().settled); after < before {
		t.Fatalf("an append dipped the settled rows: %d after %d", after, before)
	}
}

// linesOf renders the chat and returns the rows a live call draws.
func linesOf(t testing.TB, app *App, call int) [][]hostapi.Segment {
	t.Helper()
	st := app.uiState()
	lines, _ := renderChat(t, app, 98, 20, st)
	var out [][]hostapi.Segment
	for _, row := range lines {
		if rowCall(row) == call {
			out = append(out, row)
		}
	}
	return out
}
