package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// newTestHostDB boots a host with a real session store in temp dirs.
func newTestHostDB(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{
		StateHome:  t.TempDir(),
		ProjectDir: t.TempDir(),
	})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// TestCommandSendsTypedLine: a builtin command that sends a ready prompt
// shows the line the user typed; the prompt rides behind the marker and
// names the zemi skill files it follows by a path the read tool opens, which
// holds with the skill switched off.
func TestCommandSendsTypedLine(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	h.RecordError("lua", "boom")
	h.Dirs.DataHome = t.TempDir()
	if err := h.materializeBuiltinSkills(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		typed string
		send  func() error
		file  string
	}{
		{"/init", h.InitProject, ""},
		{"/plugins new show cost", func() error { return h.pluginsCommand("new show cost") }, "plugins.md"},
		{"/agents new reviews my diffs", func() error { return h.agentsCommands()[0].GoFn(h, "new reviews my diffs") }, "agents.md"},
		{"/errors ask lua: boom", func() error { return h.OpenErrors("ask 0") }, "debug.md"},
	} {
		sent := m.streams.Load()
		if err := c.send(); err != nil {
			t.Errorf("%s: %v", c.typed, err)
			continue
		}
		waitFor(t, func() error {
			if m.streams.Load() == sent {
				return errors.New("nothing sent")
			}
			return nil
		})
		waitIdle(t, h)
		typed, tail := agent.CutAttached(lastUserText(t, h))
		if typed != c.typed || !strings.Contains(tail, "<command>") {
			t.Errorf("%s: typed %q, tail %q", c.typed, typed, tail)
		}
		if c.file == "" {
			continue
		}
		for _, f := range []string{"SKILL.md", c.file} {
			if _, err := os.Stat(filepath.Join(h.zemiSkillDir(), f)); err != nil || !strings.Contains(tail, f) || !strings.Contains(tail, " in "+h.zemiSkillDir()) {
				t.Errorf("%s: the tail does not name %s by its path (%v): %q", c.typed, f, err, tail)
			}
		}
	}
}

// runExpectErr runs src as a user chunk and asserts it raises, matching
// every substring in want. The timeout guard turns an interpreter wedge
// into a loud failure (same contract as runUser).
func runExpectErr(t *testing.T, r *lua.Runtime, src string, want ...string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.ExecuteChunk(ctx, src, "test.lua", lua.OriginUser) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error containing %q, got success", want)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("error %q does not contain %q", err.Error(), w)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("chunk did not complete within 3s (interpreter deadlock?)")
	}
}

func TestProfileTemperatureReachesRunner(t *testing.T) {
	h, r := newTestHost(t)
	reflectStub(t, h, "")
	runUser(t, r, `zemi.agent.profile.add("warm", { temperature = 0.3 })`)
	p, err := h.resolveProfile("warm")
	if err != nil {
		t.Fatal(err)
	}
	runner, err := h.ensureRunner(p)
	if err != nil {
		t.Fatal(err)
	}
	if runner.Temperature == nil || *runner.Temperature != 0.3 {
		t.Fatalf("temperature = %v", runner.Temperature)
	}
}

// TestPermissionRuleValidation covers the zemi.permission.rule contract:
// examples must be provided and must behave exactly as the rule claims
// (DESIGN §6.2), allow rules are user-config-only, and accepted rules
// take effect in the permission engine.
// TestProviderBody: extra request-body fields are kept on the spec, and a
// kind whose requests cannot carry them is refused where it is written.
func TestProviderBody(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.add({ id = "gw", kind = "openai_compat", base_url = "https://gw/v1", api_key = "k",
			body = { service_tier = "flex", provider = { order = { "a" } } }, models = { { id = "m" } } })
		local ok, err = pcall(zemi.provider.add, { id = "g", kind = "google", api_key = "k",
			body = { service_tier = "flex" }, models = { { id = "m" } } })
		assert(not ok and tostring(err):find("only by the anthropic, openai, openai_compat"), tostring(err))
	`)
	spec, _ := h.Providers.Lookup("gw")
	if spec.Body["service_tier"] != "flex" {
		t.Fatalf("body = %+v", spec.Body)
	}
	if order, _ := spec.Body["provider"].(map[string]any); len(order) == 0 {
		t.Fatalf("nested table lost: %+v", spec.Body)
	}
}

func TestPermissionRuleValidation(t *testing.T) {
	h, r := newTestHost(t)

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "tool.shell", pattern = "git status", decision = "allow",
	})`, "needs a match example")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "tool.shell", pattern = "git status", decision = "allow",
		match = "git push",
	})`, "does not match pattern")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "tool.shell", pattern = "git status", decision = "allow",
		match = "git status", scope = "permanent",
	})`, "a rule in init.lua applies on every start")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "tool.shell", pattern = "git *", decision = "allow",
		not_match = "git push*", match = "git status",
	})`, "needs both examples")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "shell", pattern = "git status", decision = "allow", match = "git status",
	})`, "starts with none of")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "tool.read", pattern = "*", decision = "deny", match = "x",
	})`, `no tool asks as "tool.read": tool subjects are tool.* tool.shell tool.lua tool.workflow and tool.<a Lua tool>`)

	runUser(t, r, `zemi.permission.rule.add({
		subject = "tool.shell", pattern = "git status", decision = "allow",
		match = "git status",
	})`)
	runUser(t, r, `
		local rules = zemi.permission.rule.list()
		assert(#rules == 1, "rules=" .. #rules)
		assert(rules[1].pattern == "git status", "pattern=" .. rules[1].pattern)
		assert(rules[1].decision == "allow", "decision=" .. rules[1].decision)
	`)

	err := r.ExecuteChunk(context.Background(), `zemi.permission.rule.add({
		subject = "tool.shell", pattern = "git status", decision = "allow",
		match = "git status",
	})`, "plugin.lua", lua.OriginPlugin)
	if err == nil || !strings.Contains(err.Error(), "only come from user config") {
		t.Fatalf("plugin-origin allow rule: %v", err)
	}

	// Lua may always deny, from any origin.
	runUser(t, r, `zemi.permission.rule.add({
		subject = "tool.shell", pattern = "rm -rf *", decision = "deny",
		match = "rm -rf /tmp/x",
	})
	zemi.permission.rule.add({ subject = "tool.ticket", pattern = "*", decision = "deny", match = "x" })
	zemi.tool.add({ name = "ticket", description = "a ticket", execute = function() end })
	zemi.permission.rule.add({ subject = "tool.bash", pattern = "*", decision = "deny", match = "x" })
	for _, name in ipairs({ "shell", "ticket", "mcp_gh_search", "bash" }) do
		zemi.tool.override(name, { pre = function() end })
	end`)
	// only what no tool answers to once boot is done is named
	h.CheckToolNames()
	var named []string
	for _, e := range h.errs.entries {
		named = append(named, e.msg[:strings.Index(e.msg, ",")])
	}
	if want := []string{`no tool asks as "tool.bash"`, `zemi.tool.override names "bash"`}; !slices.Equal(named, want) {
		t.Fatalf("recorded %q, want %q", named, want)
	}

	if out := h.Perm.Evaluate("tool.shell", "git status", "model"); out.Decision != perm.Allow {
		t.Fatalf("git status: %v (%s)", out.Decision, out.Reason)
	}
	if out := h.Perm.Evaluate("tool.shell", "git push", "model"); out.Decision != perm.Ask {
		t.Fatalf("git push: %v (%s)", out.Decision, out.Reason)
	}
	if out := h.Perm.Evaluate("tool.shell", "rm -rf /tmp/x", "model"); out.Decision != perm.Deny {
		t.Fatalf("rm -rf: %v (%s)", out.Decision, out.Reason)
	}

	// a principal scopes a user rule to one plugin; nobody else may scope one
	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "lua.model.ask", pattern = "*", decision = "allow", match = "x", principal = "model",
	})`, `principal must be "plugin:<name>"`)
	err = r.ExecuteChunk(context.Background(), `zemi.permission.rule.add({
		subject = "lua.model.ask", pattern = "*", decision = "deny", match = "x", principal = "plugin:b",
	})`, "plugin.lua", lua.OriginPlugin)
	if err == nil || !strings.Contains(err.Error(), "only user config scopes a rule") {
		t.Fatalf("plugin-origin principal: %v", err)
	}
	runUser(t, r, `
		zemi.permission.rule.add({ subject = "lua.model.ask", pattern = "*", decision = "allow", match = "x", principal = "plugin:a" })
		local rules = zemi.permission.rule.list()
		assert(rules[#rules].principal == "plugin:a", "principal=" .. tostring(rules[#rules].principal))
	`)
	if a, b := h.Perm.Evaluate("lua.model.ask", "x", "plugin:a"), h.Perm.Evaluate("lua.model.ask", "x", "plugin:b"); a.Decision != perm.Allow || b.Decision == perm.Allow {
		t.Fatalf("plugin:a %v, plugin:b %v", a.Decision, b.Decision)
	}

	// A reload replays config, so its rules go; an "always" answer keeps its.
	always := perm.Rule{Subject: "tool.shell", Pattern: "go test *", Decision: perm.Allow, Principal: "model"}
	h.Perm.Add(always)
	for range 2 {
		if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.Perm.Rules(); !slices.Equal(got, []perm.Rule{always}) {
		t.Fatalf("rules after reload: %+v", got)
	}
}

