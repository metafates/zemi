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

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// Gen identifies a runtime generation; registrations are tagged with the
// generation that created them so a reload can purge the dead ones.
type Gen int64

// RHS is what a keymap binding executes.
type RHS struct {
	Fn      *glua.LFunction // Lua callback (interpreter goroutine only)
	Command string          // registered command name
	API     string          // dotted host-API call (zemi.*)
}

// KeyBinding maps one key to a right-hand side.
type KeyBinding struct {
	Key    string // bubbletea key string: "ctrl+s", "tab", "alt+enter"
	RHS    RHS
	Desc   string
	Origin lua.Origin
	Path   string
	Gen    Gen
}

// KeymapRegistry stores one binding per key with origin precedence
// (user > builtin > plugin > project) and last-writer-wins within one
// origin (DESIGN §6.2).
type KeymapRegistry struct {
	mu       sync.RWMutex
	bindings map[string]*KeyBinding
}

func NewKeymapRegistry() *KeymapRegistry {
	return &KeymapRegistry{bindings: map[string]*KeyBinding{}}
}

// Set binds or rebinds a key; a key bound by a more trusted origin is
// refused.
func (r *KeymapRegistry) Set(b *KeyBinding) error {
	if b.Key == "" {
		return errors.New("keymap: key is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.bindings[b.Key]; old != nil && old.Origin.Rank() > b.Origin.Rank() {
		return fmt.Errorf("keymap: %s is already bound by a %s registration", b.Key, old.Origin)
	}
	r.bindings[b.Key] = b
	return nil
}

// Del unbinds a key set by the same or a less trusted origin, so user
// config can remove shipped defaults but a plugin cannot remove user keys.
func (r *KeymapRegistry) Del(origin lua.Origin, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch b := r.bindings[key]; {
	case b == nil:
		return fmt.Errorf("keymap: %s is not bound", key)
	case b.Origin.Rank() > origin.Rank():
		return fmt.Errorf("keymap: %s is bound by a %s registration", key, b.Origin)
	}
	delete(r.bindings, key)
	return nil
}

// Lookup finds the binding for a key.
func (r *KeymapRegistry) Lookup(key string) *KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bindings[key]
}

// IsPrefix reports whether seq begins a longer bound key sequence.
func (r *KeymapRegistry) IsPrefix(seq string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k := range r.bindings {
		if strings.HasPrefix(k, seq+" ") {
			return true
		}
	}
	return false
}

// List returns all bindings sorted by key.
func (r *KeymapRegistry) List() []*KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.bindings), func(a, b *KeyBinding) int { return cmp.Compare(a.Key, b.Key) })
}

// Purge drops every binding (replay re-registers on reload).
func (r *KeymapRegistry) Purge() {
	r.mu.Lock()
	r.bindings = map[string]*KeyBinding{}
	r.mu.Unlock()
}

// Command is a slash command.
type Command struct {
	Name     string
	Desc     string
	NArgs    string // "", "?", "+", "*"
	Complete *glua.LFunction
	Fn       *glua.LFunction
	GoFn     func(h *Host, args string) error
	// GoComplete offers argument candidates for Go-owned commands (lead is
	// the argument under the cursor, line the text up to it).
	GoComplete func(h *Host, lead, line string) []string
	// Hidden commands run when typed but stay out of the palette and help.
	Hidden bool
	Origin lua.Origin
	Path   string
	Gen    Gen
}

// CommandRegistry stores commands. Names collide case-insensitively: a
// case-variant of an existing name is refused.
type CommandRegistry struct {
	mu       sync.RWMutex
	commands map[string]*Command // canonical name
}

func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{commands: map[string]*Command{}}
}

func canonical(name string) string { return strings.ToLower(name) }

