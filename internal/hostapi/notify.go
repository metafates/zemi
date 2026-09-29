package hostapi

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/sanitize"
)

// notifyLevels are zemi.notify's levels by name, each also a function.
var notifyLevels = map[string]slog.Level{"info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

// notifyName is level's name in notifyLevels, what zemi.notify.list reports.
func notifyName(level slog.Level) string {
	for name, l := range notifyLevels {
		if l == level {
			return name
		}
	}
	return "info"
}

// notifyNamespace builds zemi.notify: a callable table. The notify body and
// title cross the sanitize boundary like every model-influenced string
// (DESIGN §8).
func (h *Host) notifyNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	for name, level := range notifyLevels {
		tb.RawSetString(name, L.NewFunction(h.notifyFn(level)))
	}
	tb.RawSetString("level", enumTable(L, slices.Collect(maps.Keys(notifyLevels))...))
	tb.RawSetString("list", L.NewFunction(h.notifyListFn))
	return tb
}

func (h *Host) notifyCall(L *glua.LState) *glua.LFunction {
	inner := h.notifyFn(slog.LevelInfo)
	return L.NewFunction(func(L *glua.LState) int {
		// __call passes the callable table first; drop it so the spec is
		// argument 1.
		L.Remove(1)
		return inner(L)
	})
}

func (h *Host) notifyFn(level slog.Level) glua.LGFunction {
	return func(L *glua.LState) int {
		v := luaToGoChecked(L, 1, "zemi.notify")
		m, isSpec := v.(map[string]any)
		title := strField(m, "title")
		body := strField(m, "body")
		s, isText := v.(string)
		switch {
		case isText:
			body, title = s, L.OptString(2, "")
		case !isSpec:
			L.RaiseError("zemi.notify: expected a spec table or a string")
		}
		// the closure lives for the generation: a spec's level must not stick
		level := level
		if l, ok := notifyLevels[strField(m, "level")]; ok {
			level = l
		}
		h.Log.Log(context.Background(), level, "notify: "+title+" "+body, "origin", string(h.RT.CurrentOrigin(L)))
		h.Notify(level, sanitize.String(title), sanitize.String(body))
		return 0
	}
}

func strField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func boolField(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}
