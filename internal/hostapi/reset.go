// reset.go wipes the chat (/reset): the log, every summary node and view
// line, and the compaction view go in one store transaction, VACUUM reclaims
// the file, and the next message is seq 0. The chat never ends; a reset is
// the only wipe.

package hostapi

import (
	"log/slog"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// ResetChat wipes the eternal chat and starts it over (/reset): the log,
// every summary node and view line go, VACUUM reclaims the file, and the
// next message is seq 0. It refuses mid-turn and while any run lives; once
// confirmed it waits on nothing - the builder's in-flight calls are
// aborted, not waited out, and no model is resolved on its path.
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
		// the builder first: its in-flight calls abort - a reset deletes
		// what they are building, so none is waited out - its dispatch
		// holds until the wipe is done, and it resumes whatever the wipe
		// left
		if err := h.chat.quiesce(); err != nil {
			return err
		}
		defer h.chat.resume()
		// one transaction: the wipe takes the log, the tree, the view and
		// the compaction view
		return h.DB.ResetLog()
	}()
	if err != nil {
		return err
	}
	// the wipe's one visible trace, a system note the chat draws nothing of:
	// zoom and the messages picker reach it, and the splash a fresh chat
	// opens with stays
	if err := h.chat.systemNote("the chat was reset"); err != nil {
		h.Log.Warn("reset note not logged", "origin", "reset", "error", err)
	}
	if err := h.DB.SetTodos(store.MainSessionID, nil); err != nil {
		h.Log.Warn("todo list not cleared", "origin", "reset", "error", err)
	}
	h.todosStale.Store(true)
	h.chatDropped()
	h.setTranscriptFromLog()
	h.Notify(slog.LevelInfo, "Chat reset", "the log, its summary tree and the view start over")
	return nil
}

// chatDropped drops what belongs to the chat that is gone, so the window it
// left is fresh: a reset's wipe ends the chat this way.
func (h *Host) chatDropped() {
	ag := h.agents
	ag.mu.Lock()
	ag.runner, ag.turnModel = nil, ""
	ag.lastIn = 0
	ag.shellNotes = nil
	ag.mu.Unlock()
	// a dropped chat starts its meter over: the stored context is gone with it
	if h.DB != nil {
		if err := h.DB.SetSessionLastIn(store.MainSessionID, 0); err != nil {
			h.Log.Warn("usage not logged", "origin", "reset", "error", err)
		}
	}
	h.resetTodoNag(nil)
	h.goalDropped()
	h.forgetInstructions()
	h.EmitAsync(EventSessionReset, nil)
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
			Detail: "Every message, summary and view line is deleted from the store, the todo list with it, the store file shrinks back, and the next message is the first again. A reset touches no file.",
			Keys:   []ItemKey{{Key: "x", Desc: "reset", Danger: true, Run: "reset"}},
		}},
	})
	return nil
}
