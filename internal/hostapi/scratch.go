// scratch.go gives each project a scratchpad: a directory for the agent's
// temporary files (notes, drafts, command output, scripts, handoffs between
// subagents), which the file tools read, write and edit without asking, plan
// mode included. A pipeline binds it once (tools.Pipeline.Pad). A pad lives in
// the project's own state dir, outside every repository, and is removed once
// unused for scratchTTL, or at once with its project.

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

// ScratchDir is the project's scratchpad, "" without a state home or a
// project root.
func (h *Host) ScratchDir() string {
	if h.Dirs.StateHome == "" || h.trustRoot() == "" {
		return ""
	}
	return filepath.Join(projectState(h.Dirs.StateHome, h.trustRoot()), "scratch")
}

// ensureScratch creates the pad and marks it used; the sweep of the other
// pads walks their trees, so it runs off the send path.
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
	h.GoSafe("scratch sweep", func() { h.sweepScratch(now.Add(-scratchTTL)) })
}

// sweepScratch removes the pads of other projects, and those of the
// session-keyed root an older zemi kept, unused since t. The live pad is read
// at each removal, since a resume may make an old pad live mid-sweep. A pad
// counts as used when anything in it changed or a command ran with it.
func (h *Host) sweepScratch(t time.Time) {
	projects := filepath.Join(h.Dirs.StateHome, "projects")
	entries, _ := os.ReadDir(projects)
	for _, e := range entries {
		if e.IsDir() {
			h.dropPad(filepath.Join(projects, e.Name(), "scratch"), t)
		}
	}
	old := filepath.Join(h.Dirs.StateHome, "scratch")
	entries, _ = os.ReadDir(old)
	for _, e := range entries {
		h.dropPad(filepath.Join(old, e.Name()), t)
	}
}

func (h *Host) dropPad(pad string, t time.Time) {
	if pad == h.ScratchDir() || usedSince(pad, t) {
		return
	}
	if err := removeTree(pad); err != nil {
		h.Log.Warn("scratchpad not removed", "origin", "scratch", "path", pad, "error", err)
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
