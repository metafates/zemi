package hostapi

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// newTestHostDB boots a host with a real session store in temp dirs.
func newTestHostDB(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	log := logging.New(t.TempDir())
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
	runUser(t, r, `zemi.agent.profile("warm", { temperature = 0.3 })`)
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
		local ok, err = pcall(zemi.provider.add, { id = "oa", kind = "openai", api_key = "k",
			body = { service_tier = "flex" }, models = { { id = "m" } } })
		assert(not ok and tostring(err):find("anthropic and openai_compat"), tostring(err))
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
		subject = "tool.bash", pattern = "git status", decision = "allow",
	})`, "needs a match example")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "tool.bash", pattern = "git status", decision = "allow",
		match = "git push",
	})`, "does not match pattern")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "tool.bash", pattern = "git status", decision = "allow",
		match = "git status", scope = "permanent",
	})`, "a rule in init.lua applies on every start")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "tool.bash", pattern = "git *", decision = "allow",
		not_match = "git push*", match = "git status",
	})`, "needs both examples")

	runExpectErr(t, r, `zemi.permission.rule.add({
		subject = "bash", pattern = "git status", decision = "allow", match = "git status",
	})`, "starts with none of")

	runUser(t, r, `zemi.permission.rule.add({
		subject = "tool.bash", pattern = "git status", decision = "allow",
		match = "git status",
	})`)
	runUser(t, r, `
		local rules = zemi.permission.rule.list()
		assert(#rules == 1, "rules=" .. #rules)
		assert(rules[1].pattern == "git status", "pattern=" .. rules[1].pattern)
		assert(rules[1].decision == "allow", "decision=" .. rules[1].decision)
	`)

	err := r.ExecuteChunk(context.Background(), `zemi.permission.rule.add({
		subject = "tool.bash", pattern = "git status", decision = "allow",
		match = "git status",
	})`, "plugin.lua", lua.OriginPlugin)
	if err == nil || !strings.Contains(err.Error(), "only come from user config") {
		t.Fatalf("plugin-origin allow rule: %v", err)
	}

	// Lua may always deny, from any origin.
	runUser(t, r, `zemi.permission.rule.add({
		subject = "tool.bash", pattern = "rm -rf *", decision = "deny",
		match = "rm -rf /tmp/x",
	})`)

	if out := h.Perm.Evaluate("tool.bash", "git status", "model"); out.Decision != perm.Allow {
		t.Fatalf("git status: %v (%s)", out.Decision, out.Reason)
	}
	if out := h.Perm.Evaluate("tool.bash", "git push", "model"); out.Decision != perm.Ask {
		t.Fatalf("git push: %v (%s)", out.Decision, out.Reason)
	}
	if out := h.Perm.Evaluate("tool.bash", "rm -rf /tmp/x", "model"); out.Decision != perm.Deny {
		t.Fatalf("rm -rf: %v (%s)", out.Decision, out.Reason)
	}

	// A reload replays config, so its rules go; an "always" answer keeps its.
	always := perm.Rule{Subject: "tool.bash", Pattern: "go test *", Decision: perm.Allow, Principal: "model"}
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
// several times included; an edited config value wins.
func TestReloadKeepsRuntimePicks(t *testing.T) {
	dir, log := t.TempDir(), logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	initLua := filepath.Join(dir, "init.lua")
	for _, c := range []struct {
		name       string
		config     []string // what init.lua sets models.main to, in order
		pick, want string
	}{
		{"first load applies config", []string{"p/a"}, "", "p/a"},
		{"unchanged config keeps a pick", []string{"p/a"}, "p/b", "p/b"},
		{"edited config wins over a pick", []string{"p/c"}, "p/b", "p/c"},
		{"a second set of the same key applies", []string{"p/c", "p/d"}, "", "p/d"},
		{"an unchanged double set keeps a pick", []string{"p/c", "p/d"}, "p/e", "p/e"},
		{"editing the second set wins", []string{"p/c", "p/f"}, "p/e", "p/f"},
		{"dropping the second set wins", []string{"p/c"}, "p/e", "p/c"},
	} {
		src := ""
		for _, v := range c.config {
			src += `zemi.config.set("models.main", "` + v + `")` + "\n"
		}
		if err := os.WriteFile(initLua, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if c.pick != "" {
			if _, err := h.Settings.SetQuiet("models.main", c.pick); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep(), lua.LoadFileStep("init", initLua, lua.OriginUser)}); err != nil {
			t.Fatal(err)
		}
		if got, _ := h.Settings.Get("models.main"); got != c.want {
			t.Errorf("%s: models.main = %v, want %s", c.name, got, c.want)
		}
	}
}

