package hostapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/tools"
)

// waitHandle is a handle's wait(): it suspends the task until done closes,
// the timeout passes (none when timeout <= 0) or the task ends, and resumes
// it with ret's values (err "timeout" when the cap passed). stop, when set,
// runs off the interpreter unless done closed.
func waitHandle(L *glua.LState, api string, done <-chan struct{}, timeout time.Duration, stop func(), ret func(L *glua.LState, err error) []glua.LValue) int {
	return lua.Suspend(L, api, func(ctx context.Context) func(*glua.LState) []glua.LValue {
		var expired <-chan time.Time
		if timeout > 0 {
			expired = time.After(timeout)
		}
		select {
		case <-done:
			return func(L *glua.LState) []glua.LValue { return ret(L, nil) }
		case <-ctx.Done(): // the task ends; what this returns is never run
		case <-expired:
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
		lua.RaiseScript(L, "%s must be a %s, not a %s", key, t.Type(), v.Type())
	}
	return t, ok
}

// arg is opt for a positional argument: false when it is absent, and a raise
// naming where when it holds another type.
func arg[T glua.LValue](L *glua.LState, n int, where string) (T, bool) {
	v := L.Get(n)
	t, ok := v.(T)
	if !ok && v != glua.LNil {
		lua.RaiseScript(L, "%s: the argument must be a %s, not a %s", where, t.Type(), v.Type())
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

// truncOutput cuts a host tool's output to what a result may carry and
// saves the whole text to pad, the scratchpad of the pipeline that made the
// call, so the rest is one read away instead of gone.
func (h *Host) truncOutput(pad, name, s string) string {
	return tools.Cut(pad, name, s, hostResultMax, false)
}

// truncScriptOutput is truncOutput for a call a lua script drove: the script
// reads the whole output and says what of it the model gets, which is how a
// script's own shell and read calls are already treated.
func (h *Host) truncScriptOutput(p *tools.Pipeline, name, s string) string {
	return tools.Cut(p.Pad, name, s, hostResultMax, p.Script != "")
}

// hostResultMax is what a host tool's result carries of its output.
const hostResultMax = 30000

// upperFirst capitalizes a sentence's first rune, leaving the rest as it is:
// a transcript headline and an overlay's empty-state prose read as sentences.
func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// readRegularFile reads a config file, refusing symlinks and oversized files.
func readRegularFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: not a regular file", path)
	}
	if fi.Size() > maxAgentFileBytes {
		return "", fmt.Errorf("%s: exceeds %d bytes", path, maxAgentFileBytes)
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

// readFrontmatterFile reads a markdown file with an optional leading
// `---` yaml block (symlinks and oversized files refused).
func readFrontmatterFile(path string) (block, body string, err error) {
	data, err := readRegularFile(path)
	if err != nil {
		return "", "", err
	}
	block, body, err = skill.SplitFrontmatter(data)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	return block, body, nil
}

// appendLast appends v and keeps the newest n: the recent-entry logs.
func appendLast[T any](s []T, v T, n int) []T {
	s = append(s, v)
	return s[max(0, len(s)-n):]
}

// luaErrText is a Lua error's message without its traceback.
func luaErrText(err error) string {
	if ae, ok := errors.AsType[*glua.ApiError](err); ok {
		return ae.Object.String()
	}
	return err.Error()
}

// errText drops a Lua error's traceback, which names Go and temp frames.
func errText(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(luaErrText(err))
}

// luaLiteral writes a string with only the escapes Lua 5.1 reads; other
// bytes, UTF-8 included, stay as they are. A number is written whole, 1000000
// and never 1e+06.
func luaLiteral(v any) string {
	if list, ok := v.([]string); ok {
		parts := make([]string, len(list))
		for i, it := range list {
			parts[i] = luaLiteral(it)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	s, ok := v.(string)
	if f, isNum := v.(float64); isNum {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	if !ok {
		return fmt.Sprint(v)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range []byte(s) {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < ' ' || c == 0x7f:
			fmt.Fprintf(&b, `\%03d`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// openInEditorLabel is the row action every list zemi keeps of files you can
// edit offers: the verb's own open command puts it in the editor.
const openInEditorLabel = "open in the editor"
