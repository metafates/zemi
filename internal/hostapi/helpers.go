package hostapi

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// luaCtx is the running request's context, which the queue cancels at the
// request's ceiling and on an interrupt.
func luaCtx(L *glua.LState) context.Context {
	return cmp.Or(L.Context(), context.Background())
}

// waitHandle is a handle's wait(): it suspends the task until done closes,
// the timeout passes or the task ends, and resumes it with ret's values
// (err "timeout" or "canceled" when done did not close). stop, when set,
// runs off the interpreter on either error.
func waitHandle(L *glua.LState, api string, done <-chan struct{}, timeout time.Duration, stop func(), ret func(L *glua.LState, err error) []glua.LValue) int {
	return lua.Suspend(L, api, func(ctx context.Context) func(*glua.LState) []glua.LValue {
		var err error
		select {
		case <-done:
		case <-ctx.Done():
			err = errors.New("canceled")
		case <-time.After(timeout):
			err = errors.New("timeout")
		}
		if err != nil && stop != nil {
			stop()
		}
		return func(L *glua.LState) []glua.LValue { return ret(L, err) }
	})
}

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

// rawScalar reads a string, number or boolean as text (env PORT = 8080).
func rawScalar(v glua.LValue) (string, bool) {
	switch v.(type) {
	case glua.LString, glua.LNumber, glua.LBool:
		return v.String(), true
	}
	return "", false
}

// rawStrings reads the scalars of a list field off a raw spec table as
// strings; nil when the field is not a table, so a caller can tell absent
// from empty.
func rawStrings(tb *glua.LTable, key string) []string {
	list, ok := tb.RawGetString(key).(*glua.LTable)
	if !ok {
		return nil
	}
	out := []string{}
	list.ForEach(func(_, v glua.LValue) {
		if s, ok := rawScalar(v); ok {
			out = append(out, s)
		}
	})
	return out
}

// rawStringMap reads the string keys and scalar values of a map field off a
// raw spec table, skipping other entries; nil when absent.
func rawStringMap(tb *glua.LTable, key string) map[string]string {
	m, ok := tb.RawGetString(key).(*glua.LTable)
	if !ok {
		return nil
	}
	out := map[string]string{}
	m.ForEach(func(k, v glua.LValue) {
		ks, kok := k.(glua.LString)
		vs, vok := rawScalar(v)
		if kok && vok {
			out[string(ks)] = vs
		}
	})
	return out
}

func rawNumber(tb *glua.LTable, key string) float64 {
	if n, ok := tb.RawGetString(key).(glua.LNumber); ok {
		return float64(n)
	}
	return 0
}

func rawBool(tb *glua.LTable, key string) bool {
	if b, ok := tb.RawGetString(key).(glua.LBool); ok {
		return bool(b)
	}
	return false
}
