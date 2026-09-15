// Package config: runtime settings with a Go-owned schema. Only keys the
// schema marks user-writable can be set from Lua, and only during boot from
// user-origin code (the window closes when app.ready is emitted).
package config

import (
	"sort"
	"sync"
)

// Setting describes one config key.
type Setting struct {
	Path         string
	Kind         string // "string" | "int" | "bool" | "float"
	Default      any
	UserWritable bool
	AutonomyKnob bool // refused by zemi.config.set from every origin after boot
	Desc         string
}

// Store holds the merged config values and user watches.
type Store struct {
	mu       sync.RWMutex
	values   map[string]any
	schema   map[string]Setting
	watchers []Watcher
	watchSeq int
	ready    bool // boot window closed when true
}

// WatchFired is one watch that a SetQuiet would have notified.
type WatchFired struct {
	ID       int
	Old, New any
}

// Watcher is a registered change observer.
type Watcher struct {
	ID   int // assigned by AddWatch
	Path string
	Fn   func(old, new any)
}

func NewStore() *Store {
	s := &Store{
		values: map[string]any{},
		schema: map[string]Setting{},
	}
	s.defineDefaults()
	return s
}

func (s *Store) defineDefaults() {
	defaults := []Setting{
		{Path: "agent.model", Kind: "string", Default: "", UserWritable: true, Desc: "active model id (provider/model)"},
		{Path: "permissions.mode", Kind: "string", Default: "manual", UserWritable: true, Desc: "who answers permission asks: manual (you), auto (a model approves routine actions, you get the rest) or yolo (allow everything)"},
		{Path: "permissions.auto_model", Kind: "string", Default: "", UserWritable: true, Desc: "model that screens asks in auto mode (provider/model; empty uses agent.model)"},
		{Path: "providers.crush_json", Kind: "string", Default: "~/.config/crush/crush.json", UserWritable: true, Desc: "crush.json imported at boot for providers (empty disables)"},
		{Path: "ui.theme", Kind: "string", Default: "default", UserWritable: true, Desc: "active theme name"},
		{Path: "ui.animations", Kind: "bool", Default: true, UserWritable: true, Desc: "animate the working indicator (false keeps it static)"},
		{Path: "agent.effort", Kind: "string", Default: "", UserWritable: true, Desc: "reasoning effort: low, medium, high, or empty for the provider default"},
		{Path: "session.auto_title", Kind: "bool", Default: true, UserWritable: true, Desc: "name new sessions from their first exchange with a short model call (reflector.model when set)"},
		{Path: "lsp.auto", Kind: "bool", Default: true, UserWritable: true, Desc: "start installed language servers (gopls, pyright, rust-analyzer, typescript-language-server, clangd) for projects with their marker files"},
		{Path: "keymap.leader", Kind: "string", Default: "ctrl+g", UserWritable: true, Desc: "the key <leader> stands for in zemi.keymap.set sequences (set it before binding)"},
		{Path: "ui.mouse", Kind: "bool", Default: true, UserWritable: true, Desc: "drag to select and copy, click buttons and rows (false leaves selection to the terminal)"},
		{Path: "ui.notify", Kind: "bool", Default: true, UserWritable: true, Desc: "when zemi is not focused, notify through the terminal as a turn ends or a permission prompt waits"},
		{Path: "ui.composer.multiline", Kind: "bool", Default: true, UserWritable: true, Desc: "composer accepts embedded newlines (alt/shift+enter)"},
		{Path: "ui.composer.history", Kind: "int", Default: 50, UserWritable: true, Desc: "composer history entries kept (0 disables)"},
		{Path: "ui.completion.files", Kind: "bool", Default: true, UserWritable: true, Desc: "file completion on the trigger char in the composer"},
		{Path: "ui.completion.trigger", Kind: "string", Default: "@", UserWritable: true, Desc: "composer completion trigger character"},
		{Path: "ui.completion.max_items", Kind: "int", Default: 10, UserWritable: true, Desc: "completion popup rows"},
		{Path: "reflector.every_n_sessions", Kind: "int", Default: 1, Desc: "run the reflector every N sessions"},
		{Path: "reflector.model", Kind: "string", Default: "", UserWritable: true, Desc: "model id the reflector runs on (provider/model; empty uses agent.model)"},
		{Path: "reflector.timeout_ms", Kind: "int", Default: 120000, Desc: "reflector run timeout in milliseconds"},
		{Path: "memory.autonomy", Kind: "string", Default: "ask", AutonomyKnob: true, Desc: "memory autonomy mode"},
		{Path: "memory.digest_lines", Kind: "int", Default: 12, UserWritable: true, Desc: "memory digest lines in the system prompt"},
		{Path: "skills.autonomy", Kind: "string", Default: "ask", AutonomyKnob: true, Desc: "skills autonomy mode"},
		{Path: "config.autonomy", Kind: "string", Default: "ask", AutonomyKnob: true, Desc: "config revision autonomy mode"},
	}
	for _, def := range defaults {
		s.schema[def.Path] = def
		s.values[def.Path] = def.Default
	}
}

