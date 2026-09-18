package hostapi

import (
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// Registries accept any non-nil function at the Go level (real guards live
// in the host API layer); an empty LFunction satisfies the shape checks.
var noopFn = &glua.LFunction{}

func bind(key, focus string, origin lua.Origin) *KeyBinding {
	return &KeyBinding{Key: key, Focus: focus, RHS: RHS{API: "zemi.ui.help"}, Origin: origin}
}

// TestKeymapRegistry: set rebinds within and above an origin, lower trust is
// refused for set and del, a focused binding cannot take what a more trusted
// global one holds, lookup prefers the focused binding unless a more trusted
// global one holds the key, and list is sorted.
func TestKeymapRegistry(t *testing.T) {
	r := NewKeymapRegistry()
	for _, s := range []struct {
		name, key, focus string
		origin           lua.Origin
		ok               bool
	}{
		{"builtin default", "tab", "", lua.OriginBuiltin, true},
		{"user overrides builtin", "tab", "", lua.OriginUser, true},
		{"project cannot override user", "tab", "", lua.OriginProject, false},
		{"builtin cannot override user", "tab", "", lua.OriginBuiltin, false},
		{"plugin key", "x", "", lua.OriginPlugin, true},
		{"plugin rebinds its own", "x", "", lua.OriginPlugin, true},
		{"user sequence", "ctrl+g s", "", lua.OriginUser, true},
		{"project cannot bind a user sequence's prefix", "ctrl+g", "", lua.OriginProject, false},
		{"project cannot extend a user key", "tab x", "", lua.OriginProject, false},
		{"empty key", "", "", lua.OriginUser, false},
		{"plugin focused key", "k", "todo", lua.OriginPlugin, true},
		{"plugin cannot focus-bind a user global key", "tab", "todo", lua.OriginPlugin, false},
		{"plugin cannot focus-bind a user sequence's prefix", "ctrl+g", "todo", lua.OriginPlugin, false},
		{"user focused key over its own global one", "tab", "todo", lua.OriginUser, true},
		{"user global key over a plugin focused one", "k", "", lua.OriginUser, true},
		{"project focused key in another node", "tab", "tree", lua.OriginProject, false},
	} {
		if err := r.Set(bind(s.key, s.focus, s.origin)); (err == nil) != s.ok {
			t.Fatalf("%s: err=%v", s.name, err)
		}
	}
	for _, l := range []struct {
		focus, key string
		want       lua.Origin // "" for no binding
		wantFocus  string
	}{
		{"", "tab", lua.OriginUser, ""},
		{"todo", "tab", lua.OriginUser, "todo"},
		{"tree", "tab", lua.OriginUser, ""},
		{"todo", "k", lua.OriginUser, ""}, // the more trusted global binding wins
		{"", "j", "", ""},
	} {
		got := r.Lookup(l.focus, l.key)
		if l.want == "" && got != nil || l.want != "" && (got == nil || got.Origin != l.want || got.Focus != l.wantFocus) {
			t.Fatalf("Lookup(%q, %q) = %+v", l.focus, l.key, got)
		}
	}
	if !r.IsPrefix("todo", "ctrl+g") || r.IsPrefix("todo", "k") {
		t.Fatal("IsPrefix should see global sequences from a focused node")
	}
	if all := r.List(); len(all) != 6 || all[3].Key != "tab" || all[4].Focus != "todo" {
		t.Fatalf("List = %+v", all)
	}
	for _, d := range []struct {
		name, key, focus string
		origin           lua.Origin
		removed, wantErr bool
	}{
		{"plugin cannot delete a user key", "tab", "", lua.OriginPlugin, false, true},
		{"user deletes a plugin key", "x", "", lua.OriginUser, true, false},
		{"deleting twice is not an error", "x", "", lua.OriginUser, false, false},
		{"user deletes its own", "tab", "", lua.OriginUser, true, false},
		{"a focused key is not the global one", "k", "tree", lua.OriginUser, false, false},
		{"user deletes a plugin focused key", "k", "todo", lua.OriginUser, true, false},
	} {
		removed, err := r.Del(d.origin, d.focus, d.key)
		if removed != d.removed || (err != nil) != d.wantErr {
			t.Fatalf("%s: removed=%v err=%v", d.name, removed, err)
		}
	}
	r.Set(bind("q", "", lua.OriginUser))
	r.Purge()
	if len(r.List()) != 0 {
		t.Fatal("Purge must clear all bindings")
	}
}

func TestCommandCaseCollision(t *testing.T) {
	r := NewCommandRegistry()

	add := func(name string, origin lua.Origin) (*Command, error) {
		return r.Add(&Command{Name: name, Fn: noopFn, Origin: origin})
	}

	// The reserved-name guard chain: a plugin cannot shadow :Reload via a
	// case variant; only identical-name same-or-higher trust replaces.
	if _, err := add("Reload", lua.OriginBuiltin); err != nil {
		t.Fatalf("builtin :Reload: %v", err)
	}
	if _, err := add("reload", lua.OriginPlugin); err == nil {
		t.Fatal(":reload must collide with :Reload")
	}
	if _, err := add("RELOAD", lua.OriginUser); err == nil {
		t.Fatal(":RELOAD must collide with :Reload even at user rank")
	} else if !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("equal-or-higher case-collision error text = %v", err)
	}
	if got := r.Lookup("reload"); got.Name != "Reload" || got.Origin != lua.OriginBuiltin {
		t.Fatalf("case-collision attempt changed the command: %+v", got)
	}
	if got := r.Lookup("ReLoAd"); got == nil || got.Name != "Reload" {
		t.Fatalf("Lookup must be case-insensitive: %+v", got)
	}

	// Equal rank, identical name: LWW replaces.
	replacement := &Command{Name: "Reload", GoFn: func(*Host, string) error { return nil }, Origin: lua.OriginBuiltin}
	if _, err := r.Add(replacement); err != nil {
		t.Fatalf("same-origin identical-name re-add: %v", err)
	}
	if got := r.Lookup("reload"); got.GoFn == nil {
		t.Fatal("identical-name replacement did not apply")
	}

	// Lower trust cannot replace.
	if _, err := add("Reload", lua.OriginProject); err == nil {
		t.Fatal("lower-trust re-registration must be refused")
	}

	// Higher trust identical name may replace.
	if _, err := add("Reload", lua.OriginUser); err != nil {
		t.Fatalf("user replacement of builtin command: %v", err)
	}
	if got := r.Lookup("RELOAD"); got.Origin != lua.OriginUser {
		t.Fatalf("user replacement did not apply: %+v", got)
	}

	if _, err := add("", lua.OriginUser); err == nil {
		t.Fatal("empty name must be refused")
	}
}

