// scratch.go gives each session a scratchpad: a directory for the agent's
// temporary files (notes, drafts, command output, Lua scripts, handoffs
// between subagents) that file tools read and write without asking, plan
// mode included; it is a sandboxed command's $TMPDIR too. Pads live in
// <state>/scratch, outside every repository and short enough for a unix
// socket path on Linux, and are removed once idle for scratchTTL.
package hostapi

import (
	"os"
	"path/filepath"
	"time"
)

const scratchTTL = 7 * 24 * time.Hour

// ScratchDir is the live session's scratchpad, "" before the first send. A
// pipeline binds it once (tools.Pipeline.Pad); a subagent run binds its
// parent session's.
func (h *Host) ScratchDir() string { return h.scratchFor(h.AgentSessionID()) }

func (h *Host) scratchFor(sid string) string {
	if sid == "" || h.Dirs.StateHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.StateHome, "scratch", sid)
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
