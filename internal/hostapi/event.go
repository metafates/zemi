package hostapi

import (
	"maps"
	"slices"
	"strings"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// Host events Go code emits or checks by name, outside hostapi too.
const (
	EventAppReady          = "app.ready"
	EventConfigReloaded    = "config.reloaded"
	EventAppQuit           = "app.quit"
	EventSessionNew        = "session.new"
	EventComposerSubmit    = "composer.submit"
	EventToolPre           = "tool.pre"
	EventToolPost          = "tool.post"
	EventPermissionRequest = "permission.request"
)

// eventCatalogue is the v1 event list (DESIGN §6.2). Lua subscribes to
// these or to `user.*` names; host-owned prefixes cannot be emitted.
var eventCatalogue = map[string]bool{
	EventAppReady: true, EventAppQuit: true, EventConfigReloaded: true,
	EventSessionNew: true, "session.open": true, "session.close": true,
	"agent.start": true, "agent.finish": true, "agent.error": true,
	"agent.assemble": true, "agent.subagent.start": true, "agent.subagent.finish": true,
	"agent.text_delta": true, "agent.reasoning_delta": true, "agent.step": true,
	EventToolPre: true, EventToolPost: true, "tool.denied": true,
	EventPermissionRequest: true, "permission.resolved": true,
	"ui.resize": true, "composer.change": true, EventComposerSubmit: true,
	"memory.written":  true,
	"reflector.start": true, "reflector.done": true,
	"job.exit":    true,
	"lsp.request": true,
}

// eventNames lists the catalogue for an error message: a near miss
// ("agent.done") is fixed from the list without a doc lookup.
func eventNames() string {
	return strings.Join(slices.Sorted(maps.Keys(eventCatalogue)), ", ")
}

func (h *Host) eventNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("on", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		fn := L.CheckFunction(2)
		if !eventCatalogue[name] && !strings.HasPrefix(name, "user.") {
			L.RaiseError("zemi.event.on: unknown event %q (%s, or a user.* name)", name, eventNames())
		}
		hd := &EventHandler{
			Event:  name,
			Fn:     fn,
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
		}
		if opts := L.OptTable(3, nil); opts != nil {
			hd.ID, hd.Once = rawString(L, opts, "id"), rawBool(L, opts, "once")
		}
		added, err := h.Events.Add(hd)
		if err != nil {
			L.RaiseError("zemi.event.on: %v", err)
		}
		L.Push(glua.LString(added.ID))
		return 1
	}))

	// off(id) removes a handler the caller may replace (mayReplace); it
	// returns whether a handler was removed.
	tb.RawSetString("off", L.NewFunction(func(L *glua.LState) int {
		ok, err := h.Events.Remove(L.CheckString(1), h.RT.CurrentOrigin(L), h.RT.CurrentPath(L))
		if err != nil {
			L.RaiseError("zemi.event.off: %v", err)
		}
		L.Push(glua.LBool(ok))
		return 1
	}))

	tb.RawSetString("emit", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		if !strings.HasPrefix(name, "user.") {
			L.RaiseError("zemi.event.emit: only user.* events can be emitted from Lua")
		}
		payload := map[string]any{}
		if L.Get(2) != glua.LNil {
			v, err := lua.ToGo(L.Get(2))
			if err != nil {
				L.RaiseError("zemi.event.emit: %v", err)
			}
			// a sequence marshals to []any, and eventTable hands handlers
			// string-keyed fields: its values would vanish without a word
			m, ok := v.(map[string]any)
			if !ok {
				L.RaiseError("zemi.event.emit: payload is a table with string keys")
			}
			payload = m
		}
		from := string(h.RT.CurrentOrigin(L))
		h.recordEvent(name, from, payload)
		h.emitSync(L, h.Events.Handlers(name), name, payload, from)
		return 0
	}))
	tb.RawSetString("log", L.NewFunction(h.eventLogFn))
	tb.RawSetString("stats", L.NewFunction(h.eventStatsFn))

	return tb
}
