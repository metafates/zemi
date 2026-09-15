package hostapi

import (
	glua "github.com/yuin/gopher-lua"
)

func (h *Host) cmdlineNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("open", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.cmdline.open") {
			return 0
		}
		prefix := ":"
		if L.GetTop() >= 1 && L.Get(1) != glua.LNil {
			prefix = L.CheckString(1)
		}
		if ui := h.hooks(); ui != nil {
			ui.OpenCmdline(prefix)
		}
		return 0
	}))

	tb.RawSetString("close", L.NewFunction(func(L *glua.LState) int {
		if ui := h.hooks(); ui != nil {
			ui.CloseCmdline()
		}
		return 0
	}))

	return tb
}
