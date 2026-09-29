package hostapi

import (
	"errors"
	"fmt"
)

// errReserved marks a Go-reserved action that no origin may bind or
// dispatch through Lua (DESIGN §6.2: cancel trigger and :Reload).
var errReserved = errors.New("zemi: action is Go-reserved and cannot be bound or executed from Lua")

// ErrNeedsTUI is what a command that opens a screen returns with none to
// draw on (zemi run); RunCommand puts the command's name in front.
var ErrNeedsTUI = errors.New("needs the TUI")

// ErrUsage marks a command error that is how it was asked: a bad argument,
// nothing to act on, or a state the message names. CommandFailed warns.
var ErrUsage = errors.New("usage")

type usageError struct{ error }

func (usageError) Is(target error) bool { return target == ErrUsage }
func (e usageError) Unwrap() error      { return e.error }

// Usagef is fmt.Errorf for an ErrUsage error; the text leaves the sentinel out.
func Usagef(format string, a ...any) error { return usageError{fmt.Errorf(format, a...)} }
