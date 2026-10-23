package hostapi

import (
	"maps"
	"slices"
	"strings"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/set"
)

// Host events Go code emits or checks by name, outside hostapi too.
const (
	EventAppReady            = "app.ready"
	EventAppQuit             = "app.quit"
	EventConfigReloaded      = "config.reloaded"
	EventSessionNew          = "session.new"
	EventSessionOpen         = "session.open"
	EventSessionClose        = "session.close"
	EventAgentStart          = "agent.start"
	EventAgentFinish         = "agent.finish"
	EventAgentError          = "agent.error"
	EventAgentAssemble       = "agent.assemble"
	EventAgentSubagentStart  = "agent.subagent.start"
	EventAgentSubagentFinish = "agent.subagent.finish"
	EventAgentTextDelta      = "agent.text_delta"
	EventAgentReasoningDelta = "agent.reasoning_delta"
	EventAgentStep           = "agent.step"
	EventToolPre             = "tool.pre"
	EventToolPost            = "tool.post"
	EventToolDenied          = "tool.denied"
	EventPermissionRequest   = "permission.request"
	EventPermissionResolved  = "permission.resolved"
	EventUIResize            = "ui.resize"
	EventComposerChange      = "composer.change"
	EventComposerSubmit      = "composer.submit"
	EventMemoryWritten       = "memory.written"
	EventReflectorStart      = "reflector.start"
	EventReflectorDone       = "reflector.done"
	EventJobExit             = "job.exit"
	EventLSPRequest          = "lsp.request"
)

// eventCatalogue is the v1 event list (DESIGN §6.2). Lua subscribes to
// these or to `user.*` names; host-owned prefixes cannot be emitted.
var eventCatalogue = set.Of(
	EventAppReady, EventAppQuit, EventConfigReloaded,
	EventSessionNew, EventSessionOpen, EventSessionClose,
	EventAgentStart, EventAgentFinish, EventAgentError,
	EventAgentAssemble, EventAgentSubagentStart, EventAgentSubagentFinish,
	EventAgentTextDelta, EventAgentReasoningDelta, EventAgentStep,
	EventToolPre, EventToolPost, EventToolDenied,
	EventPermissionRequest, EventPermissionResolved,
	EventUIResize, EventComposerChange, EventComposerSubmit,
	EventMemoryWritten,
	EventReflectorStart, EventReflectorDone,
	EventJobExit,
	EventLSPRequest,
)

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
		if !eventCatalogue.Has(name) && !strings.HasPrefix(name, "user.") {
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
		if name == EventPermissionRequest {
			h.requirePermUse(L, "zemi.event.on")
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
	tb.RawSetString("list", L.NewFunction(h.eventLogFn))
	tb.RawSetString("stats", L.NewFunction(h.eventStatsFn))

	return tb
}
