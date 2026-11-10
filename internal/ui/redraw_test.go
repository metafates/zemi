package ui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
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

// TestPopupCaret: a menu's filter and an input overlay draw the terminal's
// own caret at the cell the input marks, and a filter without the keys
// draws none.
func TestPopupCaret(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 14)
	var items []hostapi.OverlayItem
	for _, s := range []string{"alpha one", "Beta two", "gamma three", "xyz", "qqq"} { // filterMin rows
		items = append(items, hostapi.OverlayItem{Text: s})
	}
	app.openOverlay(hostapi.Overlay{ID: "menu", Title: "menu", Items: items})
	if cur := app.View().Cursor; cur != nil {
		t.Fatalf("a filter that does not type draws no caret, got %+v", cur)
	}
	app.overlayKey("", "/")
	cur := app.View().Cursor
	if cur == nil || !strings.Contains(hostapi.SegsText(app.lastRows[cur.Y]), "/ ") {
		t.Fatalf("an open filter should draw the caret on its query row, got %+v", cur)
	}
	x := cur.X
	app.overlayKey("", "b")
	cur = app.View().Cursor
	if cur == nil || cur.X != x+1 || !strings.Contains(hostapi.SegsText(app.lastRows[cur.Y]), "/ b") {
		t.Fatalf("typing should move the caret with the query, got %+v", cur)
	}
	app.overlayKey("esc", "")
	if cur := app.View().Cursor; cur != nil {
		t.Fatalf("a filter without the keys draws no caret, got %+v", cur)
	}
	app.openOverlay(hostapi.Overlay{ID: "ask", Title: "ask", Input: true})
	app.bump() // the uiFunc boundary a dispatched open crosses
	if cur := app.View().Cursor; cur == nil || !strings.Contains(hostapi.SegsText(app.lastRows[cur.Y]), ">") {
		t.Fatalf("a typed line should draw the caret on its > prompt row, got %+v", cur)
	}
}

// TestComposerCaretBlinks: the caret holds steady while the user is at the
// keys, blinks once the composer sits idle, and steadies on the next input.
func TestComposerCaretBlinks(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 60, 12)
	if cur := app.View().Cursor; cur == nil || !cur.Blink {
		t.Fatalf("an untouched composer should blink, got %+v", cur)
	}
	clock := time.Now()
	app.now = func() time.Time { return clock }
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cur := app.View().Cursor; cur == nil || cur.Blink {
		t.Fatalf("typing should steady the caret, got %+v", cur)
	}
	app.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, Y: 5})
	if cur := app.View().Cursor; cur == nil || cur.Blink {
		t.Fatalf("scrolling should leave the caret steady, got %+v", cur)
	}
	app.now = func() time.Time { return clock.Add(2 * _caretIdle) }
	if cur := app.View().Cursor; cur == nil || !cur.Blink {
		t.Fatalf("an idle caret should blink again, got %+v", cur)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if cur := app.View().Cursor; cur == nil || cur.Blink {
		t.Fatalf("moving the caret should steady it, got %+v", cur)
	}
}

// TestComposerCaretShape: ui.cursor picks the caret's DECSCUSR shape.
func TestComposerCaretShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		set   string
		shape tea.CursorShape
	}{
		{"", tea.CursorBar},
		{"bar", tea.CursorBar},
		{"block", tea.CursorBlock},
		{"underline", tea.CursorUnderline},
	} {
		app, _ := bootAgentApp(t, 60, 12)
		if tc.set != "" {
			if err := app.host.SetConfig(config.KeyUICursor, tc.set); err != nil {
				t.Fatalf("ui.cursor %q: %v", tc.set, err)
			}
		}
		if cur := app.View().Cursor; cur == nil || cur.Shape != tc.shape {
			t.Fatalf("ui.cursor %q: got %+v, want shape %v", tc.set, cur, tc.shape)
		}
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

// TestFrameClock: a tick lands on the next frame boundary, from the timer
// and from a wake that cuts an idle sleep short alike, so the spinner and the
// shimmer step one glyph per tick.
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
	// the boot's paint rises like the scrollbar's: sunk through the delay,
	// settled after it
	if lines := draw("chat", st(1, 1)); len(lines) == 0 || maxFade(lines) != 1 {
		t.Fatalf("the boot's chat did not draw sunk: fade %v", maxFade(lines))
	}
	at = at.Add(_barDelay + _riseDuration)
	if lines := draw("chat", st(1, 1)); len(lines) == 0 || maxFade(lines) != 0 {
		t.Fatalf("the boot's chat never settled: fade %v", maxFade(lines))
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

// TestUserPromptRise: a prompt the reader sends lands styled queued and its
// colors run into their own; one a session opens with is read as it is.
func TestUserPromptRise(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	f := app.chatFolds()
	at := time.UnixMilli(1000)
	f.now = func() time.Time { return at }

	promptRow := func(app *App) ([]hostapi.Segment, bool) {
		lines, _ := renderChat(t, app, 98, 20, app.uiState())
		for _, row := range lines {
			if len(row) > 1 && row[0].Text == "▌ " && row[0].HL == "UserMsg" {
				return row, true
			}
		}
		return nil, false
	}

	// the boot's empty frames read the log the splash owns
	renderChat(t, app, 98, 20, app.uiState())
	host.AppendChat(hostapi.ChatMessage{Role: fantasy.MessageRoleUser, Text: "a question"})
	row, ok := promptRow(app)
	if !ok || row[0].Rise != 1 || row[0].From != "Queued" || row[1].From != "Dim" {
		t.Fatalf("the sent prompt did not land styled queued: %+v", row)
	}
	at = at.Add(_riseDuration / 2)
	row, _ = promptRow(app)
	if row[0].Rise <= 0 || row[0].Rise >= 1 {
		t.Fatalf("the prompt's colors are not on the ramp: rise %v", row[0].Rise)
	}
	// the background holds through the ramp: the prompt queued on the panel
	// rises out of the panel, its cells never dip toward the canvas
	midBg := app.segStyle(row[1]).Bg
	at = at.Add(2 * _riseDuration)
	row, _ = promptRow(app)
	if row[0].Rise != 0 {
		t.Fatalf("the prompt stayed sunk after the ramp: rise %v", row[0].Rise)
	}
	if end := app.segStyle(row[1]).Bg; !sameColor(midBg, end) {
		t.Fatalf("the prompt's background moved during the ramp: %v then %v", midBg, end)
	}

	// a prompt a session opens with (a resume) is read as it is
	app2, host2 := bootAgentApp(t, 100, 30)
	app2.chatFolds().now = func() time.Time { return at }
	host2.AppendChat(hostapi.ChatMessage{Role: fantasy.MessageRoleUser, Text: "an old question"})
	if row, ok := promptRow(app2); ok && row[0].Fade != 0 {
		t.Fatalf("a resumed prompt rose: fade %v", row[0].Fade)
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
