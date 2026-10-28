// yaml.go is zemi.yaml, beside zemi.json: encode a table of values into
// yaml text and decode text back, the round trip a config snippet or a
// handoff to another tool takes. The reader and writer are goccy/go-yaml,
// already the yaml zemi itself speaks.

package hostapi

import (
	"github.com/goccy/go-yaml"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// yamlNamespace is zemi.yaml, in config Lua and in a workflow script. Both
// calls take an optional options table: encode speaks indent,
// indent_sequences and literal_multiline; decode speaks strict.
func (h *Host) yamlNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("encode", L.NewFunction(func(L *glua.LState) int {
		v, err := lua.ToGo(L.CheckAny(1))
		if err != nil {
			L.RaiseError("zemi.yaml.encode: %v", err)
		}
		var opts []yaml.EncodeOption
		if o := codecOpts(L); o != nil {
			if n := optInt(L, o, "indent", 0); n != 0 {
				opts = append(opts, yaml.Indent(n))
			}
			if optBool(L, o, "indent_sequences", false) {
				opts = append(opts, yaml.IndentSequence(true))
			}
			if optBool(L, o, "literal_multiline", false) {
				opts = append(opts, yaml.UseLiteralStyleIfMultiline(true))
			}
		}
		b, err := yaml.MarshalWithOptions(v, opts...)
		if err != nil {
			L.RaiseError("zemi.yaml.encode: %v", err)
		}
		L.Push(glua.LString(b))
		return 1
	}))
	tb.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		var opts []yaml.DecodeOption
		if optBool(L, codecOpts(L), "strict", false) {
			opts = append(opts, yaml.Strict())
		}
		var v any
		if err := yaml.UnmarshalWithOptions([]byte(L.CheckString(1)), &v, opts...); err != nil {
			return luaResult(L, err)
		}
		L.Push(lua.FromGo(L, v))
		return 1
	}))
	return tb
}
