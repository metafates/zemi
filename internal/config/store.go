package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

// Key is a config key's dotted path. Every path a caller names is a
// constant of this type or built from one, so no call site spells it.
type Key string

// The builtin keys, in the order defineDefaults declares them.
const (
	KeyModelsDefault      Key = "models.default"
	KeyModelsAgents       Key = "models.agents"
	KeyModelsCompact      Key = "models.compact"
	KeyModelsFavorites    Key = "models.favorites"
	KeyProvidersBuiltin   Key = "providers.builtin"
	KeyAgentEffort        Key = "agent.effort"
	KeySessionRecap       Key = "session.recap"
	KeyAgentMaxBackground Key = "agent.max_background"
	KeyPluginsAutoUpdate  Key = "plugins.auto_update"
	KeyLSPAuto            Key = "lsp.auto"
	KeyEditorCommand      Key = "editor.command"
	KeyKeymapLeader       Key = "keymap.leader"
	KeyUIMouse            Key = "ui.mouse"
	KeyUINotify           Key = "ui.notify"
	KeyLogLevel           Key = "log.level"
)

// ModelKey is the key a model target's own model is set at (models.<name>).
func ModelKey(role string) Key { return Key("models." + role) }

// PluginOf is the plugin a plugins.<name>.<key> key belongs to.
func PluginOf(key Key) (string, bool) {
	rest, ok := strings.CutPrefix(string(key), "plugins.")
	name, _, nested := strings.Cut(rest, ".")
	if !ok || !nested {
		return "", false
	}
	return name, true
}

// ModelRole is the target a models.<role> key names; favorites is a list key,
// not a slot.
func ModelRole(key Key) (string, bool) {
	role, ok := strings.CutPrefix(string(key), "models.")
	if !ok || key == KeyModelsFavorites {
		return "", false
	}
	return role, true
}

// PluginKey is the key a plugin's own setting is registered at.
func PluginKey(plugin, key string) Key { return Key("plugins." + plugin + "." + key) }

// Kind is the type of value a setting holds.
type Kind string

const (
	KindString   Kind = "string"
	KindNumber   Kind = "number"
	KindBool     Kind = "bool"
	KindList     Kind = "list"     // a list of strings
	KindDuration Kind = "duration" // a Go duration ("3m", "90s"); "" means the default
)

var Kinds = []Kind{KindString, KindNumber, KindBool, KindList, KindDuration}

// Setting describes one config key.
type Setting struct {
	Path    Key
	Kind    Kind
	Default any
	Desc    string
	Choices []string // the only values a string key takes; nil takes any
	// Check narrows a value past its kind; the settings whose path alone
	// cannot say what they hold (a model key holds an id, never a role).
	Check func(any) (any, error)
	// builtin marks a key defineDefaults owns: a key registered later (a
	// plugin's, a model target's) may take its value over, never the key.
	builtin bool
}

// Store holds the merged config values and user watches. The schema is
// Go-owned: a key outside it cannot be set.
type Store struct {
	mu       sync.RWMutex
	values   map[Key]any
	schema   map[Key]Setting
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
	path Key
}

// The ui.notify styles, in the order the setting lists them: the resolver that
// picks one and the dispatch that fires it read the same names.
const (
	NotifyOSC    = "osc"
	NotifyNative = "native"
	NotifyBell   = "bell"
)

var _notifyStyles = []string{NotifyOSC, NotifyNative, NotifyBell}

// The log levels log.level takes, in rising order.
var _logLevels = []string{"debug", "info", "warn", "error"}

// LogLevel parses a log.level value; ok is false for anything else, which
// the key's choices already refuse.
func LogLevel(v string) (lvl slog.Level, ok bool) {
	switch v {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return 0, false
	}
}

// OrList joins words as "a, b or c".
func OrList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

func NewStore() *Store {
	s := &Store{
		values: map[Key]any{},
		schema: map[Key]Setting{},
	}
	s.defineDefaults()
	return s
}

