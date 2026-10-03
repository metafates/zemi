package config

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Kind is the type of value a setting holds.
type Kind string

const (
	KindString Kind = "string"
	KindNumber Kind = "number"
	KindBool   Kind = "bool"
)

var Kinds = []Kind{KindString, KindNumber, KindBool}

// Setting describes one config key.
type Setting struct {
	Path    string
	Kind    Kind
	Default any
	Desc    string
	Choices []string // the only values a string key takes; nil takes any
}

// Store holds the merged config values and user watches. The schema is
// Go-owned: a key outside it cannot be set.
type Store struct {
	mu       sync.RWMutex
	values   map[string]any
	schema   map[string]Setting
	watchers []watcher
	watchSeq int
}

// WatchFired is one watch that a SetQuiet would have notified.
type WatchFired struct {
	ID       int
	Old, New any
}

type watcher struct {
	id   int
	path string
}

// The builtin permission modes' one-line descriptions: the mode list
// (hostapi's builtinModes) and the permissions.mode setting's help both read
// them, so a screen and the config name a mode the same way.
const (
	PermModeAutoDesc = "safe actions run on their own, risky ones ask"
	PermModeYoloDesc = "everything runs without asking"
)

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
		{Path: "models.chat", Kind: KindString, Default: "", Desc: "the model the conversation runs on, and every role left empty (provider/model; empty uses the first added provider's default model, else the first usable builtin's)"},
		{Path: "models.agents", Kind: KindString, Default: "", Desc: "the model subagents run on when their definition names none (provider/model; empty follows models.chat)"},
		{Path: "models.titles", Kind: KindString, Default: "", Desc: "the model that names sessions (provider/model; empty follows models.chat)"},
		{Path: "models.compact", Kind: KindString, Default: "", Desc: "the model that writes compaction summaries (provider/model; empty follows models.chat)"},
		{Path: "models.memory", Kind: KindString, Default: "", Desc: "the model the reflector reviews sessions for memories on (provider/model; empty follows models.chat)"},
		{Path: "providers.builtin", Kind: KindBool, Default: true, Desc: "the builtin providers, each on while its key is exported (/model lists them); false leaves only what init.lua adds"},
		{Path: "permissions.mode", Kind: KindString, Default: "auto", Desc: "the permission mode zemi starts in: auto (" + PermModeAutoDesc + "), yolo (" + PermModeYoloDesc + "), or a mode added with zemi.permission.mode.add"},
		{Path: "permissions.env_keep", Kind: KindString, Default: "", Desc: "env var names a child process keeps although they look secret-shaped (*KEY*, *TOKEN*, *SECRET*, *PASSWORD*, ...), comma separated, such as an NPM_TOKEN a build needs"},
		{Path: "ui.theme", Kind: KindString, Default: "whorl", Desc: "theme name (/theme picks one): whorl, catppuccin, tokyonight, gruvbox, nord, dracula, solarized, rose-pine, kanagawa, one, everforest, github, or one zemi.ui.theme.add registers; a <name>-light twin draws on a light terminal"},
		{Path: "ui.animations", Kind: KindBool, Default: true, Desc: "animate the working indicator (false keeps it static)"},
		{Path: "agent.effort", Kind: KindString, Default: "", Desc: "reasoning effort: low, medium, high, or empty for the model's default"},
		{Path: "agent.profile", Kind: KindString, Default: "default", Desc: "the profile the main conversation runs as: default, planner, or a name defined with zemi.agent.profile.add"},
		{Path: "session.auto_title", Kind: KindBool, Default: true, Desc: "name new sessions from their first exchange with a short model call"},
		{Path: "session.recap", Kind: KindBool, Default: true, Desc: "after a turn with tool calls, if there is no input for 3 minutes, add a one-line recap from a short model call on the cached conversation (billed to the session)"},
		{Path: "agent.context_window", Kind: KindNumber, Default: 0.0, Desc: "context window in tokens, overriding the model's context_window (0 uses the model's); drives the context percent and auto compaction"},
		{Path: "agent.max_background", Kind: KindNumber, Default: 4.0, Desc: "background agents and workflows running at once; one more is refused until one ends (1 at least)"},
		{Path: "agent.max_steps", Kind: KindNumber, Default: 200.0, Desc: "steps one turn may take before it stops and hands back, its tool calls included; 0 or less keeps 200, and a sent message continues the turn"},
		{Path: "lsp.auto", Kind: KindBool, Default: true, Desc: "start installed language servers for files with their marker in a trusted project, and lua-language-server for the Lua of your config and local plugins"},
		{Path: "editor.command", Kind: KindString, Default: "", Desc: "editor for zemi.ui.edit and the composer draft (empty uses $VISUAL, then $EDITOR, then vi)"},
		{Path: "keymap.leader", Kind: KindString, Default: "ctrl+x", Desc: "the key <leader> stands for in zemi.keymap.set sequences (set it before binding)"},
		{Path: "ui.mouse", Kind: KindBool, Default: true, Desc: "drag to select and copy, click rows (false leaves selection to the terminal)"},
		{Path: "ui.notify", Kind: KindString, Default: "auto", Choices: []string{"auto", "osc", "native", "bell", "off"}, Desc: "how zemi alerts while its window is unfocused, as a turn ends or a prompt waits: osc (a terminal notification), native (osascript or notify-send), bell, off; auto picks per terminal (/health names it)"},
		{Path: "ui.progress", Kind: KindBool, Default: true, Desc: "report a running turn to the terminal with OSC 9;4, which taskbars and tab bars draw as progress"},
		{Path: "ui.composer.multiline", Kind: KindBool, Default: true, Desc: "composer accepts embedded newlines (shift+enter, alt+enter)"},
		{Path: "ui.composer.history", Kind: KindNumber, Default: 50.0, Desc: "composer history entries kept (0 disables)"},
		{Path: "ui.completion.files", Kind: KindBool, Default: true, Desc: "complete file paths after @ in the composer"},
		{Path: "ui.completion.max_items", Kind: KindNumber, Default: 10.0, Desc: "completion popup rows"},
		{Path: "memory.autonomy", Kind: KindString, Default: "auto", Choices: []string{"auto", "ask"}, Desc: "memories the model or the reflector propose: auto stores policy-clean ones at once, ask queues them for /memory; auto also queues in an untrusted project or after fetch, web_search, lua or a non-builtin tool"},
	}
	for _, def := range defaults {
		s.schema[def.Path] = def
		s.values[def.Path] = def.Default
	}
}

