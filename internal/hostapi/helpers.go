package hostapi

import (
	"context"
	"errors"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// waitHandle is a handle's wait(): it suspends the task until done closes,
// the timeout passes or the task ends, and resumes it with ret's values
// (err "timeout" when it passed). stop, when set, runs off the interpreter
// unless done closed.
func waitHandle(L *glua.LState, api string, done <-chan struct{}, timeout time.Duration, stop func(), ret func(L *glua.LState, err error) []glua.LValue) int {
	return lua.Suspend(L, api, func(ctx context.Context) func(*glua.LState) []glua.LValue {
		select {
		case <-done:
			return func(L *glua.LState) []glua.LValue { return ret(L, nil) }
		case <-ctx.Done(): // the task ends; what this returns is never run
		case <-time.After(timeout):
		}
		if stop != nil {
			stop()
		}
		return func(L *glua.LState) []glua.LValue { return ret(L, errors.New("timeout")) }
	})
}

// callable is fn as a namespace's __call, which passes the table first.
func callable(L *glua.LState, fn glua.LGFunction) *glua.LFunction {
	return L.NewFunction(func(L *glua.LState) int {
		L.Remove(1)
		return fn(L)
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

// opt reads a field of a raw spec table (specs that carry Lua callbacks
// cannot be marshaled wholesale): false when it is absent, and a raise at
// the caller naming the key when it holds another type (DESIGN §6.0).
func opt[T glua.LValue](L *glua.LState, tb *glua.LTable, key string) (T, bool) {
	v := tb.RawGetString(key)
	t, ok := v.(T)
	if !ok && v != glua.LNil {
		L.RaiseError("%s must be a %s, not a %s", key, t.Type(), v.Type())
	}
	return t, ok
}

func rawString(L *glua.LState, tb *glua.LTable, key string) string {
	s, _ := opt[glua.LString](L, tb, key)
	return string(s)
}

func rawBool(L *glua.LState, tb *glua.LTable, key string) bool {
	b, _ := opt[glua.LBool](L, tb, key)
	return bool(b)
}

// rawScalar reads a string, number or boolean as text (env PORT = 8080).
func rawScalar(v glua.LValue) (string, bool) {
	switch v.(type) {
	case glua.LString, glua.LNumber, glua.LBool:
		return v.String(), true
	}
	return "", false
}

// rawStrings reads the scalars of a list field as strings; nil when the
// field is absent, so a caller can tell absent from empty.
func rawStrings(L *glua.LState, tb *glua.LTable, key string) []string {
	list, ok := opt[*glua.LTable](L, tb, key)
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

// rawStringMap reads the string keys and scalar values of a map field,
// skipping other entries; nil when absent.
func rawStringMap(L *glua.LState, tb *glua.LTable, key string) map[string]string {
	m, ok := opt[*glua.LTable](L, tb, key)
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
