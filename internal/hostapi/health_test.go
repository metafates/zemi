package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// TestHealthLuaChecks: checks registered from Lua become /health rows, in
// either return shape, and one that raises reports as an error row rather
// than taking the whole page down.
func TestHealthLuaChecks(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.health.add("disk", function() return "replaced" end)
		zemi.health.add("disk", function() return { level = "warn", detail = "low" } end)
		zemi.health.add("note", function() return "plain detail" end)
		zemi.health.add("boom", function() error("nope") end)
	`)
	err := r.ExecuteChunk(context.Background(), `zemi.health.add("disk", function() return "planted" end)`, "p.lua", lua.OriginPlugin)
	if err == nil || !strings.Contains(err.Error(), "more trusted") {
		t.Errorf("plugin replacing a user check: %v", err)
	}
	got := map[string]healthCheck{}
	for _, c := range h.healthChecks() {
		if _, dup := got[c.name]; dup {
			t.Errorf("two %s rows", c.name)
		}
		got[c.name] = c
	}
	for _, c := range []struct{ name, level, detail string }{
		{"disk", "warn", "low"},
		{"note", "ok", "plain detail"},
		{"boom", "error", "nope"},
	} {
		row := got[c.name]
		if row.level != c.level || !strings.Contains(row.detail, c.detail) {
			t.Errorf("%s = %+v, want level %s containing %q", c.name, row, c.level, c.detail)
		}
	}
}

// TestHealthListFromLua: list() answers on the interpreter goroutine, where
// the queue luaHealthRows uses would deadlock, and runs registered checks.
func TestHealthListFromLua(t *testing.T) {
	_, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.health.add("disk", function() return { level = "warn", detail = "low" } end)
		local seen = {}
		for _, row in ipairs(zemi.health.list()) do seen[row.name] = row.level .. " " .. row.detail end
		assert(seen.store, "no built-in rows")
		assert(seen.disk == "warn low", "registered check: " .. tostring(seen.disk))
	`)
}

func TestHealthChecks(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.reflectErr = fmt.Errorf("%w: no JSON object in the output", errNoProposal)
	runUser(t, h.RT, `zemi.ui.widget.add({ id = "lost", region = "nowhere", render = function() return {} end })`)
	got := map[string]healthCheck{}
	for _, c := range h.healthChecks() {
		got[c.name] = c
	}
	for name, level := range map[string]string{
		"store": "ok", "model": "error", "reflector": "warn", "learning": "ok", "skills": "ok", "lua": "ok", "mcp": "ok", "layout": "warn",
	} {
		if got[name].level != level {
			t.Errorf("%s = %+v, want level %s", name, got[name], level)
		}
	}

	items := h.healthItems()
	if first, last := items[0].(map[string]any), items[len(items)-1].(map[string]any); first["section"] != "Needs attention" || last["section"] != "OK" {
		t.Fatalf("problems first: %v ... %v", first, last)
	}
	for _, it := range items {
		if m := it.(map[string]any); m["text"] == "store" && m["detail"] != nil {
			t.Fatalf("a fine short row opens a detail view: %v", m)
		}
	}
}