// Add registers a key beside the builtin ones. One registered again with
// the same kind, default and choices keeps its value and takes the new
// Desc; a new or changed one starts at its default, and changed says so.
func (s *Store) Add(def Setting) (changed bool, err error) {
	if def.Default, err = checkKind(def, def.Default); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.schema[def.Path]; ok && old.Kind == def.Kind && old.Default == def.Default && slices.Equal(old.Choices, def.Choices) {
		old.Desc = def.Desc
		s.schema[def.Path] = old
		return false, nil
	}
	s.schema[def.Path] = def
	s.values[def.Path] = def.Default
	return true, nil
}

// Del drops a key Add registered, with its value.
func (s *Store) Del(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.schema, path)
	delete(s.values, path)
}

// Schema returns the sorted setting list (for introspection).
func (s *Store) Schema() []Setting {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.SortedFunc(maps.Values(s.schema), func(a, b Setting) int { return cmp.Compare(a.Path, b.Path) })
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

// Str reads a string schema key; Bool and Int read the other two kinds, Int
// truncating a fraction. Every schema key holds
// its default until someone sets it, so only a key outside the schema reads
// as the zero value, and under go test it panics, so a mistyped key fails
// the suite.
func (s *Store) Str(path string) string { return s.value[string](path) }
func (s *Store) Bool(path string) bool  { return s.value[bool](path) }
func (s *Store) Int(path string) int    { return int(s.value[float64](path)) }

func (s *Store) value[T any](path string) T {
	s.mu.RLock()
	_, known := s.schema[path]
	t, _ := s.values[path].(T)
	s.mu.RUnlock()
	if !known && testing.Testing() {
		panic("config: " + path + " is not a schema key")
	}
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
		return nil, fmt.Errorf("unknown config key: %s", path)
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
		if w.path == path {
			fired = append(fired, WatchFired{ID: w.id, Old: old, New: val})
		}
	}
	s.mu.Unlock()
	return fired, nil
}

