package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestProgressReport: OSC 9;4 goes out when a turn starts, is resent to keep
// terminals from hiding it, once when it ends, and not at all with the setting off.
func TestProgressReport(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 10)
	if app.progressCmd(true) == nil || app.progressCmd(true) != nil {
		t.Fatal("a running turn should report, and not again before the interval")
	}
	app.now = func() time.Time { return time.Now().Add(progressInterval + time.Second) }
	if app.progressCmd(true) == nil {
		t.Fatal("a running turn should report again after the refresh interval")
	}
	if app.progressCmd(false) == nil {
		t.Fatal("the end of a turn should clear the report")
	}
	app.host.Settings.SetQuiet("ui.progress", false)
	if app.progressCmd(true) != nil {
		t.Fatal("ui.progress off should report nothing")
	}
}

// TestRedrawAfterCallbacks: a one-shot timer's change reaches the screen
// with no other redraw, zemi.ui.redraw asks for one, and a burst of
// refreshes waits in the hook queue as one.
func TestRedrawAfterCallbacks(t *testing.T) {
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

// TestIdleDrawsNothing: the 30fps frame loop re-renders nothing while zemi
// sits idle, and the animation tick alone never does. A running turn ticks
// the readouts, and the mascot's done frame comes and goes once each.
func TestIdleDrawsNothing(t *testing.T) {
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

// TestBumpWakesIdleTick: a change while the tick sleeps idle reaches the
// screen at frame rate, and every stale widget comes back in one message.
func TestBumpWakesIdleTick(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 24)
	tick := app.frameTickCmd(idleInterval)
	start := time.Now()
	app.bump()
	tick()
	if d := time.Since(start); d >= idleInterval {
		t.Fatalf("the bump waited out the idle tick: %v", d)
	}
	if msg, ok := app.renderStaleWidgetsCmd()().(widgetsRenderedMsg); !ok || len(msg) < 2 {
		t.Fatalf("stale widgets should render as one message, got %T %v", msg, msg)
	}
	tick = app.frameTickCmd(idleInterval)
	start = time.Now()
	app.onWidgetRendered(widgetRender{id: "chat", more: true})
	tick()
	if d := time.Since(start); d >= idleInterval {
		t.Fatalf("a render asking to run again waited out the idle tick: %v", d)
	}
}
