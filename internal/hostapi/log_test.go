package hostapi

import (
	"strings"
	"testing"
)

// TestMessagesRing: print and zemi.log lines land in the ring /messages
// shows and zemi.log.list reads back, newest first.
func TestMessagesRing(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `
		zemi.log.info("first note")
		zemi.log.warn("second note")
		local rows = zemi.log.list()
		assert(#rows == 2, "rows=" .. #rows)
		assert(rows[1].message == "second note", rows[1].message)
		assert(rows[1].source == "user warn", rows[1].source)
	`)
	h.RecordMessage("user", "printed line")
	if err := h.OpenMessages(); err != nil || len(ui.opened) != 1 {
		t.Fatalf("messages: %v %+v", err, ui.opened)
	}
	body := ui.opened[0].Body
	if !strings.Contains(body, "printed line") || strings.Index(body, "printed line") > strings.Index(body, "first note") {
		t.Fatalf("body is not newest first:\n%s", body)
	}
}
