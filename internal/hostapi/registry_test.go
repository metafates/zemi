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

func bind(mode, keys string, origin lua.Origin) *KeyBinding {
	return &KeyBinding{Mode: mode, Keys: keys, RHS: RHS{API: "zemi.ui.help"}, Origin: origin}
}

func TestKeymapPrecedence(t *testing.T) {
	r := NewKeymapRegistry()

	// Builtin first (boot order), then user rebinding wins.
	if _, err := r.Bind(bind("normal", "tab", lua.OriginBuiltin)); err != nil {
		t.Fatalf("builtin bind: %v", err)
	}
	if _, err := r.Bind(bind("normal", "tab", lua.OriginUser)); err != nil {
		t.Fatalf("user rebind: %v", err)
	}
	if got := r.Lookup("normal", "tab"); got.Origin != lua.OriginUser {
		t.Fatalf("after user rebind, origin = %v, want user", got.Origin)
	}

	// Lower trust cannot replace user: project refused, builtin refused.
	if _, err := r.Bind(bind("normal", "tab", lua.OriginProject)); err == nil {
		t.Fatal("project rebind of a user binding must be refused")
	}
	if _, err := r.Bind(bind("normal", "tab", lua.OriginBuiltin)); err == nil {
		t.Fatal("builtin rebind of a user binding must be refused")
	}
	if got := r.Lookup("normal", "tab"); got.Origin != lua.OriginUser {
		t.Fatalf("refused binds changed the binding: origin = %v", got.Origin)
	}

	// LWW within one origin (plugin replaces plugin).
	if _, err := r.Bind(bind("normal", "x", lua.OriginPlugin)); err != nil {
		t.Fatalf("plugin bind: %v", err)
	}
	second := bind("normal", "x", lua.OriginPlugin)
	second.RHS.API = "zemi.cmdline.open"
	if _, err := r.Bind(second); err != nil {
		t.Fatalf("plugin rebind (LWW): %v", err)
	}
	if got := r.Lookup("normal", "x"); got.RHS.API != "zemi.cmdline.open" {
		t.Fatalf("LWW within origin did not apply: %+v", got.RHS)
	}
}

func TestKeymapDelOriginScoped(t *testing.T) {
	r := NewKeymapRegistry()
	if _, err := r.Bind(bind("normal", "gf", lua.OriginUser)); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// A plugin cannot delete a user binding.
	if r.Del(lua.OriginPlugin, "normal", "gf") {
		t.Fatal("cross-origin Del must be refused")
	}
	if r.Lookup("normal", "gf") == nil {
		t.Fatal("binding vanished after refused Del")
	}
	if !r.Del(lua.OriginUser, "normal", "gf") {
		t.Fatal("same-origin Del must succeed")
	}
	if r.Lookup("normal", "gf") != nil {
		t.Fatal("binding still present after same-origin Del")
	}
	if r.Del(lua.OriginUser, "normal", "gf") {
		t.Fatal("second Del must be false")
	}
}

func TestKeymapPrefixAndList(t *testing.T) {
	r := NewKeymapRegistry()
	if _, err := r.Bind(bind("normal", "gm", lua.OriginUser)); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !r.IsPrefix("normal", "g") {
		t.Fatal("'g' must be a prefix of 'gm'")
	}
	if r.IsPrefix("normal", "gm") {
		t.Fatal("exact binding is not a prefix of itself")
	}
	if r.IsPrefix("insert", "g") {
		t.Fatal("prefix must be mode-scoped")
	}

	if _, err := r.Bind(bind("", "x", lua.OriginUser)); err == nil {
		t.Fatal("empty mode must be refused")
	}
	if _, err := r.Bind(bind("normal", "", lua.OriginUser)); err == nil {
		t.Fatal("empty keys must be refused")
	}

	all := r.List()
	if len(all) != 1 || all[0].Keys != "gm" {
		t.Fatalf("List = %+v", all)
	}
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

	// InRegion orders by priority ascending.
	lo := mk("lo", lua.OriginUser)
	lo.Priority = 2
	hi := mk("hi", lua.OriginUser)
	hi.Priority = 9
	r.Add(lo)
	r.Add(hi)
	inRegion := r.InRegion("statusline.left")
	if len(inRegion) != 3 || inRegion[0].ID != "clock" || inRegion[1].ID != "lo" || inRegion[2].ID != "hi" {
		t.Fatalf("InRegion order = %+v", inRegion)
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
	replacement.Coalesce = true
	if _, err := r.Add(replacement); err != nil {
		t.Fatalf("same-origin replace: %v", err)
	}
	got := r.Handlers("user.ping")
	if len(got) != 2 || got[1] != replacement || !got[1].Coalesce {
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
