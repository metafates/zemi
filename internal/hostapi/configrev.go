// configrev.go closes out config_revisions rows left pending by a crash:
// the row lands before its git commit (DESIGN §10.4), so a run that dies
// between the two leaves a pending row nothing else revisits.
package hostapi

import (
	"time"
)

// staleRevisionAge is how long a revision may stay pending before a boot
// calls it failed. The git commit it waits on times out in gitTimeout, so
// nothing still in flight is ever this old.
const staleRevisionAge = 5 * time.Minute

// sweepPendingRevisions marks stale pending rows failed. The file write
// already happened, so the rule is in force: the log is what says the audit
// trail has a hole.
func (h *Host) sweepPendingRevisions() {
	if h.GlobalDB == nil {
		return
	}
	n, err := h.GlobalDB.FailStalePending(time.Now().Add(-staleRevisionAge).UnixMilli())
	switch {
	case err != nil:
		h.Log.Warn("config-rev", "pending revision sweep failed", map[string]any{"error": err.Error()})
	case n > 0:
		h.Log.Warn("config-rev", "revisions left pending by an earlier run marked failed; their commits never landed",
			map[string]any{"count": n})
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
