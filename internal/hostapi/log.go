package hostapi

import (
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/logging"
	glua "github.com/yuin/gopher-lua"
)

// logNamespace builds the zemi.log member table; logCall is the __call
// dispatch so `zemi.log(msg)` = `zemi.log.info(msg)` (DESIGN §6.1).
func (h *Host) logNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	member := func(level logging.Level) glua.LGFunction {
		return func(L *glua.LState) int {
			msg := L.CheckString(1)
			fields := logFields(L, 2)
			h.Log.Log(level, string(h.RT.CurrentOrigin()), msg, fields)
			return 0
		}
	}

	tb.RawSetString("debug", L.NewFunction(member(logging.Debug)))
	tb.RawSetString("info", L.NewFunction(member(logging.Info)))
	tb.RawSetString("warn", L.NewFunction(member(logging.Warn)))
	tb.RawSetString("error", L.NewFunction(member(logging.Error)))
	return tb
}

func (h *Host) logCall(L *glua.LState) *glua.LFunction {
	return L.NewFunction(func(L *glua.LState) int {
		// The table is the first argument of the __call dispatch.
		msg := L.CheckString(2)
		fields := logFields(L, 3)
		h.Log.Log(logging.Info, string(h.RT.CurrentOrigin()), msg, fields)
		return 0
	})
}

// logFields converts an optional trailing context table to log fields.
func logFields(L *glua.LState, idx int) logging.Fields {
	if L.GetTop() < idx || L.Get(idx) == glua.LNil {
		return nil
	}
	v, err := lua.ToGo(L.Get(idx))
	if err != nil {
		L.RaiseError("zemi.log: %v", err)
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		L.RaiseError("zemi.log: context must be a table of key/values")
		return nil
	}
	return logging.Fields(m)
}
