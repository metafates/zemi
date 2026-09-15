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
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
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
	data, err := os.ReadFile(abs)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.Log.Warn("rewind", "cannot checkpoint file", map[string]any{"path": abs, "error": err.Error()})
		return
	}
	if len(data) > maxCheckpointBytes {
		h.Log.Warn("rewind", "file too large to checkpoint", map[string]any{"path": abs, "bytes": len(data)})
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
	s, _, _ := strings.Cut(concatText(m.Content), attachMarker)
	if strings.HasPrefix(s, compactPrefix) {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

// RewindTo restores the live session to just before the user message id
// and returns how many files it restored. Files are restored first; if any
// fails the conversation is left intact so the rewind can be retried.
func (h *Host) RewindTo(id string) (int, error) {
	ag := h.agents
	ag.mu.Lock()
	sid, active := ag.sessionID, ag.active
	ag.mu.Unlock()
	if active {
		return 0, errors.New("a turn is running; esc interrupts it")
	}
	if sid == "" || h.DB == nil {
		return 0, errors.New("no session to rewind")
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return 0, err
	}
	cut := -1
	for i, r := range rows {
		if r.ID == id {
			cut = i
		}
	}
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
	history, tr := resumeHistory(sess, rows[:cut])
	ag.mu.Lock()
	ag.runner = &agent.Runner{History: history}
	ag.lastIn, ag.turnMsgID = 0, ""
	ag.mu.Unlock()
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
	for i := len(rows) - 1; i >= 0; i-- {
		text, ok := userText(rows[i])
		if !ok {
			continue
		}
		files := ""
		if n := counts[rows[i].ID]; n > 0 {
			files = fmt.Sprintf("  (%d %s)", n, plural(n, "file", "files"))
		}
		first := strings.SplitN(text, "\n", 2)[0]
		items = append(items, map[string]any{
			"text": fmt.Sprintf("%-8s %s%s", ago(rows[i].CreatedAt), truncRunes(first, 60), files),
			"run":  "rewind " + rows[i].ID,
		})
	}
	if len(items) == 0 {
		return errors.New("nothing to rewind yet")
	}
	ui.OpenOverlay(Overlay{ID: "rewind", Title: "Rewind to before", Items: items})
	return nil
}
