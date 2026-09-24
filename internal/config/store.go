package config

import (
	"cmp"
	"slices"
	"strings"
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
		{Path: "models.chat", Kind: "string", Default: "", Desc: "the model the conversation runs on, and every role left empty (provider/model; empty uses the first added provider's default model, else the first usable builtin's)"},
		{Path: "models.agents", Kind: "string", Default: "", Desc: "the model subagents run on when their definition names none (provider/model; empty follows models.chat)"},
		{Path: "models.titles", Kind: "string", Default: "", Desc: "the model that names sessions (provider/model; empty follows models.chat)"},
		{Path: "models.compact", Kind: "string", Default: "", Desc: "the model that writes compaction summaries (provider/model; empty follows models.chat)"},
		{Path: "models.memory", Kind: "string", Default: "", Desc: "the model the reflector reviews sessions for memories on (provider/model; empty follows models.chat)"},
		{Path: "providers.builtin", Kind: "bool", Default: true, Desc: "the builtin providers, each on while its key is exported (/model lists them); false leaves only what init.lua adds"},
		{Path: "permissions.mode", Kind: "string", Default: "auto", Desc: "the permission mode zemi starts in: auto (safe actions run, risky ones ask), yolo (allow everything), or a mode added with zemi.permission.mode.add"},
		{Path: "permissions.env_keep", Kind: "string", Default: "", Desc: "env var names a child process keeps although they look secret-shaped (*KEY*, *TOKEN*, *SECRET*, *PASSWORD*, ...), comma separated, such as an NPM_TOKEN a build needs"},
		{Path: "ui.theme", Kind: "string", Default: "default", Desc: "theme name: default, default-light or one zemi.ui.theme.add registers"},
		{Path: "ui.animations", Kind: "bool", Default: true, Desc: "animate the working indicator (false keeps it static)"},
		{Path: "web.search_url", Kind: "string", Default: "", Desc: "SearXNG search endpoint for the web_search tool, such as http://localhost:8888/search, with the json format enabled (empty uses DuckDuckGo Lite)"},
		{Path: "agent.effort", Kind: "string", Default: "", Desc: "reasoning effort: low, medium, high, or empty for the model's default"},
		{Path: "agent.profile", Kind: "string", Default: "default", Desc: "the profile the main conversation runs as: default, planner, or a name defined with zemi.agent.profile"},
		{Path: "session.auto_title", Kind: "bool", Default: true, Desc: "name new sessions from their first exchange with a short model call"},
		{Path: "agent.context_window", Kind: "int", Default: 0, Desc: "context window in tokens, overriding the model's context_window (0 uses the model's); drives the context percent and auto compaction"},
		{Path: "ui.chat.reasoning", Kind: "bool", Default: false, Desc: "show finished reasoning blocks expanded (ctrl+o toggles for the session)"},
		{Path: "lsp.auto", Kind: "bool", Default: true, Desc: "start installed language servers (gopls, pyright, typescript-language-server, clangd, rust-analyzer) in a trusted project with their marker file"},
		{Path: "editor.command", Kind: "string", Default: "", Desc: "editor for zemi.ui.edit and the composer draft (empty uses $VISUAL, then $EDITOR, then vi)"},
		{Path: "keymap.leader", Kind: "string", Default: "ctrl+x", Desc: "the key <leader> stands for in zemi.keymap.set sequences (set it before binding)"},
		{Path: "ui.mouse", Kind: "bool", Default: true, Desc: "drag to select and copy, click rows (false leaves selection to the terminal)"},
		{Path: "ui.notify", Kind: "bool", Default: true, Desc: "when zemi is not focused, notify through the terminal as a turn ends or a permission prompt waits"},
		{Path: "ui.progress", Kind: "bool", Default: true, Desc: "report a running turn to the terminal with OSC 9;4, which taskbars and tab bars draw as progress"},
		{Path: "ui.composer.multiline", Kind: "bool", Default: true, Desc: "composer accepts embedded newlines (shift+enter, alt+enter, ctrl+j)"},
		{Path: "ui.composer.history", Kind: "int", Default: 50, Desc: "composer history entries kept (0 disables)"},
		{Path: "ui.completion.files", Kind: "bool", Default: true, Desc: "complete file paths after @ in the composer"},
		{Path: "ui.completion.max_items", Kind: "int", Default: 10, Desc: "completion popup rows"},
		{Path: "memory.autonomy", Kind: "string", Default: "auto", Desc: "memories the model or the reflector propose: auto stores policy-clean ones at once, ask queues them for /memory; auto queues too in an untrusted project or a session that used fetch, web_search, the lua tool or a tool zemi does not ship (MCP, zemi.tool)"},
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

// Default is the schema default of path, nil for a key outside the schema.
func (s *Store) Default(path string) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schema[path].Default
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
		var s string
		s, ok = val.(string)
		// a models.* key holds an id, never a role name: roles never nest
		if ok && strings.HasPrefix(def.Path, "models.") && s != "" && !strings.Contains(s, "/") {
			return nil, &ErrBadValue{Path: def.Path, Want: `a provider/model id or ""`}
		}
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
