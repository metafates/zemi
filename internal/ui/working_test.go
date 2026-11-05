package ui

import (
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestWorkingRuleCarriesThePendingRowOnlyOutOfView: the chat's pending row is
// where background work lives; the rule carries nothing while the chat shows
// it, and once the reader scrolls it out of view the rule centres the same
// row minus its busy labels - mark and shimmer, no key the chat row does not
// carry.
func TestWorkingRuleCarriesThePendingRowOnlyOutOfView(t *testing.T) {
	st := &hostapi.ScreenState{Width: 80, Busy: []hostapi.BusyItem{{Label: "Answering the side question"}}}
	e := &eyes{}
	row := func() string {
		t.Helper()
		lines, _ := workingRule(e, st)
		return hostapi.SegsText(lines[0])
	}
	if got := row(); strings.Contains(got, "side question") || strings.Contains(got, "esc") {
		t.Fatalf("the rule carries the pending row the chat shows: %q", got)
	}
	st.RowsBelow = 1
	if got := row(); strings.Contains(got, "side question") || strings.Contains(got, "esc") {
		t.Fatalf("the rule carries a busy label: %q", got)
	}
	st = &hostapi.ScreenState{Width: 80, AgentsRunning: 2, RowsBelow: 1}
	if got := row(); !strings.Contains(got, "2 agents") {
		t.Fatalf("the scrolled-away row never reached the rule: %q", got)
	}
}
