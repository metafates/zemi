// rewind.go undoes turns: before a write tool first changes a file in a
// turn, its content is checkpointed against that turn's user message.
// Rewinding to a message restores every file changed since, drops the
// message and the rest of the conversation with what it learned and its todo
// list, and hands its text back to the composer. A shell call's changes are
// rewound only for repository files git does not ignore and the sandbox does
// not hide, at most snapCheckpoints of them per call.

package hostapi

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
	"github.com/metafates/zemi/internal/tools"
)

// NOTE: larger files are not checkpointed (a rewind leaves them as they
// are); raise the cap or store blobs on disk if that bites.
const maxCheckpointBytes = 4 << 20

// checkpointFile is the pipeline's Checkpoint hook.
func (h *Host) checkpointFile(path string) {
	// the file tools' key: a symlink and its target are one file, also before
	// the file exists (the store folds case where the filesystem does)
	abs := perm.RealPath(path)
	if abs == "" {
		return
	}
	data, err := os.ReadFile(abs)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.Log.Warn("cannot checkpoint file", "origin", "rewind", "path", abs, "error", err)
		return
	}
	h.checkpoint(store.Checkpoint{Path: abs, Existed: existed, Content: data})
}

func (h *Host) turnMsgID() string {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.agents.turnMsgID
}

// checkpoint records a file as it was before the running turn changed it:
// the file tools read it from disk, a shell call's changes come from the
// tree treeDiff compared against. First write per turn wins in the store.
func (h *Host) checkpoint(cp store.Checkpoint) {
	if h.DB == nil {
		return
	}
	sid, mid := h.AgentSessionID(), h.turnMsgID()
	if sid == "" || mid == "" {
		return
	}
	if len(cp.Content) > maxCheckpointBytes {
		h.uncheckpointed(cp.Path, "is too large to checkpoint")
		return
	}
	if err := h.DB.AddCheckpoint(sid, mid, cp); err != nil {
		h.Log.Warn("checkpoint failed", "origin", "rewind", "path", cp.Path, "error", err)
	}
}

// uncheckpointed says, once per file and turn, that a rewind will not restore
// abs: a later rewind reports only the files it restored, and this one would
// quietly stay changed.
func (h *Host) uncheckpointed(abs, why string) {
	if _, said := h.snaps.skipped.LoadOrStore(h.turnMsgID()+"\x00"+abs, true); said {
		return
	}
	h.Log.Warn("file not checkpointed", "origin", "rewind", "path", abs, "why", why)
	h.Notify(slog.LevelWarn, "Checkpoint skipped", filepath.Base(abs)+" "+why+"; a rewind will not restore it")
}

