package hostapi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
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

// TestPermissionRuleValidation covers the zemi.permission.rule contract:
// examples must be provided and must behave exactly as the rule claims
// (DESIGN §6.2), allow rules are user/project only, and accepted rules
// take effect in the permission engine.
func TestPermissionRuleValidation(t *testing.T) {
	h, r := newTestHost(t)

	runExpectErr(t, r, `zemi.permission.rule({
		subject = "tool.bash", pattern = "git status", decision = "allow",
	})`, "needs a match example")

	runExpectErr(t, r, `zemi.permission.rule({
		subject = "tool.bash", pattern = "git status", decision = "allow",
		match = "git push",
	})`, "does not match pattern")

	runExpectErr(t, r, `zemi.permission.rule({
		subject = "tool.bash", pattern = "git *", decision = "allow",
		not_match = "git push*", match = "git status",
	})`, "needs both examples")

	runUser(t, r, `zemi.permission.rule({
		subject = "tool.bash", pattern = "git status", decision = "allow",
		match = "git status",
	})`)
	runUser(t, r, `
		local rules = zemi.permission.rules()
		assert(#rules == 1, "rules=" .. #rules)
		assert(rules[1].pattern == "git status", "pattern=" .. rules[1].pattern)
		assert(rules[1].decision == "allow", "decision=" .. rules[1].decision)
	`)

	err := r.ExecuteChunk(context.Background(), `zemi.permission.rule({
		subject = "tool.bash", pattern = "git status", decision = "allow",
		match = "git status",
	})`, "plugin.lua", lua.OriginPlugin)
	if err == nil || !strings.Contains(err.Error(), "user or project") {
		t.Fatalf("plugin-origin allow rule: %v", err)
	}

	// Lua may always deny, from any origin.
	runUser(t, r, `zemi.permission.rule({
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
}

// TestToolRegisterValidation covers zemi.tool.register input checks and
// the builtin-name shadowing ban (DESIGN §6.4).
func TestToolRegisterValidation(t *testing.T) {
	_, r := newTestHost(t)

	runExpectErr(t, r, `zemi.tool.register({ name = "x" })`, "execute callback is required")
	runExpectErr(t, r, `zemi.tool.register({ name = "bash", execute = function() end })`, "builtin tool name")
	runExpectErr(t, r, `zemi.tool.register({ name = "my tool", execute = function() end })`, "identifier")

	runUser(t, r, `
		zemi.tool.register({
			name = "weather",
			description = "look up weather",
			input = { type = "object", properties = { city = { type = "string" } }, required = { "city" } },
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

	runUser(t, r, `zemi.provider.register({
		id = "myllm", kind = "openai_compat", base_url = "https://llm.internal/v1",
		api_key_env = "MYLLM_KEY",
		models = { { id = "main", display = "Main" }, { id = "mini" } },
	})`)

	runUser(t, r, `zemi.model.set("myllm/main")`)
	if v, ok := h.Settings.Get("agent.model"); !ok || v != "myllm/main" {
		t.Fatalf("agent.model=%v ok=%v", v, ok)
	}
	runUser(t, r, `
		local models = zemi.model.list()
		assert(#models == 2, "models=" .. #models)
		for _, m in ipairs(models) do
			assert(type(m.has_key) == "boolean", "has_key must be a boolean")
			assert(type(m.id) == "string" and m.id ~= "")
		end
	`)
	runExpectErr(t, r, `zemi.provider.register({ id = "myllm", kind = "openai" })`, "already registered")
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
	runUser(t, r, `assert(zemi.session.id() ~= "", "session id missing")`)
}

// TestSteeringQueue covers the DESIGN §6.3 steering contract: a send
// during an active turn enqueues instead of starting a second runner, and
// cancel is safe without a runner.
func TestSteeringQueue(t *testing.T) {
	h, _ := newTestHost(t)

	h.agents.mu.Lock()
	h.agents.active = true
	h.agents.mu.Unlock()

	if err := h.AgentSend("follow-up"); err != nil {
		t.Fatalf("steered send: %v", err)
	}
	if n := h.AgentSteering(); n != 1 {
		t.Fatalf("steering depth = %d", n)
	}
	if !h.AgentActive() {
		t.Fatal("agent should still be active")
	}

	h.AgentCancel() // no runner: must not panic
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
	pipe2.Ask = func(req tools.Request) (bool, string) { return true, "user:once" }
	if v := pipe2.Check(tools.Request{Subject: "tool.bash", Arg: "git push"}); v.Decision != perm.Allow || v.DecidedBy != "user:once" {
		t.Fatalf("asked bash: %v by %q", v.Decision, v.DecidedBy)
	}
}

// TestStoreBackedState covers zemi.state roundtrip through the global
// store (DESIGN §6.5).
func TestStoreBackedState(t *testing.T) {
	_, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.state.put("note", { text = "hi", n = 2 })
		local v = zemi.state.get("note")
		assert(v and v.text == "hi" and v.n == 2, "state roundtrip failed")
	`)
}

// fakeUI is a UIHooks stub driving the host-side overlay paths in tests.
type fakeUI struct {
	askAllowed bool
	askBy      string
	asked      []PermRequest
	pickID     string
	pickOK     bool
	pickItems  []ModelOption
}

func (f *fakeUI) Notify(level, title, body string) {}
func (f *fakeUI) OpenCmdline(init string)          {}
func (f *fakeUI) CloseCmdline()                    {}
func (f *fakeUI) OpenOverlay(o Overlay)            {}
func (f *fakeUI) CloseOverlay(id string) bool      { return true }
func (f *fakeUI) Refresh()                         {}
func (f *fakeUI) AgentEvent(ev AgentEvent)         {}

func (f *fakeUI) AskPermission(req PermRequest) (bool, string) {
	f.asked = append(f.asked, req)
	return f.askAllowed, f.askBy
}

func (f *fakeUI) PickModel(items []ModelOption, current string) (string, bool) {
	f.pickItems = items
	return f.pickID, f.pickOK
}

// TestAskHookAlwaysStoresRules drives the real askHook (veto event, UI
// overlay, rule storage) through the model pipeline: an "always" answer
// stores one exact model-principal rule that answers the next check
// without asking (DESIGN §6.2).
func TestAskHookAlwaysStoresRules(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{askAllowed: true, askBy: "user:always"}
	h.SetUIHooks(ui)

	pipe := h.PipelineFor()
	v := pipe.Check(tools.Request{Subject: "tool.bash", Arg: "git push origin main"})
	if v.Decision != perm.Allow || v.DecidedBy != "user:always" {
		t.Fatalf("always bash: %v by %q", v.Decision, v.DecidedBy)
	}
	if len(ui.asked) != 1 || ui.asked[0].Arg != "git push origin main" {
		t.Fatalf("asked: %+v", ui.asked)
	}
	rules := h.Perm.Rules()
	if len(rules) != 1 || rules[0].Pattern != "git push origin main" || rules[0].Decision != perm.Allow {
		t.Fatalf("stored rules: %+v", rules)
	}

	// The stored rule answers the next check without asking again.
	if got := h.PipelineFor().Check(tools.Request{Subject: "tool.bash", Arg: "git push origin main"}); got.Decision != perm.Allow {
		t.Fatalf("ruled re-check: %v by %q", got.Decision, got.DecidedBy)
	}
	if len(ui.asked) != 1 {
		t.Fatalf("ruled re-check must not ask: %+v", ui.asked)
	}
}

// TestModelPickStores drives zemi.model.pick through the fake UI: the
// picked id is stored in agent.model; a cancelled pick stores nothing.
func TestModelPickStores(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{pickID: "myllm/main", pickOK: true}
	h.SetUIHooks(ui)

	runUser(t, r, `
		zemi.provider.register({
			id = "myllm", kind = "openai_compat", base_url = "https://llm.internal/v1",
			models = { { id = "main", display = "Main" } },
		})
		local id = zemi.model.pick()
		assert(id == "myllm/main", "picked=" .. tostring(id))
	`)
	if v, ok := h.Settings.Get("agent.model"); !ok || v != "myllm/main" {
		t.Fatalf("agent.model=%v ok=%v", v, ok)
	}

	ui.pickID, ui.pickOK = "", false
	runUser(t, r, `assert(zemi.model.pick() == nil, "cancel must return nil")`)
	if v, _ := h.Settings.Get("agent.model"); v != "myllm/main" {
		t.Fatalf("cancel changed agent.model to %v", v)
	}
}