func TestWidgetRegistry(t *testing.T) {
	r := NewWidgetRegistry()
	mk := func(id string, origin lua.Origin) *Widget {
		return &Widget{ID: id, Region: "statusline.left", Render: noopFn, Origin: origin, Priority: 5}
	}

	if _, err := r.Add(mk("clock", lua.OriginBuiltin)); err != nil {
		t.Fatalf("builtin widget: %v", err)
	}
	// Cross-origin id collision: project loses to builtin.
	if _, err := r.Add(mk("clock", lua.OriginProject)); err == nil {
		t.Fatal("project widget must lose to builtin of the same id")
	}
	// User replaces builtin.
	userClock := mk("clock", lua.OriginUser)
	userClock.Priority = 1
	if _, err := r.Add(userClock); err != nil {
		t.Fatalf("user widget replace: %v", err)
	}
	if got := r.Get("clock"); got.Priority != 1 {
		t.Fatalf("user replacement did not apply: %+v", got)
	}
	// Plugin cannot replace user.
	if _, err := r.Add(mk("clock", lua.OriginPlugin)); err == nil {
		t.Fatal("plugin widget must lose to user of the same id")
	}

	// Required fields.
	if _, err := r.Add(&Widget{ID: "x", Region: "r"}); err == nil {
		t.Fatal("missing render must be refused")
	}
	if _, err := r.Add(&Widget{ID: "x", Render: noopFn}); err == nil {
		t.Fatal("missing region must be refused")
	}

	// InRegion orders by priority ascending, ties by id, stable across calls.
	lo := mk("lo", lua.OriginUser)
	lo.Priority = 2
	hi := mk("hi", lua.OriginUser)
	hi.Priority = 9
	tie := mk("a.tie", lua.OriginUser)
	tie.Priority = 9
	r.Add(lo)
	r.Add(hi)
	r.Add(tie)
	for range 20 {
		inRegion := r.InRegion("statusline.left")
		if len(inRegion) != 4 || inRegion[0].ID != "clock" || inRegion[1].ID != "lo" || inRegion[2].ID != "a.tie" || inRegion[3].ID != "hi" {
			t.Fatalf("InRegion order = %+v", inRegion)
		}
	}

	r.Purge()
	if len(r.List()) != 0 {
		t.Fatal("Purge must clear widgets")
	}
}

