package ui

import (
	"strings"
	"testing"
	"time"
)

// TestProgressReport: OSC 9;4 goes out once when a turn starts and once
// when it ends, and not at all with the setting off.
func TestProgressReport(t *testing.T) {
	app, _ := bootAgentApp(t, 60, 10)
	if app.progressCmd(true) == nil || app.progressCmd(true) != nil {
		t.Fatal("a running turn should report once")
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
zemi.timer.after(0, function() n = 10 end)`)
	gen := app.frameGen
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
