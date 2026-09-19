package hostapi

import (
	"strings"
	"testing"
)

// TestHealthLuaChecks: checks registered from Lua become /health rows, in
// either return shape, and one that raises reports as an error row rather
// than taking the whole page down.
func TestHealthLuaChecks(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.health.add("disk", function() return { level = "warn", detail = "low" } end)
		zemi.health.add("note", function() return "plain detail" end)
		zemi.health.add("boom", function() error("nope") end)
	`)
	got := map[string]healthCheck{}
	for _, c := range h.healthChecks() {
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
	h.agents.reflectErr = "reflector: no JSON object in output"
	runUser(t, h.RT, `zemi.ui.widget.add({ id = "lost", region = "nowhere", render = function() return {} end })`)
	got := map[string]healthCheck{}
	for _, c := range h.healthChecks() {
		got[c.name] = c
	}
	for name, level := range map[string]string{
		"store": "ok", "model": "error", "reflector": "error", "learning": "ok", "skills": "ok", "lua": "ok", "mcp": "ok", "layout": "warn",
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
	if c := got["model"]; c.level != "error" || c.detail != "b/m: api key not set" {
		t.Errorf("model = %+v, want b/m without a key", c)
	}
	if c := got["context window"]; c.level != "warn" || !strings.Contains(c.detail, "unknown for b/m, a/m:") {
		t.Errorf("context window = %+v, want a warning naming both models", c)
	}
}
