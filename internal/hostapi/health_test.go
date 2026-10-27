package hostapi

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
)

// TestHealthLuaChecks: checks registered from Lua become /health rows, in
// either return shape, and one that raises or returns an unknown level, a
// number included, reports as an error row rather than taking the whole page down.
func TestHealthLuaChecks(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.health.add("disk", function() return "replaced" end)
		zemi.health.add("disk", function() return { level = "warn", detail = "low" } end)
		zemi.health.add("note", function() return "plain detail" end)
		zemi.health.add("boom", function() error("nope") end)
		zemi.health.add("typo", function() return { level = "warning", detail = "low" } end)
		zemi.health.add("number", function() return { level = 3 } end)
		zemi.health.add("unset", function() return { detail = "fine" } end)
	`)
	if err := r.ExecuteChunk(t.Context(), `zemi.health.add("disk", function() return "planted" end)`, "p.lua", lua.OriginPlugin); err != nil {
		t.Errorf("plugin replacing a user check: %v", err)
	}
	got := map[string]HealthRow{}
	for _, c := range h.HealthRows() {
		if _, dup := got[c.Name]; dup {
			t.Errorf("two %s rows", c.Name)
		}
		got[c.Name] = c
	}
	for _, c := range []struct{ name, level, detail string }{
		{"disk", "ok", "planted"},
		{"note", "ok", "plain detail"},
		{"boom", "error", "nope"},
		{"typo", "error", `level "warning" is not one of ok, warn, error`},
		{"number", "error", `level is not a string: want one of ok, warn, error`},
		{"unset", "ok", "fine"},
	} {
		row := got[c.name]
		if string(row.Level) != c.level || !strings.Contains(row.Detail, c.detail) {
			t.Errorf("%s = %+v, want level %s containing %q", c.name, row, c.level, c.detail)
		}
	}
}

// TestHealthListFromLua: list() answers on the interpreter goroutine, where
// the queue luaHealthRows uses would deadlock, and runs registered checks.
func TestHealthListFromLua(t *testing.T) {
	t.Parallel()
	_, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.health.add("disk", function() return { level = "warn", detail = "low" } end)
		local seen = {}
		for _, row in ipairs(zemi.health.list()) do seen[row.name] = row.level .. " " .. row.detail end
		assert(seen.store, "no built-in rows")
		assert(seen.disk == "warn low", "registered check: " .. tostring(seen.disk))
	`)
}

// TestHealthAction: a row's action label and key come from its Run, and a
// run the table does not label takes no action.
func TestHealthAction(t *testing.T) {
	label, key, ok := healthAction("memory")
	if !ok || label != "Review memories" || key != "r" {
		t.Errorf("memory = %q, %q, %v", label, key, ok)
	}
	if _, _, ok := healthAction("nothing"); ok {
		t.Error("a run with no label took an action")
	}
}

// TestHealthActionCoversRuns: every Run a check sets names a label, so the
// row it belongs to keeps its detail action.
func TestHealthActionCoversRuns(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	for _, c := range h.goHealthRows() {
		if _, _, ok := healthAction(c.Run); c.Run != "" && !ok {
			t.Errorf("check %q runs %q, which has no label", c.Name, c.Run)
		}
	}
}

func TestHealthChecks(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	runUser(t, h.RT, `
		zemi.ui.widget.add({ id = "lost", region = "nowhere", render = function() return {} end })
		zemi.ui.widget.add({ id = "bye", region = "exit", render = function() return {} end })`)
	got := map[string]HealthRow{}
	for _, c := range h.HealthRows() {
		got[c.Name] = c
	}
	// no layout draws the exit region: it prints on quit
	if d := got["layout"].Detail; !strings.Contains(d, "lost") || strings.Contains(d, "bye") {
		t.Errorf("layout row %q", d)
	}
	for name, level := range map[string]string{
		"store": "ok", "model": "error", "skills": "ok", "lua": "ok", "mcp": "ok", "layout": "warn",
	} {
		if string(got[name].Level) != level {
			t.Errorf("%s = %+v, want level %s", name, got[name], level)
		}
	}

	items := h.healthItems()
	if first, last := items[0], items[len(items)-1]; first.Section != "Needs attention" || last.Section != "OK" {
		t.Fatalf("problems first: %+v ... %+v", first, last)
	}
	for _, item := range items {
		if item.Text == "store" && item.Detail != "" {
			t.Fatalf("a fine short row opens a detail view: %+v", item)
		}
	}
}

