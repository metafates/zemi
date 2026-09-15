package hostapi

import (
	"fmt"
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
	"ui.resize": true, "ui.theme": true, "overlay.open": true,
	"overlay.close": true, "composer.change": true, "composer.submit": true,
	"memory.written": true, "skill.learned": true,
	"config.proposed": true, "config.applied": true,
	"reflector.start": true, "reflector.done": true,
	"job.exit": true, "timer.fire": true, "fs.changed": true,
}

func knownEvent(name string) bool {
	return eventCatalogue[name] || strings.HasPrefix(name, "user.")
}

func (h *Host) eventNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("on", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.event.on") {
			return 0
		}
		name := L.CheckString(1)
		fn := L.CheckFunction(2)
		if !knownEvent(name) {
			L.RaiseError("zemi.event.on: unknown event %q (v1 catalogue or user.*)", name)
			return 0
		}
		hd := &EventHandler{
			Event:  name,
			Fn:     fn,
			Origin: h.RT.CurrentOrigin(),
			Path:   h.RT.CurrentPath(),
			Gen:    h.currentGen(),
		}
		if L.GetTop() >= 3 && L.Get(3) != glua.LNil {
			opts, ok := L.Get(3).(*glua.LTable)
			if !ok {
				L.RaiseError("zemi.event.on: opts must be a table")
				return 0
			}
			if id, ok := opts.RawGetString("id").(glua.LString); ok {
				hd.ID = string(id)
			}
			if c, ok := opts.RawGetString("coalesce").(glua.LBool); ok {
				hd.Coalesce = bool(c)
			}
			hd.Once = rawBool(opts, "once")
		}
		added, err := h.Events.Add(hd)
		if err != nil {
			L.RaiseError("zemi.event.on: %v", err)
			return 0
		}
		L.Push(glua.LString(added.ID))
		return 1
	}))

	// off(id) removes a handler registered by this origin or a lower-trust
	// one; it returns whether a handler was removed.
	tb.RawSetString("off", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.event.off") {
			return 0
		}
		ok, err := h.Events.Remove(L.CheckString(1), h.RT.CurrentOrigin())
		if err != nil {
			L.RaiseError("zemi.event.off: %v", err)
			return 0
		}
		L.Push(glua.LBool(ok))
		return 1
	}))

	tb.RawSetString("emit", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.event.emit") {
			return 0
		}
		name := L.CheckString(1)
		if !strings.HasPrefix(name, "user.") {
			L.RaiseError("zemi.event.emit: only user.* events can be emitted from Lua")
			return 0
		}
		payload := map[string]any{}
		if L.GetTop() >= 2 && L.Get(2) != glua.LNil {
			v, err := lua.ToGo(L.Get(2))
			if err != nil {
				L.RaiseError("zemi.event.emit: %v", err)
				return 0
			}
			if m, ok := v.(map[string]any); ok {
				payload = m
			}
		}
		handlers := h.Events.Handlers(name)
		if len(handlers) == 0 {
			return 0
		}
		from := string(h.RT.CurrentOrigin())
		h.emitSync(L, handlers, name, payload, from)
		return 0
	}))

	return tb
}

// CheckEventName validates a host-side emission target.
func CheckEventName(name string) error {
	if !knownEvent(name) {
		return fmt.Errorf("unknown event %q", name)
	}
	return nil
}
