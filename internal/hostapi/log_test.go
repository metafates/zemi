package hostapi

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestLogsRing: print and zemi.log lines land in the ring /logs shows and
// zemi.log.list reads back, newest first.
func TestLogsRing(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	var file bytes.Buffer
	h.Log = slog.New(slog.NewTextHandler(&file, nil))
	runUser(t, r, `
		zemi.log.info("first note", { origin = "builtin", level = "ERROR" })
		zemi.log.warn("second note")
		local rows = zemi.log.list()
		assert(#rows == 2, "rows=" .. #rows)
		assert(rows[1].message == "second note", rows[1].message)
		assert(rows[1].source == "user warn", rows[1].source)
	`)
	if line, _, _ := strings.Cut(file.String(), "\n"); strings.Count(line, " origin=") != 1 || strings.Count(line, " level=") != 1 || !strings.Contains(line, " origin=user ") {
		t.Fatalf("a context key posed as the record's own: %s", line)
	}
	// nothing printed reads as an empty list, which offers no copy
	h2, _ := newTestHost(t)
	ui2 := &fakeUI{}
	h2.SetUIHooks(ui2)
	if err := h2.OpenLogs(); err != nil || ui2.opened[0].Body != "" || ui2.opened[0].Empty == "" {
		t.Fatalf("empty logs: %v %+v", err, ui2.opened)
	}
	h.RecordMessage("user", "printed line")
	if err := h.OpenLogs(); err != nil || len(ui.opened) != 1 {
		t.Fatalf("logs: %v %+v", err, ui.opened)
	}
	body := ui.opened[0].Body
	if ui.opened[0].Empty != "" || !strings.Contains(body, "printed line") || strings.Index(body, "printed line") > strings.Index(body, "first note") {
		t.Fatalf("body is not newest first:\n%s", body)
	}
}
