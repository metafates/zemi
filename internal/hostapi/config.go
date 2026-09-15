package hostapi

import (
	"context"
	"errors"
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

type watchReg struct {
	mu   sync.Mutex
	next int
	byID map[int]watchRec
}

func newWatchReg() *watchReg { return &watchReg{byID: map[int]watchRec{}} }

func (w *watchReg) add(rec watchRec) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	w.byID[w.next] = rec
	return w.next
}

func (w *watchReg) rec(id int) (watchRec, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec, ok := w.byID[id]
	return rec, ok
}

func (w *watchReg) idsFor(gen Gen) []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []int
	for id, rec := range w.byID {
		if rec.gen == gen {
			out = append(out, id)
		}
	}
	return out
}

func (h *Host) purgeWatches() {
	gen := h.currentGen()
	for _, id := range h.watches.idsFor(gen) {
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
				"writable": s.UserWritable && !s.AutonomyKnob,
			})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.config.set") {
			return 0
		}
		path := L.CheckString(1)
		v, err := lua.ToGo(L.Get(2))
		if err != nil {
			L.RaiseError("zemi.config.set: %v", err)
		}
		if h.RT.CurrentOrigin() != lua.OriginUser {
			L.RaiseError("zemi.config.set: only user config may set %q", path)
		}
		if h.Settings.BootWindowClosed() && isAutonomyKnob(h.Settings, path) {
			L.RaiseError("zemi.config.set: autonomy knob %q is host-owned and cannot be set after boot", path)
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
			origin: h.RT.CurrentOrigin(),
			src:    h.RT.CurrentPath(),
		}
		id := h.watches.add(rec)
		h.Settings.AddWatch(config.Watcher{
			Path: path,
			Fn: func(old, new any) {
				// Go-side Set path: deliver on the interpreter via the queue.
				h.deliverWatch(rec, id, old, new)
			},
		})
		L.Push(glua.LNumber(id))
		return 1
	}))

	return tb
}

func isAutonomyKnob(s *config.Store, path string) bool {
	for _, def := range s.Schema() {
		if def.Path == path && def.AutonomyKnob {
			return true
		}
	}
	return false
}

// deliverWatch runs a config-watch callback on the interpreter in a ui-class
// request. It never blocks the setter: stale-generation callbacks are
// dropped and failures logged (DESIGN §4). Only for Go-side Set callers;
// interpreter-side Sets use deliverFired (a blocking RT.Do from inside a
// running request would deadlock the queue).
func (h *Host) deliverWatch(rec watchRec, watchID int, old, new any) {
	if h.currentGen() != rec.gen {
		h.Log.Debug("config", "stale config watch dropped", map[string]any{
			"path": rec.path, "watch": watchID,
		})
		return
	}
	_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		h.pushWatchFrame(rec)
		defer h.RT.PopFrame()
		base := L.GetTop()
		L.Push(rec.fn)
		L.Push(lua.FromGo(L, new))
		L.Push(lua.FromGo(L, old))
		if err := L.PCall(2, 0, nil); err != nil {
			L.SetTop(base)
			return nil, err
		}
		return nil, nil
	})
	if err != nil && !errors.Is(err, lua.ErrHot) && !errors.Is(err, lua.ErrReloaded) {
		h.Log.Error("config", "config watch failed", map[string]any{
			"path": rec.path, "error": err.Error(),
		})
	}
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
		h.pushWatchFrame(rec)
		base := L.GetTop()
		L.Push(rec.fn)
		L.Push(lua.FromGo(L, f.New))
		L.Push(lua.FromGo(L, f.Old))
		if err := L.PCall(2, 0, nil); err != nil {
			h.Log.Error("config", "config watch failed", map[string]any{
				"path": rec.path, "watch": f.ID, "error": err.Error(),
			})
		}
		h.RT.PopFrame()
		L.SetTop(base)
	}
}

func (h *Host) pushWatchFrame(rec watchRec) {
	h.RT.PushFrame(rec.origin, rec.src)
}
