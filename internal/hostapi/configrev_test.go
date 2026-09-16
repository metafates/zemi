package hostapi

import (
	"testing"
	"time"

	"github.com/metafates/zemi/internal/store"
)

// TestStalePendingRevisionsMarkedFailed: a row from a run that died before
// its commit is closed out at boot; one still in flight is left alone.
func TestStalePendingRevisionsMarkedFailed(t *testing.T) {
	h, _ := newTestHostDB(t)
	stale := &store.ConfigRevision{ID: store.NewID(), Title: "stale", CreatedAt: time.Now().Add(-time.Hour).UnixMilli()}
	fresh := &store.ConfigRevision{ID: store.NewID(), Title: "fresh"}
	for _, r := range []*store.ConfigRevision{stale, fresh} {
		if err := h.GlobalDB.AddRevision(r); err != nil {
			t.Fatal(err)
		}
	}
	h.sweepPendingRevisions()
	got, err := h.GlobalDB.GetRevision(stale.ID)
	if err != nil || got.Status != "failed" {
		t.Fatalf("stale revision = %+v, %v", got, err)
	}
	if got, _ := h.GlobalDB.GetRevision(fresh.ID); got.Status != "pending" {
		t.Fatalf("in-flight revision = %+v", got)
	}
}
