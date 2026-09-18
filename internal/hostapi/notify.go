package hostapi

import (
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/sanitize"
	glua "github.com/yuin/gopher-lua"
)

// notifyNamespace builds zemi.notify: a callable table. The notify body and
// title cross the sanitize boundary like every model-influenced string
// (DESIGN §8).
func (h *Host) notifyNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("info", L.NewFunction(h.notifyFn(logging.Info)))
	tb.RawSetString("warn", L.NewFunction(h.notifyFn(logging.Warn)))
	tb.RawSetString("error", L.NewFunction(h.notifyFn(logging.Error)))
	tb.RawSetString("list", L.NewFunction(h.notifyListFn))
	return tb
}

func (h *Host) notifyCall(L *glua.LState) *glua.LFunction {
	inner := h.notifyFn(logging.Info)
	return L.NewFunction(func(L *glua.LState) int {
		// __call passes the callable table first; drop it so the spec is
		// argument 1.
		L.Remove(1)
		return inner(L)
	})
}

func (h *Host) notifyFn(level logging.Level) glua.LGFunction {
	return func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.notify")
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
		switch m["level"] {
		case "info":
			level = logging.Info
		case "warn":
			level = logging.Warn
		case "error":
			level = logging.Error
		}
		h.Log.Log(level, string(h.RT.CurrentOrigin(L)), "notify: "+title+" "+body, nil)
		h.Notify(level.String(), sanitize.String(title), sanitize.String(body))
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
