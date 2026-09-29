// luaapi.go holds the smaller zemi.* surfaces: zemi.json, zemi.composer,
// the session list/switch helpers and the agent.assemble prompt hook.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// ---------------------------------------------------------------------------
// zemi.json

// jsonNamespace is zemi.json, in config Lua and in a workflow script.
func (h *Host) jsonNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("encode", L.NewFunction(func(L *glua.LState) int {
		v, err := lua.ToGo(L.CheckAny(1))
		if err != nil {
			L.RaiseError("zemi.json.encode: %v", err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			L.RaiseError("zemi.json.encode: %v", err)
		}
		L.Push(glua.LString(b))
		return 1
	}))
	tb.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		var v any
		if err := json.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
			return luaResult(L, err)
		}
		L.Push(lua.FromGo(L, v))
		return 1
	}))
	return tb
}

// ---------------------------------------------------------------------------
// zemi.composer

// SetComposerSnapshot records the draft after the UI changed it, so
// zemi.composer.get never waits on the UI goroutine.
func (h *Host) SetComposerSnapshot(text string, cursor int) {
	h.composerMu.Lock()
	changed := h.composerText != text
	h.composerText, h.composerCursor = text, cursor
	h.composerMu.Unlock()
	if changed {
		h.EmitAsync("composer.change", map[string]any{"text": text, "cursor": cursor})
	}
}

// composerNamespace is zemi.composer: get() -> { text, cursor } (cursor is
// a 0-based character offset), set(text [, cursor]), insert(text) at the
// cursor, submit() as if enter was pressed, history_prev() and
// history_next() through sent messages, paste() the clipboard's image or
// text, and edit() the draft in the user's editor. Edits apply on the UI
// goroutine.
func (h *Host) composerNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	edit := func(what ComposerOp, op func(L *glua.LState) (string, int)) *glua.LFunction {
		return L.NewFunction(func(L *glua.LState) int {
			// a submitted line reaches slash commands, !shell and the agent
			hint := ""
			if what == ComposerSet || what == ComposerInsert {
				hint = `an overlay item's fill = "text" fills the composer from any origin`
			}
			h.refuseBelowUser(L, "composer."+string(what), hint)
			text, cursor := op(L)
			if ui := h.hooks(); ui != nil {
				ui.ComposerEdit(what, text, cursor)
			}
			return 0
		})
	}
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		h.composerMu.Lock()
		text, cursor := h.composerText, h.composerCursor
		h.composerMu.Unlock()
		L.Push(lua.FromGo(L, map[string]any{"text": text, "cursor": cursor}))
		return 1
	}))
	tb.RawSetString(string(ComposerSet), edit(ComposerSet, func(L *glua.LState) (string, int) {
		return L.CheckString(1), L.OptInt(2, -1)
	}))
	tb.RawSetString(string(ComposerInsert), edit(ComposerInsert, func(L *glua.LState) (string, int) {
		return L.CheckString(1), 0
	}))
	tb.RawSetString("complete", L.NewFunction(h.addCompletion))
	for _, op := range []ComposerOp{ComposerSubmit, ComposerHistoryPrev, ComposerHistoryNext, ComposerPaste, ComposerEditor} {
		tb.RawSetString(string(op), edit(op, func(L *glua.LState) (string, int) { return "", 0 }))
	}
	return tb
}

// ---------------------------------------------------------------------------
// zemi.session additions

// sessionExtraFns adds list, new, open and todos to zemi.session. Switching
// sessions is a runtime operation: it returns nil, err (a turn is running,
// no such session) instead of raising.
func (h *Host) sessionExtraFns(L *glua.LState, tb *glua.LTable) {
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		limit := 20
		if opts, ok := L.Get(1).(*glua.LTable); ok {
			if n, ok := opt[glua.LNumber](L, opts, "limit"); ok && n > 0 {
				limit = min(int(n), 200)
			}
		}
		rows := []any{}
		if h.DB != nil {
			current := h.AgentSessionID()
			list, _ := h.DB.ListSessions(limit)
			for _, s := range list {
				rows = append(rows, map[string]any{
					"id": s.ID, "title": s.Title, "updated_at": s.UpdatedAt, "ago": ago(s.UpdatedAt),
					"tokens": s.PromptTokens + s.CompletionTokens, "cost": s.Cost, "current": s.ID == current,
				})
			}
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	tb.RawSetString("new", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "session.new")
		return luaResult(L, h.NewSession())
	}))
	tb.RawSetString("resume", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "session.resume")
		return luaResult(L, h.ResumeSession(L.CheckString(1)))
	}))
	// rewind(message_id) -> restored file count, or nil, err
	tb.RawSetString("rewind", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "session.rewind")
		n, err := h.RewindTo(L.CheckString(1))
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(glua.LNumber(n))
		return 1
	}))
	tb.RawSetString("todos", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		if sid := h.AgentSessionID(); sid != "" && h.DB != nil {
			todos, _ := h.DB.Todos(sid)
			for _, td := range todos {
				rows = append(rows, map[string]any{"content": td.Content, "status": td.Status, "active_form": td.ActiveForm})
			}
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
}

// ---------------------------------------------------------------------------
// agent.assemble

const (
	assembleSectionMax = 16 << 10
	assembleTotalMax   = 64 << 10
)

// assembleSections runs the agent.assemble handlers while a system prompt is
// built (DESIGN §6.3). A handler returns a string, or { title = , text = },
// to add a section; nil adds nothing. Project handlers are
// skipped: they must not steer the prompt. Each handler gets the veto
// deadline, sections are capped, and each is labeled with its origin.
//
// It calls into the interpreter, so it must never run on the interpreter
// goroutine (zemi.agent.send starts turns asynchronously for that reason).
func (h *Host) assembleSections() string {
	h.recordEvent("agent.assemble", "host", map[string]any{"cwd": h.Dirs.ProjectDir})
	handlers := h.Events.Handlers("agent.assemble")
	if len(handlers) == 0 {
		return ""
	}
	var b strings.Builder
	_, err := h.RT.Do(context.Background(), lua.ClassUI, time.Duration(len(handlers))*lua.VetoDeadline+2*time.Second, 0,
		func(L *glua.LState) ([]any, error) {
			for _, hd := range handlers {
				if hd.Origin.Rank() <= lua.OriginProject.Rank() {
					continue
				}
				title, text := h.callAssembleHandler(L, hd)
				if text = strings.TrimSpace(text); text == "" || b.Len() >= assembleTotalMax {
					continue
				}
				b.WriteString("\n\n## " + cmp.Or(title, "context") + " (from " + string(hd.Origin) + " config)\n" + text[:min(len(text), assembleSectionMax)])
			}
			return nil, nil
		})
	if err != nil {
		h.Log.Warn("agent.assemble skipped", "origin", "events", "error", err)
		return ""
	}
	return b.String()
}

func (h *Host) callAssembleHandler(L *glua.LState, hd *EventHandler) (title, text string) {
	ev := map[string]any{"name": "agent.assemble", "from": "host", "cwd": h.Dirs.ProjectDir}
	ret, timedOut, err := h.callEvent(L, hd, ev)
	switch {
	case timedOut:
		h.Log.Warn("agent.assemble handler timed out", "origin", "events", "id", hd.ID, "from", string(hd.Origin))
	case err != nil:
		h.logHandlerError(hd, err)
	}
	switch v := ret.(type) {
	case glua.LString:
		return "", string(v)
	case *glua.LTable:
		return glua.LVAsString(v.RawGetString("title")), glua.LVAsString(v.RawGetString("text"))
	}
	return "", ""
}
