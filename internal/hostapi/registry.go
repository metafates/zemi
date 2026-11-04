// Package hostapi implements the zemi.* namespaces (thin adapters over
// core services, DESIGN §3) and the registration registries shared with
// the UI (keymaps, commands, widgets, event handlers, themes).
package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// Gen identifies a runtime generation; registrations are tagged with the
// generation that created them so a reload can purge the dead ones.
type Gen int64

// RHS is what a keymap binding executes.
type RHS struct {
	Fn      *glua.LFunction // Lua callback (interpreter goroutine only)
	Command string          // registered command name
	API     string          // dotted host-API call (zemi.*)
	Move    string          // a list alias: the move it stands for ("down"), one of listMoves
}

// listMoves are the keys a list alias may stand for: moves only, so no
// alias answers, confirms or copies on a prompt.
var listMoves = []string{"up", "down", "pgup", "pgdown", "home", "end", "ctrl+d", "ctrl+u"}

// KeyBinding maps one key to a right-hand side.
type KeyBinding struct {
	Key    string // bubbletea key string: "ctrl+s", "tab", "alt+enter"
	List   bool   // a list alias: live while a list, popup or prompt has the keys
	RHS    RHS
	Desc   string
	Origin lua.Origin
	Path   string
	Gen    Gen
	order  int // registration order, which /help and zemi.keymap.list keep
}

// bindKey is a binding's slot. Lists take their keys before keymaps are
// consulted, so an alias never overlaps a global binding or sequence.
type bindKey struct {
	key  string
	list bool
}

func (b *KeyBinding) slot() bindKey { return bindKey{key: b.Key, list: b.List} }

// mayReplace refuses one plugin's registration or removal over another's: both
// carry OriginPlugin, so the later would silently replace or drop the first
// with nothing to attribute it to. owner is Host.PluginOf; a bare registry
// (tests) has none and allows every registration.
func mayReplace(owner func(string) string, what string, old lua.Origin, oldPath string, by lua.Origin, byPath string) error {
	if owner != nil && old == lua.OriginPlugin && by == lua.OriginPlugin && owner(oldPath) != owner(byPath) {
		return fmt.Errorf("%s is registered by plugin %s, not %s", what, owner(oldPath), owner(byPath))
	}
	return nil
}

// RefuseBy is the overlay isolation rule the types promise (zemi.ui.overlay):
// user config, builtin code or the origin that opened an overlay may close or
// replace it, and of the plugins only the one that opened it (mayReplace). A
// Go screen's overlay (Gen 0) is out of every plugin's reach either way, so no
// stand-in takes its id. by of "" is a Go caller that set no origin.
func (o Overlay) RefuseBy(by lua.Origin, byPath string, owner func(string) string) error {
	if by == lua.OriginUser || by == lua.OriginBuiltin || by == "" {
		return nil
	}
	if by == o.Origin && (o.Origin != lua.OriginPlugin || owner(o.Path) == owner(byPath)) {
		return nil
	}
	what := fmt.Sprintf("overlay %q", o.ID)
	if o.Gen == 0 {
		return fmt.Errorf("%s is opened by zemi itself", what)
	}
	if o.Origin == lua.OriginPlugin {
		return fmt.Errorf("%s is opened by plugin %s, not %s", what, owner(o.Path), owner(byPath))
	}
	return fmt.Errorf("%s is opened by %s code", what, o.Origin)
}

// KeymapRegistry stores one binding per key and focus; the last registration
// wins (DESIGN §6.2).
type KeymapRegistry struct {
	mu       sync.RWMutex
	bindings map[bindKey]*KeyBinding
	next     int
	owner    func(path string) string // Host.PluginOf, see mayReplace
}

