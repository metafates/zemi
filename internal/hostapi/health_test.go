package hostapi

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
)

// TestHealthLuaChecks: checks registered from Lua become /health rows, in
// either return shape, and one that raises or returns an unknown level
// reports as an error row rather than taking the whole page down.
func TestHealthLuaChecks(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.health.add("disk", function() return "replaced" end)
		zemi.health.add("disk", function() return { level = "warn", detail = "low" } end)
		zemi.health.add("note", function() return "plain detail" end)
		zemi.health.add("boom", function() error("nope") end)
		zemi.health.add("typo", function() return { level = "warning", detail = "low" } end)
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
		{"typo", "error", `level "warning" is not one of ok, warn, error`},
	} {
		row := got[c.name]
		if string(row.level) != c.level || !strings.Contains(row.detail, c.detail) {
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
		if string(got[name].level) != level {
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
// model, which is the one a turn uses, not models.chat.
func TestHealthChecksProfileModel(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", models = { { id = "m" } } })
		zemi.provider.add({ id = "b", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = { { id = "m" } } })
		zemi.model.set("a/m")
		zemi.agent.profile.add("p", { model = "b/m" })
		zemi.config.set("agent.profile", "p")
	`)
	got := map[string]healthCheck{}
	for _, c := range h.healthChecks() {
		got[c.name] = c
	}
	if c := got["model"]; c.level != "ok" || c.detail != "b/m" {
		t.Errorf("model = %+v, want b/m", c)
	}
	if c := got["context"]; c.level != "warn" || !strings.Contains(c.detail, "unknown for b/m: no context percent, no auto compaction; a/m: subagents") {
		t.Errorf("context window = %+v, want a warning naming both models", c)
	}
}

// TestHealthContextRoles: the context row warns only about a model whose
// unknown window costs something, the conversation's and the agents'.
func TestHealthContextRoles(t *testing.T) {
	for _, c := range []struct{ name, lua, level string }{
		{"titles and memory read no window", `zemi.config.set("models.titles", "b/m") zemi.config.set("models.memory", "b/m")`, "ok"},
		{"compact falls back to the chat window", `zemi.config.set("models.compact", "b/m")`, "ok"},
		{"agents read one", `zemi.config.set("models.agents", "b/m")`, "warn"},
	} {
		h, r := newTestHostDB(t)
		runUser(t, r, `
			zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", models = { { id = "m", context_window = 100000 } } })
			zemi.provider.add({ id = "b", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", models = { { id = "m" } } })
			zemi.model.set("a/m")
		`+c.lua)
		for _, row := range h.healthChecks() {
			if row.name == "context" && string(row.level) != c.level {
				t.Errorf("%s: context = %+v, want %s", c.name, row, c.level)
			}
		}
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
		{"key missing", ``, []agent.MissingKey{{ID: "x", Env: "X_API_KEY"}}, "warn", "none usable"},
		{"registered", `zemi.provider.add({ id = "a", kind = "anthropic", api_key = "k", models = { { id = "m" } } })`, []agent.MissingKey{{ID: "x", Env: "X_API_KEY"}}, "ok", "a; 1 builtin without a key exported"},
		{"keyless", `zemi.provider.add({ id = "d", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })`, nil, "ok", "d (no key)"},
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
		if string(row.level) != c.level || !strings.HasPrefix(row.detail, c.detail) {
			t.Errorf("%s: providers row = %+v", c.name, row)
		}
	}
}

// TestHealthModelRow: the model row and a send name the same cause in the
// same words, before any provider SDK gets to complain in its own; a role's
// own model gets a row of its own.
func TestHealthModelRow(t *testing.T) {
	for _, c := range []struct{ name, row, lua, level, detail string }{
		{"none", "model", ``, "error", "no model yet: export OPENAI_API_KEY or add a provider to init.lua"},
		{"unknown provider", "model", `zemi.config.set("models.chat", "nobody/m")`, "error", `provider "nobody" is not registered: add a provider to `},
		{"builtin without key", "model", `zemi.config.set("models.chat", "openai/gpt-5")`, "error", `builtin provider "openai" needs its key: export OPENAI_API_KEY`},
		{"empty key", "model", `zemi.provider.add({ id = "a", kind = "anthropic", models = { { id = "m" } } })`, "error", "a: api_key is empty in "},
		{"unlisted model", "model", `zemi.provider.add({ id = "a", kind = "anthropic", api_key = "k", models = { { id = "m" } } })
			zemi.config.set("models.chat", "a/other")`, "warn", "a/other: not in the provider's models list"},
		{"a role's row is named after it", "titles model", `zemi.provider.add({ id = "a", kind = "anthropic", api_key = "k", models = { { id = "m" } } })
			zemi.config.set("models.titles", "nobody/m")`, "error", `provider "nobody" is not registered: add a provider to `},
	} {
		h, r := newTestHostDB(t)
		h.Providers.Reset(agent.Catalog{Missing: []agent.MissingKey{{ID: "openai", Env: "OPENAI_API_KEY"}}})
		runUser(t, r, c.lua)
		row := healthCheck{}
		for _, x := range h.healthChecks() {
			if x.name == c.row {
				row = x
			}
		}
		if string(row.level) != c.level || !strings.HasPrefix(row.detail, c.detail) {
			t.Errorf("%s: %s row = %+v", c.name, c.row, row)
		}
		if c.level == "error" && c.row == "model" {
			if _, err := h.resolveModel(nil); err == nil || !strings.HasPrefix(err.Error(), c.detail) {
				t.Errorf("%s: resolveModel = %v", c.name, err)
			}
		}
	}
}

// TestHealthMCPRow: only ready servers read as ready; a starting one is
// named, one that failed or stopped warns, and a sign-in wait says so.
func TestHealthMCPRow(t *testing.T) {
	for _, c := range []struct {
		statuses      []ServiceStatus
		level, detail string
	}{
		{[]ServiceStatus{StatusReady, StatusReady}, "ok", "2 servers ready"},
		{[]ServiceStatus{StatusReady, StatusStarting}, "ok", "2 servers: 1 ready, 1 starting"},
		{[]ServiceStatus{StatusFailed, StatusReady, StatusStopped}, "warn", "3 servers: 1 failed, 1 ready, 1 stopped"},
		{[]ServiceStatus{StatusFailed, StatusSignIn}, "warn", "2 servers: 1 failed, 1 needs sign-in"},
	} {
		h, _ := newTestHostDB(t)
		for i, st := range c.statuses {
			h.mcp.servers[strconv.Itoa(i)] = &mcpServer{status: st}
		}
		i := slices.IndexFunc(h.goHealthRows(), func(r healthCheck) bool { return r.name == "mcp" })
		if row := h.goHealthRows()[i]; string(row.level) != c.level || row.detail != c.detail {
			t.Errorf("%v: mcp row = %+v", c.statuses, row)
		}
	}
}

// TestWaitMCP: the wait ends when the last server leaves "starting", or
// when its context ends.
func TestWaitMCP(t *testing.T) {
	h, _ := newTestHostDB(t)
	s := &mcpServer{status: "starting"}
	h.mcp.servers["a"] = s
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.WaitMCP(ctx) // returns: ctx is done
	done := make(chan struct{})
	go func() { h.WaitMCP(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("returned while a server is starting")
	case <-time.After(50 * time.Millisecond):
	}
	s.mu.Lock()
	s.status = "failed"
	s.mu.Unlock()
	<-done
}
