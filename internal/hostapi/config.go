package hostapi

import (
	"encoding/json/v2"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

// watchRec tracks config watches registered from Lua so a force-reload can
// drop the dead generation's callbacks instead of firing them into a
// dropped LState (DESIGN §4 stale-callback rule).
type watchRec struct {
	gen    Gen
	path   config.Key
	fn     *glua.LFunction
	origin lua.Origin
	src    string
}

// watchReg keys records by the config.Store watch id.
type watchReg struct {
	mu    sync.Mutex
	byID  map[int]watchRec
	owner func(path string) string // Host.PluginOf, see mayReplace
}

func newWatchReg() *watchReg { return &watchReg{byID: map[int]watchRec{}} }

func (w *watchReg) add(id int, rec watchRec) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.byID[id] = rec
}

// del drops one record the caller may replace (mayReplace), reporting
// whether there was one.
func (w *watchReg) del(id int, by lua.Origin, byPath string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec, ok := w.byID[id]
	if !ok {
		return false, nil
	}
	if err := mayReplace(w.owner, "watch "+strconv.Itoa(id), rec.origin, rec.src, by, byPath); err != nil {
		return false, err
	}
	delete(w.byID, id)
	return true, nil
}

func (w *watchReg) rec(id int) (watchRec, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec, ok := w.byID[id]
	return rec, ok
}

// purge drops the generation's records and returns their ids.
func (w *watchReg) purge(gen Gen) []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	var ids []int
	for id, rec := range w.byID {
		if rec.gen == gen {
			ids = append(ids, id)
			delete(w.byID, id)
		}
	}
	return ids
}

func (h *Host) purgeWatches() {
	for _, id := range h.watches.purge(h.currentGen()) {
		h.Settings.RemoveWatch(id)
	}
}

// cfgReplayed records a boot-time write of path and reports whether the
// previous boot wrote the same value at the same point in that key's
// sequence, so there is nothing to apply and a value picked at runtime
// stays. The first write that differs makes the key's later writes apply
// too, config setting one key twice included. Every setter init.lua can
// call runs a valid value through it, so /config knows what config set.
func (h *Host) cfgReplayed(path config.Key, v any) bool {
	if !h.RT.Booting() {
		return false
	}
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.cfgRotate()
	i := len(h.cfgNow[path])
	h.cfgNow[path] = append(h.cfgNow[path], v)
	if i < len(h.cfgPrev[path]) && reflect.DeepEqual(h.cfgPrev[path][i], v) {
		return true
	}
	delete(h.cfgPrev, path) // diverged: every later write of this key applies
	return false
}

// cfgRotate makes the last boot's writes cfgPrev on the first look of a new
// generation. cfgSettle calls it too: a config with no set left never
// reaches cfgReplayed.
func (h *Host) cfgRotate() {
	if gen := h.currentGen(); gen != h.cfgGen {
		h.cfgGen, h.cfgPrev, h.cfgNow = gen, h.cfgNow, map[config.Key][]any{}
	}
}

// cfgSettle runs after a replay: a key whose every write matched the last
// boot but whose config now writes it fewer times ends at a value nothing
// applied, so its last write is applied here. A key config stopped writing
// returns to its saved pick, else its default, unless a runtime pick moved
// it since. Like a skipped write neither notifies a watcher.
func (h *Host) cfgSettle() {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.cfgRotate()
	for path, prev := range h.cfgPrev {
		if live, _ := h.Settings.Get(path); len(prev) > 0 && len(h.cfgNow[path]) == 0 && reflect.DeepEqual(live, prev[len(prev)-1]) {
			back, ok := h.picked[path]
			if !ok {
				back = h.Settings.Default(path)
			}
			if _, err := h.Settings.SetQuiet(path, back); err != nil {
				h.Log.Warn("settle failed", "origin", "config", "path", path, "error", err)
			}
		}
	}
	for path, now := range h.cfgNow {
		prev := h.cfgPrev[path]
		if len(now) == 0 || len(prev) == 0 || reflect.DeepEqual(now[len(now)-1], prev[len(prev)-1]) {
			continue
		}
		if _, err := h.Settings.SetQuiet(path, now[len(now)-1]); err != nil {
			h.Log.Warn("settle failed", "origin", "config", "path", path, "error", err)
		}
	}
}

