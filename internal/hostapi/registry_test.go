package hostapi

import (
	"strings"
	"testing"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// Registries accept any non-nil function at the Go level (real guards live
// in the host API layer); an empty LFunction satisfies the shape checks.
var noopFn = &glua.LFunction{}

func bind(key string, origin lua.Origin) *KeyBinding {
	return &KeyBinding{Key: key, RHS: RHS{API: "zemi.ui.help"}, Origin: origin}
}

// TestKeymapRegistry: set rebinds any key (the last registration wins), a
// sequence keeps its place ahead of the shorter key it starts, and list
// keeps registration order.
func TestKeymapRegistry(t *testing.T) {
	r := &KeymapRegistry{}
	for _, s := range []struct {
		name, key string
		origin    lua.Origin
		ok        bool
	}{
		{"builtin default", "tab", lua.OriginBuiltin, true},
		{"user overrides builtin", "tab", lua.OriginUser, true},
		{"a project key rebinds it", "tab", lua.OriginProject, true},
		{"plugin key", "x", lua.OriginPlugin, true},
		{"plugin rebinds its own", "x", lua.OriginPlugin, true},
		{"user sequence", "ctrl+g s", lua.OriginUser, true},
		{"a prefix of a sequence", "ctrl+g", lua.OriginProject, true},
		{"empty key", "", lua.OriginUser, false},
	} {
		if err := r.Set(bind(s.key, s.origin)); (err == nil) != s.ok {
			t.Fatalf("%s: err=%v", s.name, err)
		}
	}
	for _, l := range []struct {
		key  string
		want lua.Origin // "" for no binding
	}{
		{"tab", lua.OriginProject},
		{"x", lua.OriginPlugin},
		{"j", ""},
	} {
		if got := r.Lookup(l.key); l.want == "" && got != nil || l.want != "" && (got == nil || got.Origin != l.want) {
			t.Fatalf("Lookup(%q) = %+v", l.key, got)
		}
	}
	if !r.IsPrefix("ctrl+g") || r.IsPrefix("x") {
		t.Fatal("IsPrefix should see the longer sequence")
	}
	if all := r.List(); len(all) != 4 || all[0].Key != "tab" || all[3].Key != "ctrl+g" {
		t.Fatalf("List = %+v", all)
	}
	for _, d := range []struct {
		name, key string
		removed   bool
	}{
		{"a plugin deletes a key", "x", true},
		{"deleting twice is not an error", "x", false},
	} {
		removed, err := r.Del(lua.OriginPlugin, "", d.key, false)
		if removed != d.removed || err != nil {
			t.Fatalf("%s: removed=%v err=%v", d.name, removed, err)
		}
	}
	// a list alias lives beside a global binding of its key, and only Lookup
	// sees that one
	alias := func(origin lua.Origin) *KeyBinding {
		return &KeyBinding{Key: "ctrl+j", List: true, RHS: RHS{Move: "down"}, Origin: origin}
	}
	for _, err := range []error{r.Set(alias(lua.OriginUser)), r.Set(bind("ctrl+j", lua.OriginPlugin))} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if b := r.ListAlias("ctrl+j"); b == nil || b.RHS.Move != "down" || r.ListAlias("ctrl+k") != nil || r.Lookup("ctrl+j").List {
		t.Fatalf("ListAlias = %+v", b)
	}
	if err := r.Set(alias(lua.OriginPlugin)); err != nil {
		t.Fatalf("a plugin rebinds a list alias: %v", err)
	}
	if removed, _ := r.Del(lua.OriginUser, "", "ctrl+j", true); !removed || r.ListAlias("ctrl+j") != nil || r.Lookup("ctrl+j") == nil {
		t.Fatal("deleting the alias must leave the global binding")
	}
	r.Set(bind("q", lua.OriginUser))
	r.Purge()
	if len(r.List()) != 0 {
		t.Fatal("Purge must clear all bindings")
	}
}

func TestCommandCaseCollision(t *testing.T) {
	r := &CommandRegistry{}

	add := func(name string, origin lua.Origin) (*Command, error) {
		return r.Add(&Command{Name: name, Fn: noopFn, Origin: origin})
	}

	// The reserved-name guard chain: a case variant never shadows :Reload; an
	// identical name replaces.
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

	// An identical name replaces, whatever registered it.
	replacement := &Command{Name: "Reload", GoFn: func(*Host, string, bool) error { return nil }, Origin: lua.OriginBuiltin}
	if _, err := r.Add(replacement); err != nil {
		t.Fatalf("same-origin identical-name re-add: %v", err)
	}
	if got := r.Lookup("reload"); got.GoFn == nil {
		t.Fatal("identical-name replacement did not apply")
	}

	if _, err := add("Reload", lua.OriginProject); err != nil {
		t.Fatalf("project replacement of a builtin command: %v", err)
	}
	if got := r.Lookup("RELOAD"); got.Origin != lua.OriginProject {
		t.Fatalf("project replacement did not apply: %+v", got)
	}

	if _, err := add("", lua.OriginUser); err == nil {
		t.Fatal("empty name must be refused")
	}
}

func TestEventRegistry(t *testing.T) {
	r := &EventRegistry{}
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

	// Equal-rank same id replaces in place (order preserved).
	replacement := mk("user.ping", "handler.2", lua.OriginPlugin)
	if _, err := r.Add(replacement); err != nil {
		t.Fatalf("same-origin replace: %v", err)
	}
	got := r.Handlers("user.ping")
	if len(got) != 2 || got[1] != replacement {
		t.Fatalf("in-place replace failed: %+v", got)
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

// TestPluginOwnership: one plugin neither replaces nor removes another's
// registration, and replaces and removes its own from any of its files; every
// other origin replaces and removes whatever it likes.
func TestPluginOwnership(t *testing.T) {
	owner := func(p string) string { name, _, _ := strings.Cut(p, "/"); return name }
	const pl, user, proj = lua.OriginPlugin, lua.OriginUser, lua.OriginProject
	type ops struct {
		add func(o lua.Origin, path string) error         // nil: ids are fresh, so only removal can clash
		del func(o lua.Origin, path string) (bool, error) // nil: no removal
	}
	for _, c := range []struct {
		name string
		reg  func() ops
	}{
		{"keymap", func() ops {
			r := &KeymapRegistry{}
			r.owner = owner
			return ops{
				func(o lua.Origin, p string) error { return r.Set(&KeyBinding{Key: "x", Origin: o, Path: p}) },
				func(o lua.Origin, p string) (bool, error) { return r.Del(o, p, "x", false) },
			}
		}},
		{"command", func() ops {
			r := &CommandRegistry{}
			r.owner = owner
			return ops{
				func(o lua.Origin, p string) error {
					_, err := r.Add(&Command{Name: "x", Fn: noopFn, Origin: o, Path: p})
					return err
				},
				func(o lua.Origin, p string) (bool, error) { return r.Remove("x", o, p) },
			}
		}},
		{"event handler", func() ops {
			r := &EventRegistry{}
			r.owner = owner
			return ops{
				func(o lua.Origin, p string) error {
					_, err := r.Add(&EventHandler{ID: "x", Event: "permission.request", Fn: noopFn, Origin: o, Path: p})
					return err
				},
				func(o lua.Origin, p string) (bool, error) { return r.Remove("x", o, p) },
			}
		}},
		{"tool", func() ops {
			r := &toolReg{}
			r.owner = owner
			return ops{add: func(o lua.Origin, p string) error { return r.add(&luaTool{Name: "x", Origin: o, Path: p}) }}
		}},
		{"config watch", func() ops {
			r := &watchReg{}
			r.owner = owner
			r.add(1, watchRec{origin: pl, src: "a/init.lua"})
			return ops{del: func(o lua.Origin, p string) (bool, error) { return r.del(1, o, p) }}
		}},
	} {
		o := c.reg()
		if o.add != nil {
			if err := o.add(pl, "a/init.lua"); err != nil {
				t.Fatalf("%s: plugin a adds: %v", c.name, err)
			}
			if err := o.add(pl, "b/init.lua"); err == nil || !strings.Contains(err.Error(), "registered by plugin a, not b") {
				t.Fatalf("%s: plugin b replaces a's: %v", c.name, err)
			}
			if err := o.add(pl, "a/lua/a/x.lua"); err != nil {
				t.Fatalf("%s: plugin a replaces its own: %v", c.name, err)
			}
		}
		if o.del != nil {
			if ok, err := o.del(pl, "b/init.lua"); ok || err == nil {
				t.Fatalf("%s: plugin b removes a's: %v %v", c.name, ok, err)
			}
			if ok, err := o.del(pl, "a/init.lua"); !ok || err != nil {
				t.Fatalf("%s: plugin a removes its own: %v %v", c.name, ok, err)
			}
		}
		if o.add == nil {
			continue
		}
		if err := o.add(user, "init.lua"); err != nil {
			t.Fatalf("%s: user adds over a plugin: %v", c.name, err)
		}
		if err := o.add(proj, ".agents/init.lua"); err != nil {
			t.Fatalf("%s: project replaces the user's: %v", c.name, err)
		}
		if o.del != nil {
			if ok, err := o.del(proj, ".agents/init.lua"); !ok || err != nil {
				t.Fatalf("%s: project removes the user's: %v %v", c.name, ok, err)
			}
		}
	}
}
