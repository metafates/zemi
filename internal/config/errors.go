package config

import "fmt"

// ErrUnknownKey means the key is not in the Go schema.
type ErrUnknownKey struct{ Path string }

func (e *ErrUnknownKey) Error() string { return "unknown config key: " + e.Path }

// ErrNotWritable means the key exists but is not user-writable.
type ErrNotWritable struct{ Path string }

func (e *ErrNotWritable) Error() string { return "config key is not writable from Lua: " + e.Path }

// ErrBadValue means the value type does not match the schema.
type ErrBadValue struct {
	Path string
	Want string
}

func (e *ErrBadValue) Error() string {
	return fmt.Sprintf("config key %s expects %s", e.Path, e.Want)
}
