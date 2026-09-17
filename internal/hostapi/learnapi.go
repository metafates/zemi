// learnapi.go is the zemi.learn namespace (DESIGN §6.7, §10): proposals and
// decide cover memory proposals. decide needs user or builtin origin: the
// model can propose, never decide.
package hostapi

import (
	"fmt"

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
		h.refuseBelowUser(L, "learn.decide")
		if action != "accept" && action != "reject" {
			L.RaiseError("zemi.learn.decide: action must be accept or reject")
		}
		if action == "reject" {
			if h.takeProposal(id) == nil {
				return luaResult(L, fmt.Errorf("no such proposal %q", id))
			}
			h.Notify("info", "Suggestion dropped", "")
			return luaResult(L, nil)
		}
		res, err := h.acceptProposal(id, "")
		if err != nil {
			return luaResult(L, err)
		}
		h.Notify("info", "Remembered", res.Memory.Text)
		return luaResult(L, nil)
	}))

	return tb
}
