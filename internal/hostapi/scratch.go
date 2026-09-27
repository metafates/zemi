// scratch.go gives each session a scratchpad: a directory for the agent's
// temporary files (notes, drafts, command output, scripts, handoffs between
// subagents), every model command's $TMPDIR, which the file tools read,
// write and edit without asking, plan mode included. A pipeline binds it
// once (tools.Pipeline.Pad). Pads live in <state>/scratch, outside every
// repository, and are removed once unused for scratchTTL.
package hostapi

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	scratchTTL = 7 * 24 * time.Hour
	// padIDLen is how much of the session id names its pad: 48 random bits
	// cannot collide within a week, and the short path leaves a unix socket
	// under it room below macOS's 104 bytes.
	padIDLen = 12
)

// ScratchDir is the live session's scratchpad, "" before the first send.
func (h *Host) ScratchDir() string { return h.scratchFor(h.AgentSessionID()) }

func (h *Host) scratchFor(sid string) string {
	if sid == "" || h.Dirs.StateHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.StateHome, "scratch", sid[:min(len(sid), padIDLen)])
}

// ensureScratch creates the live session's pad, marks it used and removes
// the pads unused past scratchTTL; "" when there is no pad.
// NOTE: a pad counts as used when anything in it changed or a command ran
// with it; one another zemi window holds with neither for scratchTTL is
// removed under it. A lock per live process would lift that.
func (h *Host) ensureScratch() string {
	dir := h.ScratchDir()
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.Log.Warn("scratch", "scratchpad not created", map[string]any{"error": err.Error()})
		return ""
	}
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	root := filepath.Dir(dir)
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if pad := filepath.Join(root, e.Name()); e.IsDir() && pad != dir && !usedSince(pad, now.Add(-scratchTTL)) {
			_ = os.RemoveAll(pad)
		}
	}
	return dir
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