func (s *Store) defineDefaults() {
	defaults := []Setting{
		{Path: KeyModelsDefault, Kind: KindString, Default: "", Check: checkModelRef, Desc: "the model the conversation runs on and every role left empty follows (provider/model; empty uses the first added provider's default model, else the first usable builtin's)"},
		{Path: KeyModelsAgents, Kind: KindString, Default: "", Check: checkModelRef, Desc: "the model subagents run on when their definition names none (provider/model; empty follows models.default)"},
		{Path: KeyModelsCompact, Kind: KindString, Default: "", Check: checkModelRef, Desc: "the model that writes compaction summaries (provider/model; empty follows models.default)"},
		{Path: KeyModelsFavorites, Kind: KindList, Default: []string{}, Desc: "the models /model lists first, marked with f (provider/model ids)"},
		{Path: KeyProvidersBuiltin, Kind: KindBool, Default: true, Desc: "the builtin providers, each on while its key is exported (/model lists them); false leaves only what init.lua adds"},
		{Path: KeyAgentEffort, Kind: KindString, Default: "", Desc: "reasoning effort: low, medium, high, or empty for the model's default"},
		{Path: KeySessionRecap, Kind: KindDuration, Default: "3m", Desc: "after a turn with tool calls, if there is no input for this long, add a one-line recap from a short model call on the cached conversation (billed to the session); 0 turns it off, empty for the 3m default"},
		{Path: KeyAgentMaxBackground, Kind: KindNumber, Default: 4.0, Desc: "background agents and workflows running at once; one more is refused until one ends (1 at least)"},
		{Path: KeyPluginsAutoUpdate, Kind: KindBool, Default: true, Desc: "pull every registered marketplace once a day when zemi's UI opens, in the background, moving the plugins installed from them; false only fetches and tells you a marketplace has updates, pulling stays yours with plugin marketplace update"},
		{Path: KeyLSPAuto, Kind: KindBool, Default: true, Desc: "start installed language servers for files with their marker in a trusted project, and lua-language-server for the Lua of your config and local plugins"},
		{Path: KeyEditorCommand, Kind: KindString, Default: "", Desc: "editor for zemi.ui.edit and the composer draft (empty uses $VISUAL, then $EDITOR, then vi)"},
		{Path: KeyKeymapLeader, Kind: KindString, Default: "ctrl+x", Desc: "the key <leader> stands for in zemi.keymap.set sequences (set it before binding)"},
		{Path: KeyUIMouse, Kind: KindBool, Default: true, Desc: "drag to select and copy, click rows (false leaves selection to the terminal)"},
		{Path: KeyUINotify, Kind: KindString, Default: NotifyOSC, Choices: _notifyStyles, Desc: "how zemi alerts while its window is unfocused, as a turn ends, a prompt waits, or background work completes: osc (default; a terminal notification), native (osascript or notify-send), bell"},
		{Path: KeyLogLevel, Kind: KindString, Default: "debug", Choices: _logLevels, Desc: "what the launch log file (~/.local/state/zemi/logs) records: debug, info, warn or error"},
	}
	for _, def := range defaults {
		def.builtin = true
		s.schema[def.Path] = def
		s.values[def.Path] = def.Default
	}
}

// checkModelRef refuses a model key that names a role instead of a model:
// roles never nest, so a models.<role> key holds a provider/model id or "".
func checkModelRef(v any) (any, error) {
	s, ok := v.(string)
	if ok && s != "" && !strings.Contains(s, "/") {
		return nil, &BadValueError{Path: "", Want: `a provider/model id or ""`}
	}
	return v, nil
}

// ModelRefCheck is checkModelRef for a key registered outside this package (a
// model target's models.<name>).
func ModelRefCheck(v any) (any, error) { return checkModelRef(v) }

// Add registers a key beside the builtin ones. One registered again with
// the same kind, default and choices keeps its value and takes the new
// Desc; a new or changed one starts at its default, and changed says so.
func (s *Store) Add(def Setting) (changed bool, err error) {
	if def.Default, err = checkKind(def, def.Default); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.schema[def.Path]; ok {
		def.builtin = old.builtin // a key the schema owns stays the schema's
		if old.Kind == def.Kind && reflect.DeepEqual(old.Default, def.Default) && slices.Equal(old.Choices, def.Choices) {
			old.Desc = def.Desc
			s.schema[def.Path] = old
			return false, nil
		}
	}
	s.schema[def.Path] = def
	s.values[def.Path] = def.Default
	return true, nil
}

