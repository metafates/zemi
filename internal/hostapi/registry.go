// Package hostapi implements the zemi.* namespaces (thin adapters over
// core services, DESIGN §3) and the registration registries shared with
// the UI (keymaps, commands, widgets, event handlers, themes).
package hostapi

import (
	"fmt"
	"sort"
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
	Fn       *glua.LFunction // Lua callback (interpreter goroutine only)
	Command  string          // registered command name
	API      string          // dotted host-API call (zemi.*)
}

// KeyBinding maps a key sequence in a mode to a right-hand side.
type KeyBinding struct {
	Mode   string
	Keys   string // normalized sequence, e.g. "gM", "ctrl+s"
	RHS    RHS
	Desc   string
	Origin lua.Origin
	Path   string
	Gen    Gen
}

// KeymapRegistry stores bindings per mode with origin precedence
// (user > builtin > plugin > project) and last-writer-wins within one
// origin (DESIGN §6.2).
type KeymapRegistry struct {
	mu       sync.RWMutex
	bindings map[string]map[string]*KeyBinding // mode -> keys -> binding
}

func NewKeymapRegistry() *KeymapRegistry {
	return &KeymapRegistry{bindings: map[string]map[string]*KeyBinding{}}
}

// Bind registers or replaces a binding. Returns the effective binding and
// whether an existing higher-trust binding refused this one.
func (r *KeymapRegistry) Bind(b *KeyBinding) (*KeyBinding, error) {
	if b.Mode == "" || b.Keys == "" {
		return nil, fmt.Errorf("keymap: mode and keys are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	mode, ok := r.bindings[b.Mode]
	if !ok {
		mode = map[string]*KeyBinding{}
		r.bindings[b.Mode] = mode
	}
	if old, exists := mode[b.Keys]; exists {
		if old.Origin.Rank() > b.Origin.Rank() {
			return old, fmt.Errorf("keymap: %s in %s mode is already bound by a %s registration",
				b.Keys, b.Mode, old.Origin)
		}
	}
	mode[b.Keys] = b
	return b, nil
}

// Del removes a binding registered by the same origin only.
func (r *KeymapRegistry) Del(origin lua.Origin, mode, keys string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	modeMap, ok := r.bindings[mode]
	if !ok {
		return false
	}
	b, ok := modeMap[keys]
	if !ok || b.Origin != origin {
		return false
	}
	delete(modeMap, keys)
	return true
}

// Lookup finds the binding for an exact key sequence in a mode.
func (r *KeymapRegistry) Lookup(mode, keys string) *KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bindings[mode][keys]
}

// IsPrefix reports whether some binding's key sequence starts with the
// given prefix (for pending multi-key state).
func (r *KeymapRegistry) IsPrefix(mode, prefix string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for keys := range r.bindings[mode] {
		if len(keys) > len(prefix) && strings.HasPrefix(keys, prefix) {
			return true
		}
	}
	return false
}

// List returns all bindings grouped by mode, sorted.
func (r *KeymapRegistry) List() []*KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*KeyBinding
	for _, m := range r.bindings {
		for _, b := range m {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mode != out[j].Mode {
			return out[i].Mode < out[j].Mode
		}
		return out[i].Keys < out[j].Keys
	})
	return out
}

// Purge drops every binding (replay re-registers on reload).
func (r *KeymapRegistry) Purge() {
	r.mu.Lock()
	r.bindings = map[string]map[string]*KeyBinding{}
	r.mu.Unlock()
}

// Command is a :cmdline command.
type Command struct {
	Name      string
	Desc      string
	NArgs     string // "", "?", "+", "*"
	Complete  *glua.LFunction
	Fn        *glua.LFunction
	GoFn      func(h *Host, args string) error
	// GoComplete offers argument candidates for Go-owned commands (lead is
	// the argument under the cursor, line the text up to it).
	GoComplete func(h *Host, lead, line string) []string
	Origin    lua.Origin
	Path      string
	Gen       Gen
}

// CommandRegistry stores commands. Names collide case-insensitively across
// the registry (§11): a case-variant of an existing name is refused so
// :reload/:RELOAD cannot shadow the reserved :Reload.
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
		return nil, fmt.Errorf("command: name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := canonical(c.Name)
	if old, exists := r.commands[key]; exists {
		if old.Origin.Rank() > c.Origin.Rank() {
			return old, fmt.Errorf("command :%s is already registered by a %s registration", c.Name, old.Origin)
		}
		if old.Name != c.Name {
			return old, fmt.Errorf("command :%s collides with :%s (case-insensitive)", c.Name, old.Name)
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
	out := make([]*Command, 0, len(r.commands))
	for _, c := range r.commands {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
	Keys     *glua.LFunction
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
		return nil, fmt.Errorf("widget: id, region and render are required")
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (r *WidgetRegistry) List() []*Widget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Widget, 0, len(r.widgets))
	for _, w := range r.widgets {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *WidgetRegistry) Purge() {
	r.mu.Lock()
	r.widgets = map[string]*Widget{}
	r.mu.Unlock()
}

// EventHandler is a Lua callback subscribed to an event.
type EventHandler struct {
	ID       string
	Event    string
	Fn       *glua.LFunction
	Origin   lua.Origin
	Path     string
	Gen      Gen
	Coalesce bool
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
		return nil, fmt.Errorf("event: event and handler are required")
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
	mu     sync.RWMutex
	themes map[string]*Theme
}

func NewThemeRegistry() *ThemeRegistry {
	return &ThemeRegistry{themes: map[string]*Theme{}}
}

func (r *ThemeRegistry) Add(t *Theme) (*Theme, error) {
	if t.Name == "" {
		return nil, fmt.Errorf("theme: name is required")
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
	out := make([]*Theme, 0, len(r.themes))
	for _, t := range r.themes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *ThemeRegistry) Purge() {
	r.mu.Lock()
	r.themes = map[string]*Theme{}
	r.mu.Unlock()
}
