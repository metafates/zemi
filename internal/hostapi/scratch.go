// scratch.go gives each session a scratchpad: a directory for the agent's
// temporary files (notes, drafts, command output, scripts, handoffs between
// subagents), which the file tools read, write and edit without asking,
// plan mode included. A pipeline binds it once (tools.Pipeline.Pad). Pads
// live in <state>/scratch, outside every repository, and are removed once
// unused for scratchTTL.

package hostapi

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const scratchTTL = 7 * 24 * time.Hour

// removeTree removes dir; when that fails it makes the tree writable and
// retries once, since a module cache copies its directories 0555 and one
// such dir would keep a whole pad alive forever. The caller logs the
// error: a sweep that discards it hides a pad that never ages out.
func removeTree(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, _ error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// ScratchDir is the live session's scratchpad, "" before the first send.
func (h *Host) ScratchDir() string { return h.scratchFor(h.AgentSessionID()) }

func (h *Host) scratchFor(sid string) string {
	if sid == "" || h.Dirs.StateHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.StateHome, "scratch", sid)
}

// ensureScratch creates the live session's pad and marks it used; the sweep
// of the others walks their trees, so it runs off the send path.
func (h *Host) ensureScratch() {
	dir := h.ScratchDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.Log.Warn("scratchpad not created", "origin", "scratch", "error", err)
		return
	}
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	h.GoSafe("scratch sweep", func() { h.sweepScratch(filepath.Dir(dir), now.Add(-scratchTTL)) })
}

// sweepScratch removes the pads under root unused since t. The live pad is
// read at each removal, since a resume may make an old pad live mid-sweep.
// NOTE: a pad counts as used when anything in it changed or a command ran
// with it; one another zemi window holds with neither for scratchTTL is
// removed under it. A lock per live process would lift that.
func (h *Host) sweepScratch(root string, t time.Time) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if pad := filepath.Join(root, e.Name()); e.IsDir() && !usedSince(pad, t) && pad != h.ScratchDir() {
			if err := removeTree(pad); err != nil {
				h.Log.Warn("scratchpad not removed", "origin", "scratch", "path", pad, "error", err)
			}
		}
	}
}

// usedSince reports anything under dir, dir included, modified after t: a
// directory's own time moves only when an entry right in it comes or goes.
func usedSince(dir string, t time.Time) bool {
	used := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(t) {
			used = true
			return filepath.SkipAll
		}
		return nil
	})
	return used
}
