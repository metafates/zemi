// learnapi.go is the zemi.learn namespace (DESIGN §6.7, §10): proposals and
// decide cover memory proposals. decide needs user or builtin origin: the
// model can propose, never decide.
package hostapi

import (
	glua "github.com/yuin/gopher-lua"
)

func (h *Host) learnNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("proposals", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, m := range h.memoryProposals() {
			row := L.NewTable()
			row.RawSetString("id", glua.LString(m.ID))
			row.RawSetString("kind", glua.LString(m.Kind))
			row.RawSetString("scope", glua.LString(m.Scope))
			row.RawSetString("origin", glua.LString(m.Origin))
			row.RawSetString("text", glua.LString(m.Text))
			tags := L.NewTable()
			for _, t := range m.Tags {
				tags.Append(glua.LString(t))
			}
			row.RawSetString("tags", tags)
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))

	tb.RawSetString("decide", L.NewFunction(func(L *glua.LState) int {
		id := L.CheckString(1)
		action := L.CheckString(2)
		if !memoryManageAllowed(h.RT.CurrentOrigin()) {
			L.RaiseError("zemi.learn.decide: only user or builtin origin may decide proposals")
		}
		if action != "accept" && action != "reject" {
			L.RaiseError("zemi.learn.decide: action must be accept or reject")
		}
		res := h.takeProposal(id)
		if res == nil {
			L.RaiseError("zemi.learn.decide: no such proposal %q", id)
		}
		if action == "reject" {
			h.Notify("info", "learn", "proposal rejected")
			return 0
		}
		if err := h.storeProposal(res); err != nil {
			L.RaiseError("zemi.learn.decide: %v", err)
		}
		h.Notify("info", "learn", "stored: "+res.Memory.Text)
		return 0
	}))

	return tb
}
