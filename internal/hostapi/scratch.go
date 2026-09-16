// scratch.go gives each session a scratchpad: a directory for the agent's
// temporary files (notes, drafts, command output, Lua scripts, handoffs
// between subagents) that file tools read and write without asking, plan
// mode included. Pads live under the project's state directory, outside the
// repository, and are removed once idle for scratchTTL.
package hostapi

import (
	"os"
	"path/filepath"
	"time"

	"github.com/metafates/zemi/internal/store"
)

const scratchTTL = 7 * 24 * time.Hour

// ScratchDir is the live session's scratchpad, "" before the first send;
// subagents share their session's pad.
func (h *Host) ScratchDir() string {
	sid := h.AgentSessionID()
	if sid == "" || h.Dirs.StateHome == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(store.ProjectDBPath(h.Dirs.StateHome, h.Dirs.ProjectDir)), "scratch", sid)
}

func (h *Host) inScratch(path string) bool {
	dir := h.ScratchDir()
	return dir != "" && h.inJail(path, dir)
}

// ensureScratch creates the session's pad and removes the pads idle past
// scratchTTL; "" when there is no pad.
func (h *Host) ensureScratch() string {
	dir := h.ScratchDir()
	if dir == "" {
		return ""
	}
	// sweep first: a resumed session's own pad may be idle past the TTL too
	root := filepath.Dir(dir)
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() && time.Since(info.ModTime()) > scratchTTL {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.Log.Warn("scratch", "scratchpad not created", map[string]any{"error": err.Error()})
		return ""
	}
	return dir
}
