package hostapi

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
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
	mu   sync.Mutex
	byID map[int]watchRec
}

func newWatchReg() *watchReg { return &watchReg{byID: map[int]watchRec{}} }

func (w *watchReg) add(id int, rec watchRec) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.byID[id] = rec
}

// del drops one record, reporting whether the origin may and there was one.
func (w *watchReg) del(id int, origin lua.Origin) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec, ok := w.byID[id]
	switch {
	case !ok:
		return false, nil
	case rec.origin.Rank() > origin.Rank():
		return false, fmt.Errorf("the watch was registered by a %s registration", rec.origin)
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
// too, config setting one key twice included.
func (h *Host) cfgReplayed(path string, v any) bool {
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
// returns to its default unless a runtime pick moved it since. Like a
// skipped write neither notifies a watcher.
func (h *Host) cfgSettle() {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	h.cfgRotate()
	for path, prev := range h.cfgPrev {
		if live, _ := h.Settings.Get(path); len(prev) > 0 && len(h.cfgNow[path]) == 0 && reflect.DeepEqual(live, prev[len(prev)-1]) {
			h.Settings.SetQuiet(path, h.Settings.Default(path))
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

	// schema() lists every known setting: path, kind, default and desc.
	tb.RawSetString("schema", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, s := range h.Settings.Schema() {
			rows = append(rows, map[string]any{
				"path": s.Path, "kind": s.Kind, "default": s.Default, "desc": s.Desc,
			})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	// reload() replays the boot steps, so config the agent just wrote
	// applies without the user typing /reload.
	tb.RawSetString("reload", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.config.reload")
		h.refuseBelowUser(L, "config.reload")
		return luaResult(L, h.ReloadConfig())
	}))

	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.config.set")
		path := L.CheckString(1)
		v, err := lua.ToGo(L.Get(2))
		if err != nil {
			L.RaiseError("zemi.config.set: %v", err)
		}
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.config.set: only user config may set %q", path)
		}
		// a replay writing what the last load wrote keeps what /model, the
		// mode cycle, /effort or a flag picked since; an edited value wins
		if h.RT.Booting() && h.cfgReplayed(path, v) {
			return 0
		}
		fired, err := h.Settings.SetQuiet(path, v)
		if err != nil {
			L.RaiseError("zemi.config.set: %v (zemi.config.schema() lists every key and its type)", err)
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
		removed, err := h.watches.del(id, h.RT.CurrentOrigin(L))
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
