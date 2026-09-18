package config

import (
	"cmp"
	"slices"
	"sync"
)

// Setting describes one config key.
type Setting struct {
	Path    string
	Kind    string // "string" | "int" | "bool"
	Default any
	Desc    string
}

// Store holds the merged config values and user watches. The schema is
// Go-owned: a key outside it cannot be set.
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
		{Path: "agent.model", Kind: "string", Default: "", Desc: "active model id (provider/model)"},
		{Path: "permissions.mode", Kind: "string", Default: "manual", Desc: "the permission mode zemi starts in: manual (you answer asks), plan, auto (safe actions run, risky ones ask), yolo (allow everything), or a mode added with zemi.permission.mode.add"},
		{Path: "agent.small_model", Kind: "string", Default: "", Desc: "a fast, cheap model for small jobs: session titles and, unless permissions.auto_model is set, auto-mode screening (provider/model; empty uses agent.model)"},
		{Path: "permissions.auto_model", Kind: "string", Default: "", Desc: "model that screens asks in auto mode (provider/model; empty uses agent.small_model, then agent.model)"},
		{Path: "ui.theme", Kind: "string", Default: "default", Desc: "active theme name"},
		{Path: "ui.animations", Kind: "bool", Default: true, Desc: "animate the working indicator (false keeps it static)"},
		{Path: "agent.subagent_model", Kind: "string", Default: "", Desc: "default model for subagents whose definition sets none (provider/model; empty uses the session model); the agent may pick another configured model per run"},
		{Path: "web.search_url", Kind: "string", Default: "", Desc: "SearXNG search endpoint for the web_search tool, such as http://localhost:8888/search, with the json format enabled (empty uses DuckDuckGo Lite)"},
		{Path: "agent.effort", Kind: "string", Default: "", Desc: "reasoning effort: low, medium, high, or empty for the provider default"},
		{Path: "session.auto_title", Kind: "bool", Default: true, Desc: "name new sessions from their first exchange with a short model call (agent.small_model when set)"},
		{Path: "agent.context_window", Kind: "int", Default: 0, Desc: "context window in tokens, overriding the model's context_window (0 uses the model's); drives the context percent and auto compaction"},
		{Path: "ui.chat.reasoning", Kind: "bool", Default: false, Desc: "show finished reasoning blocks expanded (ctrl+o toggles for the session)"},
		{Path: "lsp.auto", Kind: "bool", Default: true, Desc: "start installed language servers (gopls, pyright, typescript-language-server, clangd) for projects with their marker files"},
		{Path: "editor.command", Kind: "string", Default: "", Desc: "editor for open_in_editor, zemi.ui.edit and the composer draft (empty uses $VISUAL, then $EDITOR, then vi)"},
		{Path: "ui.window.unfocus", Kind: "string", Default: "ctrl+\\", Desc: "the key that takes typing back from a focused window to zemi"},
		{Path: "keymap.leader", Kind: "string", Default: "ctrl+g", Desc: "the key <leader> stands for in zemi.keymap.set sequences (set it before binding)"},
		{Path: "ui.mouse", Kind: "bool", Default: true, Desc: "drag to select and copy, click rows (false leaves selection to the terminal)"},
		{Path: "ui.notify", Kind: "bool", Default: true, Desc: "when zemi is not focused, notify through the terminal as a turn ends or a permission prompt waits"},
		{Path: "ui.composer.multiline", Kind: "bool", Default: true, Desc: "composer accepts embedded newlines (alt/shift+enter)"},
		{Path: "ui.composer.history", Kind: "int", Default: 50, Desc: "composer history entries kept (0 disables)"},
		{Path: "ui.completion.files", Kind: "bool", Default: true, Desc: "file completion on the trigger char in the composer"},
		{Path: "ui.completion.max_items", Kind: "int", Default: 10, Desc: "completion popup rows"},
		{Path: "reflector.model", Kind: "string", Default: "", Desc: "model id the reflector runs on (provider/model; empty uses agent.model)"},
		{Path: "compact.model", Kind: "string", Default: "", Desc: "model that writes compaction summaries (provider/model; empty uses agent.model)"},
		{Path: "memory.autonomy", Kind: "string", Default: "ask", Desc: "ask: memories the model or the reflector propose wait for /memory review; auto: policy-clean ones are stored at once, except in an untrusted project or a session that used fetch, web_search, the Lua tool or a tool zemi does not ship (MCP, zemi.tool)"},
		{Path: "memory.digest_lines", Kind: "int", Default: 12, Desc: "memory digest lines in the system prompt (0 leaves it out)"},
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
	slices.SortFunc(out, func(a, b Setting) int { return cmp.Compare(a.Path, b.Path) })
	return out
}

// Get looks up a dotted path over the merged values.
func (s *Store) Get(path string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[path]
	return v, ok
}

// Str, Bool and Int read a schema key in its kind. Every schema key holds
// its default until someone sets it, so only a key outside the schema reads
// as the zero value.
func (s *Store) Str(path string) string { return s.value[string](path) }
func (s *Store) Bool(path string) bool  { return s.value[bool](path) }
func (s *Store) Int(path string) int    { return s.value[int](path) }

func (s *Store) value[T any](path string) T {
	v, _ := s.Get(path)
	t, _ := v.(T)
	return t
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

// SetHost writes a schema key from Go code without firing watches.
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

// AddWatch registers a change observer and returns its handle id. A path
// outside the schema is refused: watchers match by exact path, so a typo
// would sit there and never fire.
func (s *Store) AddWatch(w Watcher) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schema[w.Path]; !ok {
		return 0, &ErrUnknownKey{Path: w.Path}
	}
	s.watchSeq++
	w.ID = s.watchSeq
	s.watchers = append(s.watchers, w)
	return w.ID, nil
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
