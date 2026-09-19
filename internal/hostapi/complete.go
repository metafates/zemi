package hostapi

import (
	"context"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	glua "github.com/yuin/gopher-lua"
)

// CompletionItem is one row a Lua completion source offers.
type CompletionItem struct{ Text, Desc string }

// completionSource is a zemi.composer.complete registration: it completes
// the word after trigger, or the arguments of command.
type completionSource struct {
	id, trigger, command string
	fn                   *glua.LFunction
	origin               lua.Origin
	path                 string
	gen                  Gen
}

type completeReg struct {
	mu   sync.Mutex
	list []completionSource
}

func (r *completeReg) purge() {
	r.mu.Lock()
	r.list = nil
	r.mu.Unlock()
}

// maxCompletions caps what the sources for one popup offer together.
const maxCompletions = 50

// HasCompletions reports whether a Lua source completes after trigger, or
// the arguments of command, so the UI calls Lua only when one does.
func (h *Host) HasCompletions(trigger, command string) bool {
	h.completions.mu.Lock()
	defer h.completions.mu.Unlock()
	return slices.ContainsFunc(h.completions.list, func(s completionSource) bool {
		return s.trigger == trigger && strings.EqualFold(s.command, command)
	})
}

// Complete runs the Lua sources for trigger or command, each under its
// origin, and returns their sanitized items in registration order. A
// failing source offers nothing and lands in /errors. It waits on the interpreter: never call it
// on the UI goroutine.
func (h *Host) Complete(ctx context.Context, trigger, command, lead, line string) []CompletionItem {
	h.completions.mu.Lock()
	var srcs []completionSource
	for _, s := range h.completions.list {
		if s.trigger == trigger && strings.EqualFold(s.command, command) {
			srcs = append(srcs, s)
		}
	}
	h.completions.mu.Unlock()
	if len(srcs) == 0 {
		return nil
	}
	var out []CompletionItem
	_, _ = h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		for _, s := range srcs {
			if s.gen != h.currentGen() || len(out) >= maxCompletions {
				continue
			}
			items, err := h.callSource(L, s, lead, line)
			if err != nil {
				h.Log.Error("ui", "completion source failed", map[string]any{"id": s.id, "error": err.Error()})
				h.RecordError("completion "+s.id, err.Error())
				continue
			}
			out = append(out, items...)
		}
		return nil, nil
	})
	return out[:min(len(out), maxCompletions)]
}

func (h *Host) callSource(L *glua.LState, s completionSource, lead, line string) ([]CompletionItem, error) {
	defer h.RT.PushFrame(L, s.origin, s.path)()
	base := L.GetTop()
	defer L.SetTop(base)
	L.Push(s.fn)
	L.Push(glua.LString(lead))
	L.Push(glua.LString(line))
	if err := L.PCall(2, 1, nil); err != nil {
		return nil, err
	}
	v, err := lua.ToGo(L.Get(-1))
	if err != nil {
		return nil, err
	}
	return completionItems(v), nil
}

// completionItems reads what a completer returned: a list of strings or
// { text, desc } rows, sanitized, rows without text dropped.
func completionItems(v any) []CompletionItem {
	list, _ := v.([]any)
	var out []CompletionItem
	for _, item := range list {
		switch it := item.(type) {
		case string:
			out = append(out, CompletionItem{Text: sanitize.Line(it)})
		case map[string]any:
			text, _ := it["text"].(string)
			desc, _ := it["desc"].(string)
			out = append(out, CompletionItem{Text: sanitize.Line(text), Desc: sanitize.Line(desc)})
		}
	}
	return slices.DeleteFunc(out, func(c CompletionItem) bool { return c.Text == "" })
}

// addCompletion is zemi.composer.complete(spec).
func (h *Host) addCompletion(L *glua.LState) int {
	h.refuseDuringRender(L, "zemi.composer.complete")
	spec := L.CheckTable(1)
	fn, _ := spec.RawGetString("items").(*glua.LFunction)
	s := completionSource{
		id: rawString(spec, "id"), trigger: rawString(spec, "trigger"), command: rawString(spec, "command"),
		fn: fn, origin: h.RT.CurrentOrigin(L), path: h.RT.CurrentPath(L), gen: h.currentGen(),
	}
	r, size := utf8.DecodeRuneInString(s.trigger)
	switch {
	case s.id == "" || fn == nil:
		L.RaiseError("zemi.composer.complete: id and items are required")
	case (s.trigger == "") == (s.command == ""):
		L.RaiseError("zemi.composer.complete: set trigger or command, not both")
	case s.trigger != "" && (size != len(s.trigger) || !unicode.IsPunct(r) && !unicode.IsSymbol(r) || r == '/'):
		L.RaiseError("zemi.composer.complete: trigger must be one punctuation character other than /, got %q", s.trigger)
	}
	reg := &h.completions
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if i := slices.IndexFunc(reg.list, func(o completionSource) bool { return o.id == s.id }); i >= 0 {
		if reg.list[i].origin.Rank() > s.origin.Rank() {
			L.RaiseError("zemi.composer.complete: source %q is registered by %s config", s.id, reg.list[i].origin)
		}
		reg.list[i] = s
		return 0
	}
	reg.list = append(reg.list, s)
	return 0
}