func (r *CommandRegistry) Add(c *Command) (*Command, error) {
	if c.Name == "" {
		return nil, errors.New("command: name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := canonical(c.Name)
	if old, exists := r.commands[key]; exists {
		if old.Origin.Rank() > c.Origin.Rank() {
			return old, fmt.Errorf("command /%s is already registered by a %s registration", c.Name, old.Origin)
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

// Remove drops a command, refusing one registered by a higher-trust origin
// than the caller; it reports whether one was removed.
func (r *CommandRegistry) Remove(name string, caller lua.Origin) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.commands[canonical(name)]
	if !ok {
		return false, nil
	}
	if c.Origin.Rank() > caller.Rank() {
		return false, fmt.Errorf("command /%s belongs to %s config", c.Name, c.Origin)
	}
	delete(r.commands, canonical(name))
	return true, nil
}

func (r *CommandRegistry) Purge() {
	r.mu.Lock()
	r.commands = map[string]*Command{}
	r.mu.Unlock()
}

// Widget is a UI region component registered from Lua.
type Widget struct {
	ID       string
	Region   string
	Priority int
	Render   *glua.LFunction
	Wrap     bool
	Origin   lua.Origin
	Path     string
	Gen      Gen
}

// WidgetRegistry stores widgets with cross-origin id collision rules
// (§11): the lower-trust origin loses, last-writer-wins within one origin.
type WidgetRegistry struct {
	mu      sync.RWMutex
	widgets map[string]*Widget
}

func NewWidgetRegistry() *WidgetRegistry {
	return &WidgetRegistry{widgets: map[string]*Widget{}}
}

func (r *WidgetRegistry) Add(w *Widget) (*Widget, error) {
	if w.ID == "" || w.Region == "" || w.Render == nil {
		return nil, errors.New("widget: id, region and render are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, exists := r.widgets[w.ID]; exists {
		if old.Origin.Rank() > w.Origin.Rank() {
			return old, fmt.Errorf("widget %q is already registered by a %s registration", w.ID, old.Origin)
		}
	}
	r.widgets[w.ID] = w
	return w, nil
}

func (r *WidgetRegistry) Get(id string) *Widget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.widgets[id]
}

// InRegion returns the region's widgets ordered by priority ascending, then
// id: widgets live in a map, so equal priorities need a total order or the
// statusline reshuffles between frames.
func (r *WidgetRegistry) InRegion(region string) []*Widget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Widget
	for _, w := range r.widgets {
		if w.Region == region {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b *Widget) int {
		return cmp.Or(cmp.Compare(a.Priority, b.Priority), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func (r *WidgetRegistry) List() []*Widget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.widgets), func(a, b *Widget) int { return cmp.Compare(a.ID, b.ID) })
}

func (r *WidgetRegistry) Purge() {
	r.mu.Lock()
	r.widgets = map[string]*Widget{}
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

// EventRegistry stores event subscriptions. Handler ids collide across
// origins with the lower-trust origin losing (§11).
type EventRegistry struct {
	mu       sync.RWMutex
	handlers map[string][]*EventHandler // event -> handlers in registration order
	nextID   int
}

func NewEventRegistry() *EventRegistry {
	return &EventRegistry{handlers: map[string][]*EventHandler{}}
}

func (r *EventRegistry) Add(h *EventHandler) (*EventHandler, error) {
	if h.Event == "" || h.Fn == nil {
		return nil, errors.New("event: event and handler are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ID != "" {
		for _, existing := range r.handlers[h.Event] {
			if existing.ID == h.ID {
				if existing.Origin.Rank() > h.Origin.Rank() {
					return existing, fmt.Errorf("event handler id %q on %s is already registered by a %s registration",
						h.ID, h.Event, existing.Origin)
				}
				// replace within trust level
				r.replaceLocked(h.Event, existing, h)
				return h, nil
			}
		}
	} else {
		r.nextID++
		h.ID = fmt.Sprintf("handler.%d", r.nextID)
	}
	r.handlers[h.Event] = append(r.handlers[h.Event], h)
	return h, nil
}

func (r *EventRegistry) replaceLocked(event string, old, new *EventHandler) {
	list := r.handlers[event]
	for i, h := range list {
		if h == old {
			list[i] = new
			return
		}
	}
}

// Handlers returns the subscription list for an event.
func (r *EventRegistry) Handlers(event string) []*EventHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*EventHandler(nil), r.handlers[event]...)
}

// Remove drops the handler with id, refusing one registered by a
// higher-trust origin than the caller; it reports whether one was removed.
func (r *EventRegistry) Remove(id string, caller lua.Origin) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ev, list := range r.handlers {
		for i, hd := range list {
			if hd.ID != id {
				continue
			}
			if hd.Origin.Rank() > caller.Rank() {
				return false, fmt.Errorf("event handler %q belongs to %s config", id, hd.Origin)
			}
			r.handlers[ev] = append(list[:i:i], list[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// drop removes one handler by identity (once handlers).
func (r *EventRegistry) drop(h *EventHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.handlers[h.Event]
	for i, hd := range list {
		if hd == h {
			r.handlers[h.Event] = append(list[:i:i], list[i+1:]...)
			return
		}
	}
}

func (r *EventRegistry) Purge() {
	r.mu.Lock()
	r.handlers = map[string][]*EventHandler{}
	r.mu.Unlock()
}

// HLSpec is one highlight group definition from a theme.
type HLSpec struct {
	Fg        string
	Bg        string
	Bold      bool
	Italic    bool
	Underline bool
	Reverse   bool
}

// Theme is a palette + highlight group set registered from Lua.
type Theme struct {
	Name    string
	Palette map[string]string
	HL      map[string]HLSpec
	Origin  lua.Origin
	Path    string
	Gen     Gen
}

// ThemeRegistry stores themes by name; same rules as widgets.
type ThemeRegistry struct {
	mu        sync.RWMutex
	themes    map[string]*Theme
	overrides map[string]HLSpec // zemi.ui.hl: groups set on top of any theme
}

// SetHL overrides one highlight group on top of every theme.
func (r *ThemeRegistry) SetHL(group string, spec HLSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overrides == nil {
		r.overrides = map[string]HLSpec{}
	}
	r.overrides[group] = spec
}

// Group resolves a highlight group: an override first, then the theme's.
func (r *ThemeRegistry) Group(t *Theme, group string) (HLSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.overrides[group]; ok {
		return s, true
	}
	s, ok := t.HL[group]
	return s, ok
}

func NewThemeRegistry() *ThemeRegistry {
	return &ThemeRegistry{themes: map[string]*Theme{}}
}

func (r *ThemeRegistry) Add(t *Theme) (*Theme, error) {
	if t.Name == "" {
		return nil, errors.New("theme: name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, exists := r.themes[t.Name]; exists {
		if old.Origin.Rank() > t.Origin.Rank() {
			return old, fmt.Errorf("theme %q is already registered by a %s registration", t.Name, old.Origin)
		}
	}
	r.themes[t.Name] = t
	return t, nil
}

func (r *ThemeRegistry) Get(name string) *Theme {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.themes[name]
}

func (r *ThemeRegistry) List() []*Theme {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.themes), func(a, b *Theme) int { return cmp.Compare(a.Name, b.Name) })
}

func (r *ThemeRegistry) Purge() {
	r.mu.Lock()
	r.themes, r.overrides = map[string]*Theme{}, nil
	r.mu.Unlock()
}