// TestHealthChecksProfileModel: the model row checks the default profile's
// model, which is the one a turn uses, not models.main.
func TestHealthChecksProfileModel(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", models = { { id = "m" } } })
		zemi.provider.add({ id = "b", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = { { id = "m" } } })
		zemi.model.set("a/m")
		zemi.agent.profile("p", { model = "b/m" })
		zemi.config.set("agent.profile", "p")
	`)
	got := map[string]healthCheck{}
	for _, c := range h.healthChecks() {
		got[c.name] = c
	}
	if c := got["model"]; c.level != "ok" || c.detail != "b/m" {
		t.Errorf("model = %+v, want b/m", c)
	}
	if c := got["context"]; c.level != "warn" || !strings.Contains(c.detail, "unknown for b/m, a/m:") {
		t.Errorf("context window = %+v, want a warning naming both models", c)
	}
}

// TestPluginsStepContinues: a failing plugin file does not stop the ones
// sorted after it, and the /health row counts it.
func TestPluginsStepContinues(t *testing.T) {
	data, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirs := config.Dirs{ProjectDir: t.TempDir(), DataHome: data}
	for name, src := range map[string]string{"a": `error("broken")`, "b": `zemi.command.add("from-b", function() end, {})`} {
		p := filepath.Join(dirs.PluginsDir(), "p", "start", name, "plugin", name+".lua")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dirs.PluginsDir(), Origin: lua.OriginPlugin}}})
	h := NewHost(r, log, config.NewStore(), dirs)
	step := h.PluginsStep()
	run := step.Run
	var stepErr error
	step.Run = func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
		stepErr = run(ctx, r, g)
		return nil
	}
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), step}); err != nil {
		t.Fatal(err)
	}
	if stepErr == nil || !strings.Contains(stepErr.Error(), "broken") {
		t.Errorf("step error = %v, want a.lua's", stepErr)
	}
	if h.Commands.Lookup("from-b") == nil {
		t.Error("b.lua did not load after a.lua failed")
	}
	// made after boot: the counts stay the boot's, and only a pack with
	// neither plugin/*.lua nor lua/ is named
	for _, d := range []string{"lib/lua", "hollow", "c/plugin/c.lua"} {
		if err := os.MkdirAll(filepath.Join(dirs.PluginsDir(), "p", "start", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	i := slices.IndexFunc(h.goHealthRows(), func(c healthCheck) bool {
		return c.name == "plugins" && c.level == "warn" && strings.Contains(c.detail, "1 of 2 files failed") &&
			strings.Contains(c.detail, "nothing to load in hollow:")
	})
	if i < 0 {
		t.Errorf("no plugins warn row in %+v", h.goHealthRows())
	}
}

// TestHealthProvidersRow: the row lists what is registered, tags catalog
// specs, counts the builtin ones waiting for a key, and with nothing
// registered says what to export.
func TestHealthProvidersRow(t *testing.T) {
	for _, c := range []struct {
		name, lua     string
		missing       []agent.MissingKey
		level, detail string
	}{
		{"nothing", ``, nil, "warn", "none usable: add a provider in "},
		{"key missing", ``, []agent.MissingKey{{ID: "x", Env: "X_API_KEY"}}, "warn", "none usable: export X_API_KEY, or add a provider in "},
		{"deleted", `zemi.provider.del("x")`, []agent.MissingKey{{ID: "x", Env: "X_API_KEY"}}, "warn", "none usable: add a provider in "},
		{"registered", `zemi.provider.add({ id = "a", kind = "anthropic", api_key = "k", models = { { id = "m" } } })`, []agent.MissingKey{{ID: "x", Env: "X_API_KEY"}}, "ok", "a; 1 builtin without a key exported"},
	} {
		h, r := newTestHostDB(t)
		h.Providers.Reset(agent.Catalog{Missing: c.missing})
		runUser(t, r, c.lua)
		row := healthCheck{}
		for _, x := range h.healthChecks() {
			if x.name == "providers" {
				row = x
			}
		}
		if row.level != c.level || !strings.HasPrefix(row.detail, c.detail) {
			t.Errorf("%s: providers row = %+v", c.name, row)
		}
	}
}

// TestHealthModelRow: the model row and a send name the same cause in the
// same words, before any provider SDK gets to complain in its own.
func TestHealthModelRow(t *testing.T) {
	for _, c := range []struct{ name, lua, level, detail string }{
		{"none", ``, "error", "no model yet: export OPENAI_API_KEY or init.lua, more in /model"},
		{"unknown provider", `zemi.config.set("models.main", "nobody/m")`, "error", `provider "nobody" is not registered: add a provider in `},
		{"builtin without key", `zemi.config.set("models.main", "openai/gpt-5")`, "error", `builtin provider "openai" needs its key: export OPENAI_API_KEY`},
		{"empty key", `zemi.provider.add({ id = "a", kind = "anthropic", models = { { id = "m" } } })`, "error", "a: api_key is empty in "},
		{"unlisted model", `zemi.provider.add({ id = "a", kind = "anthropic", api_key = "k", models = { { id = "m" } } })
			zemi.config.set("models.main", "a/other")`, "warn", "a/other: not in the provider's models list"},
	} {
		h, r := newTestHostDB(t)
		h.Providers.Reset(agent.Catalog{Missing: []agent.MissingKey{{ID: "openai", Env: "OPENAI_API_KEY"}}})
		runUser(t, r, c.lua)
		row := healthCheck{}
		for _, x := range h.healthChecks() {
			if x.name == "model" {
				row = x
			}
		}
		if row.level != c.level || !strings.HasPrefix(row.detail, c.detail) {
			t.Errorf("%s: model row = %+v", c.name, row)
		}
		if c.level == "error" {
			if _, err := h.resolveModel(nil); err == nil || !strings.HasPrefix(err.Error(), c.detail) {
				t.Errorf("%s: resolveModel = %v", c.name, err)
			}
		}
	}
}