// TestHealthModelRows: the providers, model and context rows. The model row
// checks the model a turn uses (the chat pick), and it and a send name the
// same cause in the same words; a role's own model gets a row of its own;
// the context row warns only about a model whose unknown window costs
// something, the conversation's and the agents'.
func TestHealthModelRows(t *testing.T) {
	t.Parallel()
	const ab = `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", models = { { id = "m", context_window = 100000 } } })
		zemi.provider.add({ id = "b", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", models = { { id = "m" } } })
		zemi.model.set("a/m")
	`
	const anthropic = `zemi.provider.add({ id = "a", kind = "anthropic", api_key = "k", models = { { id = "m" } } })
	`
	// neither model knows its window; agents pick the other one, so the
	// context row names both
	const chat = `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", api_key = "k", models = { { id = "m" } } })
		zemi.provider.add({ id = "b", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = { { id = "m" } } })
		zemi.model.set("a/m") zemi.config.set("models.agents", "b/m")`
	openai := []agent.MissingKey{{ID: "openai", Env: "OPENAI_API_KEY"}}
	x := []agent.MissingKey{{ID: "x", Env: "X_API_KEY"}}
	for _, c := range []struct {
		name, row, lua string
		missing        []agent.MissingKey
		level, detail  string // detail is a prefix
	}{
		{"no provider", "providers", ``, x, "warn", "none usable"},
		{"a builtin waits for its key", "providers", anthropic, x, "ok", "a; 1 builtin without a key exported"},
		{"keyless", "providers", `zemi.provider.add({ id = "d", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })`, nil, "ok", "d (no key)"},
		{"no model", "model", ``, openai, "error", "no model yet: export OPENAI_API_KEY and start zemi again, or add a provider to init.lua"},
		{"unknown provider", "model", `zemi.config.set("models.default", "nobody/m")`, openai, "error", `provider "nobody" is not registered: add a provider to `},
		{"builtin without key", "model", `zemi.config.set("models.default", "openai/gpt-5")`, openai, "error", `builtin provider "openai" needs its key: export OPENAI_API_KEY and start zemi again`},
		{"empty key", "model", `zemi.provider.add({ id = "a", kind = "anthropic", models = { { id = "m" } } })`, openai, "error", "a: api_key is empty in "},
		{"unlisted model", "model", anthropic + `zemi.config.set("models.default", "a/other")`, openai, "warn", "a/other: not in the provider's models list"},
		{"a role's row is named after it", "compact model", anthropic + `zemi.config.set("models.compact", "nobody/m")`, openai, "error", `provider "nobody" is not registered: add a provider to `},
		{"the conversation pick", "model", chat, nil, "ok", "a/m"},
		{"the conversation's and the agents' windows", "context", chat, nil, "warn", "window unknown for a/m: no context percent, no auto compaction; b/m: subagents"},
		{"a role reads no window", "context", ab + `zemi.config.set("models.compact", "b/m")`, nil, "ok", ""},
		{"agents read one", "context", ab + `zemi.config.set("models.agents", "b/m")`, nil, "warn", ""},
	} {
		h, r := newTestHostDB(t)
		if c.missing != nil {
			h.Providers.Reset(agent.Catalog{Missing: c.missing})
		}
		runUser(t, r, c.lua)
		rows := h.HealthRows()
		i := slices.IndexFunc(rows, func(r HealthRow) bool { return r.Name == c.row })
		if i < 0 || string(rows[i].Level) != c.level || !strings.HasPrefix(rows[i].Detail, c.detail) {
			t.Errorf("%s: %s row in %+v", c.name, c.row, rows)
			continue
		}
		if c.level == "error" && c.row == "model" {
			if _, err := h.resolveModel(h.roleModel("default")); err == nil || !strings.HasPrefix(err.Error(), c.detail) {
				t.Errorf("%s: resolveModel = %v", c.name, err)
			}
		}
	}
}

// TestHealthMCPRow: only ready servers read as ready; a starting one is
// named, one that failed or stopped warns, and a sign-in wait says so.
func TestHealthMCPRow(t *testing.T) {
	t.Parallel()
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
		h.mcp.servers = map[string]*mcpServer{}
		for i, st := range c.statuses {
			h.mcp.servers[strconv.Itoa(i)] = &mcpServer{status: st}
		}
		i := slices.IndexFunc(h.goHealthRows(), func(r HealthRow) bool { return r.Name == "mcp" })
		if row := h.goHealthRows()[i]; string(row.Level) != c.level || row.Detail != c.detail {
			t.Errorf("%v: mcp row = %+v", c.statuses, row)
		}
	}
}

// TestWaitMCP: the wait ends when the last server leaves "starting", or
// when its context ends.
func TestWaitMCP(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.mcp.servers = map[string]*mcpServer{}
	s := &mcpServer{status: "starting"}
	h.mcp.servers["a"] = s
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.WaitMCP(ctx) // returns: ctx is done
	done := make(chan struct{})
	go func() { h.WaitMCP(t.Context()); close(done) }()
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

// TestNotifyStyle: auto sends OSC 9 only where a terminal draws it, the
// native notifier where one shows (macOS first, since a terminal's own banner
// is dropped when it was focused), and a bell otherwise.
func TestNotifyStyle(t *testing.T) {
	for _, c := range []struct {
		name, setting, goos, env, tools, want string
		missing                               bool
	}{
		{"ghostty", "auto", "darwin", "TERM_PROGRAM=ghostty", "osascript", "native", false},
		{"ghostty without a notifier", "auto", "darwin", "TERM_PROGRAM=ghostty", "", "osc", false},
		{"kitty over ssh", "auto", "linux", "TERM=xterm-kitty SSH_TTY=/dev/pts/1", "notify-send", "osc", false},
		{"kitty on linux", "auto", "linux", "TERM=xterm-kitty", "notify-send", "osc", false},
		{"tmux on a mac", "auto", "darwin", "TERM_PROGRAM=tmux TERM=xterm-ghostty TMUX=x", "osascript", "native", false},
		{"screen on a mac", "auto", "darwin", "TERM_PROGRAM=ghostty TERM=screen STY=1.pts-0", "osascript", "native", false},
		{"Apple Terminal without a notifier", "auto", "darwin", "TERM_PROGRAM=Apple_Terminal", "", "bell", false},
		{"linux over ssh", "auto", "linux", "TERM=xterm-256color SSH_CONNECTION=x", "notify-send", "bell", false},
		{"native with no tool", "native", "linux", "", "", "bell", true},
		{"off", "off", "darwin", "TERM_PROGRAM=ghostty", "osascript", "off", false},
	} {
		env := map[string]string{}
		for kv := range strings.FieldsSeq(c.env) {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		has := func(name string) bool { return name == c.tools }
		got, why, missing := notifyStyle(c.setting, func(k string) string { return env[k] }, c.goos, has)
		if got != c.want || missing != c.missing {
			t.Errorf("%s: %s missing=%v (%s), want %s missing=%v", c.name, got, missing, why, c.want, c.missing)
		}
	}
}