// TestReloadKeepsRuntimePicks: a reload whose config writes what the last
// load wrote keeps a value picked at runtime, config writing one key
// several times included, through zemi.config.set or a setter of its own;
// an edited config value wins. A kept pick outlives a restart under
// init.lua. /config tags the value by where it came from.
func TestReloadKeepsRuntimePicks(t *testing.T) {
	dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
	dirs := config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir()}
	initLua := filepath.Join(dir, "init.lua")
	const head = `zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://x", api_key = "k", models = { { id = "a" }, { id = "b" }, { id = "c" }, { id = "e" }, { id = "z" } } })
zemi.permission.mode.add("wild", { base = "yolo" })
`
	if err := os.WriteFile(initLua, []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	var h *Host
	steps := func() []lua.BootStep {
		return []lua.BootStep{h.NamespacesStep(), lua.LoadFileStep("init", initLua, lua.OriginUser)}
	}
	start := func() {
		r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
		h = NewHost(r, log, config.NewStore(), dirs)
		h.SetUIHooks(&fakeUI{}) // /config reset needs a screen
		if err := r.Boot(t.Context(), steps()); err != nil {
			t.Fatal(err)
		}
	}
	start()
	for _, c := range []struct {
		name     string
		key, set string // the key, and the call init.lua sets it with; "" is models.chat and zemi.config.set
		config   []any  // what init.lua sets the key to, in order
		pick     any    // through SetConfig; nil picks nothing, "reset" runs /config reset
		want     any
		source   string // cfgSource after the reload
		keep     bool   // KeepPicks for the pick: off is a flag's
		restart  bool   // a new zemi instead of a reload
		row      string // a kv row written straight to the global store before the restart
		unkept   bool   // the pick must leave no row in the global store
	}{
		{name: "first load applies config", config: []any{"p/a"}, want: "p/a", source: "init.lua"},
		{name: "unchanged config keeps a pick", config: []any{"p/a"}, pick: "p/b", want: "p/b", source: "this run"},
		{name: "edited config wins over a pick", config: []any{"p/c"}, pick: "p/b", want: "p/c", source: "init.lua"},
		{name: "a second set of the same key applies", config: []any{"p/c", "p/d"}, want: "p/d", source: "init.lua"},
		{name: "an unchanged double set keeps a pick", config: []any{"p/c", "p/d"}, pick: "p/e", want: "p/e", source: "this run"},
		{name: "editing the second set wins", config: []any{"p/c", "p/f"}, pick: "p/e", want: "p/f", source: "init.lua"},
		{name: "dropping the second set wins", config: []any{"p/c"}, pick: "p/e", want: "p/c", source: "init.lua"},
		{name: "a pick outlives the last set", pick: "p/e", want: "p/e", source: "this run"},
		{name: "a set comes back", config: []any{"p/c"}, want: "p/c", source: "init.lua"},
		{name: "dropping every set returns the default", want: "", source: "default"},
		{name: "a key's own setter is a config write", key: "permissions.mode", set: "zemi.permission.mode.set(%[2]s)", config: []any{"yolo"}, want: "yolo", source: "init.lua"},
		{name: "and keeps a pick like one", key: "permissions.mode", set: "zemi.permission.mode.set(%[2]s)", config: []any{"yolo"}, pick: "auto", want: "auto", source: "this run"},
		{name: "a Lua number sets a number key", key: "ui.composer.history", config: []any{5.0}, want: 5.0, source: "init.lua"},
		{name: "so dropping it returns the default", key: "ui.composer.history", want: 50.0, source: "default"},
		{name: "a pick persists", pick: "p/b", keep: true, restart: true, want: "p/b", source: "saved"},
		{name: "init.lua overrides it", config: []any{"p/a"}, restart: true, want: "p/a", source: "init.lua"},
		{name: "a pick over config lasts the run", config: []any{"p/a"}, pick: "p/c", keep: true, want: "p/c", source: "this run"},
		{name: "a restart runs config", config: []any{"p/a"}, restart: true, want: "p/a", source: "init.lua"},
		{name: "dropping the line restores the pick", want: "p/c", source: "saved"},
		{name: "reset forgets it", pick: "reset", keep: true, want: "", source: "default"},
		{name: "for good", restart: true, want: "", source: "default"},
		{name: "yolo is not saved", key: "permissions.mode", pick: "yolo", keep: true, unkept: true, restart: true, want: "auto", source: "default"},
		{name: "nor a mode based on it", key: "permissions.mode", pick: "wild", keep: true, unkept: true, restart: true, want: "auto", source: "default"},
		{name: "nor loaded", key: "permissions.mode", row: `"yolo"`, restart: true, want: "auto", source: "default"},
		{name: "a flag is not saved", pick: "p/z", restart: true, want: "", source: "default"},
	} {
		key := cmp.Or(c.key, "models.chat")
		src := head
		for _, v := range c.config {
			src += fmt.Sprintf(cmp.Or(c.set, "zemi.config.set(%[1]q, %[2]s)"), key, luaLiteral(v)) + "\n"
		}
		if err := os.WriteFile(initLua, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		h.KeepPicks.Store(c.keep)
		switch c.pick {
		case nil:
		case "reset":
			if err := h.configCommand("reset " + key); err != nil {
				t.Fatal(err)
			}
		default:
			if err := h.SetConfig(key, c.pick); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.GlobalDB.GetKV("config:" + key); c.unkept && !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: the pick left a row in the global store (%v)", c.name, err)
		}
		if c.row != "" {
			if err := h.GlobalDB.SetKV("config:"+key, c.row); err != nil {
				t.Fatal(err)
			}
		}
		if c.restart {
			start()
		} else if err := h.ForceReload(t.Context(), steps()); err != nil {
			t.Fatal(err)
		}
		if got, _ := h.Settings.Get(key); got != c.want {
			t.Errorf("%s: %s = %v, want %v", c.name, key, got, c.want)
		}
		if got := h.cfgSource(key); got != c.source {
			t.Errorf("%s: source %q, want %q", c.name, got, c.source)
		}
	}
}

// TestReloadKeepsUserVetoes: while a reload replays config, tool calls are
// still checked against the old Lua rules and wait for the tool.pre handlers
// to come back instead of passing without them.
func TestReloadKeepsUserVetoes(t *testing.T) {
	dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	initLua := filepath.Join(dir, "init.lua")
	src := `
		zemi.permission.rule.add({ subject = "tool.shell", pattern = "rm *", decision = "deny", match = "rm x" })
		zemi.event.on("tool.pre", function() return { decision = "deny", reason = "vetoed" } end)`
	if err := os.WriteFile(initLua, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, src+`
		zemi.permission.rule.add({ subject = "tool.shell", pattern = "curl *", decision = "deny", match = "curl x" })`)
	decision := func(cmd string) perm.Decision { return h.Perm.Evaluate("tool.shell", cmd, "model").Decision }

	pre := h.toolPreHook()
	var vetoed tools.PreVerdict
	done := make(chan struct{})
	probe := lua.BootStep{Name: "probe", Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
		if d := decision("rm x"); d != perm.Deny {
			t.Errorf("mid-reload rm x = %v, want deny", d)
		}
		go func() {
			vetoed, _, _, _ = pre("shell", map[string]any{"command": "ls"})
			close(done)
		}()
		select {
		case <-done:
			t.Error("tool.pre passed before the config replayed its handler")
		case <-time.After(50 * time.Millisecond):
		}
		return nil
	}}
	steps := []lua.BootStep{h.NamespacesStep(), probe, lua.LoadFileStep("init", initLua, lua.OriginUser)}
	if err := h.ForceReload(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	<-done
	if vetoed != "deny" {
		t.Fatalf("tool.pre during reload = %q, want deny", vetoed)
	}
	if decision("rm x") != perm.Deny || decision("curl x") == perm.Deny {
		t.Fatalf("after reload: rm x %v, curl x %v", decision("rm x"), decision("curl x"))
	}
	// the replayed rule is tracked again, so a config without it drops it
	if err := os.WriteFile(initLua, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.ForceReload(context.Background(), []lua.BootStep{h.NamespacesStep(), lua.LoadFileStep("init", initLua, lua.OriginUser)}); err != nil {
		t.Fatal(err)
	}
	if d := decision("rm x"); d == perm.Deny {
		t.Fatal("a rule the config no longer adds survived the reload")
	}
}

// TestToolRegisterValidation covers zemi.tool.add: a spec that could
// never reach the model raises instead of registering (DESIGN §6.0), and
// the builtin-name shadowing ban holds (DESIGN §6.4).
func TestToolRegisterValidation(t *testing.T) {
	_, r := newTestHost(t)

	for _, c := range []struct{ src, want string }{
		{`{ name = "x" }`, "execute callback is required"},
		{`{ name = "shell", execute = function() end }`, "builtin tool name"},
		{`{ name = "tool_search", execute = function() end }`, "builtin tool name"},
		{`{ name = "my tool", execute = function() end }`, "identifier"},
		{`{ name = "x", execute = function() end }`, "description is required"},
		{`{ name = "x", description = "d", execute = function() end,
			input = { type = "object", properties = { city = { type = "string" } } } }`, `input holds "properties"`},
		{`{ name = "x", description = "d", execute = function() end,
			input = { city = { type = "string" }, required = { "city" } } }`, "beside input, not inside it"},
		{`{ name = "x", description = "d", execute = function() end,
			input = { city = { type = "string" } }, required = { "town" } }`, `required names "town"`},
		{`{ name = "x", description = "d", execute = function() end, required = { "city" } }`, "there is no input"},
	} {
		runExpectErr(t, r, `zemi.tool.add(`+c.src+`)`, c.want)
	}

	runUser(t, r, `
		zemi.tool.add({
			name = "weather",
			description = "look up weather",
			input = { city = { type = "string", description = "the city" } },
			required = { "city" },
			execute = function(args) return "sunny in " .. args.city end,
		})
		local found = false
		for _, tl in ipairs(zemi.tool.list()) do
			if tl.name == "weather" then
				found = true
				assert(tl.builtin == false, "weather marked builtin")
			end
		end
		assert(found, "weather tool not listed")
	`)
}

// TestModelNamespace covers user-registered providers and zemi.model
// validation: nothing ships built in, the registry fills from config.
func TestModelNamespace(t *testing.T) {
	h, r := newTestHost(t)

	runUser(t, r, `assert(#zemi.model.list() == 0, "registry must start empty")`)

	runExpectErr(t, r, `zemi.model.set("myllm/main")`, `provider "myllm" is not registered`)

	runUser(t, r, `zemi.provider.add({
		id = "myllm", kind = "openai_compat", base_url = "https://llm.internal/v1",
		api_key = "test-key",
		models = { { id = "main", display = "Main" }, { id = "mini" } },
	})`)
	runExpectErr(t, r, `zemi.provider.add({ id = "old", kind = "openai", api_key_env = "OLD_KEY" })`, `spec: unknown field "api_key_env"`)
	runExpectErr(t, r, `zemi.provider.add({ id = "old", kind = "openai", models = { { id = "a" }, { id = "b", images = false } } })`, `models[2]: unknown field "images"`)
	runExpectErr(t, r, `zemi.provider.add({ id = "old", kind = "openai", models = { [1] = { id = "a" }, [3] = { id = "b" } } })`, `models must be a list`)
	runExpectErr(t, r, `zemi.provider.add({ id = "old", kind = "openai", models = { "a" } })`, `models[1] must be a table`)
	t.Setenv("ZEMI_TEST_TOKEN", "abc")
	runUser(t, r, `assert(zemi.env.get("ZEMI_TEST_TOKEN") == "abc", "user config reads secret-shaped env vars")`)

	runUser(t, r, `zemi.model.set("myllm/main")`)
	if v, ok := h.Settings.Get("models.chat"); !ok || v != "myllm/main" {
		t.Fatalf("models.chat=%v ok=%v", v, ok)
	}
	runUser(t, r, `
		local models = zemi.model.list()
		assert(#models == 2, "models=" .. #models)
		for _, m in ipairs(models) do
			assert(type(m.has_key) == "boolean", "has_key must be a boolean")
			assert(type(m.id) == "string" and m.id ~= "")
		end
	`)
	// a reload replays init.lua: the same id replaces, it does not raise
	runUser(t, r, `
		zemi.provider.add({ id = "myllm", kind = "openai", api_key = "k2", models = { { id = "main" } } })
		assert(#zemi.model.list() == 1, "re-register must replace, not add")
	`)
	runExpectErr(t, r, `zemi.provider.add({ id = "typo", kind = "open_ai" })`, "unknown provider kind")
}

// TestFsDefaults pins the §9 built-in permission defaults: reads inside
// the jail allow unless the path is secret-shaped; everything else asks.
// The Lua boundary fails closed on asks (DESIGN §6.5).
func TestFsDefaults(t *testing.T) {
	h, r := newTestHost(t)
	proj := h.Dirs.ProjectDir

	if out := h.Perm.Evaluate("fs.read", filepath.Join(proj, "main.go"), "model"); out.Decision != perm.Allow {
		t.Fatalf("in-jail read: %v (%s)", out.Decision, out.Reason)
	}
	if out := h.Perm.Evaluate("fs.read", filepath.Join(proj, ".env"), "model"); out.Decision != perm.Ask {
		t.Fatalf(".env read: %v (%s)", out.Decision, out.Reason)
	}
	if out := h.Perm.Evaluate("fs.read", "/etc/hosts", "model"); out.Decision != perm.Ask {
		t.Fatalf("out-of-jail read: %v (%s)", out.Decision, out.Reason)
	}
	if out := h.Perm.Evaluate("fs.write", filepath.Join(proj, "main.go"), "model"); out.Decision != perm.Ask {
		t.Fatalf("in-jail write: %v (%s)", out.Decision, out.Reason)
	}
	if !h.inJail("/etc/hosts", "/") {
		t.Fatal("a root of / holds nothing")
	}

	// In-jail reads pass the gate (file may be missing; that is a plain
	// read error, not a permission failure).
	runUser(t, r, `
		local ok, err = pcall(function() return zemi.fs.read("main.go") end)
		if not ok and tostring(err):find("permission ask required", 1, true) then
			error("in-jail read was gated: " .. tostring(err))
		end
	`)
	runExpectErr(t, r, `zemi.fs.read("/etc/hosts")`, "outside the jail")
	// judged by its name too, not only by its innocently named target
	os.WriteFile(filepath.Join(proj, "notes.txt"), []byte("K=v"), 0o644)
	if err := os.Symlink("notes.txt", filepath.Join(proj, ".env")); err != nil {
		t.Fatal(err)
	}
	runExpectErr(t, r, `zemi.fs.read(".env")`, "permission ask required")
	runExpectErr(t, r, `zemi.ui.edit(".env", {}, function() end)`, "permission ask required")
	pipe := h.PipelineFor()
	pipe.Ask = nil
	if v := pipe.Check(tools.Request{Subject: perm.SubjectRead, Arg: filepath.Join(proj, ".env")}); v.Decision != perm.Ask {
		t.Fatalf("model read of the .env link: %+v", v)
	}
}

// TestSessionNamespace covers the session surface: empty id before the
// first send, title set, and store-backed session creation.
func TestSessionNamespace(t *testing.T) {
	h, r := newTestHostDB(t)

	runUser(t, r, `
		assert(zemi.session.id() == "", "session should start empty")
		zemi.session.rename("my session")
		assert(zemi.session.title() == "my session", "title=" .. zemi.session.title())
		assert(#zemi.session.messages() == 0, "messages should be empty")
	`)

	if err := h.ensureSession(nil, "hello world"); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	if h.AgentSessionID() == "" {
		t.Fatal("session id still empty after ensureSession")
	}
	if err := h.persistMessage(fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi there"}}}); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `assert(zemi.session.id() ~= "", "session id missing")
		local m = zemi.session.messages()[1]
		assert(m.text == "hi there", "text=" .. tostring(m.text))
		assert(m.json:find("hi there", 1, true), "json missing")`)
}

// TestSteeringQueue covers the DESIGN §4 queue: sends during a turn queue
// in order, taking the queue empties it, leftovers become one follow-up
// turn with active and a cancellable scope held across the handoff, and
// cancel is safe without a runner.
func TestSteeringQueue(t *testing.T) {
	h, _ := newTestHost(t)
	claim := func() *turnScope {
		h.agents.mu.Lock()
		defer h.agents.mu.Unlock()
		return h.claim()
	}
	scope := claim()

	send := func(msgs ...string) {
		for _, m := range msgs {
			if err := h.AgentSend(m); err != nil {
				t.Fatalf("queued send: %v", err)
			}
		}
	}
	send("one", "two")
	if q := h.AgentTakeQueue(); len(q) != 2 || q[0] != "one" || q[1] != "two" {
		t.Fatalf("queue = %q", q)
	}
	if q := h.AgentTakeQueue(); len(q) != 0 {
		t.Fatalf("queue after take = %q", q)
	}

	send("a", "b")
	next, text := h.nextTurn(scope, nil)
	if text != "a\n\nb" || !h.AgentActive() {
		t.Fatalf("follow-up = %q, active = %v", text, h.AgentActive())
	}
	h.AgentCancel() // before the follow-up's first step
	if next.ctx.Err() == nil {
		t.Fatal("a cancel during the handoff missed the follow-up turn")
	}
	if next, text := h.nextTurn(claim(), nil); next != nil || text != "" || h.AgentActive() {
		t.Fatalf("idle handoff = %q, active = %v", text, h.AgentActive())
	}

	// a cancelled turn starts nothing: the queue waits for the composer, a
	// command as its typed line
	scope = claim()
	send("c", "/init"+agent.Attached("command", "x"))
	if st := h.AgentStream(); st.Reports != 0 {
		t.Fatalf("a queued command counts as a report: %d", st.Reports)
	}
	h.AgentCancel()
	if next, text := h.nextTurn(scope, nil); next != nil || text != "" || h.AgentActive() {
		t.Fatalf("cancelled handoff = %q, active = %v", text, h.AgentActive())
	}
	if q := h.AgentTakeQueue(); len(q) != 2 || q[0] != "c" || q[1] != "/init" {
		t.Fatalf("queue after a cancel = %q", q)
	}

	// a report's markup stays out of the composer, and joined messages keep
	// every typed part ahead of the marker readers cut at
	report := "Background agent x finished." + agent.AttachMarker + "\n<agent-report run=\"x\" agent=\"explore\" status=\"finished\" description=\"d\">\nr\n</agent-report>\n</attached-files>"
	scope = claim()
	send(report, "d")
	if st := h.AgentStream(); len(st.Steering) != 2 || st.Steering[0] != "Background agent x finished." || st.Reports != 1 {
		t.Fatalf("shown queue with a report = %q, reports = %d", st.Steering, st.Reports)
	}
	if q := h.AgentTakeQueue(); len(q) != 1 || q[0] != "d" {
		t.Fatalf("queue with a report = %q", q)
	}
	send("e")
	if _, text := h.nextTurn(scope, nil); !strings.HasPrefix(text, "Background agent x finished.\n\ne"+agent.AttachMarker) || !strings.Contains(text, "<agent-report run=") {
		t.Fatalf("follow-up with a report = %q", text)
	}

	// a report a cancel leaves queued is announced at once, and a closing
	// session tells it whole instead of dropping it
	notes := func() string {
		h.evlog.mu.Lock()
		defer h.evlog.mu.Unlock()
		var b strings.Builder
		for _, n := range h.evlog.notes {
			b.WriteString(n.title + ": " + n.body + "\n")
		}
		return b.String()
	}
	scope = claim()
	send(agent.AttachMarker + "\n<agent-report run=\"a1\" agent=\"explore\" status=\"finished\" description=\"find \\\"x\\\"\">\nx is in y.go\n</agent-report>\n</attached-files>")
	h.AgentCancel()
	h.nextTurn(scope, nil)
	if !strings.Contains(notes(), "Waiting for your next message: Background agent a1 (explore) finished") {
		t.Fatalf("no waiting notice: %q", notes())
	}
	if err := h.closeSession(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notes(), "Background agent a1 (explore) finished: find \"x\"\nx is in y.go") {
		t.Fatalf("closing dropped the report: %q", notes())
	}

	h.AgentCancel() // no runner: must not panic
}

// TestHistoryCarriesAcrossTurns: the runner is rebuilt on every send, and the
// second turn's prompt still carries the first exchange (system, context,
// user, assistant, user), and both turns land in the transcript.
func TestHistoryCarriesAcrossTurns(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	for _, text := range []string{"one", "two"} {
		if err := h.AgentSend(text); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		waitIdle(t, h)
	}
	if n := m.lastPrompt.Load(); n != 5 {
		t.Fatalf("second turn prompt has %d messages, want 5", n)
	}
	if tr := h.Session().Transcript; len(tr) != 4 {
		t.Fatalf("transcript = %+v", tr)
	}
}

// TestPipelineForCheck verifies the model-principal pipeline the host
// builds for agent tool calls: rules decide, unresolved asks fail closed
// when no UI is attached, and an asker resolves them (DESIGN §6.2).
func TestPipelineForCheck(t *testing.T) {
	h, _ := newTestHost(t)
	pipe := h.PipelineFor()

	// No rules, no UI: the ask hook fails closed.
	if v := pipe.Check(tools.Request{Subject: "tool.shell", Arg: "git status"}); v.Decision != perm.Deny || v.DecidedBy != tools.ByNobody {
		t.Fatalf("unruled shell: %v by %q (%s)", v.Decision, v.DecidedBy, v.Reason)
	}

	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "git status", Decision: perm.Allow, Principal: "model"})
	if v := pipe.Check(tools.Request{Subject: "tool.shell", Arg: "git status"}); v.Decision != perm.Allow {
		t.Fatalf("ruled shell: %v (%s)", v.Decision, v.Reason)
	}
	if v := pipe.Check(tools.Request{Subject: "tool.shell", Arg: "git push"}); v.Decision != perm.Deny {
		t.Fatalf("other shell: %v (%s)", v.Decision, v.Reason)
	}

	// An asker (the UI overlay) resolves the leftover ask.
	pipe2 := h.PipelineFor()
	pipe2.Ask = func(req tools.Request) (bool, string, string) { return true, "user:once", "" }
	if v := pipe2.Check(tools.Request{Subject: "tool.shell", Arg: "git push"}); v.Decision != perm.Allow || v.DecidedBy != "user:once" {
		t.Fatalf("asked shell: %v by %q", v.Decision, v.DecidedBy)
	}
}

// TestStoreBackedState runs its steps in order against one global store:
// a lower origin neither reads nor clobbers a user key (DESIGN §6.5).
func TestStoreBackedState(t *testing.T) {
	_, r := newTestHostDB(t)
	for _, step := range []struct {
		name   string
		origin lua.Origin
		src    string
	}{
		{"user roundtrip", lua.OriginUser, `
			zemi.state.set("note", { text = "hi", n = 2 })
			local v = zemi.state.get("note")
			assert(v and v.text == "hi" and v.n == 2, "state roundtrip failed")`},
		{"project own space", lua.OriginProject, `
			assert(zemi.state.get("note") == nil, "project read a user key")
			assert(zemi.state.set("note", "p"))
			assert(zemi.state.get("note") == "p", "project roundtrip failed")
			local k = zemi.state.keys()
			assert(#k == 1 and k[1] == "note", "project keys: " .. table.concat(k, ","))`},
		{"plugin code outside a plugin file", lua.OriginPlugin, `
			assert(not pcall(zemi.state.get, "note"), "a nameless plugin read a state space")
			assert(not pcall(zemi.state.del, "note"), "a nameless plugin deleted from a state space")`},
		{"user key intact", lua.OriginUser, `
			assert(zemi.state.get("note").text == "hi", "user key clobbered")
			assert(zemi.state.get("project:note") == "p", "user cannot see project state")`},
	} {
		if err := r.ExecuteChunk(t.Context(), step.src, step.name+".lua", step.origin); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
}

// fakeUI is a UIHooks stub driving the host-side overlay paths in tests.
type fakeUI struct {
	askAllowed  bool
	askBy       string
	onAsk       func(PermRequest) (bool, string) // answers in place of askAllowed and askBy
	asked       []PermRequest
	qAsked      []tools.Question
	qAnswer     string
	qOK         bool
	planVerdict tools.PlanVerdict
	planOK      bool
	planAsked   int
	mu          sync.Mutex // guards opened: a status change opens off the test goroutine
	opened      []Overlay
	closed      []string
	closedLua   int
	turnsDone   atomic.Int32
	screenshot  string
	copied      string
	edit        func(path string) // what the user does in the editor
	relaunched  string
}

func (f *fakeUI) Notify(slog.Level, string, string) {}
func (f *fakeUI) OpenOverlay(o Overlay) {
	f.mu.Lock()
	f.opened = append(f.opened, o)
	f.mu.Unlock()
}

func (f *fakeUI) RefreshOverlay(string, func() Overlay) {}

func (f *fakeUI) overlays() []Overlay {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.opened)
}
func (f *fakeUI) CloseOverlay(id string, _ lua.Origin) { f.closed = append(f.closed, id) }
func (f *fakeUI) CloseLuaOverlays()                    { f.closedLua++ }
func (f *fakeUI) Refresh()                             {}
func (f *fakeUI) Scroll(ScrollAction, string)          {}
func (f *fakeUI) LayoutRects() map[string]Rect         { return nil }
func (f *fakeUI) AgentStream()                         {}
func (f *fakeUI) TurnDone(err string, stopped bool)    { f.turnsDone.Add(1) }
func (f *fakeUI) ComposerEdit(ComposerOp, string, int) {}
func (f *fakeUI) EditFile(_ context.Context, req EditRequest) error {
	if f.edit != nil {
		f.edit(req.Path)
	}
	return nil
}
func (f *fakeUI) Copy(text string)    { f.copied = text }
func (f *fakeUI) Relaunch(dir string) { f.relaunched = dir }

func (f *fakeUI) AskPermission(req PermRequest) (bool, string) {
	f.asked = append(f.asked, req)
	if f.onAsk != nil {
		return f.onAsk(req)
	}
	return f.askAllowed, f.askBy
}

func (f *fakeUI) Screenshot() string { return f.screenshot }

func (f *fakeUI) AskQuestion(qs []tools.Question) ([]tools.Answer, bool) {
	f.qAsked = append(f.qAsked, qs...)
	answers := make([]tools.Answer, len(qs))
	for i := range answers {
		answers[i] = tools.Answer{Picked: []string{f.qAnswer}}
	}
	return answers, f.qOK
}

func (f *fakeUI) AskPlan(plan, implementMode string) (tools.PlanVerdict, bool) {
	f.planAsked++
	return f.planVerdict, f.planOK
}

// TestAskHookAlwaysStoresRules drives the real askHook (veto event, UI
// overlay, rule storage) through the model pipeline: an "always" answer
// stores the command's prefix rules, which answer the next check without
// asking (DESIGN §6.2).
func TestAskHookAlwaysStoresRules(t *testing.T) {
	h, _ := newTestHost(t)
	fakeSandbox(h, true)
	ui := &fakeUI{askAllowed: true, askBy: "user:always"}
	h.SetUIHooks(ui)

	pipe := h.PipelineFor()
	v := pipe.Check(tools.Request{Subject: "tool.shell", Arg: "git push origin main"})
	if v.Decision != perm.Allow || v.DecidedBy != "user:always" {
		t.Fatalf("always shell: %v by %q", v.Decision, v.DecidedBy)
	}
	if len(ui.asked) != 1 || ui.asked[0].Arg != "git push origin main" || ui.asked[0].Always != "allow git push outside the sandbox" {
		t.Fatalf("asked: %+v", ui.asked)
	}
	rules := h.Perm.Rules()
	if len(rules) != 2 || rules[1].Pattern != "git push *" || rules[1].Decision != perm.Allow {
		t.Fatalf("stored rules: %+v", rules)
	}

	// The prefix rule answers the next push without asking again.
	if got := h.PipelineFor().Check(tools.Request{Subject: "tool.shell", Arg: "git push origin feature"}); got.Decision != perm.Allow {
		t.Fatalf("ruled re-check: %v by %q", got.Decision, got.DecidedBy)
	}
	if len(ui.asked) != 1 {
		t.Fatalf("ruled re-check must not ask: %+v", ui.asked)
	}
}

// TestMemoryToolThroughPipeline drives the memory tool as the model sees
// it: under ask autonomy the memory queues for review, and the answer says
// so.
func TestMemoryToolThroughPipeline(t *testing.T) {
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	if _, err := h.Settings.SetQuiet("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}
	run := func(name, input string) fantasy.ToolResponse {
		t.Helper()
		for _, ag := range tools.All(h.PipelineFor()) {
			if ag.Info().Name == name {
				resp, err := ag.Run(context.Background(), fantasy.ToolCall{ID: "t", Name: name, Input: input})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return resp
			}
		}
		t.Fatalf("no tool %s", name)
		return fantasy.ToolResponse{}
	}
	if resp := run("memory", `{"text":"bump the labels sequence before deploying"}`); resp.IsError || resp.Content != "queued for the user's review" {
		t.Fatalf("memory: %+v", resp)
	}
	if n := len(h.memoryProposals()); n != 1 {
		t.Fatalf("proposals=%d", n)
	}
}

// TestPipelineQuestionToolEndToEnd drives the question tool through
// PipelineFor and the fakeUI: the structured ask reaches the UI hooks and
// the chosen label comes back as the tool output; with no UI the pipeline
// has no question hook, so the tool tells the model nobody can answer.
func TestPipelineQuestionToolEndToEnd(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{qAnswer: "proceed", qOK: true}
	h.SetUIHooks(ui)
	pipe := h.PipelineFor()

	var resp fantasy.ToolResponse
	for _, ag := range tools.All(pipe) {
		if ag.Info().Name != "question" {
			continue
		}
		var err error
		resp, err = ag.Run(context.Background(), fantasy.ToolCall{
			ID: "t1", Name: "question",
			Input: `{"questions":[{"type":"single_choice","question":"Ship it?","options":[{"label":"proceed"},{"label":"abort"}]}]}`,
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		break
	}
	if resp.IsError || !strings.Contains(resp.Content, `"answer":"proceed"`) {
		t.Fatalf("question through pipeline: %+v", resp)
	}
	if len(ui.qAsked) != 1 || ui.qAsked[0].Question != "Ship it?" || len(ui.qAsked[0].Options) != 2 {
		t.Fatalf("ask plumbing: %+v", ui.qAsked)
	}

	bare, _ := newTestHost(t)
	if bare.PipelineFor().Question != nil {
		t.Fatal("a host without UI must leave the question hook unset")
	}
}

// TestPipelineTodos: todos round-trip through the store for the active
// session.
func TestPipelineTodos(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)

	pipe := h.PipelineFor()
	if err := pipe.TodosWrite([]store.Todo{{Content: "first", Status: "in_progress", ActiveForm: "doing first"}}); err != nil {
		t.Fatalf("TodosWrite: %v", err)
	}
	rows, err := pipe.TodosRead()
	if err != nil || len(rows) != 1 || rows[0].Content != "first" || rows[0].Status != "in_progress" {
		t.Fatalf("TodosRead: %v %+v", err, rows)
	}
	persisted, err := h.DB.Todos(sid)
	if err != nil || len(persisted) != 1 || persisted[0].Content != "first" {
		t.Fatalf("todos must persist for the session: %v %+v", err, persisted)
	}
}

// TestTodoNag: a stale in_progress todo earns a model-only remind after
// todoNagAfter steps, once per engagement; any todos write re-arms it, and
// a step counts once however many calls it batched.
func TestTodoNag(t *testing.T) {
	both := []store.Todo{{Content: "a", Status: "done"}, {Content: "b", Status: "in_progress"}}
	set := func(h *Host, todos []store.Todo) {
		if err := h.PipelineFor().TodosWrite(todos); err != nil {
			t.Fatalf("TodosWrite: %v", err)
		}
	}
	engage := func(h *Host, n int) {
		h.agents.mu.Lock()
		h.agents.active = true // maybeNagTodos only queues while live
		h.agents.mu.Unlock()
		for range n {
			h.maybeNagTodos()
		}
	}
	// takeNag delivers the armed nag the way a step boundary does
	takeNag := func(h *Host) string {
		h.agents.mu.Lock()
		defer h.agents.mu.Unlock()
		nag := h.agents.todoNag
		h.agents.todoNag = ""
		return nag
	}

	t.Run("nags once past the threshold, not again without an update", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		seedSession(t, h)
		set(h, both)
		engage(h, todoNagAfter-1)
		if got := takeNag(h); got != "" {
			t.Fatalf("pre-threshold nag = %q", got)
		}
		engage(h, 1)
		if got := takeNag(h); !strings.Contains(got, "in_progress") {
			t.Fatalf("post-threshold nag = %q", got)
		}
		engage(h, 5)
		if got := takeNag(h); got != "" {
			t.Fatalf("nag must not repeat without an update: %q", got)
		}
	})

	t.Run("a step counts once whatever its calls, none with a todos call", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		seedSession(t, h)
		set(h, both)
		engage(h, 0)
		s := h.streamHooks()
		for i := range todoNagAfter * 2 {
			s.ToolCallEnd(strconv.Itoa(i), "read", true, "", "")
		}
		s.StepDone(agent.Step{})
		s.ToolCallEnd("t", tools.ToolTodos, true, "", "")
		s.StepDone(agent.Step{})
		for range todoNagAfter - 2 {
			s.StepDone(agent.Step{})
		}
		if got := takeNag(h); got != "" {
			t.Fatalf("a batch of calls is one step: %q", got)
		}
		s.StepDone(agent.Step{})
		if got := takeNag(h); !strings.Contains(got, fmt.Sprintf("for %d steps", todoNagAfter)) {
			t.Fatalf("nag after the threshold: %q", got)
		}
	})

	t.Run("a nag the turn ended before delivering starts no turn", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		seedSession(t, h)
		set(h, both)
		engage(h, todoNagAfter) // a StopTurn step (a dismissed question) past the threshold
		h.agents.mu.Lock()
		scope := h.claim()
		h.agents.mu.Unlock()
		if next, text := h.nextTurn(scope, nil); next != nil || text != "" || h.AgentActive() {
			t.Fatalf("the nag started a turn: %q", text)
		}
		if got := takeNag(h); got != "" {
			t.Fatalf("the next turn's first step would carry the stale nag: %q", got)
		}
		engage(h, 1)
		if got := takeNag(h); got == "" {
			t.Fatal("a dropped nag must re-arm, the model never saw it")
		}
	})

	t.Run("a todos write re-arms the tracker", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		seedSession(t, h)
		set(h, both)
		engage(h, todoNagAfter)
		if takeNag(h) == "" {
			t.Fatal("expected one nag before re-arm")
		}
		set(h, both) // any update re-arms
		engage(h, todoNagAfter)
		if takeNag(h) == "" {
			t.Fatal("after re-arm expect a second nag")
		}
	})

	t.Run("a call while idle does not count", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		seedSession(t, h)
		set(h, both)
		for range todoNagAfter {
			h.maybeNagTodos() // a background agent's calls, the main agent idle
		}
		engage(h, todoNagAfter-1)
		if got := takeNag(h); got != "" {
			t.Fatalf("idle calls must not count or arm: %q", got)
		}
	})

	t.Run("a call the interrupt ended arms nothing", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		seedSession(t, h)
		set(h, both)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h.turn.Store(&turnScope{ctx: ctx, cancel: cancel})
		engage(h, todoNagAfter)
		if got := takeNag(h); got != "" {
			t.Fatalf("a nag for a cancelled turn would lead the next message: %q", got)
		}
	})

	t.Run("a closed session takes its tracker with it", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		seedSession(t, h)
		set(h, both)
		engage(h, todoNagAfter-1)
		h.agents.mu.Lock()
		h.agents.active = false
		h.agents.mu.Unlock()
		if err := h.closeSession(nil); err != nil {
			t.Fatal(err)
		}
		seedSession(t, h)
		engage(h, 1)
		if got := takeNag(h); got != "" {
			t.Fatalf("the old session's item nagged the new one: %q", got)
		}
	})

	t.Run("a single in_progress item or none never nags", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		seedSession(t, h)
		set(h, []store.Todo{{Content: "solo", Status: "in_progress"}})
		engage(h, todoNagAfter+2)
		if got := takeNag(h); got != "" {
			t.Fatalf("single item must not nag: %q", got)
		}
		set(h, []store.Todo{{Content: "a", Status: "done"}, {Content: "b", Status: "done"}})
		engage(h, todoNagAfter+2)
		if got := takeNag(h); got != "" {
			t.Fatalf("all done must not nag: %q", got)
		}
	})
}

