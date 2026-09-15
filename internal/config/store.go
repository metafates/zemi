// Package config: runtime settings with a Go-owned schema. Only keys the
// schema marks user-writable can be set from Lua; the rest (autonomy knobs
// among them) only from Go.
package config

import (
	"sort"
	"sync"
)

// Setting describes one config key.
type Setting struct {
	Path         string
	Kind         string // "string" | "int" | "bool"
	Default      any
	UserWritable bool
	Desc         string
}

// Store holds the merged config values and user watches.
type Store struct {
	mu       sync.RWMutex
	values   map[string]any
	schema   map[string]Setting
	watchers []Watcher
	watchSeq int
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
		{Path: "permissions.mode", Kind: "string", Default: "manual", UserWritable: true, Desc: "the permission mode zemi starts in: manual (you answer asks), plan, auto (safe actions run, risky ones ask), yolo (allow everything), or a mode added with zemi.permission.mode.add"},
		{Path: "agent.small_model", Kind: "string", Default: "", UserWritable: true, Desc: "a fast, cheap model for small jobs: session titles and, unless permissions.auto_model is set, auto-mode screening (provider/model; empty uses agent.model)"},
		{Path: "permissions.auto_model", Kind: "string", Default: "", UserWritable: true, Desc: "model that screens asks in auto mode (provider/model; empty uses agent.small_model, then agent.model)"},
		{Path: "ui.theme", Kind: "string", Default: "default", UserWritable: true, Desc: "active theme name"},
		{Path: "ui.animations", Kind: "bool", Default: true, UserWritable: true, Desc: "animate the working indicator (false keeps it static)"},
		{Path: "agent.effort", Kind: "string", Default: "", UserWritable: true, Desc: "reasoning effort: low, medium, high, or empty for the provider default"},
		{Path: "session.auto_title", Kind: "bool", Default: true, UserWritable: true, Desc: "name new sessions from their first exchange with a short model call (agent.small_model when set)"},
		{Path: "agent.context_window", Kind: "int", Default: 0, UserWritable: true, Desc: "context window in tokens, overriding the model's context_window (0 uses the model's); drives the context percent and auto compaction"},
		{Path: "ui.chat.reasoning", Kind: "bool", Default: false, UserWritable: true, Desc: "show finished reasoning blocks expanded (ctrl+o toggles for the session)"},
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
		{Path: "memory.autonomy", Kind: "string", Default: "ask", Desc: "memory autonomy mode"},
		{Path: "memory.digest_lines", Kind: "int", Default: 12, UserWritable: true, Desc: "memory digest lines in the system prompt"},
		{Path: "config.autonomy", Kind: "string", Default: "ask", Desc: "config revision autonomy mode"},
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
	val, err := checkKind(def, val)
	if err != nil {
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

// SetHost writes any schema key from Go code, bypassing the Lua writability
// gate but still validating kind.
func (s *Store) SetHost(path string, val any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.schema[path]
	if !ok {
		return &ErrUnknownKey{Path: path}
	}
	val, err := checkKind(def, val)
	if err != nil {
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

// checkKind validates val and returns it in stored form: ints as int, so
// readers can assert int whether Lua (float64) or Go handed it in.
func checkKind(def Setting, val any) (any, error) {
	ok := true
	switch def.Kind {
	case "string":
		_, ok = val.(string)
	case "int":
		switch v := val.(type) {
		case int:
		case int64:
			val = int(v)
		case float64:
			ok = v == float64(int64(v))
			val = int(v)
		default:
			ok = false
		}
	case "bool":
		_, ok = val.(bool)
	}
	if !ok {
		return nil, &ErrBadValue{Path: def.Path, Want: def.Kind}
	}
	return val, nil
}
