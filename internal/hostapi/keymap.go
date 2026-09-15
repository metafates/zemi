package hostapi

import (
	"strings"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// normalizeKey lowercases named keys ("Ctrl+T" is "ctrl+t", as bubbletea
// reports it); a single character keeps its case.
func normalizeKey(key string) string {
	key = strings.TrimSpace(key)
	if len([]rune(key)) > 1 {
		return strings.ToLower(key)
	}
	return key
}

// resolveRHS turns a string right-hand side into a command reference or a
// dotted host-API call, validating at bind time (DESIGN §6.2).
func (h *Host) resolveRHS(s string) (RHS, error) {
	if cmd := h.Commands.Lookup(s); cmd != nil {
		return RHS{Command: cmd.Name}, nil
	}
	fn, ok := h.hostActions[s]
	if !ok {
		return RHS{}, nil // unknown: empty RHS marks it
	}
	if fn == nil {
		return RHS{}, ErrReserved
	}
	return RHS{API: s}, nil
}

// keymapNamespace is zemi.keymap: set(key, action, opts?), del(key),
// get(key) and list(). An action is a function, a command name or a host
// action; setting a bound key again rebinds it.
func (h *Host) keymapNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		key := normalizeKey(L.CheckString(1))
		if h.refuseDuringRender(L, "zemi.keymap.set") {
			return 0
		}
		b := &KeyBinding{Key: key, Origin: h.RT.CurrentOrigin(), Path: h.RT.CurrentPath(), Gen: h.currentGen()}
		switch v := L.Get(2).(type) {
		case *glua.LFunction:
			b.RHS = RHS{Fn: v}
		case glua.LString:
			rhs, err := h.resolveRHS(string(v))
			if err != nil {
				L.RaiseError("zemi.keymap.set: %v", err)
				return 0
			}
			if rhs.Command == "" && rhs.API == "" {
				L.RaiseError("zemi.keymap.set: %q is neither a command nor a host action", string(v))
				return 0
			}
			// Loosening permissions is the user's call: a plugin or project
			// key must not flip the mode on an innocent-looking press.
			if (strings.HasPrefix(rhs.API, "zemi.permission.mode.") || rhs.Command == "mode") &&
				b.Origin != lua.OriginUser && b.Origin != lua.OriginBuiltin {
				L.RaiseError("zemi.keymap.set: only user config may bind permission mode changes")
				return 0
			}
			b.RHS = rhs
		default:
			L.RaiseError("zemi.keymap.set: action must be a function or a string, got %s", v.Type().String())
			return 0
		}
		if opts := L.OptTable(3, nil); opts != nil {
			if d, ok := opts.RawGetString("desc").(glua.LString); ok {
				b.Desc = string(d)
			}
		}
		if err := h.Keymaps.Set(b); err != nil {
			L.RaiseError("zemi.keymap.set: %v", err)
		}
		return 0
	}))

	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		key := normalizeKey(L.CheckString(1))
		if h.refuseDuringRender(L, "zemi.keymap.del") {
			return 0
		}
		if err := h.Keymaps.Del(h.RT.CurrentOrigin(), key); err != nil {
			L.RaiseError("zemi.keymap.del: %v", err)
		}
		return 0
	}))

	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		if b := h.Keymaps.Lookup(normalizeKey(L.CheckString(1))); b != nil {
			L.Push(keymapRow(L, b))
		} else {
			L.Push(glua.LNil)
		}
		return 1
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, b := range h.Keymaps.List() {
			out.Append(keymapRow(L, b))
		}
		L.Push(out)
		return 1
	}))

	return tb
}

// keymapRow is a binding snapshot: key, action ("/cmd", a host action or
// "<function>"), desc and origin.
func keymapRow(L *glua.LState, b *KeyBinding) *glua.LTable {
	row := L.NewTable()
	row.RawSetString("key", glua.LString(b.Key))
	row.RawSetString("action", glua.LString(describeRHS(b.RHS)))
	row.RawSetString("desc", glua.LString(b.Desc))
	row.RawSetString("origin", glua.LString(b.Origin))
	return row
}
