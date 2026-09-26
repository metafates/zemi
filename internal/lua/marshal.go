// Package lua hosts the embedded interpreter: one LState per runtime
// generation on a dedicated goroutine, a sandboxed stdlib, and the actor
// queue every call funnels through (DESIGN §4, §5).
package lua

import (
	"errors"
	"fmt"
	"reflect"

	lua "github.com/yuin/gopher-lua"
)

// Boundary caps for values crossing the Lua <-> Go marshal (§5).
const (
	MaxStringBytes = 16 << 20
	MaxTableDepth  = 64
)

var (
	// ErrCycle is returned when a Lua table references itself during marshal.
	ErrCycle = errors.New("zemi.lua: cyclic table refused at marshal boundary")
	// ErrTooDeep is returned when nesting exceeds MaxTableDepth.
	ErrTooDeep = errors.New("zemi.lua: table nesting exceeds depth cap")
	// ErrTooLarge is returned when a string exceeds MaxStringBytes.
	ErrTooLarge = errors.New("zemi.lua: string exceeds size cap")
)

// ToGo converts a Lua value into plain Go values (map[string]any, []any,
// string, bool, float64, nil). Caps are enforced; cycles are refused.
func ToGo(v lua.LValue) (any, error) { return toGo(v, 0, map[*lua.LTable]struct{}{}) }

func toGo(v lua.LValue, depth int, seen map[*lua.LTable]struct{}) (any, error) {
	switch t := v.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(t), nil
	case lua.LNumber:
		return float64(t), nil
	case lua.LString:
		if len(t) > MaxStringBytes {
			return nil, ErrTooLarge
		}
		return string(t), nil
	case *lua.LFunction:
		return nil, errors.New("zemi.lua: cannot marshal function")
	case *lua.LTable:
		if depth >= MaxTableDepth {
			return nil, ErrTooDeep
		}
		if _, ok := seen[t]; ok {
			return nil, ErrCycle
		}
		seen[t] = struct{}{}
		defer delete(seen, t)

		// Arrays (1..n contiguous, no holes) become []any, anything else
		// a map; a nil hole makes it a map.
		isArray := true
		count, maxI := 0, 0
		t.ForEach(func(key, _ lua.LValue) {
			if k, ok := key.(lua.LNumber); ok {
				if float64(k) == float64(int64(k)) && int64(k) >= 1 {
					count++
					if int64(k) > int64(maxI) {
						maxI = int(k)
					}
					return
				}
			}
			isArray = false
		})
		if isArray && maxI > 0 && count == maxI {
			out := make([]any, 0, maxI)
			for i := 1; i <= maxI; i++ {
				v, err := toGo(t.RawGetInt(i), depth+1, seen)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}
		out := map[string]any{}
		var err error
		t.ForEach(func(key, value lua.LValue) {
			if err != nil {
				return
			}
			var k, v any
			k, err = toGo(key, depth+1, seen)
			if err != nil {
				return
			}
			v, err = toGo(value, depth+1, seen)
			if err != nil {
				return
			}
			out[fmt.Sprint(k)] = v
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	case *lua.LUserData:
		return t, nil // handles pass through as opaque Go values
	default:
		return nil, fmt.Errorf("zemi.lua: cannot marshal %s", v.Type().String())
	}
}

// FromGo converts a Go value into a Lua value on the given state. Types
// outside the plain set raise a catchable Lua error at call time. Cyclic Go
// maps and slices are refused like their Lua counterparts.
func FromGo(L *lua.LState, v any) lua.LValue { return fromGo(L, v, 0, map[uintptr]struct{}{}) }

func fromGo(L *lua.LState, v any, depth int, seen map[uintptr]struct{}) lua.LValue {
	switch t := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(t)
	case string:
		if len(t) > MaxStringBytes {
			L.RaiseError("zemi.lua: string exceeds size cap")
			return lua.LNil
		}
		return lua.LString(t)
	case int:
		return lua.LNumber(t)
	case int64:
		return lua.LNumber(t)
	case float64:
		return lua.LNumber(t)
	case float32:
		return lua.LNumber(t)
	case []any:
		defer delete(seen, enterGo(L, v, depth, seen))
		tb := L.CreateTable(len(t), 0)
		for _, item := range t {
			tb.Append(fromGo(L, item, depth+1, seen))
		}
		return tb
	case []string:
		tb := L.CreateTable(len(t), 0)
		for _, item := range t {
			tb.Append(fromGo(L, item, depth+1, seen))
		}
		return tb
	case map[string]any:
		defer delete(seen, enterGo(L, v, depth, seen))
		tb := L.CreateTable(0, len(t))
		for k, item := range t {
			tb.RawSetString(k, fromGo(L, item, depth+1, seen))
		}
		return tb
	case map[string]string:
		tb := L.CreateTable(0, len(t))
		for k, item := range t {
			tb.RawSetString(k, fromGo(L, item, depth+1, seen))
		}
		return tb
	case error:
		return fromGo(L, t.Error(), depth, seen)
	case fmt.Stringer:
		return fromGo(L, t.String(), depth, seen)
	default:
		// a closed set is a named string type (hostapi.RowKind)
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.String {
			return fromGo(L, rv.String(), depth, seen)
		}
		L.RaiseError("zemi.lua: cannot marshal Go value of type %T", v)
		return lua.LNil
	}
}

// enterGo applies the depth cap and cycle refusal to a Go slice or map
// (RaiseError panics) and returns the seen key the caller removes after.
func enterGo(L *lua.LState, v any, depth int, seen map[uintptr]struct{}) uintptr {
	if depth >= MaxTableDepth {
		L.RaiseError("zemi.lua: table nesting exceeds depth cap")
	}
	p := reflect.ValueOf(v).Pointer()
	if _, ok := seen[p]; ok {
		L.RaiseError("zemi.lua: cyclic Go value refused at marshal boundary")
	}
	seen[p] = struct{}{}
	return p
}