// TestTurnDoneRecordsStats covers the turn stat plumbing: the assistant
// transcript entry carries tokens, thought time and elapsed time; plain
// appended messages carry none.
func TestTurnDoneRecordsStats(t *testing.T) {
	h, _ := newTestHostDB(t)
	sa := h.streamHooks()
	sa.ReasoningDelta("pondering")
	sa.TextDelta("answer")
	sa.ToolCallStart("t1", "shell", "ls")
	sa.ToolCallEnd("t1", "shell", true, "", "")
	sa.TextDelta("done")
	sa.TurnDone(nil, fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120})

	// text streamed before a tool call stays above its row
	var order []string
	for _, m := range h.Session().Transcript {
		order = append(order, string(m.Kind)+":"+m.Text)
	}
	if got := strings.Join(order, " | "); got != "reasoning:pondering | :answer | tool:ls | :done" {
		t.Fatalf("transcript order = %s", got)
	}
	last := h.Session().Transcript[len(h.Session().Transcript)-1]
	if last.Role != "assistant" || last.Stat == nil {
		t.Fatalf("last = %+v, want assistant with stats", last)
	}
	if last.Stat.In != 100 || last.Stat.Out != 20 {
		t.Fatalf("stat = %+v", last.Stat)
	}
	if last.Stat.ThoughtMS < 0 || last.Stat.ElapsedMS < 0 {
		t.Fatalf("negative timing: %+v", last.Stat)
	}
	if last.Stat.ThoughtMS > 10_000 || last.Stat.ElapsedMS > 10_000 {
		t.Fatalf("implausible timing: %+v", last.Stat)
	}

	h.AppendMessage("user", "hi")
	user := h.Session().Transcript[len(h.Session().Transcript)-1]
	if user.Role != "user" || user.Stat != nil {
		t.Fatalf("user message must carry no stats: %+v", user)
	}
}

