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
	Focus  string // the layout node id it works in while focused; "" everywhere
	RHS    RHS
	Desc   string
	Origin lua.Origin
	Path   string
	Gen    Gen
	order  int // registration order, which /help and zemi.keymap.list keep
}

type bindKey struct{ focus, key string }

// mayReplace refuses a registration or removal by a less trusted origin
// than the holder's, and one plugin's over another's: both rank as plugin,
// so the later would silently replace or drop the first. owner is
// Host.PluginOf; a bare registry (tests) has none and checks the rank only.
func mayReplace(owner func(string) string, what string, old lua.Origin, oldPath string, by lua.Origin, byPath string) error {
	switch {
	case old.Rank() > by.Rank():
		return fmt.Errorf("%s is registered by %s config", what, old)
	case owner != nil && old == lua.OriginPlugin && by == lua.OriginPlugin && owner(oldPath) != owner(byPath):
		return fmt.Errorf("%s is registered by plugin %s, not %s", what, owner(oldPath), owner(byPath))
	}
	return nil
}

// KeymapRegistry stores one binding per key and focus with origin
// precedence (user > builtin > plugin > project) and last-writer-wins
// within one origin (DESIGN §6.2).
type KeymapRegistry struct {
	mu       sync.RWMutex
	bindings map[bindKey]*KeyBinding
	next     int
	owner    func(path string) string // Host.PluginOf, see mayReplace
}

func NewKeymapRegistry() *KeymapRegistry {
	return &KeymapRegistry{bindings: map[bindKey]*KeyBinding{}}
}

