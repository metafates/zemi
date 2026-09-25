package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	zemiruntime "github.com/metafates/zemi/runtime"
)

func TestLayoutSetRefusesBadLayouts(t *testing.T) {
	_, r := newTestHost(t)
	for _, c := range []struct{ name, layout, want string }{
		{"no composer", `{ root = { kind = "box", dir = "col", children = {} } }`, "exactly one composer"},
		{"two composers", `{ root = { kind = "box", dir = "col", children = { { kind = "composer" }, { kind = "composer" } } } }`, "exactly one composer"},
		{"a hidden composer", `{ root = { kind = "box", dir = "col", hidden = true, children = { { kind = "composer" } } } }`, "cannot be hidden"},
		{"root is a leaf", `{ root = { kind = "composer" } }`, "must be a box"},
		{"unknown kind", `{ root = { kind = "box", dir = "col", children = { { kind = "composer" }, { kind = "panel" } } } }`, `unknown kind "panel"`},
		{"a typo in a field", `{ root = { kind = "box", dir = "col", childs = {} } }`, `unknown field "childs"`},
		{"a region drawn twice", `{ root = { kind = "box", dir = "col", children = { { kind = "composer" }, { kind = "region", region = "chat" }, { kind = "region", region = "chat" } } } }`, "drawn twice"},
		{"duplicate id", `{ root = { kind = "box", dir = "col", id = "a", children = { { kind = "composer", id = "a" } } } }`, `"a" is taken`},
		{"bad size", `{ root = { kind = "box", dir = "col", children = { { kind = "composer", size = "120%" } } } }`, "size must be"},
		{"content in a row", `{ root = { kind = "box", dir = "row", children = { { kind = "composer", size = "content" } } } }`, "works only for a region"},
		{"min over max", `{ root = { kind = "box", dir = "col", children = { { kind = "composer", min = 5, max = 2 } } } }`, "over max"},
		{"two stages", `{ root = { kind = "box", dir = "col", stage = true, children = { { kind = "composer", stage = true } } } }`, "one node may be the stage"},
		{"scroll without an id", `{ root = { kind = "box", dir = "col", children = { { kind = "composer" }, { kind = "region", region = "x", scroll = "top" } } } }`, "needs an id"},
		{"focusable without an id", `{ root = { kind = "box", dir = "col", children = { { kind = "composer" }, { kind = "region", region = "x", focusable = true } } } }`, "needs an id"},
		{"a focusable box", `{ root = { kind = "box", dir = "col", id = "b", focusable = true, children = { { kind = "composer" } } } }`, `unknown field "focusable"`},
		{"a focusable float without an id", `{ root = { kind = "box", dir = "col", children = { { kind = "composer" } } }, floats = { { region = "x", focusable = true } } }`, "needs an id"},
		{"a float relative to nothing", `{ root = { kind = "box", dir = "col", children = { { kind = "composer" } } }, floats = { { region = "x", relative = "nope" } } }`, `relative "nope"`},
		{"too deep", `(function() local n = { kind = "composer" } for _ = 1, 9 do n = { kind = "box", dir = "col", children = { n } } end return { root = n } end)()`, "deeper than 8"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runUser(t, r, `local ok, err = pcall(zemi.ui.layout.set, `+c.layout+`)
				assert(not ok and tostring(err):find(`+luaQuote(c.want)+`, 1, true), tostring(err))`)
		})
	}
}

func luaQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }

// TestLayoutOrigins: the shipped UI sets the layout, user config replaces
// it, lower origins are refused, and a reload keeps drawing the old layout
// until the replay sets one: the user's again, else the builtin one, else
// the Go fallback.
func TestLayoutOrigins(t *testing.T) {
	cfg, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, RuntimeFS: zemiruntime.FS, PathClass: lua.PathClasses{{Prefix: cfg, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ConfigHome: cfg, ProjectDir: t.TempDir()})
	initLua := filepath.Join(cfg, "init.lua")
	userLayout := `zemi.ui.layout.set({ root = { kind = "box", dir = "row", children = {
		{ kind = "region", id = "side", region = "side", size = 20 }, { kind = "composer" } } } })`
	if err := os.WriteFile(initLua, []byte(userLayout), 0o600); err != nil {
		t.Fatal(err)
	}
	steps := []lua.BootStep{h.NamespacesStep(), lua.RequireStep("zemi.tui.layout"), lua.LoadFileStep("init", initLua, lua.OriginUser)}
	if err := r.Boot(context.Background(), steps[:2]); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.Layout(), DefaultLayout) {
		t.Fatal("the Go fallback drifted from tui/layout.lua")
	}
	if err := h.ForceReload(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	side := func() bool { return h.Layout().Root.Children[0].Region == "side" }
	if !side() {
		t.Fatal("user config did not replace the shipped layout")
	}
	runUser(t, r, `assert(zemi.ui.layout.get().root.children[1].id == "side")`)
	for _, o := range []lua.Origin{lua.OriginBuiltin, lua.OriginPlugin, lua.OriginProject} {
		err := r.ExecuteChunk(context.Background(), `zemi.ui.layout.set({ root = { kind = "box", dir = "col", children = { { kind = "composer" } } } })`, "x.lua", o)
		if err == nil || !side() {
			t.Fatalf("%s code replaced the user's layout (err %v)", o, err)
		}
	}

	if err := h.ForceReload(context.Background(), steps); err != nil || !side() {
		t.Fatalf("reload lost the user's layout: %v", err)
	}
	if err := os.Remove(initLua); err != nil {
		t.Fatal(err)
	}
	if err := h.ForceReload(context.Background(), steps); err != nil || side() || h.Layout() == DefaultLayout {
		t.Fatalf("a replay without the user's layout should draw the shipped one: %v", err)
	}
	if err := h.ForceReload(context.Background(), steps[:1]); err != nil || h.Layout() != DefaultLayout {
		t.Fatalf("a replay that sets no layout should draw the fallback: %v", err)
	}
}