// TestTurnEndSettlesTodos: an item left in_progress when the claim ends goes
// back to pending, so no row of the list reads as running while the agent is
// idle; the rest of the model's list is kept as it wrote it.
func TestTurnEndSettlesTodos(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	wrote := []store.Todo{
		{Content: "a", Status: store.TodoDone},
		{Content: "b", Status: store.TodoInProgress, ActiveForm: "doing b"},
		{Content: "c", Status: store.TodoPending},
	}
	if err := h.PipelineFor().TodosWrite(wrote); err != nil {
		t.Fatal(err)
	}
	h.agents.mu.Lock()
	scope := h.claim()
	h.agents.mu.Unlock()
	if next, text := h.nextTurn(scope, nil); next != nil || text != "" || h.AgentActive() {
		t.Fatalf("an idle agent started a turn: %q", text)
	}
	waitFor(t, func() error {
		todos, err := h.DB.Todos(sid)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(todos, func(td store.Todo) bool { return td.Status == store.TodoInProgress }) {
			return errors.New("an item is still in progress")
		}
		return nil
	})
	todos, err := h.DB.Todos(sid)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Todo{
		{Content: "a", Status: store.TodoDone},
		{Content: "b", Status: store.TodoPending, ActiveForm: "doing b"},
		{Content: "c", Status: store.TodoPending},
	}
	if !slices.Equal(todos, want) {
		t.Fatalf("settled list = %+v, want %+v", todos, want)
	}
}

