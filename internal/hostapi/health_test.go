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
		zemi.health.register("disk", function() return { level = "warn", detail = "low" } end)
		zemi.health.register("note", function() return "plain detail" end)
		zemi.health.register("boom", function() error("nope") end)
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

func TestHealthChecks(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.reflectErr = "reflector: no JSON object in output"
	got := map[string]healthCheck{}
	for _, c := range h.healthChecks() {
		got[c.name] = c
	}
	for name, level := range map[string]string{
		"store": "ok", "model": "error", "reflector": "error", "learning": "ok", "skills": "ok", "lua": "ok",
	} {
		if got[name].level != level {
			t.Errorf("%s = %+v, want level %s", name, got[name], level)
		}
	}
}

// TestHealthChecksProfileModel: the model row checks the default profile's
// model, which is the one a turn uses, not agent.model.
func TestHealthChecksProfileModel(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.provider.register({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", models = { { id = "m" } } })
		zemi.provider.register({ id = "b", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = { { id = "m" } } })
		zemi.model.set("a/m")
		zemi.agent.profile("p", { model = "b/m" })
		zemi.agent.set_default("p")
	`)
	for _, c := range h.healthChecks() {
		if c.name == "model" && (c.level != "error" || c.detail != "b/m: api key not set") {
			t.Errorf("model = %+v, want b/m without a key", c)
		}
	}
}