// Set binds or rebinds a key; the last registration wins. A key fires before
// the longer sequences it starts, so overlapping one shadows it (or is dead).
func (r *KeymapRegistry) Set(b *KeyBinding) error {
	if b.Key == "" {
		return errors.New("keymap: key is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bindings == nil {
		r.bindings = map[bindKey]*KeyBinding{}
	}
	if old := r.bindings[b.slot()]; old != nil {
		if err := mayReplace(r.owner, "keymap "+b.Key, old.Origin, old.Path, b.Origin, b.Path); err != nil {
			return err
		}
	}
	// a key fires before the longer sequences it starts, so overlapping a
	// more trusted sequence would take it over (or be dead)
	for k, old := range r.bindings {
		related := k.list == b.List
		overlap := strings.HasPrefix(k.key, b.Key+" ") || strings.HasPrefix(b.Key, k.key+" ")
		if !related || !overlap {
			continue
		}
		if err := mayReplace(r.owner, "keymap "+b.Key+" overlaps "+k.key+", which", old.Origin, old.Path, b.Origin, b.Path); err != nil {
			return err
		}
	}
	if old := r.bindings[b.slot()]; old != nil {
		b.order = old.order // a rebind keeps its place
	} else {
		r.next++
		b.order = r.next
	}
	r.bindings[b.slot()] = b
	return nil
}

// Del unbinds a key the caller may replace (mayReplace) and reports whether
// there was one. Unbinding what is not bound is not an error: a reloaded
// config must be idempotent.
func (r *KeymapRegistry) Del(by lua.Origin, byPath, key string, list bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := bindKey{key: key, list: list}
	b := r.bindings[k]
	if b == nil {
		return false, nil
	}
	if err := mayReplace(r.owner, "keymap "+key, b.Origin, b.Path, by, byPath); err != nil {
		return false, err
	}
	delete(r.bindings, k)
	return true, nil
}

// Lookup finds the binding for a key.
func (r *KeymapRegistry) Lookup(key string) *KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bindings[bindKey{key: key}]
}

// ListAlias is the list alias bound to key, nil when none: the key loop
// swaps the key for its move in Go, never calling Lua.
func (r *KeymapRegistry) ListAlias(key string) *KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bindings[bindKey{key: key, list: true}]
}

// IsPrefix reports whether seq begins a longer bound key sequence.
func (r *KeymapRegistry) IsPrefix(seq string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k := range r.bindings {
		if !k.list && strings.HasPrefix(k.key, seq+" ") {
			return true
		}
	}
	return false
}

// List returns all bindings in registration order, so pairs bound together
// read together.
func (r *KeymapRegistry) List() []*KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.bindings), func(a, b *KeyBinding) int { return cmp.Compare(a.order, b.order) })
}

// Purge drops every binding (replay re-registers on reload).
func (r *KeymapRegistry) Purge() {
	r.mu.Lock()
	r.bindings = map[bindKey]*KeyBinding{}
	r.mu.Unlock()
}

// Command is a slash command.
type Command struct {
	Name     string
	Desc     string
	NArgs    NArgs
	Complete *glua.LFunction
	Fn       *glua.LFunction
	GoFn     func(h *Host, args string, confirm bool) error
	// GoComplete offers argument candidates for Go-owned commands (lead is
	// the argument under the cursor, line the text up to it).
	GoComplete func(h *Host, lead, line string) []string
	// Hidden commands run when typed but stay out of the bare "/" palette list.
	Hidden bool
	Origin lua.Origin
	Path   string
	Gen    Gen
}

// CommandRegistry stores commands. Names collide case-insensitively: a
// case-variant of an existing name is refused.
type CommandRegistry struct {
	mu       sync.RWMutex
	commands map[string]*Command      // canonical name
	owner    func(path string) string // Host.PluginOf, see mayReplace
}

func canonical(name string) string { return strings.ToLower(name) }