func TestAlwaysChoice(t *testing.T) {
	h, _ := newTestHost(t)
	fakeSandbox(h, false) // the effect cases below settle it
	t.Chdir(h.Dirs.ProjectDir)
	proj := perm.RealPath(h.Dirs.ProjectDir) // rules name the resolved dir
	home := perm.RealPath(os.Getenv("HOME"))
	for _, c := range []struct {
		req      tools.Request
		label    string
		patterns []string
	}{
		{tools.Request{Subject: "tool.shell", Arg: "go test ./... && git diff"}, "allow go test and git diff", []string{"go test", "go test *", "git diff", "git diff *"}},
		{tools.Request{Subject: "tool.shell", Arg: "ls -la"}, "allow ls", []string{"ls", "ls *"}},
		{tools.Request{Subject: "tool.shell", Arg: "cat notes"}, "allow cat", []string{"cat", "cat *"}},
		// a quoted program name is one word: `my *` would allow program my
		{tools.Request{Subject: "tool.shell", Arg: "'my tool' --x"}, "allow my tool", []string{"my tool", "my tool *"}},
		{tools.Request{Subject: "tool.shell", Arg: "go vet && git diff && make && ls"}, "allow go vet, git diff and 2 more", []string{"go vet", "go vet *", "git diff", "git diff *", "make", "ls", "ls *"}},
		// shell syntax is no command, and a no-op builtin is not named
		{tools.Request{Subject: "tool.shell", Arg: "for f in $(ls); do cat $f; done"}, "allow ls and cat", []string{"ls", "ls *", "cat", "cat *"}},
		{tools.Request{Subject: "tool.shell", Arg: "if true; then ls; fi"}, "allow ls", []string{"true", "true *", "ls", "ls *"}},
		{tools.Request{Subject: "tool.shell", Arg: "for f in $(ls); do rm $f; done"}, "", nil},
		// syntax is named when nothing else is, so the label says what the rules allow
		{tools.Request{Subject: "tool.shell", Arg: "set -e"}, "allow set", []string{"set", "set *"}},
		// read evaluates a quoted subscript, which can run $(...): exact
		{tools.Request{Subject: "tool.shell", Arg: "read n"}, "allow this exact command", []string{"read n"}},
		// a deleter stays exact: the rule runs unconfined and has no write path to ask on
		{tools.Request{Subject: "tool.shell", Arg: "rm build.tmp"}, "allow this exact command", []string{"rm build.tmp"}},
		{tools.Request{Subject: "tool.shell", Arg: "git status && rm -rf build && rm -f x"}, "allow rm (exact) and git status", []string{"git status", "git status *", "rm -rf build", "rm -f x"}},
		{tools.Request{Subject: "tool.shell", Arg: "/bin/rm old"}, "allow this exact command", []string{"/bin/rm old"}},
		{tools.Request{Subject: "tool.shell", Arg: "find . -type f"}, "allow this exact command", []string{"find . -type f"}},
		{tools.Request{Subject: "tool.shell", Arg: "tar czf a.tgz src"}, "allow this exact command", []string{"tar czf a.tgz src"}},
		{tools.Request{Subject: "tool.shell", Arg: "find . -delete"}, "allow this exact command", []string{"find . -delete"}},
		// a destructive subcommand too: `git clean` as a whole covers -fdx
		{tools.Request{Subject: "tool.shell", Arg: "git clean -n"}, "allow this exact command", []string{"git clean -n"}},
		{tools.Request{Subject: "tool.shell", Arg: "PATH=/tmp ls"}, "", nil},
		// a leading option keeps the subcommand pair, never the bare program
		{tools.Request{Subject: "tool.shell", Arg: "git --no-pager log -5"}, "allow git --no-pager log", []string{"git --no-pager log", "git --no-pager log *"}},
		{tools.Request{Subject: "tool.shell", Arg: "git -C sub status"}, "allow git -C sub status", []string{"git -C sub status", "git -C sub status *"}},
		{tools.Request{Subject: "tool.shell", Arg: "cargo +nightly build"}, "allow cargo +nightly build", []string{"cargo +nightly build", "cargo +nightly build *"}},
		{tools.Request{Subject: "tool.shell", Arg: "git -C /other/repo status"}, "allow git -C /other/repo status", []string{"git -C /other/repo status", "git -C /other/repo status *"}},
		{tools.Request{Subject: "tool.shell", Arg: "git --version"}, "allow this exact command", []string{"git --version"}},
		// kubectl's -s takes a value, so prod is not its subcommand
		{tools.Request{Subject: "tool.shell", Arg: "kubectl -s prod get pods"}, "allow this exact command", []string{"kubectl -s prod get pods"}},
		// a path or capitalized spelling is the same program: `/usr/bin/git *` would be every subcommand
		{tools.Request{Subject: "tool.shell", Arg: "/usr/bin/git push"}, "allow /usr/bin/git push", []string{"/usr/bin/git push", "/usr/bin/git push *"}},
		{tools.Request{Subject: "tool.shell", Arg: "Git push"}, "allow Git push", []string{"Git push", "Git push *"}},
		{tools.Request{Subject: "tool.shell", Arg: "/usr/bin/git"}, "allow this exact command", []string{"/usr/bin/git"}},
		// a bare subcommand program stays exact too: `git *` is every subcommand
		{tools.Request{Subject: "tool.shell", Arg: "git"}, "allow this exact command", []string{"git"}},
		{tools.Request{Subject: "tool.shell", Arg: "npx prettier --check ."}, "", nil},
		// interpreters and wrappers run whatever they are handed: no rule
		// written from one of these says what the next call would run
		{tools.Request{Subject: "tool.shell", Arg: "bash -c 'go test ./...'"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "/bin/sh -lc 'ls'"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "baſh -c id"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "zsh script.sh"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "node -e 'x'"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "python3 -c 'x'"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "perl -e 'x'"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "ruby -e 'x'"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "env FOO=1 ls"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "sudo ls"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "xargs rm"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "nohup ls"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "time ls"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "trap 'rm -rf x' EXIT"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "coproc ls"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "go test ./... && sudo ls"}, "", nil},
		{tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, "internal", "a.go")}, "allow edits in internal/ and below", []string{filepath.Join(proj, "internal", "*")}},
		// home, / and above hold every dotfile: only the file, named with ~
		{tools.Request{Subject: "fs.write", Arg: filepath.Join(home, ".zshrc")}, "allow edits of ~/.zshrc", []string{filepath.Join(home, ".zshrc")}},
		{tools.Request{Subject: "fs.write", Arg: "/foo.txt"}, "allow edits of /foo.txt", []string{"/foo.txt"}},
		{tools.Request{Subject: "fs.read", Arg: filepath.Join(home, "notes", "a.md")}, "allow reads in ~/notes/ and below", []string{filepath.Join(home, "notes", "*")}},
		// the gate asks about a protected write whatever the rules say
		{tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, ".agents", "init.lua")}, "", nil},
		// Pipeline.Check sends a secret-shaped file back to the ask whatever a model rule says
		{tools.Request{Subject: "fs.read", Arg: ".env"}, "allow reads of .env", []string{filepath.Join(proj, ".env")}},
		{tools.Request{Subject: "fs.read", Arg: "main.go"}, "allow reads anywhere in the project", []string{filepath.Join(proj, "*")}},
		// the subcommand spellings of npx and uvx, and a wrapper name, offer nothing
		{tools.Request{Subject: "tool.shell", Arg: "npm exec prettier -- --check ."}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "pnpm dlx x"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "uv run x.py"}, "", nil},
		// an unknown option may take the next word, so the subcommand is not sure
		{tools.Request{Subject: "tool.shell", Arg: "npm -w web test"}, "allow this exact command", []string{"npm -w web test"}},
		{tools.Request{Subject: "tool.shell", Arg: "uv --directory sub run pytest"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "busybox ls"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "zemi plugins"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "awk '{print}' f"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "npm run build"}, "allow npm run", []string{"npm run", "npm run *"}},
		{tools.Request{Subject: "tool.shell", Arg: "sed -n 1p f"}, "allow this exact command", []string{"sed -n 1p f"}},
		{tools.Request{Subject: "net.*", Arg: "https://pkg.go.dev/fmt"}, "allow pkg.go.dev", []string{"https://pkg.go.dev", "https://pkg.go.dev/*"}},
		{tools.Request{Subject: "tool.workflow", Arg: "script"}, "", nil},
		{tools.Request{Subject: "memory.write", Arg: "e9865952"}, "", nil},
		{tools.Request{Subject: "tool.jira_create", Arg: "jira_create"}, "allow the jira_create tool", []string{"jira_create"}},
		{tools.Request{Subject: "mcp.github", Arg: "create_issue"}, "allow create_issue from github", []string{"create_issue"}},
		{tools.Request{Subject: "tool.shell", Arg: "rm *.tmp"}, "", nil},
		{tools.Request{Subject: "tool.shell", Arg: "go test ./... && ls ?"}, "", nil},
		{tools.Request{Subject: "fs.read", Arg: filepath.Join(proj, "*", "notes.md")}, "", nil},
		{tools.Request{Subject: "net.*", Arg: "https://*/"}, "", nil},
	} {
		label, rules := h.alwaysChoice(c.req)
		var pats []string
		for _, r := range rules {
			pats = append(pats, r.Pattern)
		}
		if label != c.label || strings.Join(pats, "|") != strings.Join(c.patterns, "|") {
			t.Errorf("%s %q: label %q rules %q", c.req.Subject, c.req.Arg, label, pats)
		}
	}
	// a whole-command rule lets the next run differ in its arguments
	_, rules := h.alwaysChoice(tools.Request{Subject: "tool.shell", Arg: "ls -la"})
	s := new(perm.Store)
	for _, r := range rules {
		s.Add(r)
	}
	if o := s.CheckCommandFor(perm.SubjectShell, perm.Normalize("ls -R src"), "model"); o.Decision != perm.Allow {
		t.Errorf("ls -R src after allow `ls` = %v, want allow", o)
	}
	// a deleter's exact rule does not cover another target
	_, del := h.alwaysChoice(tools.Request{Subject: "tool.shell", Arg: "rm build.tmp"})
	s2 := new(perm.Store)
	for _, r := range del {
		s2.Add(r)
	}
	if o := s2.CheckCommandFor(perm.SubjectShell, perm.Normalize("rm src.orig"), "model"); o.Decision == perm.Allow {
		t.Errorf("rm src.orig after allow `rm build.tmp` must ask, got %v", o)
	}
	// a read deep in a tree also offers the whole repository
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	os.MkdirAll(filepath.Join(proj, "internal", "api"), 0o755)
	label, rules := h.repoChoice(tools.Request{Subject: "fs.read", Arg: filepath.Join(proj, "internal", "api", "x.go")})
	if label != "allow reads anywhere in the project" || len(rules) != 1 || rules[0].Pattern != filepath.Join(proj, "*") {
		t.Errorf("repo choice: %q %+v", label, rules)
	}
	if label, _ := h.repoChoice(tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, "internal", "a.go")}); label != "" {
		t.Errorf("writes must not offer the repository: %q", label)
	}
	if label, _ := h.repoChoice(tools.Request{Subject: "fs.read", Arg: filepath.Join(t.TempDir(), "x.go")}); label != "" {
		t.Errorf("a file in no repository: %q", label)
	}
	// what the pipeline decides: the rule, then the protected-write re-ask
	pipe := h.PipelineFor()
	pipe.Ask = nil // an ask comes back as Ask instead of reaching a prompt
	for approved, cases := range map[string]map[string]perm.Decision{
		"main.go": {
			"main.go": perm.Allow, "internal/a.go": perm.Allow, ".gitignore": perm.Allow, ".gitkeep": perm.Allow,
			".github/workflows/ci.yml": perm.Allow, ".gitlab-ci.yml": perm.Allow,
			".git/config": perm.Ask, ".git": perm.Ask, "sub/.git/hooks/pre-commit": perm.Ask, ".gitattributes": perm.Ask, "sub/.gitmodules": perm.Ask,
		},
		".gitmodules": {".gitmodules": perm.Ask},
		"sub/.git":    {"sub/.git": perm.Ask},
	} {
		_, rules := h.alwaysChoice(tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, approved)})
		for _, r := range rules {
			h.Perm.Add(r)
		}
		for arg, want := range cases {
			if got := pipe.Check(tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, arg)}).Decision; got != want {
				t.Errorf("always edits for %s: %s = %v, want %v", approved, arg, got, want)
			}
		}
	}
	// a segment this manager already allows for the session is not listed or
	// re-ruled: only the still-new programs get a name and a rule
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "go test *", Decision: perm.Allow, Principal: "model"})
	label, rules = h.alwaysChoice(tools.Request{Subject: "tool.shell", Arg: "go test ./x && go vet"})
	if label != "allow go vet" {
		t.Errorf("already-allowed segment must not be listed: %q", label)
	}
	var pats []string
	for _, r := range rules {
		pats = append(pats, r.Pattern)
	}
	if strings.Join(pats, "|") != "go vet|go vet *" {
		t.Errorf("already-allowed segment must not get a rule: %q", pats)
	}
	// when everything is already allowed there is nothing to offer: a label
	// with no rules would still let the project answer persist the line
	if label, rules := h.alwaysChoice(tools.Request{Subject: "tool.shell", Arg: "go test ./x"}); label != "" || len(rules) != 0 {
		t.Errorf("all-allowed command must offer nothing: %q %+v", label, rules)
	}
	// the label says where the allowed command will run
	fakeSandbox(h, true)
	for req, want := range map[tools.Request]string{
		{Subject: "tool.shell", Arg: "ls -la"}:                 "allow ls outside the sandbox",
		{Subject: "tool.shell", Arg: "ls -la", Network: true}:  "allow ls with the network",
		{Subject: "tool.shell", Arg: "ls -la", ReadOnly: true}: "allow ls outside the sandbox",
	} {
		if label, _ := h.alwaysChoice(req); label != want {
			t.Errorf("%+v: label %q, want %q", req, label, want)
		}
	}
}

