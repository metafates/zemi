package hostapi

import (
	"testing"
	"time"

	"github.com/metafates/zemi/internal/lua"
)

// TestUIAskWait: zemi.ui.select and zemi.ui.input answer a waiting task the
// same value their callback gets, cancel answers nil, and with no UI the ask
// fails closed: nil once, the call named in /errors.
func TestUIAskWait(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	const pick = `zemi.state.set("pick", zemi.ui.select({ "a", "b" }, nil, function() end):wait())`
	opened := func(n int) Overlay {
		for range 200 {
			if open := ui.overlays(); len(open) >= n {
				return open[n-1]
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the ask overlay did not open")
		return Overlay{}
	}
	done := make(chan error, 1)
	go func() { done <- chunkAs(t.Context(), h, r, lua.OriginUser, pick) }()
	h.SelectOverlay(t.Context(), opened(1), 1)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `assert(zemi.state.get("pick") == "b", "wait gives the chosen item")`)

	// cancel answers nil, like esc
	done = make(chan error, 1)
	go func() { done <- chunkAs(t.Context(), h, r, lua.OriginUser, pick) }()
	o := opened(2)
	runUser(t, r, `zemi.state.del("pick")`)
	h.CancelOverlay(t.Context(), o)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `assert(zemi.state.get("pick") == nil, "cancel answers nil")`)

	// no UI: fail closed, the call named in /errors
	h.SetUIHooks(nil)
	runUser(t, r, `assert(zemi.ui.input({}, function() end):wait() == nil, "a headless ask answers nil")
		assert(zemi.errors.list()[1].source == "zemi.ui.input", "the headless ask is named in /errors")`)
}
