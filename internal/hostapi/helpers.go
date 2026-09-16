package hostapi

import (
	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// luaToGoChecked marshals the value at arg idx, raising a Lua error tagged
// with the calling API on failure.
func luaToGoChecked(L *glua.LState, idx int, api string) any {
	v, err := lua.ToGo(L.Get(idx))
	if err != nil {
		L.RaiseError("%s: %v", api, err)
	}
	return v
}

// luaResult is the sync-op return shape (DESIGN §6.0): true, or nil and the
// error message.
func luaResult(L *glua.LState, err error) int {
	if err != nil {
		L.Push(glua.LNil)
		L.Push(glua.LString(err.Error()))
		return 2
	}
	L.Push(glua.LTrue)
	return 1
}

// rawString reads a string field off a raw spec table (specs that carry
// Lua callbacks cannot be marshaled wholesale).
func rawString(tb *glua.LTable, key string) string {
	if s, ok := tb.RawGetString(key).(glua.LString); ok {
		return string(s)
	}
	return ""
}

// rawStrings reads the strings of a list field off a raw spec table; nil
// when the field is not a table, so a caller can tell absent from empty.
func rawStrings(tb *glua.LTable, key string) []string {
	list, ok := tb.RawGetString(key).(*glua.LTable)
	if !ok {
		return nil
	}
	out := []string{}
	list.ForEach(func(_, v glua.LValue) {
		if s, ok := v.(glua.LString); ok {
			out = append(out, string(s))
		}
	})
	return out
}

func rawBool(tb *glua.LTable, key string) bool {
	if b, ok := tb.RawGetString(key).(glua.LBool); ok {
		return bool(b)
	}
	return false
}