// Check returns val in the stored form of path's kind, or why it cannot be
// set there.
func (s *Store) Check(path string, val any) (any, error) {
	s.mu.RLock()
	def, ok := s.schema[path]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown config key: %s", path)
	}
	return checkKind(def, val)
}

// SetChoices limits the string key path to choices, for a list that lives
// in a package config cannot import (agent.Efforts).
func (s *Store) SetChoices[T ~string](path string, choices []T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.schema[path]
	if !ok {
		panic("config: SetChoices on a key outside the schema: " + path)
	}
	def.Choices = nil
	for _, c := range choices {
		def.Choices = append(def.Choices, string(c))
	}
	s.schema[path] = def
}

// AddWatch registers a change observer of path and returns its id. A path
// outside the schema is refused: watchers match by exact path, so a typo
// would sit there and never fire.
func (s *Store) AddWatch(path string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schema[path]; !ok {
		return 0, fmt.Errorf("unknown config key: %s", path)
	}
	s.watchSeq++
	s.watchers = append(s.watchers, watcher{s.watchSeq, path})
	return s.watchSeq, nil
}

// RemoveWatch drops one watcher by id; returns false if it was gone.
func (s *Store) RemoveWatch(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.watchers)
	s.watchers = slices.DeleteFunc(s.watchers, func(w watcher) bool { return w.id == id })
	return len(s.watchers) < n
}

// BadValueError means the value has the wrong kind or is outside the key's
// choices; Got, set for a refused choice, is echoed back.
type BadValueError struct {
	Path string
	Want string
	Got  any
}

func (e *BadValueError) Error() string {
	if e.Got != nil {
		return fmt.Sprintf("config key %s expects %s, not %q", e.Path, e.Want, e.Got)
	}
	return fmt.Sprintf("config key %s expects %s", e.Path, e.Want)
}

// checkKind validates val and returns it in stored form: a number as
// float64, as Lua hands every number in; NaN and Inf are refused, since no
// reader means them and the kept pick could not be written as JSON.
func checkKind(def Setting, val any) (any, error) {
	ok := true
	switch def.Kind {
	case KindString:
		var s string
		s, ok = val.(string)
		// a models.* key holds an id, never a role name: roles never nest
		if ok && strings.HasPrefix(def.Path, "models.") && s != "" && !strings.Contains(s, "/") {
			return nil, &BadValueError{Path: def.Path, Want: `a provider/model id or ""`}
		}
		if ok && def.Choices != nil && !slices.Contains(def.Choices, s) {
			// "default" names a "" choice, as /config shows it
			if s == "default" && slices.Contains(def.Choices, "") {
				return "", nil
			}
			return nil, &BadValueError{Path: def.Path, Want: choiceList(def.Choices), Got: s}
		}
	case KindNumber:
		var f float64
		switch v := val.(type) {
		case int:
			f = float64(v)
		case float64:
			f = v
		default:
			ok = false
		}
		ok = ok && !math.IsNaN(f) && !math.IsInf(f, 0)
		val = f
	case KindBool:
		_, ok = val.(bool)
	}
	if !ok {
		if def.Choices != nil {
			return nil, &BadValueError{Path: def.Path, Want: choiceList(def.Choices)}
		}
		return nil, &BadValueError{Path: def.Path, Want: string(def.Kind)}
	}
	return val, nil
}

func choiceList(choices []string) string {
	words := make([]string, len(choices))
	for i, c := range choices {
		words[i] = cmp.Or(c, "default")
	}
	return OrList(words)
}

// OrList joins words as "a, b or c".
func OrList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}