// TestSendWithoutModelLeavesNoSession: a send that cannot resolve a model
// fails before it creates a session row.
func TestSendWithoutModelLeavesNoSession(t *testing.T) {
	h, _ := newTestHostDB(t)
	if err := h.AgentSend("hi"); !errors.Is(err, ErrNoModel) {
		t.Fatalf("AgentSend = %v, want ErrNoModel", err)
	}
	if list, err := h.DB.ListSessions(10); err != nil || len(list) != 0 {
		t.Fatalf("sessions = %v, %v; want none", list, err)
	}
}

// TestTurnPricedWithProfileModel: a turn records the model it was built
// with, so a profile's turn is not priced off the session default.
func TestTurnPricedWithProfileModel(t *testing.T) {
	h, r := newTestHostDB(t)
	reflectStub(t, h, "ok")
	runUser(t, r, `
		zemi.agent.profile.add("cheap", { model = "p/cheap" })
		zemi.config.set("agent.profile", "cheap")
	`)
	if err := h.AgentSend("hi"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	h.agents.mu.Lock()
	got := h.agents.turnModel
	h.agents.mu.Unlock()
	if got != "p/cheap" {
		t.Fatalf("turn model = %q, want the profile's", got)
	}
}

// TestIdleStreamUsesProfileModel: between turns the context window, the
// image check and zemi.agent.current follow the default profile's model,
// not models.chat; compaction and a subagent without a model follow
// models.chat, whatever the conversation runs on.
func TestIdleStreamUsesProfileModel(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = {
			{ id = "big", context_window = 200000, vision = false },
			{ id = "small", context_window = 32000 },
		} })
		zemi.model.set("a/small")
		zemi.agent.profile.add("p", { model = "a/big" })
		zemi.config.set("agent.profile", "p")
	`)
	if s := h.AgentStream(); s.Model != "a/big" || s.CtxWindow != 200000 {
		t.Fatalf("stream model %q window %d, want the profile's", s.Model, s.CtxWindow)
	}
	if h.PipelineFor().Images() {
		t.Fatal("the profile's model is text-only")
	}
	runUser(t, r, `assert(zemi.agent.current().model == "a/big")`)
	if id := h.roleModel("compact"); id != "a/small" {
		t.Fatalf("summary model %q, want models.chat", id)
	}
	if id := h.subagentModelID(AgentDef{}); id != "a/small" {
		t.Fatalf("subagent model %q, want models.chat", id)
	}
}

// TestActiveProfileKey: agent.profile picks the profile a send runs as,
// whatever order config defined things in, an unknown name fails the send,
// and a reload onto a config without the profile runs the shipped one.
func TestActiveProfileKey(t *testing.T) {
	for _, c := range []struct {
		name, src string
		want      string // the resolved profile, "" for the shipped one
		system    string
		wantErr   bool
	}{
		{name: "unset runs the shipped profile"},
		{name: "a defined planner replaces the shipped one", src: `zemi.agent.profile.add("planner", { system = "PLAN HARDER" }) zemi.config.set("agent.profile", "planner")`, want: "planner", system: "PLAN HARDER"},
		{name: "the key selects", src: `zemi.config.set("agent.profile", "p") zemi.agent.profile.add("p", {})`, want: "p"},
		{name: "default resets", src: `zemi.agent.profile.add("p", {}) zemi.config.set("agent.profile", "p") zemi.config.set("agent.profile", "default")`},
		{name: "an unknown name errors", src: `zemi.config.set("agent.profile", "nope")`, wantErr: true},
	} {
		h, r := newTestHost(t)
		runUser(t, r, c.src)
		p, err := h.resolveProfile("")
		got, system := "", ""
		if p != nil {
			got, system = p.Name, p.System
		}
		if (err != nil) != c.wantErr || got != c.want || system != c.system {
			t.Errorf("%s: profile %q system %q, err %v", c.name, got, system, err)
		}
	}
	h, r := newTestHost(t)
	runUser(t, r, `zemi.agent.profile.add("p", {}) zemi.agent.profile.add("planner", { model = "x/big" }) zemi.agent.profile.add("default", {})`)
	if names := h.ProfileNames(); !slices.Equal(names, []string{"default", "planner", "p"}) {
		t.Errorf("names %v", names)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if h.OpenProfilePicker() != nil || ui.opened[0].Items[1].(map[string]any)["desc"] != "x/big · every tool" {
		t.Errorf("the defined planner's picker row %v", ui.opened)
	}
	if h.SetProfile("planner") != nil || h.cycleProfile(1) != nil || h.Settings.Str("agent.profile") != "p" {
		t.Errorf("cycling from planner went to %q", h.Settings.Str("agent.profile"))
	}
	h.PurgeGeneration()
	if _, err := h.resolveProfile("p"); err == nil {
		t.Error("a purge kept the definition")
	}
}

// TestModelPickerOdd: with nothing usable the picker opens by itself, unless
// init.lua failed, and ends with the keyless builtin providers by env var,
// explained only when nothing is usable; a configured id the provider does
// not list is the selected row, which the reload toast also names.
func TestModelPickerOdd(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.Providers.Reset(agent.Catalog{Missing: []agent.MissingKey{{ID: "deepseek", Env: "DEEPSEEK_API_KEY"}}})
	h.SetInitError("init.lua:1: boom")
	if h.OfferModel(); len(ui.opened) != 0 {
		t.Fatal("a failed init.lua opened the picker")
	}
	h.SetInitError("")
	if h.OfferModel(); len(ui.opened) != 1 {
		t.Fatal("no model: the picker did not open")
	}
	// an id naming the keyless builtin gets the same listing, not a "current" row
	runUser(t, r, `zemi.config.set("models.chat", "deepseek/x")`)
	if h.OpenModelPicker("") != nil {
		t.Fatal("picker did not open")
	}
	for _, o := range ui.opened {
		if len(o.Items) != 1 || o.Items[0].(map[string]any)["text"] != "deepseek" || o.Items[0].(map[string]any)["copy"] != "export DEEPSEEK_API_KEY=" || !strings.HasPrefix(o.Body, "No models yet. Export") {
			t.Fatalf("empty picker: %+v body %q", o.Items, o.Body)
		}
	}
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })
		zemi.config.set("models.chat", "a/typo")`)
	if h.OpenModelPicker("") != nil {
		t.Fatal("picker did not open")
	}
	o := ui.opened[2]
	row := o.Items[2].(map[string]any)
	if o.Selected != 2 || len(o.Items) != 5 || row["text"] != "a/typo" || row["run"] != "model a/typo" || row["key"] != "current, not in the list" {
		t.Fatalf("unlisted id: %+v", o.Items)
	}
	// the keyless builtins still close the list, copy only, with no body
	if last := o.Items[4].(map[string]any); last["text"] != "deepseek" || last["run"] != nil || last["copy"] != "export DEEPSEEK_API_KEY=" || o.Body != "" {
		t.Fatalf("missing builtins after the models: %+v body %q", o.Items, o.Body)
	}
	if w := h.modelRow().Detail; !strings.HasPrefix(w, "a/typo: not in the provider's models list") {
		t.Fatalf("warning %q", w)
	}
	if h.OfferModel(); len(ui.opened) != 3 {
		t.Fatal("a model that runs opened the picker")
	}
}

// TestProfileSwitch: /profile refuses a name nobody defined, /model and
// zemi.model.set refuse under a profile that sets its own model, and both
// pickers mark what the next send runs on.
func TestProfileSwitch(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "main" }, { id = "cheap" } } })
		zemi.config.set("models.chat", "a/main")
		zemi.agent.profile.add("cheap", { model = "a/cheap" })
		zemi.agent.profile.add("onchat", { model = "chat" })`)
	for _, c := range []struct {
		name    string
		do      func() error
		wantErr string
	}{
		{"unknown profile", func() error { return h.SetProfile("nope") }, "want default, planner, cheap"},
		{"model under default", func() error { return h.SetModel("a/cheap") }, ""},
		{"unregistered provider", func() error { return h.SetModel("nope/m") }, `provider "nope" is not registered: add a provider to `},
		{"unlisted model under a registered provider is taken and warned about, mid-turn too", func() error {
			h.agents.active, h.agents.turnModel = true, "a/main"
			defer func() { h.agents.active, h.agents.turnModel = false, "" }()
			if err := h.SetModel("a/nope"); err != nil {
				return err
			}
			runUser(t, r, `local n = zemi.notify.list()[1]; assert(n.title == "Model not listed" and n.body:match("^a/nope: not in the provider's models list"), tostring(n and n.body))`)
			return nil
		}, ""},
		{"model under a profile on the chat role", func() error {
			if err := h.SetProfile("onchat"); err != nil {
				return err
			}
			return h.SetModel("a/main")
		}, ""},
		{"switch", func() error { return h.SetProfile("cheap") }, ""},
		{"model under a profile with one", func() error { return h.SetModel("a/main") }, "profile cheap sets its model"},
		{"zemi.model.set under it", func() error {
			return r.ExecuteChunk(context.Background(), `zemi.model.set("a/main")`, "test.lua", lua.OriginUser)
		}, "profile cheap sets its model"},
	} {
		err := c.do()
		if (c.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.wantErr)
		}
	}
	for p, want := range map[*Profile]string{
		{}:                        "every tool",
		{Tools: []string{"-lua"}}: "every tool but lua",
		{Model: "a/m", Tools: []string{"shell", "read"}}: "a/m · only shell, read",
	} {
		if got := profileDesc(p); got != want {
			t.Errorf("desc %q, want %q", got, want)
		}
	}
	if h.OpenProfilePicker() != nil || h.OpenModelPicker("chat") != nil {
		t.Fatal("a picker did not open")
	}
	// the profile is cheap's, the chat role stays models.chat
	for i, want := range []string{"cheap", "a/main"} {
		o := ui.opened[len(ui.opened)-2+i] // after the /model refreshes the SetModel calls sent
		if item := o.Items[o.Selected].(map[string]any); item["key"] != "current" || !strings.HasSuffix(item["run"].(string), " "+want) {
			t.Errorf("%s picker marks %v, want %s", o.ID, item, want)
		}
	}
}

// TestProfileContextFilesSurviveTwoSends: building the prompt must leave the
// profile's context files as written, or the second build joins them to the
// project directory twice and silently reads nothing.
func TestProfileContextFilesSurviveTwoSends(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.agent.profile.add("p", { context = { files = { "NOTES.md" } } })`)
	p, err := h.resolveProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	h.buildSystem(p)
	h.buildSystem(p)
	if got := p.ContextFiles; len(got) != 1 || got[0] != "NOTES.md" {
		t.Fatalf("context files = %v", got)
	}
}

