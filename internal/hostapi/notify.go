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
	return tb
}

func (h *Host) notifyCall(L *glua.LState) *glua.LFunction {
	inner := h.notifyFn(logging.Info)
	return L.NewFunction(func(L *glua.LState) int {
		// __call dispatch passes the callable table as the first argument;
		// shift it so the spec table is at position 1 (like zemi.log does).
		L.Remove(1)
		return inner(L)
	})
}

func (h *Host) notifyFn(level logging.Level) glua.LGFunction {
	return func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.notify")
		v := luaToGoChecked(L, 1, "zemi.notify")
		m, _ := v.(map[string]any)
		title := strField(m, "title")
		body := strField(m, "body")
		if s, ok := m["level"].(string); ok {
			switch s {
			case "info":
				level = logging.Info
			case "warn":
				level = logging.Warn
			case "error":
				level = logging.Error
			}
		}
		h.Log.Log(level, string(h.RT.CurrentOrigin()), "notify: "+title+" "+body, nil)
		if ui := h.hooks(); ui != nil {
			ui.Notify(level.String(), sanitize.String(title), sanitize.String(body))
		}
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
