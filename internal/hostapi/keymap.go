package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
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

// keyMods are the modifier prefixes ultraviolet writes before a key name.
var keyMods = map[string]bool{"ctrl": true, "alt": true, "shift": true, "super": true, "hyper": true, "meta": true}

// keyNames are the named keys the terminal can report, taken from
// ultraviolet's own table so the list cannot drift from what a keypress
// matches against.
var keyNames = sync.OnceValue(func() map[string]bool {
	names := map[string]bool{}
	add := func(c rune) {
		if n := (uv.Key{Code: c}).Keystroke(); len(n) > 1 && strings.IndexFunc(n,
			func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') }) < 0 {
			names[n] = true
		}
	}
	for _, c := range []rune{uv.KeyBackspace, uv.KeyTab, uv.KeyEnter, uv.KeyEscape, uv.KeySpace} {
		add(c)
	}
	// the special keys are one contiguous block of codes past KeyExtended
	for c := uv.KeyExtended + 1; c <= uv.KeyExtended+200; c++ {
		add(c)
	}
	return names
})

// checkKey refuses a key nothing can ever type ("ctrl-q", "control+q"):
// bindings are matched against the key name the terminal reports, so a
// misspelled one sits in /help and never fires.
func checkKey(key string) error {
	base := key
	for len([]rune(base)) > 1 {
		mod, rest, ok := strings.Cut(base, "+")
		if !ok || rest == "" {
			break
		}
		if !keyMods[mod] {
			return fmt.Errorf("unknown modifier %q in key %q: use ctrl, alt, shift, super, hyper or meta", mod, key)
		}
		base = rest
	}
	if len([]rune(base)) == 1 || keyNames()[base] {
		return nil
	}
	return fmt.Errorf("unknown key %q in key %q", base, key)
}

// keySeq normalizes a binding key: one key, or a space-separated sequence
// ("ctrl+x ctrl+s"), where a leading "<leader>" expands to keymap.leader
// ("<leader>gs" is leader, g, s). A global binding cannot start with a
// printable key: those belong to the composer, so it would never fire.
func (h *Host) keySeq(key string, focused bool) (string, error) {
	key = strings.TrimSpace(key)
	var tokens []string
	if rest, ok := strings.CutPrefix(key, "<leader>"); ok {
		tokens = strings.Fields(cmp.Or(strings.TrimSpace(h.Settings.Str("keymap.leader")), "ctrl+x"))
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
		if err := checkKey(tokens[i]); err != nil {
			return "", err
		}
		if tokens[i] == "ctrl+c" { // the UI takes it before any keymap or sequence, focused or not
			return "", errors.New("ctrl+c is reserved for cancel and quit")
		}
	}
	if len([]rune(tokens[0])) == 1 && !focused {
		return "", fmt.Errorf("%q is a printable key and only ever reaches the composer: pass opts.focus to scope it to a node, or start with a non-printable key such as ctrl+x", tokens[0])
	}
	return strings.Join(tokens, " "), nil
}

// resolveRHS turns a string right-hand side into a command reference or a
// dotted host-API call, validating at bind time (DESIGN §6.2). A host call
// may carry an argument after a space: "zemi.ui.scroll.up sidebar".
// A leading slash is accepted so what keymap.get hands back rebinds as is.
func (h *Host) resolveRHS(s string) (RHS, error) {
	if cmd := h.Commands.Lookup(strings.TrimPrefix(s, "/")); cmd != nil {
		return RHS{Command: cmd.Name}, nil
	}
	name, _, _ := strings.Cut(s, " ")
	fn, ok := h.hostActions[name]
	if !ok {
		return RHS{}, nil // unknown: empty RHS marks it
	}
	if fn == nil {
		return RHS{}, errReserved
	}
	return RHS{API: s}, nil
}

