// sweep.go removes at boot what no sweeper rides: the orphan dirs of
// removed features, the drops that wait for the next save, and the temp
// files a crash before their defer left behind.

package hostapi

import (
	"os"
	"path/filepath"
	"time"
)

// SweepBoot is the boot-time sweep, off the send path: the orphan dirs of
// removed features, whose names exist nowhere in the tree anymore and
// nothing recreates; the drops, whose sweep otherwise waits for the next
// save; and the state/tmp files idle past the scratch TTL.
func (h *Host) SweepBoot() {
	for _, dir := range []string{
		joinIf(h.Dirs.StateHome, "sandbox-cache"), // the sandbox's per-project build caches, writer removed
		joinIf(h.Dirs.CacheHome, "lsp"),           // the sandboxed language servers' caches, writer removed
		joinIf(h.Dirs.DataHome, "site"),           // the plugins root before its rename
	} {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if err := removeTree(dir); err != nil {
			h.Log.Warn("orphan dir not removed", "origin", "sweep", "dir", dir, "error", err)
			continue
		}
		h.Log.Info("removed the orphan dir of a removed feature", "origin", "sweep", "dir", dir)
	}
	if dir := joinIf(h.Dirs.StateHome, "tmp"); dir != "" {
		h.sweepTmp(dir)
	}
	if dir := h.dropsDir(); dir != "" {
		sweepDrops(dir)
	}
}

// sweepTmp removes the temp files and snapshot stores idle past the
// scratch TTL: a composer draft or a paste a zemi crash left behind. A
// snap store a live process writes stays fresh; one idle
// this long is re-warmed by its next snapshot.
func (h *Host) sweepTmp(dir string) {
	cutoff := time.Now().Add(-_scratchTTL)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if usedSince(p, cutoff) {
			continue
		}
		if err := removeTree(p); err != nil {
			h.Log.Warn("temp file not removed", "origin", "sweep", "path", p, "error", err)
		}
	}
}
