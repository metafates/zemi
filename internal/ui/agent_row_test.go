package ui

import (
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/tools"
)

// TestAgentRowsKeepTheirNames: a run row's name is the row's identity, so a
// wide box shows it whole and a long description gives way first - the
// trimming a /agents reader reads as a bug when the row has room to spare.
func TestAgentRowsKeepTheirNames(t *testing.T) {
	items := []listItem{
		{label: "New agent", row: hostapi.OverlayItem{Text: "New agent"}},
		{
			label: "Survey zemi run, how the mode behaves", mark: markSeg(tools.MarkSpin),
			desc: "running   · 2m 39s · 74 tool calls · explore",
			row:  hostapi.OverlayItem{Desc: "running   · 2m 39s · 74 tool calls · explore"},
		},
		{
			label: "fork", desc: "continues this conversation instead of starting fresh, so it needs no briefing",
			row: hostapi.OverlayItem{Desc: "continues this conversation instead of starting fresh, so it needs no briefing"},
		},
	}
	rows := listRowsW(items, items, 1, &viewport{}, 20, 90, 90)
	if run, fork := hostapi.SegsText(rows[1]), hostapi.SegsText(rows[2]); true {
		if !strings.Contains(run, "Survey zemi run, how the mode behaves") {
			t.Errorf("run row cut its name: %q", run)
		}
		if !strings.Contains(fork, "instead of…") {
			t.Errorf("fork row kept its whole description: %q", fork)
		}
	}
}