// TestReloadKeepsUserVetoes: while a reload replays config, tool calls are
// still checked against the old Lua rules and wait for the tool.pre handlers
// to come back instead of passing without them.
func TestReloadKeepsUserVetoes(t *testing.T) {
	dir, log := t.TempDir(), logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	initLua := filepath.Join(dir, "init.lua")
	src := `
		zemi.permission.rule.add({ subject = "tool.bash", pattern = "rm *", decision = "deny", match = "rm x" })
		zemi.event.on("tool.pre", function() return { decision = "deny", reason = "vetoed" } end)`
	if err := os.WriteFile(initLua, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, src+`
		zemi.permission.rule.add({ subject = "tool.bash", pattern = "curl *", decision = "deny", match = "curl x" })`)
	decision := func(cmd string) perm.Decision { return h.Perm.Evaluate("tool.bash", cmd, "model").Decision }

	pre := h.toolPreHook()
	var vetoed string
	done := make(chan struct{})
	probe := lua.BootStep{Name: "probe", Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
		if d := decision("rm x"); d != perm.Deny {
			t.Errorf("mid-reload rm x = %v, want deny", d)
		}
		go func() {
			vetoed, _, _, _ = pre("bash", map[string]any{"command": "ls"})
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
		{`{ name = "bash", execute = function() end }`, "builtin tool name"},
		{`{ name = "window", execute = function() end }`, "builtin tool name"},
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

	runExpectErr(t, r, `zemi.model.set("myllm/main")`, "unknown model")

	runUser(t, r, `zemi.provider.add({
		id = "myllm", kind = "openai_compat", base_url = "https://llm.internal/v1",
		api_key = "test-key",
		models = { { id = "main", display = "Main" }, { id = "mini" } },
	})`)
	runExpectErr(t, r, `zemi.provider.add({ id = "old", kind = "openai", api_key_env = "OLD_KEY" })`, "api_key_env is gone")
	t.Setenv("ZEMI_TEST_TOKEN", "abc")
	runUser(t, r, `assert(zemi.env.get("ZEMI_TEST_TOKEN") == "abc", "user config reads secret-shaped env vars")`)

	runUser(t, r, `zemi.model.set("myllm/main")`)
	if v, ok := h.Settings.Get("models.main"); !ok || v != "myllm/main" {
		t.Fatalf("models.main=%v ok=%v", v, ok)
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
	runExpectErr(t, r, `zemi.fs.read(".env")`, "permission ask required")
}

// TestSessionNamespace covers the session surface: empty id before the
// first send, title set, and store-backed session creation.
func TestSessionNamespace(t *testing.T) {
	h, r := newTestHostDB(t)

	runUser(t, r, `
		assert(zemi.session.id() == "", "session should start empty")
		zemi.session.set_title("my session")
		assert(zemi.session.title() == "my session", "title=" .. zemi.session.title())
		assert(#zemi.session.messages() == 0, "messages should be empty")
	`)

	if err := h.ensureSession(nil, "hello world"); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	if h.AgentSessionID() == "" {
		t.Fatal("session id still empty after ensureSession")
	}
	if err := h.persistMessage("assistant", fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi there"}}}); err != nil {
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
		h.agents.active = true
		return h.beginScope()
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
	next, text := h.nextTurn(scope)
	if text != "a\n\nb" || !h.AgentActive() {
		t.Fatalf("follow-up = %q, active = %v", text, h.AgentActive())
	}
	h.AgentCancel() // before the follow-up's first step
	if next.ctx.Err() == nil {
		t.Fatal("a cancel during the handoff missed the follow-up turn")
	}
	if next, text := h.nextTurn(claim()); next != nil || text != "" || h.AgentActive() {
		t.Fatalf("idle handoff = %q, active = %v", text, h.AgentActive())
	}

	// a cancelled turn starts nothing: the queue waits for the composer
	scope = claim()
	send("c")
	h.AgentCancel()
	if next, text := h.nextTurn(scope); next != nil || text != "" || h.AgentActive() {
		t.Fatalf("cancelled handoff = %q, active = %v", text, h.AgentActive())
	}
	if q := h.AgentTakeQueue(); len(q) != 1 || q[0] != "c" {
		t.Fatalf("queue after a cancel = %q", q)
	}

	// a report's markup stays out of the composer, and joined messages keep
	// every typed part ahead of the marker readers cut at
	report := "Background agent x finished." + attachMarker + "\n<agent-report>\nr\n</agent-report>\n</attached-files>"
	scope = claim()
	send(report, "d")
	if st := h.AgentStream(); len(st.Steering) != 2 || st.Steering[0] != "Background agent x finished." || st.Reports != 1 {
		t.Fatalf("shown queue with a report = %q, reports = %d", st.Steering, st.Reports)
	}
	if q := h.AgentTakeQueue(); len(q) != 1 || q[0] != "d" {
		t.Fatalf("queue with a report = %q", q)
	}
	send("e")
	if _, text := h.nextTurn(scope); !strings.HasPrefix(text, "Background agent x finished.\n\ne"+attachMarker) || !strings.Contains(text, "<agent-report>") {
		t.Fatalf("follow-up with a report = %q", text)
	}

	h.AgentCancel() // no runner: must not panic
}

// TestHistoryCarriesAcrossTurns is the regression for the runner rebuilt
// on every send: the second turn's prompt must carry the first exchange
// (system, user, assistant, user), and both turns land in the transcript.
func TestHistoryCarriesAcrossTurns(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	for _, text := range []string{"one", "two"} {
		if err := h.AgentSend(text); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		waitIdle(t, h)
	}
	if n := m.lastPrompt.Load(); n != 4 {
		t.Fatalf("second turn prompt has %d messages, want 4", n)
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
	if v := pipe.Check(tools.Request{Subject: "tool.bash", Arg: "git status"}); v.Decision != perm.Deny || v.DecidedBy != "default" {
		t.Fatalf("unruled bash: %v by %q (%s)", v.Decision, v.DecidedBy, v.Reason)
	}

	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "git status", Decision: perm.Allow, Principal: "model"})
	if v := pipe.Check(tools.Request{Subject: "tool.bash", Arg: "git status"}); v.Decision != perm.Allow {
		t.Fatalf("ruled bash: %v (%s)", v.Decision, v.Reason)
	}
	if v := pipe.Check(tools.Request{Subject: "tool.bash", Arg: "git push"}); v.Decision != perm.Deny {
		t.Fatalf("other bash: %v (%s)", v.Decision, v.Reason)
	}

	// An asker (the UI overlay) resolves the leftover ask.
	pipe2 := h.PipelineFor()
	pipe2.Ask = func(req tools.Request) (bool, string, string) { return true, "user:once", "" }
	if v := pipe2.Check(tools.Request{Subject: "tool.bash", Arg: "git push"}); v.Decision != perm.Allow || v.DecidedBy != "user:once" {
		t.Fatalf("asked bash: %v by %q", v.Decision, v.DecidedBy)
	}
}

// TestStoreBackedState covers zemi.state roundtrip through the global
// store (DESIGN §6.5).
func TestStoreBackedState(t *testing.T) {
	_, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.state.set("note", { text = "hi", n = 2 })
		local v = zemi.state.get("note")
		assert(v and v.text == "hi" and v.n == 2, "state roundtrip failed")
	`)
}

// fakeUI is a UIHooks stub driving the host-side overlay paths in tests.
type fakeUI struct {
	askAllowed   bool
	askBy        string
	asked        []PermRequest
	reviewItems  [][]MemoryRow
	reviewResult []MemoryRow
	qAsked       []tools.Question
	qAnswer      string
	qOK          bool
	opened       []Overlay
	closed       []string
	closedLua    int
	reloads      int
	screenshot   string
	copied       string
	edit         func(path string) // what the user does in the editor
}

func (f *fakeUI) Notify(level, title, body string)     {}
func (f *fakeUI) OpenOverlay(o Overlay)                { f.opened = append(f.opened, o) }
func (f *fakeUI) CloseOverlay(id string, _ lua.Origin) { f.closed = append(f.closed, id) }
func (f *fakeUI) CloseLuaOverlays()                    { f.closedLua++ }
func (f *fakeUI) Reload()                              { f.reloads++ }
func (f *fakeUI) Refresh()                             {}
func (f *fakeUI) Scroll(kind, id string)               {}
func (f *fakeUI) LayoutRects() map[string]Rect         { return nil }
func (f *fakeUI) AgentStream()                         {}
func (f *fakeUI) TurnDone(err string)                  {}
func (f *fakeUI) ComposerEdit(string, string, int)     {}
func (f *fakeUI) EditFile(_ context.Context, req EditRequest) error {
	if f.edit != nil {
		f.edit(req.Path)
	}
	return nil
}
func (f *fakeUI) FocusWindow(int)  {}
func (f *fakeUI) Copy(text string) { f.copied = text }

func (f *fakeUI) AskPermission(req PermRequest) (bool, string) {
	f.asked = append(f.asked, req)
	return f.askAllowed, f.askBy
}

func (f *fakeUI) ReviewMemories(items []MemoryRow) []MemoryRow {
	f.reviewItems = append(f.reviewItems, items)
	return f.reviewResult
}

func (f *fakeUI) Screenshot() string { return f.screenshot }

func (f *fakeUI) AskQuestion(qs []tools.Question) ([]string, bool) {
	f.qAsked = append(f.qAsked, qs...)
	answers := make([]string, len(qs))
	for i := range answers {
		answers[i] = f.qAnswer
	}
	return answers, f.qOK
}

// TestAskHookAlwaysStoresRules drives the real askHook (veto event, UI
// overlay, rule storage) through the model pipeline: an "always" answer
// stores the command's prefix rules, which answer the next check without
// asking (DESIGN §6.2).
func TestAskHookAlwaysStoresRules(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{askAllowed: true, askBy: "user:always"}
	h.SetUIHooks(ui)

	pipe := h.PipelineFor()
	v := pipe.Check(tools.Request{Subject: "tool.bash", Arg: "git push origin main"})
	if v.Decision != perm.Allow || v.DecidedBy != "user:always" {
		t.Fatalf("always bash: %v by %q", v.Decision, v.DecidedBy)
	}
	if len(ui.asked) != 1 || ui.asked[0].Arg != "git push origin main" || ui.asked[0].Always != "allow `git push`" {
		t.Fatalf("asked: %+v", ui.asked)
	}
	rules := h.Perm.Rules()
	if len(rules) != 2 || rules[1].Pattern != "git push *" || rules[1].Decision != perm.Allow {
		t.Fatalf("stored rules: %+v", rules)
	}

	// The prefix rule answers the next push without asking again.
	if got := h.PipelineFor().Check(tools.Request{Subject: "tool.bash", Arg: "git push origin feature"}); got.Decision != perm.Allow {
		t.Fatalf("ruled re-check: %v by %q", got.Decision, got.DecidedBy)
	}
	if len(ui.asked) != 1 {
		t.Fatalf("ruled re-check must not ask: %+v", ui.asked)
	}
}

// TestMemoryToolsThroughPipeline drives memory_add and memory_search as the
// model sees them: under ask autonomy the add queues for review, search
// finds stored memories.
func TestMemoryToolsThroughPipeline(t *testing.T) {
	h, _ := newTestHostDB(t)
	seedSession(t, h)
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
	if resp := run("memory_add", `{"kind":"gotcha","text":"bump the labels sequence before deploying"}`); resp.IsError || !strings.Contains(resp.Content, "queued") {
		t.Fatalf("memory_add: %+v", resp)
	}
	if n := len(h.memoryProposals()); n != 1 {
		t.Fatalf("proposals=%d", n)
	}
	if err := h.Remember("deploys run from bin/deploy"); err != nil {
		t.Fatal(err)
	}
	if resp := run("memory_search", `{"query":"deploy"}`); resp.IsError || !strings.Contains(resp.Content, "bin/deploy") {
		t.Fatalf("memory_search: %+v", resp)
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
		resp, err = ag.Run(context.Background(), fantasy.ToolCall{ID: "t1", Name: "question",
			Input: `{"questions":[{"question":"Ship it?","options":[{"label":"proceed"},{"label":"abort"}]}]}`})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		break
	}
	if resp.IsError || !strings.Contains(resp.Content, `"Ship it?": proceed`) {
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

// TestTurnDoneRecordsStats covers the turn stat plumbing: the assistant
// transcript entry carries tokens, thought time and elapsed time; plain
// appended messages carry none.
func TestTurnDoneRecordsStats(t *testing.T) {
	h, _ := newTestHostDB(t)
	sa := h.streamHooks()
	sa.ReasoningDelta("pondering")
	sa.TextDelta("answer")
	sa.ToolCallStart("t1", "bash", "ls")
	sa.ToolCallEnd("t1", "bash", true, "", "")
	sa.TextDelta("done")
	sa.TurnDone(nil, fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120})

	// text streamed before a tool call stays above its row
	var order []string
	for _, m := range h.Session().Transcript {
		order = append(order, m.Kind+":"+m.Text)
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

func TestAlwaysChoice(t *testing.T) {
	h, _ := newTestHost(t)
	t.Chdir(h.Dirs.ProjectDir)
	proj := perm.RealPath(h.Dirs.ProjectDir) // rules name the resolved dir
	for _, c := range []struct {
		req      tools.Request
		label    string
		patterns []string
	}{
		{tools.Request{Subject: "tool.bash", Arg: "go test ./... && git diff"}, "allow `go test` and `git diff`", []string{"go test", "go test *", "git diff", "git diff *"}},
		{tools.Request{Subject: "tool.bash", Arg: "ls -la"}, "allow this exact command", []string{"ls -la"}},
		{tools.Request{Subject: "tool.bash", Arg: "cat notes"}, "allow this exact command", []string{"cat notes"}},
		{tools.Request{Subject: "tool.bash", Arg: "go vet && git diff && make && ls"}, "allow `go vet`, `git diff` and 2 more", []string{"go vet", "go vet *", "git diff", "git diff *", "make", "ls"}},
		{tools.Request{Subject: "tool.bash", Arg: "PATH=/tmp ls"}, "", nil},
		// interpreters and wrappers run whatever they are handed: no rule
		// written from one of these says what the next call would run
		{tools.Request{Subject: "tool.bash", Arg: "bash -c 'go test ./...'"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "/bin/sh -lc 'ls'"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "zsh script.sh"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "node -e 'x'"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "python3 -c 'x'"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "perl -e 'x'"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "ruby -e 'x'"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "env FOO=1 ls"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "sudo ls"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "xargs rm"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "nohup ls"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "time ls"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "go test ./... && sudo ls"}, "", nil},
		{tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, "internal", "a.go")}, "allow edits in internal/", []string{filepath.Join(proj, "internal", "*")}},
		// the gate asks about a protected write whatever the rules say
		{tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, ".zemi", "init.lua")}, "", nil},
		{tools.Request{Subject: "fs.read", Arg: ".env"}, "allow reads anywhere in the project", []string{filepath.Join(proj, "*")}},
		{tools.Request{Subject: "net.*", Arg: "https://pkg.go.dev/fmt"}, "allow pkg.go.dev", []string{"https://pkg.go.dev", "https://pkg.go.dev/*"}},
		{tools.Request{Subject: "tool.workflow", Arg: "script"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "rm *.tmp"}, "", nil},
		{tools.Request{Subject: "tool.bash", Arg: "go test ./... && ls ?"}, "", nil},
		{tools.Request{Subject: "tool.window", Arg: "*"}, "", nil},
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
	for approved, cases := range map[string]map[string]perm.Decision{
		"main.go": {
			"main.go": perm.Allow, "internal/a.go": perm.Allow,
			".git/config": perm.Ask, ".git": perm.Ask, "sub/.git/hooks/pre-commit": perm.Ask, ".gitattributes": perm.Ask,
		},
		".gitignore":               {".gitignore": perm.Allow, ".gitkeep": perm.Ask, ".git/config": perm.Ask},
		".github/workflows/ci.yml": {".github/workflows/ci.yml": perm.Allow, ".git/config": perm.Ask},
		".gitmodules":              {".gitmodules": perm.Ask},
		"sub/.git":                 {"sub/.git": perm.Ask},
	} {
		_, rules := h.alwaysChoice(tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, approved)})
		s := perm.NewStore()
		for _, r := range rules {
			s.Add(r)
		}
		for arg, want := range cases {
			if o := s.Evaluate("fs.write", filepath.Join(proj, arg), "model"); o.Decision != want {
				t.Errorf("always edits for %s: %s = %v, want %v", approved, arg, o.Decision, want)
			}
		}
	}
}

// TestSendWithoutModelLeavesNoSession: a send that cannot resolve a model
// fails before it creates a session row.
func TestSendWithoutModelLeavesNoSession(t *testing.T) {
	h, _ := newTestHostDB(t)
	if err := h.AgentSend("hi"); err == nil || !strings.Contains(err.Error(), "no model configured") {
		t.Fatalf("AgentSend = %v, want no model configured", err)
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
		zemi.agent.profile("cheap", { model = "p/cheap" })
		zemi.agent.set_default("cheap")
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
// not models.main, and so does compaction; a subagent without one takes the
// main tier, which is the worker tier whatever the conversation runs on.
func TestIdleStreamUsesProfileModel(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = {
			{ id = "big", context_window = 200000, images = false },
			{ id = "small", context_window = 32000 },
		} })
		zemi.model.set("a/small")
		zemi.agent.profile("p", { model = "a/big" })
		zemi.agent.set_default("p")
	`)
	if s := h.AgentStream(); s.Model != "a/big" || s.CtxWindow != 200000 {
		t.Fatalf("stream model %q window %d, want the profile's", s.Model, s.CtxWindow)
	}
	if h.PipelineFor().Images() {
		t.Fatal("the profile's model is text-only")
	}
	runUser(t, r, `assert(zemi.agent.current().model == "a/big")`)
	if m, _, err := h.modelFor("compact.model"); err != nil || m.Model() != "big" {
		t.Fatalf("summary model %v, err %v, want the profile's", m, err)
	}
	if id := h.subagentModelID(AgentDef{}); id != "a/small" {
		t.Fatalf("subagent model %q, want the main tier", id)
	}
}

// TestProfileContextFilesSurviveTwoSends: building the prompt must leave the
// profile's context files as written, or the second build joins them to the
// project directory twice and silently reads nothing.
func TestProfileContextFilesSurviveTwoSends(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.agent.profile("p", { context = { files = { "NOTES.md" } } })`)
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
// start, so the failed attempt's text is dropped instead of shown twice.
func TestRetryRestreamsStep(t *testing.T) {
	h, _ := newTestHostDB(t)
	run, _ := h.newRun(context.Background(), builtinAgents[0], "d", "p", false)
	type hooks interface {
		TextDelta(string)
		StepStart()
		Retrying(error, time.Duration)
	}
	for name, c := range map[string]struct {
		hooks hooks
		text  func() string
	}{
		"main":     {h.streamHooks().(*streamAdapter), func() string { return h.AgentStream().Text }},
		"subagent": {&subagentHooks{h: h, def: builtinAgents[0], run: run}, func() string { run.mu.Lock(); defer run.mu.Unlock(); return run.text }},
	} {
		c.hooks.TextDelta("before ")
		c.hooks.StepStart()
		c.hooks.TextDelta("partial")
		c.hooks.Retrying(nil, 0)
		c.hooks.TextDelta("full")
		if got := c.text(); got != "before full" {
			t.Errorf("%s: text %q, want %q", name, got, "before full")
		}
	}
}

func TestMaxOutput(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "anthropic", models = { { id = "big" }, { id = "old", max_output = 4096, images = false } } })
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
// asks for writes under a nested .zemi or .git, but not for the scratchpad.
func TestAlwaysRuleSkipsProtectedWrites(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	proj := perm.RealPath(h.Dirs.ProjectDir)
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: filepath.Join(proj, "*"), Decision: perm.Allow, Principal: "model"})
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: filepath.Join(perm.RealPath(h.Dirs.StateHome), "*"), Decision: perm.Allow, Principal: "model"})
	pipe := h.PipelineFor()
	for _, c := range []struct {
		path string
		want perm.Decision
	}{
		{filepath.Join(proj, "main.go"), perm.Allow},
		{filepath.Join(proj, "pkg", ".zemi", "init.lua"), perm.Deny},
		{filepath.Join(proj, "pkg", ".git", "config"), perm.Deny},
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
	// the user's own rule means what it says
	h.Perm.Add(perm.Rule{Subject: "fs.read", Pattern: filepath.Join(proj, ".env"), Decision: perm.Allow})
	if v := pipe.Check(tools.Request{Subject: "fs.read", Arg: filepath.Join(proj, ".env")}); v.Decision != perm.Allow {
		t.Errorf("a user rule for .env: %s (%s)", v.Decision, v.Reason)
	}
}

// TestHeadlessToolsetDropsPrompts: a print run has nobody to answer a
// question, approve a plan, open an editor or watch a window, so those
// schemas never ship with it, the prompt stops telling the model to use
// them, and the agent tool drops background, whose report a run that exits
// at idle would throw away.
func TestHeadlessToolsetDropsPrompts(t *testing.T) {
	h, _ := newTestHost(t)
	names := func() map[string]bool {
		out := map[string]bool{}
		for _, tool := range h.profileTools(nil) {
			out[tool.Info().Name] = true
		}
		return out
	}
	for name := range headlessExcluded {
		if names()[name] {
			t.Errorf("headless toolset still ships %q", name)
		}
	}
	params := func() map[string]any { return h.agentTool().Info().Parameters }
	if params()["background"] != nil {
		t.Error("the headless agent tool still offers background")
	}
	sys := h.buildSystem(nil)
	if strings.Contains(sys, "question tool") {
		t.Error("the headless prompt still tells the model to ask")
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

// TestProfileToolsRecordUnknown: another harness's spelling in a profile
// leaves the main agent almost toolless, so the name lands in /errors.
func TestProfileToolsRecordUnknown(t *testing.T) {
	h, _ := newTestHost(t)
	names := map[string]bool{}
	for _, tool := range h.profileTools(&Profile{Name: "lean", Tools: []string{"Read", "Grep", "bash"}}) {
		names[tool.Info().Name] = true
	}
	if !names["bash"] || names["read"] {
		t.Errorf("kept %v", slices.Sorted(maps.Keys(names)))
	}
	if !h.HasUnseenErrors() || !strings.Contains(h.errs.entries[0].msg, "no tool is named Read, Grep") {
		t.Errorf("errors = %+v", h.errs.entries)
	}
}