func TestEventRegistry(t *testing.T) {
	r := NewEventRegistry()
	mk := func(event, id string, origin lua.Origin) *EventHandler {
		return &EventHandler{Event: event, ID: id, Fn: noopFn, Origin: origin}
	}

	// Auto ids.
	h1, err := r.Add(mk("user.ping", "", lua.OriginPlugin))
	if err != nil || h1.ID != "handler.1" {
		t.Fatalf("auto id = %+v err=%v", h1, err)
	}
	h2, _ := r.Add(mk("user.ping", "", lua.OriginPlugin))
	if h2.ID != "handler.2" {
		t.Fatalf("second auto id = %q", h2.ID)
	}

	// Cross-origin id collision on the same event: project loses to plugin.
	if _, err := r.Add(mk("user.ping", "handler.1", lua.OriginProject)); err == nil {
		t.Fatal("project must lose a handler-id collision with plugin")
	}
	if got := r.Handlers("user.ping"); got[0] != h1 {
		t.Fatal("refused collision replaced the handler")
	}

	// Equal-rank same id replaces in place (order preserved).
	replacement := mk("user.ping", "handler.2", lua.OriginPlugin)
	if _, err := r.Add(replacement); err != nil {
		t.Fatalf("same-origin replace: %v", err)
	}
	got := r.Handlers("user.ping")
	if len(got) != 2 || got[1] != replacement {
		t.Fatalf("in-place replace failed: %+v", got)
	}

	// Higher trust may take over the id.
	_, err = r.Add(mk("user.ping", "handler.1", lua.OriginUser))
	if err != nil {
		t.Fatalf("user takeover: %v", err)
	}
	if got := r.Handlers("user.ping"); got[0].Origin != lua.OriginUser {
		t.Fatalf("user takeover did not apply: %+v", got[0])
	}

	// Requirements and purge.
	if _, err := r.Add(&EventHandler{Event: "user.x"}); err == nil {
		t.Fatal("nil handler must be refused")
	}
	if _, err := r.Add(&EventHandler{Fn: noopFn}); err == nil {
		t.Fatal("empty event must be refused")
	}
	r.Purge()
	if len(r.Handlers("user.ping")) != 0 {
		t.Fatal("Purge must clear handlers")
	}
}

func TestThemeRegistry(t *testing.T) {
	r := NewThemeRegistry()
	mk := func(name string, origin lua.Origin) *Theme {
		return &Theme{Name: name, Palette: map[string]string{"fg": "#ffffff"}, HL: map[string]HLSpec{}, Origin: origin}
	}

	if _, err := r.Add(mk("default", lua.OriginBuiltin)); err != nil {
		t.Fatalf("builtin theme: %v", err)
	}
	if _, err := r.Add(mk("default", lua.OriginProject)); err == nil {
		t.Fatal("project theme must lose to builtin")
	}
	if _, err := r.Add(mk("default", lua.OriginUser)); err != nil {
		t.Fatalf("user theme replace: %v", err)
	}
	if got := r.Get("default"); got.Origin != lua.OriginUser {
		t.Fatalf("Get after replace = %+v", got)
	}
	if _, err := r.Add(&Theme{Origin: lua.OriginUser}); err == nil {
		t.Fatal("empty name must be refused")
	}
	if r.Get("nope") != nil {
		t.Fatal("unknown theme must be nil")
	}
	r.Purge()
	if len(r.List()) != 0 {
		t.Fatal("Purge must clear themes")
	}
}