// Del drops a key Add registered, with its value.
func (s *Store) Del(path Key) {
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
func (s *Store) Default(path Key) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schema[path].Default
}

// Builtin reports whether the schema owns path: a key registered later is not
// one, and neither is a path outside the schema. Whatever drops registrations
// must leave these alone.
func (s *Store) Builtin(path Key) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schema[path].builtin
}

// Get looks up a dotted path over the merged values. A list comes back a
// copy, so a caller sorting or trimming it leaves the store's own alone.
func (s *Store) Get(path Key) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[path]
	list, isList := v.([]string)
	if isList {
		return slices.Clone(list), ok
	}
	return v, ok
}

// Str reads a string schema key; Bool and Int read the other two kinds, Int
// truncating a fraction. Every schema key holds its default until someone
// sets it, so only a key outside the schema reads as the zero value.
func (s *Store) Str(path Key) string { return s.value[string](path) }
func (s *Store) Bool(path Key) bool  { return s.value[bool](path) }
func (s *Store) Int(path Key) int    { return int(s.value[float64](path)) }
func (s *Store) List(path Key) []string {
	return slices.Clone(s.value[[]string](path))
}

// Duration reads a duration key: "" is the schema default, 0 is off. A stale
// stored value of another kind (a bool, from before the key held a duration)
// reads as the default too.
func (s *Store) Duration(path Key) time.Duration {
	v, _ := s.Get(path)
	s2, _ := v.(string)
	if s2 == "" {
		s2, _ = s.Default(path).(string)
	}
	d, err := time.ParseDuration(s2)
	if err != nil {
		return 0
	}
	return d
}

func (s *Store) value[T any](path Key) T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, _ := s.values[path].(T)
	return t
}

// SetQuiet applies a runtime override without notifying watchers; the
// callers on the interpreter goroutine deliver the fired watches themselves
// so a Lua watch callback never re-enters the request queue.
func (s *Store) SetQuiet(path Key, val any) ([]WatchFired, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.schema[path]
	if !ok {
		return nil, fmt.Errorf("unknown config key: %s", path)
	}
	val, err := checkKind(def, val)
	if err != nil {
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
	return fired, nil
}

// Check returns val in the stored form of path's kind, or why it cannot be
// set there.
func (s *Store) Check(path Key, val any) (any, error) {
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
func (s *Store) SetChoices[T ~string](path Key, choices []T) {
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
func (s *Store) AddWatch(path Key) (int, error) {
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
	Path Key
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
	case KindDuration:
		s, isStr := val.(string)
		if !isStr {
			ok = false
			break
		}
		if s != "" {
			if _, err := time.ParseDuration(s); err != nil {
				return nil, &BadValueError{Path: def.Path, Want: "a duration like 90s or 3m, 0 for off", Got: s}
			}
		}
		val = s
	case KindList:
		val, ok = stringList(val)
	}
	if !ok {
		if def.Choices != nil {
			return nil, &BadValueError{Path: def.Path, Want: choiceList(def.Choices)}
		}
		return nil, &BadValueError{Path: def.Path, Want: string(def.Kind)}
	}
	if def.Check != nil {
		v, err := def.Check(val)
		if err != nil {
			if bad, ok := errors.AsType[*BadValueError](err); ok && bad.Path == "" {
				bad.Path = def.Path
			}
			return nil, err
		}
		val = v
	}
	return val, nil
}

// stringList is a list key's stored form: Lua and JSON both hand it []any, so
// every entry is checked as a string and the slice comes back as []string.
func stringList(val any) ([]string, bool) {
	switch v := val.(type) {
	case []string:
		return slices.Clone(v), true
	case []any:
		out := make([]string, len(v))
		for i, it := range v {
			s, ok := it.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	default:
		return nil, false
	}
}

func choiceList(choices []string) string {
	words := make([]string, len(choices))
	for i, c := range choices {
		words[i] = cmp.Or(c, "default")
	}
	return OrList(words)
}