// cfgSource says where path's live value comes from: "init.lua" when it is
// what init.lua last wrote, "saved" when it is the pick kept for a key
// init.lua does not write, "default" at the default of such a key, and
// "this run" for anything else, which a restart does not keep.
func (h *Host) cfgSource(path config.Key) string {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	live, _ := h.Settings.Get(path)
	pick, saved := h.picked[path]
	switch w := h.cfgNow[path]; {
	case len(w) > 0 && reflect.DeepEqual(live, w[len(w)-1]):
		return "init.lua"
	case len(w) > 0:
		return "this run"
	case saved && reflect.DeepEqual(live, pick):
		return "saved"
	case reflect.DeepEqual(live, h.Settings.Default(path)):
		return "default"
	}
	return "this run"
}

// cfgBase is what path is without a pick: config's last write when init.lua
// wrote the key, else its default. set reports that init.lua wrote it.
func (h *Host) cfgBase(path config.Key) (v any, set bool) {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.cfgRotate() // a generation that moved has not written the key yet
	if w := h.cfgNow[path]; len(w) > 0 {
		return w[len(w)-1], true
	}
	return h.Settings.Default(path), false
}

// writeSetting is every runtime write of a config key: the value, then
// keepPick.
func (h *Host) writeSetting(path config.Key, v any) ([]config.WatchFired, error) {
	fired, err := h.Settings.SetQuiet(path, v)
	if err == nil {
		h.keepPick(path)
	}
	return fired, err
}

// keepPick keeps path's live value in the global store for the next start,
// or forgets it when it is the key's cfgBase. A write during boot is
// init.lua's, and one before KeepPicks is a flag's.
func (h *Host) keepPick(path config.Key) {
	if h.GlobalDB == nil || h.RT.Booting() || !h.KeepPicks.Load() {
		return
	}
	live, _ := h.Settings.Get(path)
	base, _ := h.cfgBase(path)
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	if reflect.DeepEqual(live, base) {
		h.forgetPick(path)
		return
	}
	b, err := json.Marshal(live, json.Deterministic(true))
	if err == nil {
		err = h.GlobalDB.SetKV("config:"+string(path), string(b))
	}
	if err != nil {
		h.Log.Warn("could not keep a pick", "origin", "config", "path", path, "error", err)
		return
	}
	h.picked[path] = live
}

// forgetPick drops a kept pick everywhere it lives: the live copy and the
// config: row a later boot would read. Callers hold cfgMu.
func (h *Host) forgetPick(path config.Key) {
	delete(h.picked, path)
	if h.GlobalDB == nil {
		return
	}
	if _, err := h.GlobalDB.DelKV("config:" + string(path)); err != nil {
		h.Log.Warn("could not forget a kept pick", "origin", "config", "path", path, "error", err)
	}
}

// loadPicks applies the kept picks before any Lua runs, so init.lua still
// wins over them; a row the schema no longer takes is left out.
func (h *Host) loadPicks() {
	keys, err := h.GlobalDB.KeysKV("config:")
	if err != nil {
		h.Log.Warn("could not read the kept picks", "origin", "config", "error", err)
	}
	for _, k := range keys {
		path := config.Key(strings.TrimPrefix(k, "config:"))
		raw, err := h.GlobalDB.GetKV(k)
		var v any
		if err == nil {
			err = json.Unmarshal([]byte(raw), &v)
		}
		// a key the schema does not hold yet may still be registered later in
		// this boot, by a plugin or by a model target, which applies its pick
		if _, known := h.Settings.Get(path); err == nil && !known && (settingPlugin(path) != "" || strings.HasPrefix(string(path), "models.")) {
			h.picked[path] = v
			continue
		}
		if err == nil {
			v, err = h.Settings.Check(path, v)
		}
		if err == nil {
			_, err = h.Settings.SetQuiet(path, v)
		}
		if err != nil {
			h.Log.Warn("kept pick left out", "origin", "config", "path", path, "error", err)
			continue
		}
		h.picked[path] = v
	}
}

var settingKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// settingPath is the key path names: plugin code names its own settings by
// the bare key. Every builtin key has a section, so a bare name is no builtin
// one, and a plugin still reads any other key by its full path.
func (h *Host) settingPath(L *glua.LState, path string) config.Key {
	if strings.Contains(path, ".") || h.RT.CurrentOrigin(L) != lua.OriginPlugin {
		return config.Key(path)
	}
	if name := h.PluginOf(h.RT.CurrentPath(L)); name != "" {
		return config.PluginKey(name, path)
	}
	return config.Key(path)
}

// settingPlugin is the plugin that registered path, "" for a builtin key.
func settingPlugin(path config.Key) string {
	name, _ := config.PluginOf(path)
	return name
}

// addSetting is zemi.config.add: a setting registers a key by bare name while
// it loads, stored as plugins.<name>.<key> for plugin code, so the schema
// changes only with a load. A new or changed key takes its kept pick; one the
// new spec refuses is forgotten.
func (h *Host) addSetting(L *glua.LState, key string, spec *glua.LTable) {
	if !h.RT.Booting() {
		L.RaiseError("zemi.config.add: register settings while the config loads, from an init.lua")
	}
	name := h.PluginOf(h.RT.CurrentPath(L))
	path := config.Key(key)
	if name != "" {
		path = config.PluginKey(name, key)
	}
	def := config.Setting{
		Path: path, Kind: config.Kind(rawString(L, spec, "kind")),
		Desc: listDesc(rawString(L, spec, "desc")), Choices: rawStrings(L, spec, "choices"),
	}
	switch {
	case !settingKey.MatchString(key):
		L.RaiseError("zemi.config.add: key %q must be a lowercase letter, then up to 31 lowercase letters, digits and _", key)
	case !slices.Contains(config.Kinds, def.Kind):
		L.RaiseError("zemi.config.add: kind must be one of %s", config.OrList(kindNames()))
	case def.Choices != nil && def.Kind != config.KindString:
		L.RaiseError("zemi.config.add: only a string key takes choices")
	case def.Desc == "":
		L.RaiseError("zemi.config.add: desc is what /config shows for the key")
	}
	def.Default, _ = lua.ToGo(spec.RawGetString("default"))
	if err := h.addKey(def); err != nil {
		L.RaiseError("zemi.config.add: default: %v", err)
	}
}

// kindNames is the kinds zemi.config.add accepts, for its refusal.
func kindNames() []string {
	names := make([]string, len(config.Kinds))
	for i, k := range config.Kinds {
		names[i] = string(k)
	}
	return names
}

// addKey registers a key the schema does not own — a plugin's setting or a
// model target's models.<name> — and applies the pick kept for it: loadPicks
// stored that before any Lua ran, so the registration is what applies it.
func (h *Host) addKey(def config.Setting) error {
	changed, err := h.Settings.Add(def)
	if err != nil {
		return err
	}
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.pluginKeys[def.Path] = h.currentGen()
	pick, picked := h.picked[def.Path]
	if !changed {
		return nil
	}
	h.cfgRotate()
	delete(h.cfgPrev, def.Path) // it starts at its default, so init.lua's write is no replay
	if !picked {
		return nil
	}
	v, cerr := h.Settings.Check(def.Path, pick)
	if cerr == nil {
		_, cerr = h.Settings.SetQuiet(def.Path, v)
	}
	if cerr != nil {
		h.Log.Warn("kept pick dropped", "origin", "config", "path", def.Path, "error", cerr)
		h.forgetPick(def.Path)
		return nil
	}
	h.picked[def.Path] = v
	return nil
}

// settingOff reports a plugins.<name>.<key> path the schema lacks whose
// plugin is off, gone or failed to load, so init.lua's line for it does
// nothing; for a plugin that loaded it is a typo, which raises.
func (h *Host) settingOff(path config.Key) bool {
	name := settingPlugin(path)
	if _, known := h.Settings.Get(path); name == "" || known {
		return false
	}
	if p := h.pluginLoads.Load(); p != nil && slices.ContainsFunc(*p, func(l pluginLoad) bool { return l.name == name && l.err == nil }) {
		return false
	}
	h.Log.Info("setting of a plugin that did not load skipped", "origin", "config", "path", path)
	return true
}

