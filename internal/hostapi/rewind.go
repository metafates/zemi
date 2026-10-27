// rewind.go undoes turns: before a write tool first changes a file in a
// turn, its content is checkpointed against that turn's user message (the
// log seq it was appended at). Rewinding to a message restores every file
// changed since, deletes the log rows from it on with every tree node and
// view line touching them, patches the view's hole with leaf lines,
// rebuilds the todo list, and hands the prompt text back to the composer.
// A shell call's changes are rewound only for repository files git does not
// ignore, at most snapCheckpoints of them per call.

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
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// NOTE: larger files are not checkpointed (a rewind leaves them as they
// are); raise the cap or store blobs on disk if that bites.
const maxCheckpointBytes = 4 << 20

// rewindPickerRows is how many prompts the rewind picker lists.
const rewindPickerRows = 200

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

func (h *Host) turnSeq() int64 {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.agents.turnSeq
}

// checkpoint records a file as it was before the running turn changed it:
// the file tools read it from disk, a shell call's changes come from the
// tree treeDiff compared against. First write per turn wins in the store.
func (h *Host) checkpoint(cp store.Checkpoint) {
	if h.DB == nil {
		return
	}
	seq := h.turnSeq()
	if seq < 0 {
		return
	}
	if len(cp.Content) > maxCheckpointBytes {
		h.uncheckpointed(cp.Path, "is too large to checkpoint")
		return
	}
	if err := h.DB.AddCheckpoint(seq, cp); err != nil {
		h.Log.Warn("checkpoint failed", "origin", "rewind", "path", cp.Path, "error", err)
	}
}

// uncheckpointed says, once per file and turn, that a rewind will not restore
// abs: a later rewind reports only the files it restored, and this one would
// quietly stay changed.
func (h *Host) uncheckpointed(abs, why string) {
	if _, said := h.snaps.skipped.LoadOrStore(strconv.FormatInt(h.turnSeq(), 10)+"\x00"+abs, true); said {
		return
	}
	h.Log.Warn("file not checkpointed", "origin", "rewind", "path", abs, "why", why)
	h.Notify(slog.LevelWarn, "Checkpoint skipped", filepath.Base(abs)+" "+why+"; a rewind will not restore it")
}

// RewindTo restores the chat to just before the log message seq and returns
// how many files it restored. Files are restored first; if any fails the
// conversation is left intact so the rewind can be retried.
func (h *Host) RewindTo(seq int64) (int, error) {
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.mu.Unlock()
		return 0, errTurnRunning
	}
	// claimed like a turn, so no send starts on the log being cut
	scope := h.claim()
	ag.mu.Unlock()
	defer h.handOff(scope)
	// a detached run writing files would undo the restore right after it
	if err := h.tasks.stillRunning(func(string) bool { return true }); err != nil {
		return 0, err
	}
	if h.DB == nil || h.chat == nil {
		return 0, Usagef("nothing to rewind")
	}
	row, err := h.DB.LogAt(seq)
	if errors.Is(err, store.ErrNotFound) {
		return 0, Usagef("no prompt %d in this chat", seq)
	}
	if err != nil {
		return 0, err
	}
	if row.Kind != kindUser || strings.TrimSpace(row.Text) == "" {
		return 0, Usagef("message %d is not a prompt", seq)
	}
	cps, err := h.DB.CheckpointsFrom(seq)
	if err != nil {
		return 0, err
	}
	if len(cps) > 0 {
		h.lspDirty() // a restore rewrites files behind the language servers' backs
	}
	// the locks come before any cut or restore (DESIGN §7): a store another
	// zemi holds aborts the rewind with both intact
	err = func() error {
		h.cutMu.Lock()
		defer h.cutMu.Unlock()
		var failed []string
		for _, cp := range cps {
			if err := restoreCheckpoint(cp); err != nil {
				failed = append(failed, cp.Path+": "+err.Error())
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("could not restore %s", strings.Join(failed, "; "))
		}
		// the builder first: quiesced, so no build lands over the cut, and
		// resumed whatever the cut did
		h.chat.quiesce()
		defer h.chat.resume()
		if err := h.DB.DeleteLogFrom(seq); err != nil {
			return err
		}
		if err := h.patchView(seq); err != nil {
			return err
		}
		return nil
	}()
	if err != nil {
		return 0, err
	}
	todos := h.todosBefore(seq)
	if err := h.DB.SetTodos(store.MainSessionID, todos); err != nil {
		h.Log.Warn("todo list not rewound", "origin", "rewind", "error", err)
	}
	h.todosStale.Store(true)
	h.resetTodoNag(todos)
	h.goalRewound(seq)
	ag.mu.Lock()
	ag.runner, ag.turnModel, ag.turnSeq = nil, "", -1
	ag.lastIn = 0
	ag.mu.Unlock()
	h.forgetInstructions()
	h.setTranscriptFromLog()
	if ui := h.hooks(); ui != nil {
		ui.ComposerEdit(ComposerSet, row.Text, -1)
	}
	return len(cps), nil
}

