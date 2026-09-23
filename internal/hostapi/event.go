package hostapi

import (
	"maps"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// eventCatalogue is the v1 event list (DESIGN §6.2). Lua subscribes to
// these or to `user.*` names; host-owned prefixes cannot be emitted.
var eventCatalogue = map[string]bool{
	"app.ready": true, "app.quit": true, "config.reloaded": true,
	"session.new": true, "session.open": true, "session.close": true,
	"agent.start": true, "agent.finish": true, "agent.error": true,
	"agent.assemble": true, "agent.subagent.start": true, "agent.subagent.finish": true,
	"agent.text_delta": true, "agent.reasoning_delta": true,
	"tool.pre": true, "tool.post": true, "tool.denied": true,
	"permission.request": true, "permission.resolved": true,
	"ui.resize": true, "ui.theme": true, "composer.change": true, "composer.submit": true,
	"memory.written":  true,
	"reflector.start": true, "reflector.done": true,
	"job.exit":    true,
	"lsp.request": true,
}

func knownEvent(name string) bool {
	return eventCatalogue[name] || strings.HasPrefix(name, "user.")
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
		if !knownEvent(name) {
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
			hd.ID, hd.Once = rawString(opts, "id"), rawBool(opts, "once")
		}
		added, err := h.Events.Add(hd)
		if err != nil {
			L.RaiseError("zemi.event.on: %v", err)
		}
		L.Push(glua.LString(added.ID))
		return 1
	}))

	// off(id) removes a handler registered by this origin or a lower-trust
	// one; it returns whether a handler was removed.
	tb.RawSetString("off", L.NewFunction(func(L *glua.LState) int {
		ok, err := h.Events.Remove(L.CheckString(1), h.RT.CurrentOrigin(L))
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
			if m, ok := v.(map[string]any); ok {
				payload = m
			}
		}
		from := string(h.RT.CurrentOrigin(L))
		h.recordEvent(name, from, payload)
		handlers := h.Events.Handlers(name)
		if len(handlers) == 0 {
			return 0
		}
		h.emitSync(L, handlers, name, payload, from)
		return 0
	}))
	tb.RawSetString("log", L.NewFunction(h.eventLogFn))
	tb.RawSetString("stats", L.NewFunction(h.eventStatsFn))

	return tb
}