// dropPluginKeys forgets, after a replay, the settings it did not register
// again: their plugin is off, gone or stopped registering them. A key the
// schema owns stays: a registration may have taken its value over, and
// dropping it would take a builtin key out of the schema.
func (h *Host) dropPluginKeys() {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.cfgRotate()
	for path, gen := range h.pluginKeys {
		if gen == h.currentGen() {
			continue
		}
		delete(h.pluginKeys, path)
		if h.Settings.Builtin(path) {
			continue
		}
		h.Settings.Del(path)
		delete(h.cfgNow, path)
	}
}

func (h *Host) configNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		v, _ := h.Settings.Get(h.settingPath(L, L.CheckString(1)))
		L.Push(lua.FromGo(L, v))
		return 1
	}))

	// schema() lists every known setting: path, kind, default, desc and choices.
	tb.RawSetString("schema", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, s := range h.Settings.Schema() {
			row := map[string]any{"path": s.Path, "kind": s.Kind, "default": s.Default, "desc": s.Desc}
			if s.Choices != nil {
				row["choices"] = s.Choices
			}
			rows = append(rows, row)
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		h.addSetting(L, L.CheckString(1), L.CheckTable(2))
		return 0
	}))

	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		path := h.settingPath(L, L.CheckString(1))
		v, err := lua.ToGo(L.Get(2))
		if err != nil {
			L.RaiseError("zemi.config.set: %v", err)
		}
		if h.settingOff(path) {
			return 0
		}
		// the stored form, so a replay compares what the store holds
		if v, err = h.Settings.Check(path, v); err != nil {
			L.RaiseError("zemi.config.set: %v (zemi.config.schema() lists every key and its type)", err)
		}
		// a replay writing what the last load wrote keeps what /model, the
		// mode cycle, /effort or a flag picked since; an edited value wins
		if h.cfgReplayed(path, v) {
			return 0
		}
		fired, err := h.writeSetting(path, v)
		if err != nil {
			L.RaiseError("zemi.config.set: %v", err)
		}
		h.deliverFired(L, fired)
		return 0
	}))

	tb.RawSetString("watch", L.NewFunction(func(L *glua.LState) int {
		path := h.settingPath(L, L.CheckString(1))
		fn := L.CheckFunction(2)
		if h.settingOff(path) {
			return 0
		}
		rec := watchRec{
			gen:    h.currentGen(),
			path:   path,
			fn:     fn,
			origin: h.RT.CurrentOrigin(L),
			src:    h.RT.CurrentPath(L),
		}
		id, err := h.Settings.AddWatch(path)
		if err != nil {
			L.RaiseError("zemi.config.watch: %v", err)
		}
		h.watches.add(id, rec)
		L.Push(glua.LString(strconv.Itoa(id)))
		return 1
	}))

	// unwatch(id) drops a watch by the id watch returned
	tb.RawSetString("unwatch", L.NewFunction(func(L *glua.LState) int {
		id, cerr := strconv.Atoi(L.CheckString(1))
		if cerr != nil {
			L.Push(glua.LBool(false))
			return 1
		}
		removed, err := h.watches.del(id, h.RT.CurrentOrigin(L), h.RT.CurrentPath(L))
		if err != nil {
			L.RaiseError("zemi.config.unwatch: %v", err)
		}
		if removed {
			h.Settings.RemoveWatch(id)
		}
		L.Push(glua.LBool(removed))
		return 1
	}))

	return tb
}

// deliverFired runs the watches a SetQuiet fired inline on the interpreter
// state that performed the set: the interpreter goroutine is busy inside
// this very request, so a queue round-trip could never complete.
func (h *Host) deliverFired(L *glua.LState, fired []config.WatchFired) {
	for _, f := range fired {
		rec, ok := h.watches.rec(f.ID)
		if !ok || h.currentGen() != rec.gen {
			continue
		}
		if err := h.callWatch(L, rec, f.Old, f.New); err != nil {
			h.Log.Error("config watch failed", "origin", "config",
				"path", rec.path, "watch", f.ID, "error", err,
			)
		}
	}
}

func (h *Host) callWatch(L *glua.LState, rec watchRec, old, new any) error {
	_, err := h.callFn(L, luaCaller{gen: rec.gen, origin: rec.origin, path: rec.src}, rec.fn, 0,
		lua.FromGo(L, new), lua.FromGo(L, old))
	return err
}
