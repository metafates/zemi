// luaapi.go holds the smaller zemi.* surfaces: zemi.json, zemi.composer,
// zemi.session's list, switch and todo members and the agent.assemble prompt hook.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// zemi.json

// codecOpts is the options table a codec's encode or decode call takes as
// its second argument, nil when the call passed none.
func codecOpts(L *glua.LState) *glua.LTable {
	if L.GetTop() < 2 {
		return nil
	}
	L.CheckTypes(2, glua.LTNil, glua.LTTable)
	if L.Get(2).Type() == glua.LTNil {
		return nil
	}
	return L.Get(2).(*glua.LTable)
}

// optString reads a string field of the options table, def when absent.
func optString(L *glua.LState, opts *glua.LTable, name, def string) string {
	if opts == nil {
		return def
	}
	v := opts.RawGetString(name)
	switch v := v.(type) {
	case *glua.LNilType:
	case glua.LString:
		return string(v)
	default:
		L.RaiseError("%s is a %s, want a string", name, v.Type().String())
	}
	return def
}

// optBool reads a boolean field of the options table, def when absent.
func optBool(L *glua.LState, opts *glua.LTable, name string, def bool) bool {
	if opts == nil {
		return def
	}
	v := opts.RawGetString(name)
	switch v := v.(type) {
	case *glua.LNilType:
	case glua.LBool:
		return bool(v)
	default:
		L.RaiseError("%s is a %s, want a boolean", name, v.Type().String())
	}
	return def
}

// optInt reads a whole-number field of the options table, def when absent.
func optInt(L *glua.LState, opts *glua.LTable, name string, def int) int {
	if opts == nil {
		return def
	}
	v := opts.RawGetString(name)
	switch v := v.(type) {
	case *glua.LNilType:
	case glua.LNumber:
		return int(v)
	default:
		L.RaiseError("%s is a %s, want a number", name, v.Type().String())
	}
	return def
}

// optRune reads a one-character string field of the options table (a
// delimiter or a comment marker), the def's rune when absent.
func optRune(L *glua.LState, opts *glua.LTable, name, def string) rune {
	s := optString(L, opts, name, def)
	r := []rune(s)
	if len(r) != 1 {
		L.RaiseError("%s is %q, want one character", name, s)
	}
	return r[0]
}

// jsonNamespace is zemi.json, in config Lua and in a workflow script. Both
// calls take an optional options table: encode speaks deterministic (the
// default), indent, escape_for_html and escape_for_js; decode speaks
// allow_duplicate_names and match_case_insensitive_names.
func (h *Host) jsonNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("encode", L.NewFunction(func(L *glua.LState) int {
		v, err := lua.ToGo(L.CheckAny(1))
		if err != nil {
			L.RaiseError("zemi.json.encode: %v", err)
		}
		opts := []json.Options{json.Deterministic(true)}
		if o := codecOpts(L); o != nil {
			opts[0] = json.Deterministic(optBool(L, o, "deterministic", true))
			if indent := optString(L, o, "indent", ""); indent != "" {
				opts = append(opts, jsontext.WithIndent(indent))
			}
			if optBool(L, o, "escape_for_html", false) {
				opts = append(opts, jsontext.EscapeForHTML(true))
			}
			if optBool(L, o, "escape_for_js", false) {
				opts = append(opts, jsontext.EscapeForJS(true))
			}
		}
		b, err := json.Marshal(v, opts...)
		if err != nil {
			L.RaiseError("zemi.json.encode: %v", err)
		}
		L.Push(glua.LString(b))
		return 1
	}))
	tb.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		var opts []json.Options
		if o := codecOpts(L); o != nil {
			if optBool(L, o, "allow_duplicate_names", false) {
				opts = append(opts, jsontext.AllowDuplicateNames(true))
			}
			if optBool(L, o, "match_case_insensitive_names", false) {
				opts = append(opts, json.MatchCaseInsensitiveNames(true))
			}
		}
		var v any
		if err := json.Unmarshal([]byte(L.CheckString(1)), &v, opts...); err != nil {
			return luaResult(L, err)
		}
		L.Push(lua.FromGo(L, v))
		return 1
	}))
	return tb
}

// zemi.composer

// SetComposerSnapshot records the draft after the UI changed it, so
// zemi.composer.get never waits on the UI goroutine.
func (h *Host) SetComposerSnapshot(text string, cursor int) {
	h.composerMu.Lock()
	changed := h.composerText != text
	h.composerText, h.composerCursor = text, cursor
	h.composerMu.Unlock()
	if changed {
		h.EmitAsync(EventComposerChange, map[string]any{"text": text, "cursor": cursor})
	}
}

// ComposerText is the draft as the UI last pushed it, without waiting on the
// UI goroutine.
func (h *Host) ComposerText() string {
	h.composerMu.Lock()
	defer h.composerMu.Unlock()
	return h.composerText
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
	tb.RawSetString("add", L.NewFunction(h.addCompletion))
	for _, op := range []ComposerOp{ComposerSubmit, ComposerHistoryPrev, ComposerHistoryNext, ComposerPaste, ComposerEditor} {
		tb.RawSetString(string(op), edit(op, func(L *glua.LState) (string, int) { return "", 0 }))
	}
	return tb
}

// zemi.session additions

// agent.assemble

const (
	assembleSectionMax = 16 << 10
	assembleTotalMax   = 64 << 10
)

// assembleSections runs the agent.assemble handlers while a system prompt is
// built (DESIGN §6.3). A handler returns a string, or { title = , text = },
// to add a section; nil adds nothing. Each handler gets the veto deadline,
// sections are capped, and each is labeled with its origin.
//
// It calls into the interpreter, so it must never run on the interpreter
// goroutine (zemi.agent.send starts turns asynchronously for that reason).
func (h *Host) assembleSections() string {
	h.recordEvent(EventAgentAssemble, "host", map[string]any{"cwd": h.Dirs.ProjectDir})
	handlers := h.Events.Handlers(EventAgentAssemble)
	if len(handlers) == 0 {
		return ""
	}
	var b strings.Builder
	_, err := h.RT.Do(context.Background(), lua.ClassUI, lua.VetoCeiling(len(handlers), 1), 0, "hook "+EventAgentAssemble,
		func(L *glua.LState) ([]any, error) {
			for _, hd := range handlers {
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
	ev := eventTable(EventAgentAssemble, "host", map[string]any{"cwd": h.Dirs.ProjectDir})
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