// userText returns what the user typed in a stored user message; compaction
// summaries and non-user rows report false.
func userText(row *store.Message) (string, bool) {
	var m fantasy.Message
	if row.Role != string(fantasy.MessageRoleUser) || json.Unmarshal([]byte(row.PartsJSON), &m) != nil {
		return "", false
	}
	if _, ok := summaryOf(m); ok {
		return "", false
	}
	s, _, _ := strings.Cut(concatText(m.Content), agent.AttachMarker)
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
		return 0, errTurnRunning
	}
	// claimed like a turn, so no send starts on the history being cut
	sid, scope := ag.sessionID, h.claim()
	ag.mu.Unlock()
	defer h.handOff(scope)
	// a detached run writing files would undo the restore right after it
	if err := h.tasks.stillRunning(func(string) bool { return true }); err != nil {
		return 0, err
	}
	if sid == "" || h.DB == nil {
		return 0, Usagef("no session to rewind")
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
		return 0, Usagef("no prompt %q in this session", id)
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
		h.Notify(slog.LevelWarn, "Files not restored", "their snapshots for that turn have aged out; only the conversation is cut")
	case aged > 0:
		h.Notify(slog.LevelWarn, "Some files not restored", fmt.Sprintf("snapshots older than %d days have aged out (%d of those %s); files changed then may not go back",
			int(store.CheckpointTTL.Hours()/24), aged, Plural(aged, "turn", "turns")))
	}
	var failed []string
	if len(cps) > 0 {
		h.lspDirty() // a restore rewrites files behind the language servers' backs
	}
	for _, cp := range cps {
		if err := restoreCheckpoint(cp); err != nil {
			failed = append(failed, cp.Path+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return 0, fmt.Errorf("could not restore %s", strings.Join(failed, "; "))
	}
	h.cutMu.Lock()
	err = h.DB.TruncateMessages(sid, id)
	if err == nil {
		h.clampReflectSeen(sid, cut)
		h.forgetLearned(sid, rows[cut].CreatedAt, rows[len(rows)-1].CreatedAt)
	}
	h.cutMu.Unlock()
	if err != nil {
		return 0, err
	}
	todos := todosIn(rows[:cut])
	if err := h.DB.SetTodos(sid, todos); err != nil {
		h.Log.Warn("todo list not rewound", "origin", "rewind", "error", err)
	}
	h.todosStale.Store(true)
	h.resetTodoNag(todos)
	sess, err := h.DB.GetSession(sid)
	if err != nil {
		return 0, err
	}
	history, tr := resumeHistory(rows[:cut])
	ag.mu.Lock()
	ag.runner, ag.turnModel = &agent.Runner{History: history}, ""
	// the stored rows keep tool output a prune cleared, so compactDue must see them
	ag.lastIn, ag.turnMsgID = estTokens(history...)+ag.promptTokens, ""
	ag.goal, ag.goalRuns = goalIn(rows[:cut]), 0
	ag.mu.Unlock()
	h.forgetInstructions()
	h.setTranscript(sess.Title, tr)
	if ui := h.hooks(); ui != nil {
		ui.ComposerEdit(ComposerSet, text, -1)
	}
	return len(cps), nil
}

// forgetLearned undoes what session sid learned from its rows from to until:
// its learned memories and pending proposals go, and what they replaced is
// live again. The user's own memories stay, and a learned forget cannot come
// back. A memory tool write lands before its step's rows; a reflector run or
// an accepted proposal after the last row is kept.
// NOTE: an accept or a reflector run between two rewound rows goes too.
func (h *Host) forgetLearned(sid string, from, until int64) {
	var gone []string
	dbs := []*store.DB{h.DB, h.GlobalDB}
	for _, db := range dbs {
		if db == nil {
			continue
		}
		ids, err := db.DeleteLearnedBetween(sid, from, until)
		if err = errors.Join(err, db.DeleteProposalsBetween(sid, from, until)); err != nil {
			h.Log.Warn("learned memories not rewound", "origin", "rewind", "error", err)
		}
		gone = append(gone, ids...)
	}
	for _, db := range dbs {
		if db != nil && len(gone) > 0 {
			if err := db.Unsupersede(gone); err != nil {
				h.Log.Warn("replaced memories not restored", "origin", "rewind", "error", err)
			}
		}
	}
	h.refreshPending()
}

// todosIn is the todo list the last todos call in rows that ran set.
func todosIn(rows []*store.Message) []store.Todo {
	calls := map[string]string{} // tool call id -> input
	var list []store.Todo
	for _, row := range rows {
		var m fantasy.Message
		if json.Unmarshal([]byte(row.PartsJSON), &m) != nil {
			continue
		}
		for _, part := range m.Content {
			switch p := part.(type) {
			case fantasy.ToolCallPart:
				if p.ToolName == tools.ToolTodos {
					calls[p.ToolCallID] = p.Input
				}
			case fantasy.ToolResultPart:
				var in map[string]any
				input, ok := calls[p.ToolCallID]
				// a refused call set nothing
				if !ok || p.Output == nil || p.Output.GetType() == fantasy.ToolResultContentTypeError ||
					json.Unmarshal([]byte(input), &in) != nil {
					continue
				}
				if todos, _, set, err := tools.ParseTodos(in); set && err == nil {
					list = todos
				}
			}
		}
	}
	return list
}

func restoreCheckpoint(cp store.Checkpoint) error {
	// the key was resolved when saved: a parent swapped for a symlink since
	// would send the content somewhere never checked
	if dir := filepath.Dir(cp.Path); perm.RealPath(dir) != dir {
		return errors.New("the path now leads through a symlink")
	}
	// a link at the file itself is replaced, never written through
	if fi, err := os.Lstat(cp.Path); err == nil && fi.Mode()&fs.ModeSymlink != 0 || !cp.Existed || cp.Link {
		if err := os.Remove(cp.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if !cp.Existed {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cp.Path), 0o755); err != nil {
		return err
	}
	if cp.Link {
		return os.Symlink(string(cp.Content), cp.Path)
	}
	return os.WriteFile(cp.Path, cp.Content, 0o644)
}

// OpenRewind lists the session's prompts, newest first; choosing one
// rewinds to before it.
func (h *Host) OpenRewind() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	sid := h.AgentSessionID()
	if h.DB == nil {
		return Usagef("no session to rewind")
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return err
	}
	paths, _ := h.DB.CheckpointPaths(sid)
	restored := map[string]bool{} // what rewinding to this prompt restores: its files and every later prompt's
	var items []any
	for _, row := range slices.Backward(rows) {
		for _, p := range paths[row.ID] {
			restored[perm.FoldCase(p)] = true
		}
		text, ok := userText(row)
		if !ok {
			continue
		}
		desc, restore := ago(row.CreatedAt), ""
		if n := len(restored); n > 0 {
			files := fmt.Sprintf("%d %s", n, Plural(n, "file", "files"))
			desc += " · " + files
			restore = ", restores " + files + " to " + Plural(n, "how it was", "how they were") + " before it,"
		}
		effect := "Rewinding removes this prompt and everything after it" + restore + " and puts the prompt back in the composer. Files a command changed go back too when they are in the repository, not ignored and not secret-shaped."
		first, _, _ := strings.Cut(text, "\n")
		items = append(items, map[string]any{
			"text":   first,
			"desc":   desc,
			"detail": effect + "\n\n" + text, // what it does first: a long prompt scrolls below
			"actions": []any{
				map[string]any{"text": "Rewind", "hotkey": "r", "danger": true, "run": "rewind " + row.ID},
			},
		})
	}
	// builtin origin: the UI runs a row's command only for trusted overlays
	ui.OpenOverlay(Overlay{ID: "rewind", Title: "Rewind", Origin: lua.OriginBuiltin, Items: items, Empty: "No prompts to rewind yet. Send a message to make one."})
	return nil
}
