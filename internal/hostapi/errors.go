package hostapi

import "errors"

// ErrReserved marks a Go-reserved action that no origin may bind or
// dispatch through Lua (DESIGN §6.2: cancel trigger and :Reload).
var ErrReserved = errors.New("zemi: action is Go-reserved and cannot be bound or executed from Lua")

// ErrNeedsTUI is what a command that opens a screen returns with none to
// draw on (-p, --health); RunCommand puts the command's name in front.
var ErrNeedsTUI = errors.New("needs the TUI")
