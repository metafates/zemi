package config

import "fmt"

// ErrUnknownKey means the key is not in the Go schema.
type ErrUnknownKey struct{ Path string }

func (e *ErrUnknownKey) Error() string { return "unknown config key: " + e.Path }

// ErrBadValue means the value type does not match the schema.
type ErrBadValue struct {
	Path string
	Want string
}

func (e *ErrBadValue) Error() string {
	return fmt.Sprintf("config key %s expects %s", e.Path, e.Want)
}
