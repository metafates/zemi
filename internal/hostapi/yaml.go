// yaml.go is zemi.yaml, beside zemi.json: encode a table of values into
// yaml text and decode text back, the round trip a config snippet or a
// handoff to another tool takes. The reader and writer are goccy/go-yaml,
// already the yaml zemi itself speaks.

package hostapi

import (
	"math"

	"github.com/goccy/go-yaml"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// yamlNamespace is zemi.yaml, in config Lua and in a workflow script.
// Encode takes an optional options table speaking indent, indent_sequences
// and literal_multiline; decode takes none: the reader already refuses
// duplicate keys, and an any target has no fields to be strict about.
func (h *Host) yamlNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("encode", L.NewFunction(func(L *glua.LState) int {
		v, err := lua.ToGo(L.CheckAny(1))
		if err != nil {
			L.RaiseError("zemi.yaml.encode: %v", err)
		}
		var opts []yaml.EncodeOption
		if o := codecOpts(L); o != nil {
			checkOpts(L, "zemi.yaml.encode", o, "indent", "indent_sequences", "literal_multiline")
			if n := optInt(L, o, "zemi.yaml.encode", "indent", 0); n != 0 {
				if n < 0 {
					L.RaiseError("zemi.yaml.encode: indent is %d, want 0 or more", n)
				}
				opts = append(opts, yaml.Indent(n))
			}
			if optBool(L, o, "zemi.yaml.encode", "indent_sequences", false) {
				opts = append(opts, yaml.IndentSequence(true))
			}
			if optBool(L, o, "zemi.yaml.encode", "literal_multiline", false) {
				opts = append(opts, yaml.UseLiteralStyleIfMultiline(true))
			}
		}
		b, err := yaml.MarshalWithOptions(yamlNumbers(v), opts...)
		if err != nil {
			L.RaiseError("zemi.yaml.encode: %v", err)
		}
		L.Push(glua.LString(b))
		return 1
	}))
	tb.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		if codecOpts(L) != nil {
			L.RaiseError("zemi.yaml.decode takes no options")
		}
		var v any
		if err := yaml.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
			return luaResult(L, err)
		}
		L.Push(lua.FromGo(L, v))
		return 1
	}))
	return tb
}

// yamlNumbers hands the writer int64 for a whole float64: ToGo makes every
// Lua number a float64 and goccy prints it with a decimal point, where csv
// and json write 1 and a config handoff reads age: 36.0.
func yamlNumbers(v any) any {
	switch v := v.(type) {
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<62 {
			return int64(v)
		}
	case []any:
		for i, e := range v {
			v[i] = yamlNumbers(e)
		}
	case map[string]any:
		for k, e := range v {
			v[k] = yamlNumbers(e)
		}
	}
	return v
}
