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

func bind(key, focus string, origin lua.Origin) *KeyBinding {
	return &KeyBinding{Key: key, Focus: focus, RHS: RHS{API: "zemi.ui.help"}, Origin: origin}
}

// TestKeymapRegistry: set rebinds within and above an origin, lower trust is
// refused for set and del, a focused binding cannot take what a more trusted
// global one holds, lookup prefers the focused binding unless a more trusted
// global one holds the key, and list keeps registration order.
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
	if all := r.List(); len(all) != 6 || all[0].Key != "tab" || all[4].Key != "tab" || all[4].Focus != "todo" {
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
		removed, err := r.Del(d.origin, "", d.focus, d.key, false)
		if removed != d.removed || (err != nil) != d.wantErr {
			t.Fatalf("%s: removed=%v err=%v", d.name, removed, err)
		}
	}
	// a list alias lives beside a global binding of its key, and only Lookup
	// sees that one
	alias := func(origin lua.Origin) *KeyBinding {
		return &KeyBinding{Key: "ctrl+j", List: true, RHS: RHS{Move: "down"}, Origin: origin}
	}
	for _, err := range []error{r.Set(alias(lua.OriginUser)), r.Set(bind("ctrl+j", "", lua.OriginPlugin))} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if b := r.ListAlias("ctrl+j"); b == nil || b.RHS.Move != "down" || r.ListAlias("ctrl+k") != nil || r.Lookup("", "ctrl+j").List {
		t.Fatalf("ListAlias = %+v", b)
	}
	if err := r.Set(alias(lua.OriginPlugin)); err == nil {
		t.Fatal("a plugin replaced a user list alias")
	}
	if removed, _ := r.Del(lua.OriginUser, "", "", "ctrl+j", true); !removed || r.ListAlias("ctrl+j") != nil || r.Lookup("", "ctrl+j") == nil {
		t.Fatal("deleting the alias must leave the global binding")
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
	for _, w := range []*Widget{{ID: "x", Region: "r"}, {ID: "x", Render: noopFn}} {
		if _, err := r.Add(w); err == nil {
			t.Fatalf("%+v: a widget needs a region and a render", w)
		}
	}
	// InRegion orders by priority ascending, ties by id, stable across calls.
	for id, p := range map[string]float64{"clock": 1, "lo": 2, "mid": 2.5, "hi": 9, "a.tie": 9} {
		r.Add(&Widget{ID: id, Region: "statusline.left", Render: noopFn, Priority: p})
	}
	for range 20 {
		inRegion := r.InRegion("statusline.left")
		if len(inRegion) != 5 || inRegion[0].ID != "clock" || inRegion[1].ID != "lo" || inRegion[2].ID != "mid" || inRegion[3].ID != "a.tie" || inRegion[4].ID != "hi" {
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
// registration, and replaces and removes its own from any of its files; a
// project neither replaces nor removes the user's.
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
			r := NewKeymapRegistry()
			r.owner = owner
			return ops{
				func(o lua.Origin, p string) error { return r.Set(&KeyBinding{Key: "x", Origin: o, Path: p}) },
				func(o lua.Origin, p string) (bool, error) { return r.Del(o, p, "", "x", false) },
			}
		}},
		{"command", func() ops {
			r := NewCommandRegistry()
			r.owner = owner
			return ops{
				func(o lua.Origin, p string) error {
					_, err := r.Add(&Command{Name: "x", Fn: noopFn, Origin: o, Path: p})
					return err
				},
				func(o lua.Origin, p string) (bool, error) { return r.Remove("x", o, p) },
			}
		}},
		{"widget", func() ops {
			r := NewWidgetRegistry()
			r.owner = owner
			return ops{
				func(o lua.Origin, p string) error {
					_, err := r.Add(&Widget{ID: "x", Region: "r", Render: noopFn, Origin: o, Path: p})
					return err
				},
				func(o lua.Origin, p string) (bool, error) { return r.Remove("x", o, p) },
			}
		}},
		{"event handler", func() ops {
			r := NewEventRegistry()
			r.owner = owner
			return ops{
				func(o lua.Origin, p string) error {
					_, err := r.Add(&EventHandler{ID: "x", Event: "permission.request", Fn: noopFn, Origin: o, Path: p})
					return err
				},
				func(o lua.Origin, p string) (bool, error) { return r.Remove("x", o, p) },
			}
		}},
		{"theme", func() ops {
			r := NewThemeRegistry()
			r.owner = owner
			return ops{add: func(o lua.Origin, p string) error { _, err := r.Add(&Theme{Name: "x", Origin: o, Path: p}); return err }}
		}},
		{"tool", func() ops {
			r := newToolReg()
			r.owner = owner
			return ops{add: func(o lua.Origin, p string) error { return r.add(&luaTool{Name: "x", Origin: o, Path: p}) }}
		}},
		{"config watch", func() ops {
			r := newWatchReg()
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
		if err := o.add(proj, ".agents/init.lua"); err == nil || !strings.Contains(err.Error(), "registered by user config") {
			t.Fatalf("%s: project replaces the user's: %v", c.name, err)
		}
		if o.del != nil {
			if ok, err := o.del(proj, ".agents/init.lua"); ok || err == nil {
				t.Fatalf("%s: project removes the user's: %v %v", c.name, ok, err)
			}
		}
	}
}
