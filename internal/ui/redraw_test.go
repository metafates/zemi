package ui

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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
	app.now = func() time.Time { return time.Now().Add(progressInterval + time.Second) }
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
	runLua(t, app, `
local n = 0
zemi.ui.widget.add({ id = "n", region = "header", render = function() return { { { text = "n=" .. n } } } end })
zemi.event.on("user.tick", function() zemi.timer.after(0, function() n = 10 end) end)`)
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
	if frame := app.screen(t); !strings.Contains(frame, "n=10") {
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
	if app.tickInterval() != idleInterval {
		t.Fatal("idle should tick slowly")
	}
	host.SeedStream(hostapi.StreamSnapshot{Active: true})
	app.Update(frameTickMsg{})
	if app.frameGen == gen {
		t.Fatal("a running turn does not tick the working row")
	}
	if app.tickInterval() != frameInterval {
		t.Fatal("a running turn should tick at frame rate")
	}

	host.SeedStream(hostapi.StreamSnapshot{})
	app.Update(turnDoneMsg{})
	app.Update(frameTickMsg{}) // the turn ends: the mascot bows
	gen = app.frameGen
	app.now = func() time.Time { return app.turnEnd.Add(doneLinger + time.Second) }
	app.Update(frameTickMsg{}) // it stops bowing
	app.Update(frameTickMsg{})
	if app.frameGen != gen+1 {
		t.Fatalf("the done frame cost %d redraws, want 1", app.frameGen-gen)
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
		time.Sleep(frameInterval / 3) // off a boundary
		last := time.Now().UnixMilli() / frameMS
		for i := range 6 {
			d := frameInterval
			if i%2 == 1 {
				d = idleInterval
				a.wakeTick()
			}
			a.frameTickCmd(d)()
			f := time.Now().UnixMilli() / frameMS
			if f != last+1 || time.Now().UnixMilli()%frameMS != 0 {
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

	app.composerKey("x", tea.KeyPressMsg{Text: "x"})
	if frame := ansi.Strip(app.View().Content); !strings.Contains(frame, "changed under the cachex") {
		t.Fatalf("the draft did not repaint:\n%s", frame)
	}

	app.onWidgetRendered(widgetRender{id: "chat", lines: [][]hostapi.Segment{{{Text: "fresh lines"}}}, width: 78, height: 12, gen: app.frameGen})
	if frame := ansi.Strip(app.View().Content); !strings.Contains(frame, "fresh lines") {
		t.Fatalf("a widget's lines did not repaint:\n%s", frame)
	}

	app.pushToast(slog.LevelInfo, "saved", "")
	if frame := ansi.Strip(app.View().Content); !strings.Contains(frame, "saved") {
		t.Fatalf("a toast did not show:\n%s", frame)
	}
	app.pruneToasts(time.Now().Add(time.Hour))
	if frame := ansi.Strip(app.View().Content); strings.Contains(frame, "saved") {
		t.Fatalf("an expired toast stayed:\n%s", frame)
	}

	// a frame that drew something moving by itself is never clean: its
	// shimmer steps with the frame clock though nothing else changed
	app.onWidgetRendered(widgetRender{id: "chat", lines: [][]hostapi.Segment{{{Text: "breathing", HL: "Dim", Effect: hostapi.EffectShimmer}}}, width: 78, height: 12, gen: app.frameGen})
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
