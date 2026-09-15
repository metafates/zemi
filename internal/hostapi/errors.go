package hostapi

import "errors"

// ErrReserved marks a Go-reserved action that no origin may bind or
// dispatch through Lua (DESIGN §6.2: cancel trigger and :Reload).
var ErrReserved = errors.New("zemi: action is Go-reserved and cannot be bound or executed from Lua")
