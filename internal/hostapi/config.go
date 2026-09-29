package hostapi

import (
	"encoding/json/v2"
	"errors"
	"reflect"
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
	path   string
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
func (h *Host) cfgReplayed(path string, v any) bool {
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
		h.cfgGen, h.cfgPrev, h.cfgNow = gen, h.cfgNow, map[string][]any{}
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
				h.Log.Warn("config", "settle failed", map[string]any{"path": path, "error": err.Error()})
			}
		}
	}
	for path, now := range h.cfgNow {
		prev := h.cfgPrev[path]
		if len(now) == 0 || len(prev) == 0 || reflect.DeepEqual(now[len(now)-1], prev[len(prev)-1]) {
			continue
		}
		if _, err := h.Settings.SetQuiet(path, now[len(now)-1]); err != nil {
			h.Log.Warn("config", "settle failed", map[string]any{"path": path, "error": err.Error()})
		}
	}
}

// cfgSource says where path's live value comes from: "init.lua" when it is
// what init.lua last wrote, "saved" when it is the pick kept for a key
// init.lua does not write, "default" at the default of such a key, and
// "this run" for anything else, which a restart does not keep.
func (h *Host) cfgSource(path string) string {
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
func (h *Host) cfgBase(path string) (v any, set bool) {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	if w := h.cfgNow[path]; len(w) > 0 {
		return w[len(w)-1], true
	}
	return h.Settings.Default(path), false
}

// writeSetting is every runtime write of a config key: the value, then
// keepPick.
func (h *Host) writeSetting(path string, v any) ([]config.WatchFired, error) {
	fired, err := h.Settings.SetQuiet(path, v)
	if err == nil {
		h.keepPick(path)
	}
	return fired, err
}

// keepPick keeps path's live value in the global store for the next start,
// or forgets it when it is the key's cfgBase. A write during boot is
// init.lua's, and one before KeepPicks is a flag's. A yolo mode is never
// kept: a flip made for one task must not start every later zemi, in any
// project, allowing what the rules do not deny.
func (h *Host) keepPick(path string) {
	if h.GlobalDB == nil || h.RT.Booting() || !h.KeepPicks.Load() ||
		path == "permissions.mode" && h.permModeDef().Base == "yolo" {
		return
	}
	live, _ := h.Settings.Get(path)
	base, _ := h.cfgBase(path)
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	var err error
	if reflect.DeepEqual(live, base) {
		delete(h.picked, path)
		_, err = h.GlobalDB.DelKV("config:" + path)
	} else if b, jerr := json.Marshal(live); jerr != nil {
		err = jerr
	} else if err = h.GlobalDB.SetKV("config:"+path, string(b)); err == nil {
		h.picked[path] = live
	}
	if err != nil {
		h.Log.Warn("config", "could not keep a pick", map[string]any{"path": path, "error": err.Error()})
	}
}

// loadPicks applies the kept picks before any Lua runs, so init.lua still
// wins over them; a row the schema no longer takes is left out, and so is a
// yolo row, which user Lua can write to kv without keepPick.
func (h *Host) loadPicks() {
	keys, err := h.GlobalDB.KeysKV("config:")
	if err != nil {
		h.Log.Warn("config", "could not read the kept picks", map[string]any{"error": err.Error()})
	}
	for _, k := range keys {
		path := strings.TrimPrefix(k, "config:")
		raw, err := h.GlobalDB.GetKV(k)
		var v any
		if err == nil {
			err = json.Unmarshal([]byte(raw), &v)
		}
		if err == nil {
			v, err = h.Settings.Check(path, v)
		}
		if err == nil && path == "permissions.mode" && v == "yolo" {
			err = errors.New("yolo is never kept")
		}
		if err == nil {
			_, err = h.Settings.SetQuiet(path, v)
		}
		if err != nil {
			h.Log.Warn("config", "kept pick left out", map[string]any{"path": path, "error": err.Error()})
			continue
		}
		h.picked[path] = v
	}
}

func (h *Host) configNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		path := L.CheckString(1)
		v, ok := h.Settings.Get(path)
		if !ok {
			L.Push(glua.LNil)
			return 1
		}
		L.Push(lua.FromGo(L, v))
		return 1
	}))

	// schema() lists every known setting: path, kind, default, desc and choices.
	tb.RawSetString("schema", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
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

	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		path := L.CheckString(1)
		v, err := lua.ToGo(L.Get(2))
		if err != nil {
			L.RaiseError("zemi.config.set: %v", err)
		}
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.config.set: only user config may set %q", path)
		}
		// the stored form, so a Lua number matches the int it becomes
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
		path := L.CheckString(1)
		fn := L.CheckFunction(2)
		rec := watchRec{
			gen:    h.currentGen(),
			path:   path,
			fn:     fn,
			origin: h.RT.CurrentOrigin(L),
			src:    h.RT.CurrentPath(L),
		}
		id, err := h.Settings.AddWatch(config.Watcher{Path: path})
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
			h.Log.Error("config", "config watch failed", map[string]any{
				"path": rec.path, "watch": f.ID, "error": err.Error(),
			})
		}
	}
}

func (h *Host) callWatch(L *glua.LState, rec watchRec, old, new any) error {
	defer h.RT.PushFrame(L, rec.origin, rec.src)()
	base := L.GetTop()
	defer L.SetTop(base)
	L.Push(rec.fn)
	L.Push(lua.FromGo(L, new))
	L.Push(lua.FromGo(L, old))
	return L.PCall(2, 0, nil)
}