// TestConcurrentSendsQueue: two sends racing to start a turn run one after
// the other, so the history keeps both messages.
func TestConcurrentSendsQueue(t *testing.T) {
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "ok")
	errs := make(chan error, 2)
	for _, text := range []string{"one", "two"} {
		go func() { errs <- h.AgentSend(text) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	waitIdle(t, h)
	h.agents.mu.Lock()
	history := h.agents.runner.History
	h.agents.mu.Unlock()
	users := 0
	for _, m := range history {
		if m.Role == fantasy.MessageRoleUser {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("history keeps %d of 2 user messages", users)
	}
}

// TestRetryRestreamsStep: a provider retry streams the step again from its
// start, so the failed attempt's text and tool rows are dropped instead of
// shown twice or as a failed call that never ran.
func TestRetryRestreamsStep(t *testing.T) {
	h, _ := newTestHostDB(t)
	run, _ := h.newRun(context.Background(), builtinAgents[0], "d", "p", false)
	epoch := h.trEpoch.Load()
	for name, c := range map[string]struct {
		hooks agent.StreamHooks
		text  func() string
		rows  func() int // transcript rows plus live tool rows
	}{
		"main": {
			h.streamHooks(), func() string { return h.AgentStream().Text },
			func() int { return len(h.Session().Transcript) + len(h.AgentStream().Tools) },
		},
		"subagent": {
			&subagentHooks{h: h, def: builtinAgents[0], run: run},
			func() string { run.mu.Lock(); defer run.mu.Unlock(); return run.text },
			func() int { run.mu.Lock(); defer run.mu.Unlock(); return len(run.tr) },
		},
	} {
		base := c.rows()
		c.hooks.ToolCallStart("c0", "read", `{"path":"a"}`)
		c.hooks.ToolCallEnd("c0", "read", true, "", "")
		c.hooks.TextDelta("before ")
		c.hooks.StepStart()
		// the stream breaks while a call's arguments stream in
		c.hooks.TextDelta("partial")
		c.hooks.ToolCallPending("c1", "glob")
		c.hooks.Retrying(nil, 0)
		c.hooks.TextDelta("full")
		if got, rows := c.text(), c.rows(); got != "before full" || rows != base+1 {
			t.Errorf("%s: text %q and %d rows, want %q and the one earlier call", name, got, rows, "before full")
		}
	}
	if h.trEpoch.Load() == epoch {
		t.Error("the cut kept the transcript epoch, so the chat widget draws its cached rows")
	}
	main := h.streamHooks()
	h.agents.mu.Lock()
	h.agents.thought = time.Second
	h.agents.mu.Unlock()
	main.StepStart()
	main.ToolCallPending("c2", "glob") // flushes the text row, taking the thought time with it
	main.Retrying(nil, 0)
	if h.agents.thought != time.Second {
		t.Errorf("the retry left thought at %v, want the step start's 1s", h.agents.thought)
	}
}

func TestMaxOutput(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "anthropic", models = { { id = "big" }, { id = "old", max_output = 4096, vision = false } } })
		zemi.provider.add({ id = "o", kind = "openai", models = { { id = "gpt" } } })
	`)
	for model, want := range map[string]int64{"a/big": 32_000, "a/old": 4096, "o/gpt": 0} {
		if got := h.maxOutput(model); got != want {
			t.Errorf("%s: max output %d, want %d", model, got, want)
		}
	}
	for model, want := range map[string]bool{"a/big": true, "a/old": false, "x/unknown": true} {
		if got := h.takesImages(model); got != want {
			t.Errorf("%s: takes images %v, want %v", model, got, want)
		}
	}
}

// TestAlwaysRuleSkipsProtectedWrites: a broad allow for the project still
// asks for writes under a nested .agents or .git, but not for the scratchpad.
func TestAlwaysRuleSkipsProtectedWrites(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	h.Dirs.DataHome = t.TempDir()
	proj := perm.RealPath(h.Dirs.ProjectDir)
	plugin := filepath.Join(perm.RealPath(h.Dirs.DataHome), "plugins", "local", "p", "init.lua")
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: filepath.Join(proj, "*"), Decision: perm.Allow, Principal: "model"})
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: filepath.Join(perm.RealPath(h.Dirs.StateHome), "*"), Decision: perm.Allow, Principal: "model"})
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: filepath.Join(perm.RealPath(h.Dirs.DataHome), "*"), Decision: perm.Allow, Principal: "model"})
	if label, _ := h.alwaysChoice(tools.Request{Subject: "fs.write", Arg: plugin}); label != "" {
		t.Errorf("a local plugin write offers %q", label)
	}
	pipe := h.PipelineFor()
	for _, c := range []struct {
		path string
		want perm.Decision
	}{
		{filepath.Join(proj, "main.go"), perm.Allow},
		{filepath.Join(proj, "pkg", ".agents", "init.lua"), perm.Deny},
		{filepath.Join(proj, "pkg", ".git", "config"), perm.Deny},
		{plugin, perm.Deny},
		{filepath.Join(h.ScratchDir(), "notes.md"), perm.Allow},
	} {
		if v := pipe.Check(tools.Request{Subject: "fs.write", Arg: c.path}); v.Decision != c.want {
			t.Errorf("%s: %s (%s), want %s", c.path, v.Decision, v.Reason, c.want)
		}
	}
}

// TestAlwaysRuleSkipsSecrets: "allow reads anywhere in the project" is a
// directory glob, and the label does not say "including every .env and
// *.pem in it", so those still ask. A rule the user wrote does not.
func TestAlwaysRuleSkipsSecrets(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	proj := perm.RealPath(h.Dirs.ProjectDir)
	h.Perm.Add(perm.Rule{Subject: "fs.read", Pattern: filepath.Join(proj, "*"), Decision: perm.Allow, Principal: "model"})
	pipe := h.PipelineFor()
	for _, c := range []struct {
		path string
		want perm.Decision
	}{
		{filepath.Join(proj, "main.go"), perm.Allow},
		{filepath.Join(proj, ".env"), perm.Deny},
		{filepath.Join(proj, "deploy", "prod.pem"), perm.Deny},
	} {
		if v := pipe.Check(tools.Request{Subject: "fs.read", Arg: c.path}); v.Decision != c.want {
			t.Errorf("%s: %s (%s), want %s", c.path, v.Decision, v.Reason, c.want)
		}
	}
	// an always answer for the one file the user saw names it exactly
	h.Perm.Add(perm.Rule{Subject: "fs.read", Pattern: filepath.Join(proj, "deploy", "prod.pem"), Decision: perm.Allow, Principal: "model"})
	if v := pipe.Check(tools.Request{Subject: "fs.read", Arg: filepath.Join(proj, "deploy", "prod.pem")}); v.Decision != perm.Allow {
		t.Errorf("an exact model rule for prod.pem: %s (%s)", v.Decision, v.Reason)
	}
	// the user's own rule means what it says
	h.Perm.Add(perm.Rule{Subject: "fs.read", Pattern: filepath.Join(proj, ".env"), Decision: perm.Allow})
	if v := pipe.Check(tools.Request{Subject: "fs.read", Arg: filepath.Join(proj, ".env")}); v.Decision != perm.Allow {
		t.Errorf("a user rule for .env: %s (%s)", v.Decision, v.Reason)
	}
	// "allow cat" was answered for a file the user saw; a later cat of a
	// secret-shaped file, or of a glob that may expand to one, asks again
	t.Chdir(proj)
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "cat *", Decision: perm.Allow, Principal: "model"})
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "make *", Decision: perm.Allow, Source: "permissions"})
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "head *", Decision: perm.Allow, Source: "permissions"})
	for _, c := range []struct {
		cmd  string
		want perm.Decision
	}{
		{"cat main.go", perm.Allow},
		{"cat .env", perm.Deny},
		{"cat ~/.ssh/id_rsa", perm.Deny},
		// no secret name, but a path the sandbox hides from confined commands
		{"cat ~/.kube/config", perm.Deny},
		{"cat $HOME/.config/gh/hosts.yml", perm.Deny},
		{"cat .e*", perm.Deny},
		{"grep -r secret .", perm.Allow},
		{"make .env", perm.Allow},
		{"head deploy/prod.pem", perm.Deny},
		{"cat main.go && head .npmrc", perm.Deny},
	} {
		if v := pipe.Check(tools.Request{Subject: "tool.shell", Arg: c.cmd}); v.Decision != c.want {
			t.Errorf("%s: %s (%s), want %s", c.cmd, v.Decision, v.Reason, c.want)
		}
	}
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "cat .env", Decision: perm.Allow})
	if v := pipe.Check(tools.Request{Subject: "tool.shell", Arg: "cat .env"}); v.Decision != perm.Allow {
		t.Errorf("a user rule for cat: %s (%s)", v.Decision, v.Reason)
	}
}

// TestHeadlessToolsetDropsPrompts: a zemi run has nobody to answer a
// question, approve a plan, open an editor or watch a window, so those
// schemas never ship with it, the prompt stops telling the model to use
// them, and the agent tool drops background, whose report a run that exits
// at idle would throw away.
func TestHeadlessToolsetDropsPrompts(t *testing.T) {
	h, _ := newTestHost(t)
	names := func() map[string]bool {
		out := map[string]bool{}
		toolset, _ := h.profileTools(nil)
		for _, tool := range toolset {
			out[tool.Info().Name] = true
		}
		return out
	}
	for name := range headlessExcluded {
		if names()[name] {
			t.Errorf("headless toolset still ships %q", name)
		}
	}
	params := func() map[string]any { return h.agentTool(nil).Info().Parameters }
	if params()["background"] != nil {
		t.Error("the headless agent tool still offers background")
	}
	sys := h.buildSystem(nil)
	if strings.Contains(sys, "question tool") || strings.Contains(sys, "Ask before") || strings.Contains(sys, "ask why") {
		t.Error("the headless prompt still tells the model to ask")
	}
	// every fetch and search would be denied: no rule allows one
	if names()[tools.ToolFetch] || names()[tools.ToolWebSearch] || slices.Contains(h.agentTool(nil).(*agentTool).names(), "web") {
		t.Error("the headless toolset ships the web tools no rule allows")
	}
	h.Perm.Add(perm.Rule{Subject: "net.*", Pattern: "https://docs.example.com/*", Decision: perm.Allow})
	if !names()[tools.ToolFetch] || !slices.Contains(h.agentTool(nil).(*agentTool).names(), "web") {
		t.Error("a net allow rule does not bring the web tools back")
	}

	h.SetUIHooks(&fakeUI{})
	for name := range headlessExcluded {
		if !names()[name] {
			t.Errorf("the TUI toolset lost %q", name)
		}
	}
	if params()["background"] == nil {
		t.Error("the TUI agent tool lost background")
	}
	if !strings.Contains(h.buildSystem(nil), "question tool") {
		t.Error("the TUI prompt lost the asking guidance")
	}
}

// TestProfileToolsUnknown: another harness's spelling in a profile leaves
// the main agent almost toolless, so the name lands in /errors; a "-name"
// nothing answers to fails, since the tool it meant to drop may run under
// another name.
func TestProfileToolsUnknown(t *testing.T) {
	h, _ := newTestHost(t)
	toolset, err := h.profileTools(&Profile{Name: "lean", Tools: []string{"Read", "Grep", "shell"}})
	if err != nil || !hasTool(toolset, "shell") || hasTool(toolset, "read") {
		t.Errorf("kept %d tools, %v", len(toolset), err)
	}
	if !h.HasUnseenErrors() || !strings.Contains(h.errs.entries[0].msg, "no tool is named Read, Grep") {
		t.Errorf("errors = %+v", h.errs.entries)
	}
	if _, err := h.profileTools(&Profile{Name: "docs", Tools: []string{"-bash", "-plan"}}); err == nil || !strings.Contains(err.Error(), "profile docs drops bash, which no tool is named") {
		t.Errorf("an unknown drop: %v", err)
	}
}

// TestBuiltinProvidersSeed: the catalog seeds every generation, a user
// add under a builtin id replaces it and survives providers.builtin =
// false, del drops one, and the roles fall back to the first usable
// provider's default, a user one first.
func TestBuiltinProvidersSeed(t *testing.T) {
	dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	h.Builtin.Store(&agent.Catalog{Specs: []agent.Spec{
		{ID: "a", Kind: agent.KindAnthropic, Builtin: true, DefaultModel: "big", Models: []agent.Model{{ID: "big"}, {ID: "tiny"}}},
		{ID: "o", Kind: agent.KindOpenAI, Builtin: true, DefaultModel: "g", Models: []agent.Model{{ID: "g"}}},
	}, Keys: map[string]string{"a": "ka", "o": "ko"}})
	initLua := filepath.Join(dir, "init.lua")
	reload := func(init string) {
		t.Helper()
		if err := os.WriteFile(initLua, []byte(init), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep(), lua.LoadFileStep("init", initLua, lua.OriginUser)}); err != nil {
			t.Fatal(err)
		}
		if !h.Settings.Bool("providers.builtin") { // readyStep's line
			h.Providers.DropBuiltin()
		}
	}
	reload(``)
	runUser(t, r, `
		local ps = zemi.provider.list()
		assert(#ps == 2 and ps[1].id == "a" and ps[1].builtin == true and ps[2].builtin == true, "seed")
	`)
	if h.currentModelID(nil) != "a/big" || h.roleModel("titles") != "a/big" || !h.Providers.HasKey("a") {
		t.Fatalf("catalog defaults: %s %s", h.currentModelID(nil), h.roleModel("titles"))
	}
	reload(`
		zemi.provider.add({ id = "a", kind = "anthropic", base_url = "http://gw", api_key = "mine", models = { { id = "m" } } })
		zemi.provider.del("o")
		zemi.provider.del("nope")
	`)
	runUser(t, r, `
		local ps = zemi.provider.list()
		assert(#ps == 1 and ps[1].id == "a" and ps[1].builtin == false and ps[1].base_url == "http://gw", "override: " .. #ps)
	`)
	if h.currentModelID(nil) != "a/m" || h.roleModel("titles") != "a/m" {
		t.Fatalf("user defaults: %s %s", h.currentModelID(nil), h.roleModel("titles"))
	}
	reload(`
		zemi.provider.add({ id = "local", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = { { id = "q" } } })
		zemi.config.set("providers.builtin", false)
	`)
	runUser(t, r, `
		local ps = zemi.provider.list()
		assert(#ps == 1 and ps[1].id == "local", "builtin off: " .. #ps)
	`)
	reload(``)
	runUser(t, r, `assert(#zemi.provider.list() == 2, "reseeded")`)
	if err := r.ExecuteChunk(t.Context(), `zemi.provider.del("a")`, "p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("del below user origin: %v", err)
	}
}

func TestModelDesc(t *testing.T) {
	for _, c := range []struct {
		m    agent.Model
		want string
	}{
		{agent.Model{ID: "m", Display: "p/m"}, ""},
		{agent.Model{ID: "m", Display: "m"}, ""},
		{agent.Model{ID: "m", Display: "Big", ContextWindow: 200_000, CostPer1MIn: 3, CostPer1MOut: 15}, "Big · 200k · $3/$15"},
		{agent.Model{ID: "m", ContextWindow: 1_048_576, CostPer1MOut: 0.4}, "1.05M · $0/$0.4"},
	} {
		if got := modelDesc("p/m", c.m); got != c.want {
			t.Errorf("modelDesc(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
}

// btwModel records the one call /btw makes and answers it with a tool call
// along with the text; turns stream from the script.
type btwModel struct {
	*scriptModel
	call fantasy.Call
}

func (m *btwModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	last := concatText(call.Prompt[len(call.Prompt)-1].Content)
	if strings.HasSuffix(last, "hang?") {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if !strings.HasSuffix(last, "what failed?") {
		return m.scriptModel.Stream(ctx, call)
	}
	m.call = call
	return streamOf(&fantasy.Response{
		Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "the build"},
			fantasy.ToolCallContent{ToolCallID: "b1", ToolName: "todos", Input: `{"todos":[{"content":"x","status":"pending"}]}`},
		},
		FinishReason: fantasy.FinishReasonToolCalls,
		Usage:        fantasy.Usage{InputTokens: 100, OutputTokens: 7},
	}), nil
}

// TestBtw: a side question asked mid-turn sees the turn's finished steps on
// the conversation's tools, runs none of them, is billed, and leaves no row
// behind; in the TUI it is a busy job esc cancels without a toast.
func TestBtw(t *testing.T) {
	h, _ := newTestHostDB(t)
	stepped := make(chan struct{})
	m := &btwModel{scriptModel: &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
		if toolResult {
			close(stepped)
			return nil // the turn waits here until cancelled
		}
		return toolCallParts("c1", "nope", "{}")
	}}}
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	<-stepped
	defer waitIdle(t, h)
	defer h.AgentCancel()

	sid := h.AgentSessionID()
	rows, _ := h.DB.MessageCount(sid)
	s, _ := h.DB.GetSession(sid)
	answer, err := h.btw(t.Context(), "what failed?")
	if err != nil || answer != "the build" {
		t.Fatalf("btw = %q, %v", answer, err)
	}
	after, _ := h.DB.MessageCount(sid)
	billed, _ := h.DB.GetSession(sid)
	if todos, _ := h.DB.Todos(sid); len(todos) > 0 {
		t.Fatalf("a side question ran a tool: %+v", todos)
	}
	if after != rows || billed.CompletionTokens != s.CompletionTokens+7 {
		t.Fatalf("rows %d -> %d, completion tokens %d -> %d", rows, after, s.CompletionTokens, billed.CompletionTokens)
	}
	p := m.call.Prompt
	sawResult := slices.ContainsFunc(p, func(msg fantasy.Message) bool { return msg.Role == fantasy.MessageRoleTool })
	if !sawResult || !strings.HasSuffix(concatText(p[len(p)-1].Content), "what failed?") || len(m.call.Tools) == 0 {
		t.Fatalf("request: tool result %v, %d tools, last %+v", sawResult, len(m.call.Tools), p[len(p)-1])
	}

	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.Btw("hang?"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if label, _, cancellable := h.BusyNow(); label != "Answering the side question" || !cancellable {
			return fmt.Errorf("busy %q %v", label, cancellable)
		}
		return nil
	})
	h.CancelBusy()
	waitFor(t, func() error {
		if label, _, _ := h.BusyNow(); label != "" {
			return fmt.Errorf("still busy: %q", label)
		}
		return nil
	})
	h.evlog.mu.Lock()
	notes := len(h.evlog.notes)
	h.evlog.mu.Unlock()
	if notes > 0 || len(ui.opened) > 0 {
		t.Fatalf("a cancelled side question showed %d toasts, %d overlays", notes, len(ui.opened))
	}
}

func TestRecap(t *testing.T) {
	h, _ := newTestHostDB(t)
	if _, err := h.Settings.SetQuiet("session.auto_title", false); err != nil {
		t.Fatal(err)
	}
	m := reflectStub(t, h, "fixing the build;\n\n  next: run  make check\nignored")
	const recap = "Recap: fixing the build; next: run make check"
	turn := func(tools bool) func() {
		return func() {
			if tools {
				m.toolStep.Store(1)
			}
			if err := h.AgentSend("go"); err != nil {
				t.Fatal(err)
			}
			waitIdle(t, h)
		}
	}
	for _, c := range []struct {
		name  string
		setup func()
		want  string // the row the recap adds as text|detail, "" for none
	}{
		{"a turn that ran a tool gets one, the first ever naming the key", turn(true), recap + "|off in /config"},
		{"the next has no hint", func() {}, recap + "|"},
		{"off in config", func() { _, _ = h.Settings.SetQuiet("session.recap", false) }, ""},
		{"a reply-only turn is its own recap", func() { _, _ = h.Settings.SetQuiet("session.recap", true); turn(false)() }, ""},
		{"a row landing during the call makes it stale", func() {
			turn(true)()
			m.during = func() { h.AppendMessage(fantasy.MessageRoleUser, "back") }
		}, ""},
	} {
		c.setup()
		before := len(h.Session().Transcript)
		h.recap(t.Context())
		tr := h.Session().Transcript
		var got string
		if last := tr[len(tr)-1]; len(tr) > before && last.Kind == RowNotice {
			got = last.Text + "|" + last.Detail
		}
		if got != c.want {
			t.Errorf("%s: recap row %q, want %q", c.name, got, c.want)
		}
	}
}

// TestLateReasoningKeepsReplyWhole: a reasoning chunk a provider sends after
// the reply started joins the thought above it, live, in a subagent's run and
// on resume alike, so the reply is never cut after its first word.
func TestLateReasoningKeepsReplyWhole(t *testing.T) {
	const want = "reasoning:pondering summary. | :Here's it"
	rowsOf := func(tr []ChatMessage) string {
		var out []string
		for _, m := range tr {
			out = append(out, string(m.Kind)+":"+m.Text)
		}
		return strings.Join(out, " | ")
	}
	stream := func(hk agent.StreamHooks) {
		hk.ReasoningDelta("pondering")
		hk.TextDelta("Here")
		hk.ReasoningDelta(" summary.")
		hk.TextDelta("'s it")
	}
	// the last token of a thought may land after the step's call is pending
	t.Run("call", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		hk := h.streamHooks()
		hk.ReasoningDelta("pin")
		hk.ToolCallPending("t1", "grep")
		hk.ReasoningDelta("ning.")
		hk.ToolCallStart("t1", "grep", `{"pattern":"x"}`)
		hk.ToolCallEnd("t1", "grep", true, "", "")
		hk.ReasoningDelta("next")
		hk.TurnDone(nil, fantasy.Usage{})
		if got := rowsOf(h.Session().Transcript); got != "reasoning:pinning. | tool:x | reasoning:next" {
			t.Fatalf("rows = %s", got)
		}
	})
	for name, got := range map[string]func() string{
		"main": func() string {
			h, _ := newTestHostDB(t)
			stream(h.streamHooks())
			h.streamHooks().TurnDone(nil, fantasy.Usage{})
			return rowsOf(h.Session().Transcript)
		},
		"subagent": func() string {
			h, _ := newTestHostDB(t)
			run, _ := h.newRun(context.Background(), builtinAgents[0], "d", "p", false)
			stream(&subagentHooks{h: h, def: builtinAgents[0], run: run})
			run.mu.Lock()
			defer run.mu.Unlock()
			run.flush()
			return rowsOf(run.tr[1:]) // after the task row
		},
		"resume": func() string {
			m := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{Text: "pondering"},
				fantasy.TextPart{Text: "Here"},
				fantasy.ReasoningPart{Text: " summary."},
				fantasy.TextPart{Text: "'s it"},
			}}
			raw, err := json.Marshal(m, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			_, tr := historyFrom([]*store.Message{{Role: "assistant", PartsJSON: string(raw)}})
			return rowsOf(tr)
		},
	} {
		if g := got(); g != want {
			t.Errorf("%s: rows %q, want %q", name, g, want)
		}
	}
}

func TestRunFrame(t *testing.T) {
	h, _ := newTestHost(t)
	h.setTranscript("t", make([]ChatMessage, 5))
	for _, c := range []struct {
		name          string
		active        bool
		stepRows      int
		from          int
		next, pending int
	}{
		{name: "an idle session settles every row", stepRows: 2, from: 1, next: 5},
		{name: "rows of the running step wait", active: true, stepRows: 3, from: 1, next: 3, pending: 2},
		{name: "a stale step start keeps the cursor", active: true, stepRows: 0, from: 4, next: 4, pending: 1},
	} {
		h.agents.mu.Lock()
		h.agents.active, h.agents.stepRows = c.active, c.stepRows
		h.agents.mu.Unlock()
		fr := h.RunFrame(c.from)
		if fr.Next != c.next || len(fr.Pending) != c.pending || len(fr.Settled) != max(c.next-c.from, 0) {
			t.Errorf("%s: next %d, %d settled, %d pending", c.name, fr.Next, len(fr.Settled), len(fr.Pending))
		}
	}
}