// keymapNamespace is zemi.keymap: set(key, action, opts?), del(key, opts?),
// get(key, opts?) and list(). An action is a function, a command name or a
// host action; setting a bound key again rebinds it. opts.focus scopes a
// binding to a focusable layout node: it works only while that node has the
// keys, and there it may start with a printable key. opts.list makes the key
// a list alias, whose action is one of listMoves.
func (h *Host) keymapNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// binding reads the key, opts.focus and opts.list of a call
	binding := func(L *glua.LState, what string) (key, focus string, list bool, opts *glua.LTable) {
		opts = L.OptTable(3, nil)
		if what != "set" {
			opts = L.OptTable(2, nil)
		}
		if opts != nil {
			focus, list = rawString(L, opts, "focus"), rawBool(L, opts, "list")
		}
		key, err := h.keySeq(L.CheckString(1), focus != "" || list)
		switch {
		case list && focus != "":
			err = errors.New("focus and list do not combine")
		case err == nil && list && strings.Contains(key, " "):
			err = errors.New("a list key is one key")
		case err == nil && list && len([]rune(key)) == 1:
			err = errors.New("a list key is non-printable: printable keys filter the list")
		case err == nil && list && slices.Contains([]string{"enter", "esc", "tab", "shift+tab", "space", "backspace"}, key):
			// taking one would leave a prompt no way to answer or close
			err = fmt.Errorf("%s is a key the lists answer with", key)
		}
		if err != nil && what != "get" {
			L.RaiseError("zemi.keymap.%s: %v", what, err)
		}
		return key, focus, list, opts
	}

	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		key, focus, list, opts := binding(L, "set")
		b := &KeyBinding{Key: key, Focus: focus, List: list, Origin: h.RT.CurrentOrigin(L), Path: h.RT.CurrentPath(L), Gen: h.currentGen()}
		v := L.Get(2)
		if s, ok := v.(glua.LString); list && (!ok || !slices.Contains(listMoves, string(s))) {
			L.RaiseError("zemi.keymap.set: with opts.list the action is a move: %s", strings.Join(listMoves, ", "))
		}
		switch v := v.(type) {
		case *glua.LFunction:
			b.RHS = RHS{Fn: v}
		case glua.LString:
			if list {
				// an alias reaches the permission and trust prompts
				if b.Origin != lua.OriginUser && b.Origin != lua.OriginBuiltin {
					L.RaiseError("zemi.keymap.set: only user config may bind list keys")
				}
				b.RHS = RHS{Move: string(v)}
				break
			}
			rhs, err := h.resolveRHS(string(v))
			if err != nil {
				L.RaiseError("zemi.keymap.set: %v", err)
			}
			if rhs.Command == "" && rhs.API == "" {
				L.RaiseError("zemi.keymap.set: %q is neither a command nor a host action", string(v))
			}
			// Loosening permissions is the user's call: a plugin or project
			// key must not flip the profile (its prompt and tool set) on an
			// innocent-looking press, nor submit, fill or paste the
			// clipboard into the composer, whose line reaches slash
			// commands and !shell, nor move the focus, which routes every
			// later key to the node it picked.
			if (slices.Contains([]string{"profile"}, rhs.Command) ||
				strings.HasPrefix(rhs.API, "zemi.agent.profile.") ||
				strings.HasPrefix(rhs.API, "zemi.composer.") || strings.HasPrefix(rhs.API, "zemi.ui.focus.")) &&
				b.Origin != lua.OriginUser && b.Origin != lua.OriginBuiltin {
				L.RaiseError("zemi.keymap.set: only user config may bind profile, composer or focus actions")
			}
			b.RHS = rhs
		default:
			L.RaiseError("zemi.keymap.set: action must be a function or a string, got %s", v.Type().String())
		}
		if opts != nil {
			if d, ok := opt[glua.LString](L, opts, "desc"); ok {
				b.Desc = string(d)
			}
		}
		if err := h.Keymaps.Set(b); err != nil {
			L.RaiseError("zemi.keymap.set: %v", err)
		}
		return 0
	}))

	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		key, focus, list, _ := binding(L, "del")
		removed, err := h.Keymaps.Del(h.RT.CurrentOrigin(L), h.RT.CurrentPath(L), focus, key, list)
		if err != nil {
			L.RaiseError("zemi.keymap.del: %v", err)
		}
		L.Push(glua.LBool(removed))
		return 1
	}))

	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		key, focus, list, _ := binding(L, "get")
		b := h.Keymaps.Lookup(focus, key)
		if list {
			b = h.Keymaps.ListAlias(key)
		}
		if b != nil {
			L.Push(lua.FromGo(L, keymapRow(b)))
		} else {
			L.Push(glua.LNil)
		}
		return 1
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, b := range h.Keymaps.List() {
			rows = append(rows, keymapRow(b))
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	return tb
}

// keymapRow is a binding snapshot: key, action ("/cmd", a host action,
// "<function>" or a list alias's move), desc, origin, focus (nil for a
// global binding) and list (true for a list alias).
func keymapRow(b *KeyBinding) map[string]any {
	row := map[string]any{"key": b.Key, "action": describeRHS(b.RHS), "desc": b.Desc, "origin": string(b.Origin)}
	if b.Focus != "" {
		row["focus"] = b.Focus
	}
	if b.List {
		row["list"] = true
	}
	return row
}
