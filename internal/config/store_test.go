package config

import (
	"reflect"
	"testing"
	"time"
)

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

// A duration key resolves "" and a stale value of another kind to its
// schema default, and 0 means off.
func TestStoreDuration(t *testing.T) {
	s := NewStore()
	if got := s.Duration(KeySessionRecap); got != 3*time.Minute {
		t.Fatalf("the default recap delay = %v", got)
	}
	if _, err := s.SetQuiet(KeySessionRecap, "0"); err != nil {
		t.Fatal(err)
	}
	if got := s.Duration(KeySessionRecap); got != 0 {
		t.Fatalf("0 = %v, want off", got)
	}
	s.values[KeySessionRecap] = false // a pick kept before the key held a duration
	if got := s.Duration(KeySessionRecap); got != 3*time.Minute {
		t.Fatalf("a stale bool pick = %v, want the default", got)
	}
	if got := s.Duration("session.nonexistent"); got != 0 {
		t.Fatalf("an unknown key = %v", got)
	}
}

// A list key stores []string whatever handed it the entries (Lua and JSON
// both hand []any), and a model key refuses a role name where an id belongs.
func TestStoreKinds(t *testing.T) {
	cases := []struct {
		name string
		key  Key
		val  any
		want any
		bad  bool
	}{
		{"list from lua", KeyModelsFavorites, []any{"a/b", "c/d"}, []string{"a/b", "c/d"}, false},
		{"list from json", KeyModelsFavorites, []string{"a/b"}, []string{"a/b"}, false},
		{"list refuses a non-string entry", KeyModelsFavorites, []any{"a/b", 2}, nil, true},
		{"a model key takes an id", KeyModelsCompact, "a/b", "a/b", false},
		{"a model key takes empty", KeyModelsCompact, "", "", false},
		{"a model key refuses a role", KeyModelsCompact, "agents", nil, true},
		{"a duration takes a go duration", KeySessionRecap, "90s", "90s", false},
		{"a duration takes empty for the default", KeySessionRecap, "", "", false},
		{"a duration takes 0 for off", KeySessionRecap, "0", "0", false},
		{"a duration refuses a word", KeySessionRecap, "soon", nil, true},
		{"a duration refuses a number", KeySessionRecap, 120, nil, true},
	}
	s := NewStore()
	for _, c := range cases {
		got, err := s.Check(c.key, c.val)
		if c.bad != (err != nil) || !c.bad && !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Check = %v, %v; want %v, bad %v", c.name, got, err, c.want, c.bad)
		}
	}
}

// Only a models.<role> key names a model slot: favorites is a list key.
func TestKeyFamilies(t *testing.T) {
	if role, ok := ModelRole(KeyModelsCompact); !ok || role != "compact" {
		t.Fatalf("ModelRole(models.compact) = %q, %v", role, ok)
	}
	if role, ok := ModelRole(KeyModelsFavorites); ok {
		t.Fatalf("favorites names slot %q", role)
	}
	if name, ok := PluginOf(PluginKey("jira", "url")); !ok || name != "jira" {
		t.Fatalf("PluginOf = %q, %v", name, ok)
	}
	if _, ok := PluginOf(KeyUITheme); ok {
		t.Fatal("a builtin key names a plugin")
	}
}
