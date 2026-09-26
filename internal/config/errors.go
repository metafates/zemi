package config

import "fmt"

// ErrUnknownKey means the key is not in the Go schema.
type ErrUnknownKey struct{ Path string }

func (e *ErrUnknownKey) Error() string { return "unknown config key: " + e.Path }

// ErrBadValue means the value has the wrong kind or is outside the key's
// choices; Got, set for a refused choice, is echoed back.
type ErrBadValue struct {
	Path string
	Want string
	Got  any
}

func (e *ErrBadValue) Error() string {
	if e.Got != nil {
		return fmt.Sprintf("config key %s expects %s, not %q", e.Path, e.Want, e.Got)
	}
	return fmt.Sprintf("config key %s expects %s", e.Path, e.Want)
}