// Set binds or rebinds a key; a key bound by a more trusted origin is
// refused. A focused binding sits over the global ones, so it may not take
// a key or sequence a more trusted global binding holds.
func (r *KeymapRegistry) Set(b *KeyBinding) error {
	if b.Key == "" {
		return errors.New("keymap: key is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.bindings[bindKey{b.Focus, b.Key}]; old != nil {
		if err := mayReplace(r.owner, "keymap "+b.Key, old.Origin, old.Path, b.Origin, b.Path); err != nil {
			return err
		}
	}
	// a key fires before the longer sequences it starts, so overlapping a
	// more trusted sequence would take it over (or be dead)
	for k, old := range r.bindings {
		related := k.focus == b.Focus || k.focus == "" || b.Focus == ""
		overlap := strings.HasPrefix(k.key, b.Key+" ") || strings.HasPrefix(b.Key, k.key+" ") || k.key == b.Key && k.focus == "" && b.Focus != ""
		if !related || !overlap {
			continue
		}
		if err := mayReplace(r.owner, "keymap "+b.Key+" overlaps "+k.key+", which", old.Origin, old.Path, b.Origin, b.Path); err != nil {
			return err
		}
	}
	if old := r.bindings[bindKey{b.Focus, b.Key}]; old != nil {
		b.order = old.order // a rebind keeps its place
	} else {
		r.next++
		b.order = r.next
	}
	r.bindings[bindKey{b.Focus, b.Key}] = b
	return nil
}

// Del unbinds a key the caller may replace (mayReplace) and reports whether
// there was one. Unbinding what is not bound is not an error: a reloaded
// config must be idempotent.
func (r *KeymapRegistry) Del(by lua.Origin, byPath, focus, key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := bindKey{focus, key}
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

// Lookup finds the binding for a key while focus has the keys: the focused
// node's own, unless a more trusted global binding holds the key.
func (r *KeymapRegistry) Lookup(focus, key string) *KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g := r.bindings[bindKey{"", key}]
	if s := r.bindings[bindKey{focus, key}]; focus != "" && s != nil && (g == nil || s.Origin.Rank() >= g.Origin.Rank()) {
		return s
	}
	return g
}

// IsPrefix reports whether seq begins a longer key sequence bound globally
// or for focus.
func (r *KeymapRegistry) IsPrefix(focus, seq string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k := range r.bindings {
		if (k.focus == "" || k.focus == focus) && strings.HasPrefix(k.key, seq+" ") {
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
	GoFn     func(h *Host, args string) error
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

// Widget is a UI region component registered from Lua.
type Widget struct {
	ID       string
	Region   string
	Priority int
	Render   *glua.LFunction
	OnClick  *glua.LFunction
	Wrap     bool
	Origin   lua.Origin
	Path     string
	Gen      Gen
	// the last render's line tables and their segments, see tableSegments;
	// spare is the map before it, cleared and reused so a frame allocates none
	lines, spare map[*glua.LTable][]Segment
}

// WidgetRegistry stores widgets with cross-origin id collision rules
// (§11): the lower-trust origin loses, last-writer-wins within one origin.
type WidgetRegistry struct {
	mu      sync.RWMutex
	widgets map[string]*Widget
	owner   func(path string) string // Host.PluginOf, see mayReplace
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
		if err := mayReplace(r.owner, fmt.Sprintf("widget %q", w.ID), old.Origin, old.Path, w.Origin, w.Path); err != nil {
			return old, err
		}
	}
	r.widgets[w.ID] = w
	return w, nil
}

// Remove drops a widget the caller may replace (mayReplace); it reports
// whether one was removed.
func (r *WidgetRegistry) Remove(id string, by lua.Origin, byPath string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.widgets[id]
	if !ok {
		return false, nil
	}
	if err := mayReplace(r.owner, fmt.Sprintf("widget %q", id), w.Origin, w.Path, by, byPath); err != nil {
		return false, err
	}
	delete(r.widgets, id)
	return true, nil
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
	owner    func(path string) string // Host.PluginOf, see mayReplace
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
	return append([]*EventHandler(nil), r.handlers[event]...)
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

// HLSpec is one highlight group definition from a theme. Unset fields
// matter to the hover groups, which draw over the text's own style: nil
// attributes and empty colors keep what is under them.
type HLSpec struct {
	Link         string // another group this one draws as; of the other fields only Bold and Italic apply
	Fg, Bg, Sp   string // hex, a palette name, or "NONE" (no color, clearing one under a hover)
	Bold, Italic *bool
	Reverse      *bool
	Underline    Underline
	Brighten     float64 // hover groups: mix the drawn fg this far toward white, negative toward black
	Replace      bool    // hover groups: draw as this group alone, not over the text's style
}

// Underline is a highlight group's underline style; "" keeps what is under
// it and UnderlineNone removes one.
type Underline string

const (
	UnderlineNone   Underline = "none"
	UnderlineSingle Underline = "single"
	UnderlineDouble Underline = "double"
	UnderlineCurl   Underline = "curl"
	UnderlineDotted Underline = "dotted"
	UnderlineDashed Underline = "dashed"
)

var underlines = []Underline{UnderlineNone, UnderlineSingle, UnderlineDouble, UnderlineCurl, UnderlineDotted, UnderlineDashed}

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
	overrides map[string]HLSpec        // zemi.ui.hl.set: groups set on top of any theme
	owner     func(path string) string // Host.PluginOf, see mayReplace
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

// maxHLLinks bounds a chain of linked groups, so a cycle resolves to nothing.
const maxHLLinks = 8

// Group resolves a highlight group, following links: an override first,
// then the theme's (t may be nil). The first link's bold and italic apply
// over the group it draws as.
func (r *ThemeRegistry) Group(t *Theme, group string) (HLSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var over HLSpec
	for i := range maxHLLinks {
		s, ok := r.overrides[group]
		if !ok && t != nil {
			s, ok = t.HL[group]
		}
		if !ok || s.Link == "" {
			s.Bold, s.Italic = cmp.Or(over.Bold, s.Bold), cmp.Or(over.Italic, s.Italic)
			return s, ok
		}
		if i == 0 {
			over = s
		}
		group = s.Link
	}
	return HLSpec{}, false
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
		if err := mayReplace(r.owner, fmt.Sprintf("theme %q", t.Name), old.Origin, old.Path, t.Origin, t.Path); err != nil {
			return old, err
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

// Palettes merges every registered theme's palette: a zemi.ui.hl override
// outlives the theme that is active when it is set.
func (r *ThemeRegistry) Palettes() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]string{}
	for _, t := range r.themes {
		maps.Copy(out, t.Palette)
	}
	return out
}
