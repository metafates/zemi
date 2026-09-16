// rewind.go undoes turns: before a write tool first changes a file in a
// turn, its content is checkpointed against that turn's user message.
// Rewinding to a message restores every file changed since, drops the
// message and the rest of the conversation, and hands its text back to the
// composer. Files changed through bash are not tracked.
package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// NOTE: larger files are not checkpointed (a rewind leaves them as they
// are); raise the cap or store blobs on disk if that bites.
const maxCheckpointBytes = 4 << 20

// checkpointFile is the pipeline's Checkpoint hook.
func (h *Host) checkpointFile(path string) {
	if h.DB == nil {
		return
	}
	ag := h.agents
	ag.mu.Lock()
	sid, mid := ag.sessionID, ag.turnMsgID
	ag.mu.Unlock()
	if sid == "" || mid == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	// the file tools' key: a symlink and its target are one file, also before
	// the file exists (the store folds case where the filesystem does)
	abs = perm.RealPath(abs)
	data, err := os.ReadFile(abs)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.Log.Warn("rewind", "cannot checkpoint file", map[string]any{"path": abs, "error": err.Error()})
		return
	}
	if len(data) > maxCheckpointBytes {
		h.Log.Warn("rewind", "file too large to checkpoint", map[string]any{"path": abs, "bytes": len(data)})
		// say it now: a later rewind reports only the files it restored, and
		// this one would quietly stay changed
		h.Notify("warn", "rewind", filepath.Base(abs)+" is too large to checkpoint; a rewind will not restore it")
		return
	}
	if err := h.DB.AddCheckpoint(sid, mid, abs, existed, data); err != nil {
		h.Log.Warn("rewind", "checkpoint failed", map[string]any{"path": abs, "error": err.Error()})
	}
}

// userText returns what the user typed in a stored user message; compaction
// summaries and non-user rows report false.
func userText(row *store.Message) (string, bool) {
	var m fantasy.Message
	if row.Role != "user" || json.Unmarshal([]byte(row.PartsJSON), &m) != nil {
		return "", false
	}
	if _, ok := summaryOf(m); ok {
		return "", false
	}
	s, _, _ := strings.Cut(concatText(m.Content), attachMarker)
	s = strings.TrimSpace(s)
	return s, s != ""
}

// RewindTo restores the live session to just before the user message id
// and returns how many files it restored. Files are restored first; if any
// fails the conversation is left intact so the rewind can be retried.
func (h *Host) RewindTo(id string) (int, error) {
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.mu.Unlock()
		return 0, errors.New("a turn is running; esc interrupts it")
	}
	// claimed like a turn, so no send starts on the history being cut
	ag.active = true
	sid, scope := ag.sessionID, h.beginScope()
	ag.mu.Unlock()
	defer h.handOff(scope)
	// a detached run writing files would undo the restore right after it
	if n := h.tasks.running(); n > 0 {
		return 0, fmt.Errorf("%d background %s still running; stop them from /agents first", n, plural(n, "agent", "agents"))
	}
	if sid == "" || h.DB == nil {
		return 0, errors.New("no session to rewind")
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return 0, err
	}
	cut := slices.IndexFunc(rows, func(r *store.Message) bool { return r.ID == id })
	text, ok := "", false
	if cut >= 0 {
		text, ok = userText(rows[cut])
	}
	if !ok {
		return 0, fmt.Errorf("no prompt %q in this session", id)
	}
	cps, err := h.DB.CheckpointsFrom(sid, id)
	if err != nil {
		return 0, err
	}
	// snapshots age out: say so before the conversation is cut, or a rewind
	// that restored nothing looks like one that had nothing to restore. With
	// some left, a file an aged-out turn changed stays as it is or goes back
	// only to a later turn's snapshot.
	aged := 0
	for _, r := range rows[cut:] {
		if _, ok := userText(r); ok && time.Since(time.UnixMilli(r.CreatedAt)) > store.CheckpointTTL {
			aged++
		}
	}
	switch {
	case aged > 0 && len(cps) == 0:
		h.Notify("warn", "rewind", "the file snapshots for that turn have aged out; the conversation is cut but no file is restored")
	case aged > 0:
		h.Notify("warn", "rewind", fmt.Sprintf("file snapshots older than %d days have aged out (%d of those %s); files changed then may not go back to before that turn",
			int(store.CheckpointTTL.Hours()/24), aged, plural(aged, "turn", "turns")))
	}
	var failed []string
	for _, cp := range cps {
		if err := restoreCheckpoint(cp); err != nil {
			failed = append(failed, cp.Path+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return 0, fmt.Errorf("could not restore %s", strings.Join(failed, "; "))
	}
	if err := h.DB.TruncateMessages(sid, id); err != nil {
		return 0, err
	}
	sess, err := h.DB.GetSession(sid)
	if err != nil {
		return 0, err
	}
	history, tr := resumeHistory(rows[:cut])
	ag.mu.Lock()
	ag.runner = &agent.Runner{History: history}
	ag.lastIn, ag.turnMsgID = 0, ""
	ag.mu.Unlock()
	h.forgetInstructions()
	h.setTranscript(sess.Title, tr)
	if ui := h.hooks(); ui != nil {
		ui.ComposerEdit("set", text, -1)
	}
	return len(cps), nil
}

func restoreCheckpoint(cp store.Checkpoint) error {
	if !cp.Existed {
		if err := os.Remove(cp.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cp.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(cp.Path, cp.Content, 0o644)
}

// OpenRewind lists the session's prompts, newest first; choosing one
// rewinds to before it.
func (h *Host) OpenRewind() error {
	ui := h.hooks()
	sid := h.AgentSessionID()
	if ui == nil || h.DB == nil || sid == "" {
		return errors.New("no session to rewind")
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return err
	}
	counts, _ := h.DB.CheckpointCounts(sid)
	var items []any
	for _, row := range slices.Backward(rows) {
		text, ok := userText(row)
		if !ok {
			continue
		}
		files := ""
		if n := counts[row.ID]; n > 0 {
			files = fmt.Sprintf("  (%d %s)", n, plural(n, "file", "files"))
		}
		first, _, _ := strings.Cut(text, "\n")
		items = append(items, map[string]any{
			"text": fmt.Sprintf("%-8s %s%s", ago(row.CreatedAt), truncRunes(first, 60), files),
			"run":  "rewind " + row.ID,
		})
	}
	if len(items) == 0 {
		return errors.New("nothing to rewind yet")
	}
	// builtin origin: the UI runs a row's command only for trusted overlays
	ui.OpenOverlay(Overlay{ID: "rewind", Title: "Rewind to before", Origin: lua.OriginBuiltin, Items: items})
	return nil
}
