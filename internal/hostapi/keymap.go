package hostapi

import (
	"errors"
	"fmt"
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

// keySeq normalizes a binding key: one key, or a space-separated sequence
// ("ctrl+x ctrl+s"), where a leading "<leader>" expands to keymap.leader
// ("<leader>gs" is leader, g, s). A sequence cannot start with a printable
// key: those belong to the composer.
func (h *Host) keySeq(key string) (string, error) {
	key = strings.TrimSpace(key)
	var tokens []string
	if rest, ok := strings.CutPrefix(key, "<leader>"); ok {
		leader := "ctrl+g"
		if v, ok := h.Settings.Get("keymap.leader"); ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				leader = s
			}
		}
		tokens = strings.Fields(leader)
		if rest = strings.TrimSpace(rest); strings.Contains(rest, " ") {
			tokens = append(tokens, strings.Fields(rest)...)
		} else {
			for _, r := range rest {
				tokens = append(tokens, string(r))
			}
		}
	} else {
		tokens = strings.Fields(key)
	}
	if len(tokens) == 0 {
		return "", errors.New("key is required")
	}
	for i := range tokens {
		tokens[i] = normalizeKey(tokens[i])
	}
	if len(tokens) > 1 && len([]rune(tokens[0])) == 1 {
		return "", fmt.Errorf("a key sequence must start with a non-printable key such as ctrl+g, not %q", tokens[0])
	}
	return strings.Join(tokens, " "), nil
}

// resolveRHS turns a string right-hand side into a command reference or a
// dotted host-API call, validating at bind time (DESIGN §6.2). A host call
// may carry an argument after a space: "zemi.ui.scroll.up sidebar".
func (h *Host) resolveRHS(s string) (RHS, error) {
	if cmd := h.Commands.Lookup(s); cmd != nil {
		return RHS{Command: cmd.Name}, nil
	}
	name, _, _ := strings.Cut(s, " ")
	fn, ok := h.hostActions[name]
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
		key, err := h.keySeq(L.CheckString(1))
		if err != nil {
			L.RaiseError("zemi.keymap.set: %v", err)
		}
		h.refuseDuringRender(L, "zemi.keymap.set")
		b := &KeyBinding{Key: key, Origin: h.RT.CurrentOrigin(L), Path: h.RT.CurrentPath(L), Gen: h.currentGen()}
		switch v := L.Get(2).(type) {
		case *glua.LFunction:
			b.RHS = RHS{Fn: v}
		case glua.LString:
			rhs, err := h.resolveRHS(string(v))
			if err != nil {
				L.RaiseError("zemi.keymap.set: %v", err)
			}
			if rhs.Command == "" && rhs.API == "" {
				L.RaiseError("zemi.keymap.set: %q is neither a command nor a host action", string(v))
			}
			// Loosening permissions is the user's call: a plugin or project
			// key must not flip the mode on an innocent-looking press.
			if (strings.HasPrefix(rhs.API, "zemi.permission.mode.") || rhs.Command == "mode") &&
				b.Origin != lua.OriginUser && b.Origin != lua.OriginBuiltin {
				L.RaiseError("zemi.keymap.set: only user config may bind permission mode changes")
			}
			b.RHS = rhs
		default:
			L.RaiseError("zemi.keymap.set: action must be a function or a string, got %s", v.Type().String())
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
		key, err := h.keySeq(L.CheckString(1))
		if err != nil {
			L.RaiseError("zemi.keymap.del: %v", err)
		}
		h.refuseDuringRender(L, "zemi.keymap.del")
		if err := h.Keymaps.Del(h.RT.CurrentOrigin(L), key); err != nil {
			L.RaiseError("zemi.keymap.del: %v", err)
		}
		return 0
	}))

	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		key, _ := h.keySeq(L.CheckString(1))
		if b := h.Keymaps.Lookup(key); b != nil {
			L.Push(lua.FromGo(L, keymapRow(b)))
		} else {
			L.Push(glua.LNil)
		}
		return 1
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, b := range h.Keymaps.List() {
			rows = append(rows, keymapRow(b))
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	return tb
}

// keymapRow is a binding snapshot: key, action ("/cmd", a host action or
// "<function>"), desc and origin.
func keymapRow(b *KeyBinding) map[string]any {
	return map[string]any{"key": b.Key, "action": describeRHS(b.RHS), "desc": b.Desc, "origin": string(b.Origin)}
}
