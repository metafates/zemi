package hostapi

import (
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// logNamespace builds the zemi.log member table; logCall is the __call
// dispatch so `zemi.log(msg)` = `zemi.log.info(msg)` (DESIGN §6.1).
func (h *Host) logNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("debug", L.NewFunction(h.logFn(logging.Debug, 1)))
	tb.RawSetString("info", L.NewFunction(h.logFn(logging.Info, 1)))
	tb.RawSetString("warn", L.NewFunction(h.logFn(logging.Warn, 1)))
	tb.RawSetString("error", L.NewFunction(h.logFn(logging.Error, 1)))
	return tb
}

// logCall skips the table, which __call passes as the first argument.
func (h *Host) logCall(L *glua.LState) *glua.LFunction {
	return L.NewFunction(h.logFn(logging.Info, 2))
}

// logFn logs the message at arg first with an optional context table after it.
func (h *Host) logFn(level logging.Level, first int) glua.LGFunction {
	return func(L *glua.LState) int {
		msg := L.CheckString(first)
		h.Log.Log(level, string(h.RT.CurrentOrigin()), msg, logFields(L, first+1))
		return 0
	}
}

// logFields converts an optional trailing context table to log fields.
func logFields(L *glua.LState, idx int) logging.Fields {
	if L.GetTop() < idx || L.Get(idx) == glua.LNil {
		return nil
	}
	v, err := lua.ToGo(L.Get(idx))
	if err != nil {
		L.RaiseError("zemi.log: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		L.RaiseError("zemi.log: context must be a table of key/values")
	}
	return logging.Fields(m)
}
