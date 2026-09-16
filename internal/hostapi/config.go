package hostapi

import (
	"strings"
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

	// schema() lists every known setting: path, kind, default, desc and
	// whether zemi.config.set accepts it (autonomy knobs never do).
	tb.RawSetString("schema", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, s := range h.Settings.Schema() {
			rows = append(rows, map[string]any{
				"path": s.Path, "kind": s.Kind, "default": s.Default, "desc": s.Desc,
				"writable": s.UserWritable,
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
		if path == "permissions" || strings.HasPrefix(path, "permissions.") {
			h.refuseModelLua(L, "zemi.config.set "+path)
		}
		fired, err := h.Settings.SetQuiet(path, v)
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
		id := h.Settings.AddWatch(config.Watcher{Path: path})
		h.watches.add(id, rec)
		L.Push(glua.LNumber(id))
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
