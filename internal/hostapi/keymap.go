package hostapi

import (
	"strings"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// expandLeader maps the nvim-style <leader> prefix to the space key
// (configurable key arrives later).
func expandLeader(keys string) string {
	if len(keys) >= 8 && strings.EqualFold(keys[:8], "<leader>") {
		return "space" + keys[8:]
	}
	return keys
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

func (h *Host) keymapNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("bind", L.NewFunction(func(L *glua.LState) int {
		mode := L.CheckString(1)
		keys := expandLeader(L.CheckString(2))
		if h.refuseDuringRender(L, "zemi.keymap.bind") {
			return 0
		}
		binding := &KeyBinding{
			Mode:   mode,
			Keys:   keys,
			Origin: h.RT.CurrentOrigin(),
			Path:   h.RT.CurrentPath(),
			Gen:    h.currentGen(),
		}
		rhsVal := L.Get(3)
		switch v := rhsVal.(type) {
		case *glua.LFunction:
			binding.RHS = RHS{Fn: v}
		case glua.LString:
			rhs, err := h.resolveRHS(string(v))
			if err != nil {
				L.RaiseError("zemi.keymap.bind: %v", err)
				return 0
			}
			if rhs.Command == "" && rhs.API == "" && rhs.Fn == nil {
				L.RaiseError("zemi.keymap.bind: %q is neither a command nor a host action", string(v))
				return 0
			}
			// Loosening permissions is the user's call: a plugin or project
			// key must not flip the mode on an innocent-looking press.
			if (rhs.API == "zemi.permission.mode.cycle" || rhs.Command == "mode") &&
				binding.Origin != lua.OriginUser && binding.Origin != lua.OriginBuiltin {
				L.RaiseError("zemi.keymap.bind: only user config may bind permission mode changes")
				return 0
			}
			binding.RHS = rhs
		default:
			L.RaiseError("zemi.keymap.bind: rhs must be a string or function, got %s", rhsVal.Type().String())
			return 0
		}
		if L.GetTop() >= 4 && L.Get(4) != glua.LNil {
			opts, ok := L.Get(4).(*glua.LTable)
			if !ok {
				L.RaiseError("zemi.keymap.bind: opts must be a table")
				return 0
			}
			if d, ok := opts.RawGetString("desc").(glua.LString); ok {
				binding.Desc = string(d)
			}
		}
		if _, err := h.Keymaps.Bind(binding); err != nil {
			L.RaiseError("zemi.keymap.bind: %v", err)
			return 0
		}
		return 0
	}))

	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		mode := L.CheckString(1)
		keys := expandLeader(L.CheckString(2))
		if !h.Keymaps.Del(h.RT.CurrentOrigin(), mode, keys) {
			L.RaiseError("zemi.keymap.del: no %s binding in %s mode owned by this origin", keys, mode)
			return 0
		}
		return 0
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, b := range h.Keymaps.List() {
			row := L.NewTable()
			row.RawSetString("mode", glua.LString(b.Mode))
			row.RawSetString("keys", glua.LString(b.Keys))
			row.RawSetString("desc", glua.LString(b.Desc))
			row.RawSetString("origin", glua.LString(b.Origin))
			switch {
			case b.RHS.Fn != nil:
				row.RawSetString("rhs", glua.LString("<function>"))
			case b.RHS.Command != "":
				row.RawSetString("rhs", glua.LString(":"+b.RHS.Command))
			default:
				row.RawSetString("rhs", glua.LString(b.RHS.API))
			}
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))

	return tb
}