// Schema returns the sorted setting list (for introspection).
func (s *Store) Schema() []Setting {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Setting, 0, len(s.schema))
	for _, v := range s.schema {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Get looks up a dotted path over the merged values.
func (s *Store) Get(path string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[path]
	return v, ok
}

// Set applies a runtime override and synchronously notifies watchers. Only
// safe to call from goroutines that may block on the interpreter; the
// interpreter itself must use SetQuiet and deliver the returned watches
// inline (a blocking RT.Do from inside a running request deadlocks).
func (s *Store) Set(path string, val any) error {
	fired, err := s.SetQuiet(path, val)
	if err != nil {
		return err
	}
	s.mu.RLock()
	fns := make(map[int]func(any, any), len(fired))
	for _, w := range s.watchers {
		fns[w.ID] = w.Fn
	}
	s.mu.RUnlock()
	for _, f := range fired {
		if fn := fns[f.ID]; fn != nil {
			fn(f.Old, f.New)
		}
	}
	return nil
}

// SetQuiet applies a runtime override without notifying watchers; the
// callers on the interpreter goroutine deliver the fired watches themselves
// so a Lua watch callback never re-enters the request queue.
func (s *Store) SetQuiet(path string, val any) ([]WatchFired, error) {
	s.mu.Lock()
	def, ok := s.schema[path]
	if !ok {
		s.mu.Unlock()
		return nil, &ErrUnknownKey{Path: path}
	}
	if !def.UserWritable {
		s.mu.Unlock()
		return nil, &ErrNotWritable{Path: path}
	}
	if err := checkKind(def, val); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	old := s.values[path]
	s.values[path] = val
	var fired []WatchFired
	for _, w := range s.watchers {
		if w.Path == path {
			fired = append(fired, WatchFired{ID: w.ID, Old: old, New: val})
		}
	}
	s.mu.Unlock()
	return fired, nil
}

// SetHost writes any schema key from Go code (settings UI, host internals),
// bypassing the Lua writability gate but still validating kind. Autonomy
// knobs are host-owned: this is their only write path after boot.
func (s *Store) SetHost(path string, val any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.schema[path]
	if !ok {
		return &ErrUnknownKey{Path: path}
	}
	if err := checkKind(def, val); err != nil {
		return err
	}
	s.values[path] = val
	return nil
}

// AddWatch registers a change observer and returns its handle id.
func (s *Store) AddWatch(w Watcher) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchSeq++
	w.ID = s.watchSeq
	s.watchers = append(s.watchers, w)
	return w.ID
}

// RemoveWatch drops one watcher by id; returns false if it was gone.
func (s *Store) RemoveWatch(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.watchers {
		if w.ID == id {
			s.watchers = append(s.watchers[:i], s.watchers[i+1:]...)
			return true
		}
	}
	return false
}

// CloseBootWindow marks the boot load complete: autonomy knobs become
// unwritable and config.set is only legal through host API origin checks
// (which also require user origin).
func (s *Store) CloseBootWindow() {
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
}

// BootWindowClosed reports whether app.ready has been emitted.
func (s *Store) BootWindowClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

func checkKind(def Setting, val any) error {
	switch def.Kind {
	case "string":
		if _, ok := val.(string); !ok {
			return &ErrBadValue{Path: def.Path, Want: def.Kind}
		}
	case "int":
		switch v := val.(type) {
		case int:
		case int64:
		case float64:
			if v != float64(int64(v)) {
				return &ErrBadValue{Path: def.Path, Want: def.Kind}
			}
		default:
			return &ErrBadValue{Path: def.Path, Want: def.Kind}
		}
	case "bool":
		if _, ok := val.(bool); !ok {
			return &ErrBadValue{Path: def.Path, Want: def.Kind}
		}
	case "float":
		switch val.(type) {
		case float64, int, int64:
		default:
			return &ErrBadValue{Path: def.Path, Want: def.Kind}
		}
	}
	return nil
}
