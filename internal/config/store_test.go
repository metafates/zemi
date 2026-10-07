package config

import "testing"

// A reader of a path outside the schema gets the zero value: every writer
// (Add, Check, SetQuiet, AddWatch) refuses an unknown key, so this is the
// one silent hole left, and it is what a mistyped literal reads as.
func TestStoreReadUnknownKey(t *testing.T) {
	s := NewStore()
	if got := s.Str("ui.theme"); got != "whorl" {
		t.Fatalf("a schema key holds its default: %q", got)
	}
	if got := s.Str("ui.theemes"); got != "" {
		t.Fatalf("unknown string key = %q", got)
	}
	if got := s.Bool("ui.theemes"); got {
		t.Fatal("unknown bool key is not false")
	}
	if got := s.Int("ui.theemes"); got != 0 {
		t.Fatalf("unknown number key = %d", got)
	}
}