func (r *CommandRegistry) Add(c *Command) (*Command, error) {
	if c.Name == "" {
		return nil, errors.New("command: name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commands == nil {
		r.commands = map[string]*Command{}
	}
	key := canonical(c.Name)
	if old, exists := r.commands[key]; exists {
		if err := mayReplace(r.owner, "command /"+old.Name, old.Origin, old.Path, c.Origin, c.Path); err != nil {
			return old, err
		}
		if old.Name != c.Name {
			return old, fmt.Errorf("command /%s collides with /%s (case-insensitive)", c.Name, old.Name)
		}
	}
	r.commands[key] = c
	return c, nil
}

// Lookup finds a command case-insensitively.
func (r *CommandRegistry) Lookup(name string) *Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.commands[canonical(name)]
}

// List returns all commands sorted by name.
func (r *CommandRegistry) List() []*Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.commands), func(a, b *Command) int { return cmp.Compare(a.Name, b.Name) })
}

// Remove drops a command the caller may replace (mayReplace); it reports
// whether one was removed.
func (r *CommandRegistry) Remove(name string, by lua.Origin, byPath string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.commands[canonical(name)]
	if !ok {
		return false, nil
	}
	if err := mayReplace(r.owner, "command /"+c.Name, c.Origin, c.Path, by, byPath); err != nil {
		return false, err
	}
	delete(r.commands, canonical(name))
	return true, nil
}

func (r *CommandRegistry) Purge() {
	r.mu.Lock()
	r.commands = map[string]*Command{}
	r.mu.Unlock()
}

// EventHandler is a Lua callback subscribed to an event.
type EventHandler struct {
	ID     string
	Event  string
	Fn     *glua.LFunction
	Origin lua.Origin
	Path   string
	Gen    Gen
	Once   bool // removed before its first call
}

// EventRegistry stores event subscriptions; a handler id registers again in
// its list's place, under mayReplace.
type EventRegistry struct {
	mu       sync.RWMutex
	handlers map[string][]*EventHandler // event -> handlers in registration order
	nextID   int
	owner    func(path string) string // Host.PluginOf, see mayReplace
}

func (r *EventRegistry) Add(h *EventHandler) (*EventHandler, error) {
	if h.Event == "" || h.Fn == nil {
		return nil, errors.New("event: event and handler are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = map[string][]*EventHandler{}
	}
	if h.ID != "" {
		list := r.handlers[h.Event]
		if i := slices.IndexFunc(list, func(o *EventHandler) bool { return o.ID == h.ID }); i >= 0 {
			if err := mayReplace(r.owner, fmt.Sprintf("event handler %q on %s", h.ID, h.Event), list[i].Origin, list[i].Path, h.Origin, h.Path); err != nil {
				return list[i], err
			}
			list[i] = h
			return h, nil
		}
	} else {
		r.nextID++
		h.ID = fmt.Sprintf("handler.%d", r.nextID)
	}
	r.handlers[h.Event] = append(r.handlers[h.Event], h)
	return h, nil
}

// Handlers returns the subscription list for an event.
func (r *EventRegistry) Handlers(event string) []*EventHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.handlers[event])
}

// Remove drops the handler with id when the caller may replace it
// (mayReplace); it reports whether one was removed.
func (r *EventRegistry) Remove(id string, by lua.Origin, byPath string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ev, list := range r.handlers {
		for i, hd := range list {
			if hd.ID != id {
				continue
			}
			if err := mayReplace(r.owner, fmt.Sprintf("event handler %q", id), hd.Origin, hd.Path, by, byPath); err != nil {
				return false, err
			}
			r.handlers[ev] = append(list[:i:i], list[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// drop removes one handler by identity and reports whether it was still
// registered: a once handler runs only for the caller that removed it, since
// overlapping emits each hold a copy of the list.
func (r *EventRegistry) drop(h *EventHandler) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.handlers[h.Event]
	for i, hd := range list {
		if hd == h {
			r.handlers[h.Event] = append(list[:i:i], list[i+1:]...)
			return true
		}
	}
	return false
}

func (r *EventRegistry) Purge() {
	r.mu.Lock()
	r.handlers = map[string][]*EventHandler{}
	r.mu.Unlock()
}