// patchView re-covers the messages the cut left behind: every surviving
// message not covered by a view line whose node survived becomes a leaf line
// again, which the builder re-merges.
func (h *Host) patchView(cut int64) error {
	lines, err := h.DB.LoadView()
	if err != nil {
		return err
	}
	covered := make([]bool, cut)
	for _, ln := range lines {
		n := nodeID{ln.L, ln.I}
		for s := max(0, n.first()); s < min(cut, n.last()+1); s++ {
			covered[s] = true
		}
	}
	for s, ok := range covered {
		if !ok {
			if err := h.DB.AppendViewLine(0, int64(s)); err != nil {
				return err
			}
		}
	}
	return nil
}

// todosBefore is the todo list the last todos call before seq set, paired
// from the log's tool and echo rows in order.
// NOTE: two todos calls in one step pair in call order, which the echo rows
// keep; a refused call's result drops its input.
func (h *Host) todosBefore(cut int64) []store.Todo {
	rows, err := h.DB.LogFrom(0, int(cut))
	if err != nil {
		return nil
	}
	var pending []string
	var list []store.Todo
	for _, e := range rows {
		switch e.Kind {
		case kindTool:
			if rowTool(e) == string(tools.ToolTodos) {
				pending = append(pending, e.Text)
			}
		case kindEcho:
			var meta struct {
				Tool string `json:"tool"`
				Ok   bool   `json:"ok"`
			}
			if json.Unmarshal([]byte(e.MetaJSON), &meta) != nil || meta.Tool != string(tools.ToolTodos) {
				continue
			}
			if len(pending) == 0 || !meta.Ok {
				pending = nil
				continue
			}
			var in map[string]any
			if input := pending[0]; json.Unmarshal([]byte(input), &in) == nil {
				if todos, _, set, err := tools.ParseTodos(in); set && err == nil {
					list = todos
				}
			}
			pending = pending[1:]
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
		return proc.SymlinkDir(string(cp.Content), cp.Path)
	}
	return os.WriteFile(cp.Path, cp.Content, 0o644)
}

// OpenRewind lists the log's prompts, newest first; choosing one rewinds to
// before it.
func (h *Host) OpenRewind() error {
	if h.AgentActive() {
		return errTurnRunning
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if h.DB == nil {
		return Usagef("nothing to rewind")
	}
	rows, err := h.DB.LogKindBack(kindUser, rewindPickerRows)
	if err != nil {
		return err
	}
	paths, _ := h.DB.CheckpointPaths()
	restored := map[string]bool{} // what rewinding to this prompt restores
	var items []OverlayItem
	for _, row := range slices.Backward(rows) {
		for _, p := range paths[row.Seq] {
			restored[perm.FoldCase(p)] = true
		}
		text := strings.TrimSpace(row.Text)
		if text == "" {
			continue
		}
		desc, restore := agoStamp(row.CreatedAt), ""
		if n := len(restored); n > 0 {
			files := fmt.Sprintf("%d %s", n, Plural(n, "file", "files"))
			desc += " · " + files
			restore = ", restores " + files + " to " + Plural(n, "how it was", "how they were") + " before it,"
		}
		effect := "Rewinding removes this prompt and everything after it" + restore + " and puts the prompt back in the composer. Files a command changed go back too when they are in the repository and not ignored."
		first, _, _ := strings.Cut(text, "\n")
		items = append(items, OverlayItem{
			Text:   first,
			Desc:   desc,
			Detail: effect + "\n\n" + text, // what it does first: a long prompt scrolls below
			Keys: []ItemKey{
				{Key: "r", Desc: "rewind", Danger: true, Run: "rewind " + strconv.FormatInt(row.Seq, 10)},
			},
		})
	}
	// builtin origin: the UI runs a row's command only for trusted overlays
	ui.OpenOverlay(Overlay{ID: "rewind", Title: "Rewind", Origin: lua.OriginBuiltin, Items: items, Empty: "No prompts to rewind yet. Send a message to make one."})
	return nil
}

// ResetChat wipes the eternal chat and starts it over (/reset, the fresh
// start /new was): the log, every summary node and view line, and the
// checkpoints go, VACUUM reclaims the file, and the next message is seq 0.
// It refuses mid-turn and while any run lives, the way a rewind does.
func (h *Host) ResetChat(yes bool) error {
	if h.DB == nil || h.chat == nil {
		return Usagef("no chat log in this boot")
	}
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.mu.Unlock()
		return errTurnRunning
	}
	// claimed like a turn, so no send starts on the log being wiped
	scope := h.claim()
	ag.mu.Unlock()
	defer h.handOff(scope)
	// a detached run's reports would land in the log being wiped
	if err := h.tasks.stillRunning(func(string) bool { return true }); err != nil {
		return err
	}
	if !yes {
		return AskFirst("reset --yes", "", h.openReset)
	}
	err := func() error {
		h.cutMu.Lock()
		defer h.cutMu.Unlock()
		// the builder first: quiesced, so no build lands over the wipe, and
		// resumed whatever the wipe did
		h.chat.quiesce()
		defer h.chat.resume()
		if err := h.DB.ResetLog(); err != nil {
			return err
		}
		_, err := h.DB.DelKV(cvKey)
		return err
	}()
	if err != nil {
		return err
	}
	if err := h.DB.SetTodos(store.MainSessionID, nil); err != nil {
		h.Log.Warn("todo list not cleared", "origin", "reset", "error", err)
	}
	h.todosStale.Store(true)
	h.resetTodoNag(nil)
	h.goalRewound(0) // every goal: the message that set it is gone
	ag.mu.Lock()
	ag.runner, ag.turnModel, ag.turnSeq = nil, "", -1
	ag.lastIn = 0
	ag.mu.Unlock()
	h.forgetInstructions()
	h.setTranscriptFromLog()
	h.Notify(slog.LevelInfo, "Chat reset", "the log, its summary tree and the view start over")
	return nil
}

// openReset is the screen /reset asks through: one row, whose key AskFirst
// arms.
func (h *Host) openReset(confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(Overlay{
		ID: "reset", Title: "Reset", Origin: lua.OriginBuiltin, Confirm: confirm,
		Items: []OverlayItem{{
			Text:   "Wipe this chat and start it over",
			Detail: "Every message, summary and view line is deleted from the store, the vacuum reclaims the file, and the next message is the first again. A rewind restores the files of what it cuts; a reset touches no file.",
			Keys:   []ItemKey{{Key: "x", Desc: "reset", Danger: true, Run: "reset"}},
		}},
	})
	return nil
}
