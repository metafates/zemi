package hostapi

import (
	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// luaToGoChecked marshals the value at arg idx, raising a Lua error tagged
// with the calling API on failure.
func luaToGoChecked(L *glua.LState, idx int, api string) (any, error) {
	v, err := lua.ToGo(L.Get(idx))
	if err != nil {
		L.RaiseError("%s: %v", api, err)
		return nil, err
	}
	return v, nil
}

// rawString reads a string field off a raw spec table (specs that carry
// Lua callbacks cannot be marshaled wholesale).
func rawString(tb *glua.LTable, key string) string {
	if s, ok := tb.RawGetString(key).(glua.LString); ok {
		return string(s)
	}
	return ""
}

func rawBool(tb *glua.LTable, key string) bool {
	if b, ok := tb.RawGetString(key).(glua.LBool); ok {
		return bool(b)
	}
	return false
}
