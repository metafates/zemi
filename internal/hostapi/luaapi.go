// luaapi.go holds the smaller zemi.* surfaces: zemi.json, zemi.composer,
// the session list/switch helpers and the agent.assemble prompt hook.
package hostapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// ---------------------------------------------------------------------------
// zemi.json

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
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
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
	if changed && len(h.Events.Handlers("composer.change")) > 0 {
		h.EmitAsync("composer.change", map[string]any{"text": text, "cursor": cursor})
	}
}

// composerNamespace is zemi.composer: get() -> { text, cursor } (cursor is
// a 0-based character offset), set(text [, cursor]), insert(text) at the
// cursor, submit() as if enter was pressed. Edits apply on the UI goroutine.
func (h *Host) composerNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	edit := func(what string, op func(L *glua.LState) (string, int)) *glua.LFunction {
		return L.NewFunction(func(L *glua.LState) int {
			if h.refuseDuringRender(L, "zemi.composer."+what) {
				return 0
			}
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
		out := L.NewTable()
		out.RawSetString("text", glua.LString(text))
		out.RawSetString("cursor", glua.LNumber(cursor))
		L.Push(out)
		return 1
	}))
	tb.RawSetString("set", edit("set", func(L *glua.LState) (string, int) {
		return L.CheckString(1), L.OptInt(2, -1)
	}))
	tb.RawSetString("insert", edit("insert", func(L *glua.LState) (string, int) {
		return L.CheckString(1), 0
	}))
	tb.RawSetString("submit", edit("submit", func(L *glua.LState) (string, int) {
		return "", 0
	}))
	return tb
}

// ---------------------------------------------------------------------------
// zemi.session additions

// sessionExtraFns adds list, new, open and todos to zemi.session. Switching
// sessions is a runtime operation: it returns nil, err (a turn is running,
// no such session) instead of raising.
func (h *Host) sessionExtraFns(L *glua.LState, tb *glua.LTable) {
	result := func(L *glua.LState, err error) int {
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LTrue)
		return 1
	}
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		limit := 20
		if opts, ok := L.Get(1).(*glua.LTable); ok {
			if n, ok := opts.RawGetString("limit").(glua.LNumber); ok && n > 0 {
				limit = min(int(n), 200)
			}
		}
		out := L.NewTable()
		if h.DB != nil {
			current := h.AgentSessionID()
			list, _ := h.DB.ListSessions(limit)
			for _, s := range list {
				row := L.NewTable()
				row.RawSetString("id", glua.LString(s.ID))
				row.RawSetString("title", glua.LString(s.Title))
				row.RawSetString("updated_at", glua.LNumber(s.UpdatedAt))
				row.RawSetString("ago", glua.LString(ago(s.UpdatedAt)))
				row.RawSetString("tokens", glua.LNumber(s.PromptTokens+s.CompletionTokens))
				row.RawSetString("cost", glua.LNumber(s.Cost))
				row.RawSetString("current", glua.LBool(s.ID == current))
				out.Append(row)
			}
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("new", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.session.new") {
			return 0
		}
		return result(L, h.NewSession())
	}))
	tb.RawSetString("open", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.session.open") {
			return 0
		}
		return result(L, h.ResumeSession(L.CheckString(1)))
	}))
	// rewind(message_id) -> restored file count, or nil, err
	tb.RawSetString("rewind", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.session.rewind") {
			return 0
		}
		n, err := h.RewindTo(L.CheckString(1))
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LNumber(n))
		return 1
	}))
	tb.RawSetString("todos", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		if sid := h.AgentSessionID(); sid != "" && h.DB != nil {
			todos, _ := h.DB.Todos(sid)
			for _, td := range todos {
				row := L.NewTable()
				row.RawSetString("content", glua.LString(td.Content))
				row.RawSetString("status", glua.LString(td.Status))
				row.RawSetString("active_form", glua.LString(td.ActiveForm))
				out.Append(row)
			}
		}
		L.Push(out)
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
// to add a section; nil adds nothing. Project and learned handlers are
// skipped: they must not steer the prompt. Each handler gets the veto
// deadline, sections are capped, and each is labeled with its origin.
//
// It calls into the interpreter, so it must never run on the interpreter
// goroutine (zemi.agent.send starts turns asynchronously for that reason).
func (h *Host) assembleSections() string {
	handlers := h.Events.Handlers("agent.assemble")
	if len(handlers) == 0 {
		return ""
	}
	var b strings.Builder
	_, err := h.RT.Do(context.Background(), lua.ClassUI, time.Duration(len(handlers))*lua.VetoDeadline+2*time.Second, 0,
		func(L *glua.LState) ([]any, error) {
			for _, hd := range handlers {
				if hd.Origin == lua.OriginProject || hd.Origin == lua.OriginLearned {
					continue
				}
				title, text := h.callAssembleHandler(L, hd)
				if text = strings.TrimSpace(text); text == "" || b.Len() >= assembleTotalMax {
					continue
				}
				if len(text) > assembleSectionMax {
					text = text[:assembleSectionMax]
				}
				if title == "" {
					title = "context"
				}
				b.WriteString("\n\n## " + title + " (from " + string(hd.Origin) + " config)\n" + text)
			}
			return nil, nil
		})
	if err != nil {
		h.Log.Warn("events", "agent.assemble skipped", map[string]any{"error": err.Error()})
		return ""
	}
	return b.String()
}

func (h *Host) callAssembleHandler(L *glua.LState, hd *EventHandler) (title, text string) {
	if hd.Once {
		h.Events.drop(hd)
	}
	h.RT.PushFrame(hd.Origin, hd.Path)
	defer h.RT.PopFrame()
	handlerCtx, restore := setHandlerContext(L, lua.VetoDeadline)
	defer restore()
	base := L.GetTop()
	defer L.SetTop(base)
	L.Push(hd.Fn)
	L.Push(lua.FromGo(L, map[string]any{"name": "agent.assemble", "from": "host", "cwd": h.Dirs.ProjectDir}))
	err := L.PCall(1, 1, nil)
	if handlerCtx.Err() != nil {
		h.Log.Warn("events", "agent.assemble handler timed out", map[string]any{"id": hd.ID, "origin": string(hd.Origin)})
		return "", ""
	}
	if err != nil {
		h.logHandlerError(hd, err)
		return "", ""
	}
	switch v := L.Get(-1).(type) {
	case glua.LString:
		return "", string(v)
	case *glua.LTable:
		return rawString(v, "title"), rawString(v, "text")
	}
	return "", ""
}
