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
	"testing/synctest"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
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
		DataHome:   t.TempDir(),
		ProjectDir: t.TempDir(),
	})
	h.ReleaseBuilder() // tests stub the model and boot no config
	t.Cleanup(h.Close)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// TestCommandSendsTypedLine: a builtin command that sends a ready prompt
// shows the line the user typed; the prompt rides behind the marker and
// names the zemi skill files it follows by a path the read tool opens, which
// holds with the skill switched off.
func TestCommandSendsTypedLine(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	h.RecordError("lua", "", "boom")
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
		{"/plugin new show cost", func() error { return h.pluginsCommand("new show cost", false) }, "plugins.md"},
		{"/agents new reviews my diffs", func() error { return h.agentsCommands()[0].GoFn(h, "new reviews my diffs", false) }, "agents.md"},
		{"/workflows new hunts a flake", func() error { return h.workflowCommands()[0].GoFn(h, "new hunts a flake", false) }, "workflows.md"},
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
// every substring in want.
func runExpectErr(t *testing.T, r *lua.Runtime, src string, want ...string) {
	t.Helper()
	_, err := runChunk(t, r, src)
	if err == nil {
		t.Fatalf("expected error containing %q, got success", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error %q does not contain %q", err.Error(), w)
		}
	}
}

// TestCheckToolNames records a zemi.tool.override naming a tool nothing
// answers to, once boot has added every Lua tool: the tool one names may
// come from a project or a plugin, so the call that adds it cannot tell.
// TestProviderBody: extra request-body fields are kept on the spec, and a
// kind whose requests cannot carry them is refused where it is written.
func TestProviderBody(t *testing.T) {
	t.Parallel()
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

// TestProviderRateLimit: a spec's rate_limit keeps both quotas, and a value
// no pace can run is refused where it is written.
func TestProviderRateLimit(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `zemi.provider.add({ id = "gw", kind = "openai_compat", base_url = "https://gw/v1", api_key = "k", rate_limit = 10, models = { { id = "m" } } })`)
	spec, ok := h.Providers.Lookup("gw")
	if !ok || spec.RateLimit != (agent.RateLimit{Requests: 10, Window: 60}) {
		t.Fatalf("rate_limit = %+v (ok %v), want 10 requests a minute", spec.RateLimit, ok)
	}
	runUser(t, r, `zemi.provider.add({ id = "q", kind = "openai_compat", base_url = "https://q/v1", api_key = "k", rate_limit = { requests = 40, completion_tokens = 52000, window = 60 }, models = { { id = "m" } } })`)
	spec, ok = h.Providers.Lookup("q")
	if !ok || spec.RateLimit != (agent.RateLimit{Requests: 40, CompletionTokens: 52000, Window: 60}) {
		t.Fatalf("rate_limit = %+v (ok %v), want both quotas", spec.RateLimit, ok)
	}
	runUser(t, r, `zemi.provider.add({ id = "t", kind = "openai_compat", base_url = "https://t/v1", api_key = "k", rate_limit = { completion_tokens = 30000 }, models = { { id = "m" } } })`)
	spec, ok = h.Providers.Lookup("t")
	if !ok || spec.RateLimit != (agent.RateLimit{CompletionTokens: 30000, Window: 60}) {
		t.Fatalf("rate_limit = %+v (ok %v), want the token quota alone", spec.RateLimit, ok)
	}
	runUser(t, r, `zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "https://p/v1", api_key = "k", models = {
		{ id = "a" },
		{ id = "b", rate_limit = { requests = 34, completion_tokens = 30000, window = 60 } },
	} })`)
	spec, ok = h.Providers.Lookup("p")
	if !ok || len(spec.Models) != 2 {
		t.Fatalf("models = %+v (ok %v), want two", spec.Models, ok)
	}
	if spec.Models[0].RateLimit.Paced() {
		t.Fatalf("model a = %+v, want the provider's unpaced default", spec.Models[0].RateLimit)
	}
	if spec.Models[1].RateLimit != (agent.RateLimit{Requests: 34, CompletionTokens: 30000, Window: 60}) {
		t.Fatalf("model b = %+v, want its own quota", spec.Models[1].RateLimit)
	}
	runExpectErr(t, r, `zemi.provider.add({ id = "x", kind = "openai", api_key = "k", rate_limit = 0, models = { { id = "m" } } })`, "rate_limit")
	runExpectErr(t, r, `zemi.provider.add({ id = "x", kind = "openai", api_key = "k", rate_limit = "ten", models = { { id = "m" } } })`, "rate_limit must be a number")
	runExpectErr(t, r, `zemi.provider.add({ id = "x", kind = "openai", api_key = "k", rate_limit = {}, models = { { id = "m" } } })`, "rate_limit needs requests or completion_tokens")
	runExpectErr(t, r, `zemi.provider.add({ id = "x", kind = "openai", api_key = "k", rate_limit = { requests = 10, tokens = 5 }, models = { { id = "m" } } })`, "unknown field")
	runExpectErr(t, r, `zemi.provider.add({ id = "x", kind = "openai", api_key = "k", rate_limit = { requests = 10, window = 0 }, models = { { id = "m" } } })`, "at least 1")
}

func TestCheckToolNames(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.tool.add({ name = "ticket", description = "a ticket", execute = function() end })
		for _, name in ipairs({ "shell", "ticket", "mcp_gh_search", "bash" }) do
			zemi.tool.override(name, { pre = function() end })
		end`)
	// only what no tool answers to once boot is done is named
	h.CheckToolNames()
	var named []string
	for _, e := range h.errs.entries {
		named = append(named, e.msg[:strings.Index(e.msg, ",")])
	}
	if want := []string{`zemi.tool.override names "bash"`}; !slices.Equal(named, want) {
		t.Fatalf("recorded %q, want %q", named, want)
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
		key, set string // the key, and the call init.lua sets it with; "" is models.compact and zemi.config.set
		config   []any  // what init.lua sets the key to, in order
		pick     any    // through SetConfig; nil picks nothing, "reset" runs /config reset
		want     any
		source   string // cfgSource after the reload
		keep     bool   // KeepPicks for the pick: off is a flag's
		restart  bool   // a new zemi instead of a reload
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
		{name: "a Lua number sets a number key", key: "agent.max_background", config: []any{5.0}, want: 5.0, source: "init.lua"},
		{name: "so dropping it returns the default", key: "agent.max_background", want: 4.0, source: "default"},
		{name: "a pick persists", pick: "p/b", keep: true, restart: true, want: "p/b", source: "saved"},
		{name: "init.lua overrides it", config: []any{"p/a"}, restart: true, want: "p/a", source: "init.lua"},
		{name: "a pick over config lasts the run", config: []any{"p/a"}, pick: "p/c", keep: true, want: "p/c", source: "this run"},
		{name: "a restart runs config", config: []any{"p/a"}, restart: true, want: "p/a", source: "init.lua"},
		{name: "dropping the line restores the pick", want: "p/c", source: "saved"},
		{name: "reset forgets it", pick: "reset", keep: true, want: "", source: "default"},
		{name: "for good", restart: true, want: "", source: "default"},
		{name: "a flag is not saved", pick: "p/z", restart: true, want: "", source: "default"},
	} {
		key := config.Key(cmp.Or(c.key, "models.compact"))
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
			if err := h.configCommand("reset " + string(key)); err != nil {
				t.Fatal(err)
			}
		default:
			if err := h.SetConfig(key, c.pick); err != nil {
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
// still checked against the old Lua veto and wait for the tool.pre handlers
// to come back instead of passing without them.
func TestReloadKeepsUserVetoes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
		r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
		h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
		t.Cleanup(h.Close)
		if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
			t.Fatal(err)
		}
		initLua := filepath.Join(dir, "init.lua")
		src := `
		zemi.event.on("tool.pre", function() return { decision = "deny", reason = "vetoed" } end)`
		if err := os.WriteFile(initLua, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}

		pre := h.toolPreHook()
		var vetoed tools.PreVerdict
		done := make(chan struct{})
		probe := lua.BootStep{Name: "probe", Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
			go func() {
				vetoed, _, _ = pre("shell", map[string]any{"command": "ls"})
				close(done)
			}()
			// virtual time: a hook that could return would have by now
			time.Sleep(50 * time.Millisecond)
			select {
			case <-done:
				t.Error("tool.pre passed before the config replayed its handler")
			default:
			}
			return nil
		}}
		steps := []lua.BootStep{h.NamespacesStep(), probe, lua.LoadFileStep("init", initLua, lua.OriginUser)}
		if err := h.ForceReload(t.Context(), steps); err != nil {
			t.Fatal(err)
		}
		<-done
		if vetoed != "deny" {
			t.Fatalf("tool.pre during reload = %q, want deny", vetoed)
		}
	})
}

// TestToolRegisterValidation covers zemi.tool.add: a spec that could
// never reach the model raises instead of registering (DESIGN §6.0), and
// the builtin-name shadowing ban holds (DESIGN §6.4).
func TestToolRegisterValidation(t *testing.T) {
	t.Parallel()
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
	runUser(t, r, `assert(zemi.env.get("ZEMI_TEST_TOKEN") == "abc", "user config reads env vars")`)

	runUser(t, r, `zemi.model.set("myllm/main")`)
	if v, ok := h.Settings.Get("models.default"); !ok || v != "myllm/main" {
		t.Fatalf("models.default=%v ok=%v", v, ok)
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

// TestSteeringQueue covers the DESIGN §4 queue: sends during a turn queue
// in order, taking the queue empties it, leftovers become one follow-up
// turn with active and a cancellable scope held across the handoff, and
// cancel is safe without a runner.
func TestSteeringQueue(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	claim := func() *turnScope {
		h.agents.mu.Lock()
		defer h.agents.mu.Unlock()
		return h.claim()
	}
	scope := claim()
	if err := h.AgentSend(""); err == nil {
		t.Fatal("a message with nothing in it was queued")
	}

	send := func(msgs ...string) {
		for _, m := range msgs {
			if err := h.AgentSend(m); err != nil {
				t.Fatalf("queued send: %v", err)
			}
		}
	}
	send("one", "two")
	h.UserSpoke() // the user's own line arrived while the turn ran
	if q := h.AgentTakeQueue(); len(q) != 2 || q[0] != "one" || q[1] != "two" {
		t.Fatalf("queue = %q", q)
	}
	// the lines went back to the composer, so no message reaches the turn: a
	// call later in it must not be detached by the spent signal
	select {
	case <-h.userSpoke():
		t.Fatal("a signal stayed raised after its lines were taken back")
	default:
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
	if st := h.AgentStream(); len(st.Steering) != 2 || st.Steering[0].Text != "c" || st.Steering[1].Text != "/init" {
		t.Fatalf("a queued command shows as typed: %+v", st.Steering)
	}
	h.AgentCancel()
	if next, text := h.nextTurn(scope, nil); next != nil || text != "" || h.AgentActive() {
		t.Fatalf("cancelled handoff = %q, active = %v", text, h.AgentActive())
	}
	if q := h.AgentTakeQueue(); len(q) != 2 || q[0] != "c" || q[1] != "/init" {
		t.Fatalf("queue after a cancel = %q", q)
	}

	// a report's markup stays out of the composer and draws a notice row of its
	// own; joining keeps every typed part ahead of the marker readers cut at
	report := func(run string) string {
		return agent.AttachMarker + "\n<agent-report run=\"" + run + "\" agent=\"explore\" status=\"finished\" description=\"look around\">\nr\n</agent-report>\n</attached-files>"
	}
	scope = claim()
	send(report("x"), "d", report("y"))
	want := []Queued{{Text: "d"}}
	if st := h.AgentStream(); !slices.Equal(st.Steering, want) {
		t.Fatalf("queued rows = %+v, want %+v", st.Steering, want)
	}
	if q := h.AgentTakeQueue(); len(q) != 1 || q[0] != "d" {
		t.Fatalf("queue with a report = %q", q)
	}
	send("e")
	if _, text := h.nextTurn(scope, nil); !strings.HasPrefix(text, "e"+agent.AttachMarker) || !strings.Contains(text, "run=\"x\"") || !strings.Contains(text, "run=\"y\"") {
		t.Fatalf("follow-up with reports = %q", text)
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
	if !strings.Contains(notes(), "Queue resumes on its own: 1 queued message") {
		t.Fatalf("no resume notice: %q", notes())
	}
	// the queued report reaches the next turn's reader whole, still classed as
	// the model's own, not the user's words
	if _, attached, _ := userRowsFor(agent.AttachMarker + "\n<agent-report run=\"a1\" agent=\"explore\" status=\"finished\" description=\"find \\\"x\\\"\">\nx is in y.go\n</agent-report>\n</attached-files>"); !attached {
		t.Fatal("the report read as the user's words")
	}

	h.AgentCancel() // no runner: must not panic
}

// TestReportRowsDrawWhereDelivered: a report's row goes up where the report
// was delivered, and a turn whose message is all markup draws no user row of
// the reports, so the transcript never draws them twice.
func TestReportRowsDrawWhereDelivered(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	headless := func(run string) string {
		return agent.AttachMarker + "\n<agent-report run=\"" + run + "\" agent=\"explore\" status=\"finished\" description=\"look around\">\nfound it\n</agent-report>\n</attached-files>"
	}
	h.agents.mu.Lock()
	scope := h.claim()
	h.agents.mu.Unlock()
	for _, run := range []string{"a2", "a3"} {
		h.work(run, "explore", "explore", "finished", "found it")
		if err := h.AgentSend(headless(run)); err != nil {
			t.Fatal(err)
		}
	}
	if _, text := h.nextTurn(scope, nil); text == "" {
		t.Fatal("the queued reports started no turn")
	}
	for _, row := range h.Session().Transcript {
		if row.Role == fantasy.MessageRoleUser && row.Kind == RowMessage {
			t.Fatalf("the reports drew a row of the user's: %+v", row)
		}
	}
}

// TestHeldPrompt covers the command line's prompt held at boot: it is on the
// transcript from the hold on, a send during the hold steers into the turn it
// claimed, SendHeld runs that turn without adding the message twice, and a
// hold the user took back (esc during boot) leaves nothing behind and starts
// no turn.
func TestHeldPrompt(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			return textParts("done")
		}}, nil
	}

	h.HoldPrompt("hello")
	if !h.AgentActive() {
		t.Fatal("a hold does not hold the agent")
	}
	if tr := h.Session().Transcript; len(tr) != 1 || tr[0].Text != "hello" || tr[0].Role != fantasy.MessageRoleUser {
		t.Fatalf("held rows: %+v", tr)
	}
	// a message typed while boot runs steers into the turn the hold claimed
	if err := h.AgentSend("also this"); err != nil {
		t.Fatal(err)
	}
	if tr := h.Session().Transcript; len(tr) != 1 {
		t.Fatalf("a steered message took a row of its own: %+v", tr)
	}
	if err := h.SendHeld("hello"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	var hellos, also int
	for _, m := range h.Session().Transcript {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		switch m.Text {
		case "hello":
			hellos++
		case "also this":
			also++
		}
	}
	if hellos != 1 || also != 1 {
		t.Fatalf("held %d times, steered %d: %+v", hellos, also, h.Session().Transcript)
	}

	// esc during boot takes the prompt back: the message leaves the
	// transcript, the text returns to the composer, no turn starts
	h.HoldPrompt("take this back")
	if tr := h.Session().Transcript; len(tr) != 4 || tr[3].Text != "take this back" {
		t.Fatalf("second held rows: %+v", tr)
	}
	h.AgentCancel()
	if tr := h.Session().Transcript; len(tr) != 3 {
		t.Fatalf("the taken-back message stayed: %+v", tr)
	}
	if got := ui.composed; len(got) != 1 || got[0] != "take this back" {
		t.Fatalf("the prompt did not return to the composer: %q", got)
	}
	if err := h.SendHeld("take this back"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	for _, m := range h.Session().Transcript {
		if m.Text == "take this back" {
			t.Fatalf("a taken-back message went to the model: %+v", m)
		}
	}
}

// TestHeldCommandPrompt: the command line's slash prompt (the CLI holds no
// "/..."-prompt) runs as the command it names through SendHeld's fallback,
// the way a composer submit would, and one that refuses still shows the
// refusal it gives from the composer.
func TestHeldCommandPrompt(t *testing.T) {
	for _, c := range []struct {
		name     string
		refusing bool
	}{
		{"a held command runs with its args", false},
		{"a held command's refusal shows", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.SetUIHooks(&fakeUI{})
			ran := make(chan string, 1)
			if _, err := h.Commands.Add(&Command{
				Name: "heldmark", NArgs: NArgsAny,
				Origin: lua.OriginBuiltin, Gen: h.currentGen(),
				GoFn: func(h *Host, args string, _ bool) error {
					ran <- args
					if c.refusing {
						// a refusal is the command's own toast, as /compact's is
						h.Notify(slog.LevelWarn, "Nothing to hold yet", "")
					}
					return nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := h.SendHeld("/heldmark the args"); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-ran:
				if !c.refusing && got != "the args" {
					t.Fatalf("ran with %q", got)
				}
			default:
				t.Fatal("the held command never ran")
			}
			if !c.refusing {
				return
			}
			waitFor(t, func() error {
				for _, n := range h.evlog.notes {
					if strings.Contains(n.title, "Nothing to hold yet") {
						return nil
					}
				}
				return errors.New("no refusal toast")
			})
		})
	}
}

// TestFreshTurnPerTurn: every turn is a fresh call that opens on the view -
// an empty log sends none - and no exchange carries between turns. Both
// turns still land in the transcript.
func TestFreshTurnPerTurn(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "ok")
	h.SetUIHooks(&fakeUI{})
	if err := h.AgentSend("one"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	// the first turn's rows are in the log; the builder joins them before
	// the second turn reads its view
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(1, 0); err != nil {
			return errors.New("the first exchange is not summarized yet")
		}
		return nil
	})
	if err := h.AgentSend("two"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	m.mu.Lock()
	prompts := slices.Clone(m.prompts)
	m.mu.Unlock()
	if len(prompts) != 2 {
		t.Fatalf("%d turns recorded, want 2 (a short log earns no compaction calls)", len(prompts))
	}
	for i, want := range []int{3, 4} { // turn 1: system, context, user; turn 2 adds the view
		if n := len(prompts[i]); n != want {
			t.Fatalf("turn %d prompt has %d messages, want %d", i+1, n, want)
		}
		if got := concatText(prompts[i][len(prompts[i])-1].Content); got != []string{"one", "two"}[i] {
			t.Fatalf("turn %d message = %q", i+1, got)
		}
	}
	// no residue: after the context comes the view (one leaf line per
	// message until a batch merges) and the message, and no message of the
	// first turn's own
	if got := concatText(prompts[1][2].Content); !strings.Contains(got, "0+1|one") || !strings.Contains(got, "1+1|ok") {
		t.Fatalf("second turn view = %q", got)
	}
	if tr := h.Session().Transcript; len(tr) != 4 {
		t.Fatalf("transcript = %+v", tr)
	}
}

// TestPipelineForCheck verifies the pipeline the host builds for agent tool
// calls: an ask policy's veto leaves every action to the user, an
// unanswered ask fails closed when no UI is attached, and an asker resolves
// it (DESIGN §6.2).
func TestPipelineForCheck(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	askEverything(t, r)
	pipe := h.PipelineFor()

	// No UI: the ask hook fails closed.
	if v := pipe.Check(tools.Request{Subject: "tool.shell", Arg: "git status"}); v.Allowed || v.DecidedBy != tools.ByNobody {
		t.Fatalf("unanswered shell: %+v (%s)", v, v.Reason)
	}

	// An asker (the UI overlay) resolves the leftover ask.
	pipe2 := h.PipelineFor()
	pipe2.Ask = func(req tools.Request) (bool, string) { return true, "user:once" }
	if v := pipe2.Check(tools.Request{Subject: "tool.shell", Arg: "git status"}); !v.Allowed || v.DecidedBy != "user:once" {
		t.Fatalf("asked shell: %+v by %q", v, v.DecidedBy)
	}

	// No veto policy: the same call settles on its own.
	runUser(t, r, askEverythingOff)
	pipe3 := h.PipelineFor()
	if v := pipe3.Check(tools.Request{Subject: "tool.shell", Arg: "git status"}); !v.Allowed || v.DecidedBy != tools.ByDefault {
		t.Fatalf("allowed shell: %+v by %q", v, v.DecidedBy)
	}
}

// TestStoreBackedState runs its steps in order against one global store: every
// origin but a plugin shares it, and a plugin keeps its own (DESIGN §6.5).
func TestStoreBackedState(t *testing.T) {
	t.Parallel()
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
		{"project shares it", lua.OriginProject, `
			assert(zemi.state.get("note").text == "hi", "project cannot read a user key")
			assert(zemi.state.set("note", "p"))
			assert(zemi.state.get("note") == "p", "project roundtrip failed")`},
		{"plugin code outside a plugin file", lua.OriginPlugin, `
			assert(not pcall(zemi.state.get, "note"), "a nameless plugin read a state space")
			assert(not pcall(zemi.state.del, "note"), "a nameless plugin deleted from a state space")`},
		{"the last write stands", lua.OriginUser, `
			assert(zemi.state.get("note") == "p", "the project's write is gone")`},
	} {
		if err := r.ExecuteChunk(t.Context(), step.src, step.name+".lua", step.origin); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
}

// fakeUI is a UIHooks stub driving the host-side overlay paths in tests.
type fakeUI struct {
	askAllowed          bool
	askBy               string
	onAsk               func(PermRequest) (bool, string) // answers in place of askAllowed and askBy
	asked               []PermRequest
	qAsked              []tools.Question
	qAnswer             string
	qOK                 bool
	planVerdict         tools.PlanVerdict
	planOK              bool
	planAsked           int
	mu                  sync.Mutex // guards opened: a status change opens off the test goroutine
	opened              []Overlay
	refreshed           []string
	rebuilt             []Overlay
	closed              []string
	closedLua           int
	turnsDone           atomic.Int32
	screenshot          string
	chatRow             int
	copied              string
	composed            []string          // the drafts ComposerSet put in the composer
	onRefresh           func(id string)   // runs inside RefreshOverlay, mid-rebuild
	edit                func(path string) // what the user does in the editor
	relaunched, resumed string
}

func (f *fakeUI) Notify(slog.Level, string, string) {}
func (f *fakeUI) OpenOverlay(o Overlay) {
	f.mu.Lock()
	f.opened = append(f.opened, o)
	f.mu.Unlock()
}

func (f *fakeUI) RefreshOverlay(id string, build func() Overlay) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, id)
	if build != nil {
		f.rebuilt = append(f.rebuilt, build())
	}
	if f.onRefresh != nil {
		f.onRefresh(id)
	}
}

func (f *fakeUI) overlays() []Overlay {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.opened)
}

func (f *fakeUI) CloseOverlay(id string, by lua.Origin, byPath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
}
func (f *fakeUI) CloseLuaOverlays()                   { f.closedLua++ }
func (f *fakeUI) Refresh()                            {}
func (f *fakeUI) Scroll(ScrollAction, string)         {}
func (f *fakeUI) LayoutRects() map[string]Rect        { return nil }
func (f *fakeUI) KeyHelp() ([][2]string, [][2]string) { return nil, nil }
func (f *fakeUI) AgentStream()                        {}
func (f *fakeUI) TurnDone(err string, stopped bool)   { f.turnsDone.Add(1) }
func (f *fakeUI) ComposerEdit(op ComposerOp, text string, cursor int) {
	if op == ComposerSet {
		f.mu.Lock()
		f.composed = append(f.composed, text)
		f.mu.Unlock()
	}
}

func (f *fakeUI) EditFile(_ context.Context, req EditRequest) error {
	if f.edit != nil {
		f.edit(req.Path)
	}
	return nil
}
func (f *fakeUI) Copy(text string)            { f.copied = text }
func (f *fakeUI) Relaunch(dir, resume string) { f.relaunched, f.resumed = dir, resume }

func (f *fakeUI) AskPermission(req PermRequest) (bool, string) {
	f.asked = append(f.asked, req)
	if f.onAsk != nil {
		return f.onAsk(req)
	}
	return f.askAllowed, f.askBy
}

func (f *fakeUI) Screenshot() string { return f.screenshot }

func (f *fakeUI) ChatRow() int { return f.chatRow }

func (f *fakeUI) AskQuestion(qs []tools.Question) ([]tools.Answer, bool) {
	f.qAsked = append(f.qAsked, qs...)
	answers := make([]tools.Answer, len(qs))
	for i := range answers {
		answers[i] = tools.Answer{Picked: []string{f.qAnswer}}
	}
	return answers, f.qOK
}

func (f *fakeUI) AskPlan(plan string) (tools.PlanVerdict, bool) {
	f.planAsked++
	return f.planVerdict, f.planOK
}

// TestPipelineQuestionToolEndToEnd drives the question tool through
// PipelineFor and the fakeUI: the structured ask reaches the UI hooks and
// the chosen label comes back as the tool output; with no UI the pipeline
// has no question hook, so the tool tells the model nobody can answer.
func TestPipelineQuestionToolEndToEnd(t *testing.T) {
	t.Parallel()
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
		resp, err = ag.Run(t.Context(), fantasy.ToolCall{
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
	t.Parallel()
	h, _ := newTestHostDB(t)

	pipe := h.PipelineFor()
	if err := pipe.TodosWrite([]store.Todo{{Content: "first", Status: "in_progress", ActiveForm: "doing first"}}); err != nil {
		t.Fatalf("TodosWrite: %v", err)
	}
	rows, err := pipe.TodosRead()
	if err != nil || len(rows) != 1 || rows[0].Content != "first" || rows[0].Status != "in_progress" {
		t.Fatalf("TodosRead: %v %+v", err, rows)
	}
	persisted, err := h.DB.Todos(store.MainSessionID)
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
		t.Parallel()
		h, _ := newTestHostDB(t)
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
		t.Parallel()
		h, _ := newTestHostDB(t)
		set(h, both)
		engage(h, 0)
		s := h.streamHooks()
		for i := range todoNagAfter * 2 {
			s.ToolCallEnd(strconv.Itoa(i), "read", out(""), "")
		}
		s.StepDone(agent.Step{})
		s.ToolCallEnd("t", string(tools.ToolTodos), out(""), "")
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
		t.Parallel()
		h, _ := newTestHostDB(t)
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
		t.Parallel()
		h, _ := newTestHostDB(t)
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
		t.Parallel()
		h, _ := newTestHostDB(t)
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
		t.Parallel()
		h, _ := newTestHostDB(t)
		set(h, both)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		h.turn.Store(&turnScope{ctx: ctx, cancel: cancel})
		engage(h, todoNagAfter)
		if got := takeNag(h); got != "" {
			t.Fatalf("a nag for a cancelled turn would lead the next message: %q", got)
		}
	})

	t.Run("a single in_progress item or none never nags", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
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

	t.Run("working with nothing in_progress nags once", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		set(h, []store.Todo{{Content: "a", Status: "done"}, {Content: "b", Status: "pending"}, {Content: "c", Status: "pending"}})
		engage(h, todoNagAfter)
		if got := takeNag(h); !strings.Contains(got, "none is in_progress") {
			t.Fatalf("all-pending nag = %q", got)
		}
		set(h, []store.Todo{{Content: "b", Status: "in_progress"}, {Content: "c", Status: "pending"}})
		engage(h, todoNagAfter)
		if got := takeNag(h); !strings.Contains(got, "in_progress for") {
			t.Fatalf("a write re-arms onto the in_progress item: %q", got)
		}
	})

	t.Run("a turn with open todos arms one turn-start remind", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		set(h, []store.Todo{{Content: "a", Status: "done"}, {Content: "b", Status: "pending"}})
		h.armTodoTurnNote()
		if got := takeNag(h); !strings.Contains(got, "todos are open") {
			t.Fatalf("turn-start remind = %q", got)
		}
		set(h, []store.Todo{{Content: "a", Status: "done"}, {Content: "b", Status: "done"}})
		h.armTodoTurnNote()
		if got := takeNag(h); got != "" {
			t.Fatalf("all done arms nothing: %q", got)
		}
	})

	t.Run("delivered steering reminds the model to track it", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		h.agents.mu.Lock()
		h.agents.todoList = []store.Todo{{Content: "a", Status: "in_progress"}}
		h.agents.active = true
		h.agents.mu.Unlock()
		h.agents.mu.Lock()
		h.agents.steering = []string{"also update the readme"}
		h.agents.mu.Unlock()
		msgs := h.deliverSteering()
		if len(msgs) != 2 || !strings.Contains(msgs[1], "todo list") {
			t.Fatalf("steering delivery = %q, want the tracked-work remind second", msgs)
		}
		if again := h.deliverSteering(); len(again) != 0 {
			t.Fatalf("the remind must not repeat: %q", again)
		}
		h.agents.mu.Lock()
		h.agents.todoList = nil
		h.agents.steering = []string{"another"}
		h.agents.mu.Unlock()
		if msgs := h.deliverSteering(); len(msgs) != 1 {
			t.Fatalf("no list, no remind: %q", msgs)
		}
	})
}

// TestTurnDoneDropsTheRetry: a turn that ends during a retry backoff (esc,
// or the last retry failing) leaves no retry in the view - TurnDone is the
// turn-end funnel and resets what beginTurn armed.
func TestTurnDoneDropsTheRetry(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	sa := h.streamHooks()
	sa.StepStart()
	sa.Retrying(errors.New("overloaded"), 3*time.Second)
	if live := h.AgentStream(); !live.Retrying || live.Retries != 1 {
		t.Fatalf("setup: view = retrying %v (n=%d), want the retry the hook reported", live.Retrying, live.Retries)
	}
	sa.TurnDone(context.Canceled, fantasy.Usage{})
	if live := h.AgentStream(); live.Retrying || live.Retries != 0 || live.RetryWhy != "" {
		t.Fatalf("idle view = retrying %v (n=%d why=%q), want the retry dropped with the turn", live.Retrying, live.Retries, live.RetryWhy)
	}
}

// TestTurnDoneLogsTheCutCall: a call the turn's end cut off never reached the
// log (its step's OnStepFinish is gone), so a resumed window drew no rail for
// it. TurnDone logs the call and its result, and the rebuild draws the row
// the live window drew.
func TestTurnDoneLogsTheCutCall(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.agents.mu.Lock()
	h.agents.tools = []StreamTool{{
		ID: "t1", Name: "shell", Label: "slow4",
		Input: `{"command":"sleep 8","about":"slow4"}`, Status: CallRun,
		Output: "stopped: the user interrupted the command",
	}}
	h.agents.mu.Unlock()
	sa := h.streamHooks()
	sa.TurnDone(context.Canceled, fantasy.Usage{})

	live := h.Session().Transcript
	var rail ChatMessage
	for _, m := range live {
		if m.Kind == RowTool {
			rail = m
		}
	}
	rows, err := h.DB.LogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	tool, echo := false, false
	for _, r := range rows {
		tool, echo = tool || r.Kind == store.KindTool, echo || r.Kind == store.KindEcho
	}
	if !tool || !echo {
		t.Fatalf("the log holds no cut call's rows: %v", rows)
	}
	var rebuilt ChatMessage
	for _, m := range logRows(rows) {
		if m.Kind == RowTool {
			rebuilt = m
		}
	}
	if rebuilt.Tool != rail.Tool || rebuilt.Label != rail.Label || rebuilt.Status != rail.Status || rebuilt.Output != rail.Output {
		t.Fatalf("rebuilt rail %+v, want the live one %+v", rebuilt, rail)
	}
}

// TestTurnDoneLogsASettledCutCall: esc mid-command lets the tool return its
// stopped result first, so ToolCallEnd settles the row before TurnDone - the
// step still never finished, and the rail still must reach the log.
func TestTurnDoneLogsASettledCutCall(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.agents.mu.Lock()
	h.agents.tools = []StreamTool{{
		ID: "t1", Name: "shell", Label: "slow4",
		Input: `{"command":"sleep 8","about":"slow4"}`, Status: CallStopped,
		Output: "stopped: the user interrupted the command",
	}}
	h.agents.mu.Unlock()
	sa := h.streamHooks()
	sa.TurnDone(context.Canceled, fantasy.Usage{})

	rows, err := h.DB.LogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	var rebuilt ChatMessage
	for _, m := range logRows(rows) {
		if m.Kind == RowTool {
			rebuilt = m
		}
	}
	if rebuilt.Kind != RowTool || rebuilt.Status != CallStopped || rebuilt.Output != "stopped: the user interrupted the command" {
		t.Fatalf("rebuilt rail %+v, want the stopped result the live window drew", rebuilt)
	}
}

// TestTurnDoneLogsTheRawArgs: ToolCallStart keeps only the digested input on
// the row, so the cut-call loop logged the digest and the rebuilt rail lost
// its label and line; the log's tool row holds the raw args, where the
// rebuild reads both.
func TestTurnDoneLogsTheRawArgs(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.agents.mu.Lock()
	h.agents.tools = []StreamTool{{
		ID: "t1", Name: "shell", Label: "slow4",
		Input: "sleep 30", Raw: `{"command":"sleep 30","about":"slow4"}`, Status: CallStopped,
		Output: "stopped: the user interrupted the command",
	}}
	h.agents.mu.Unlock()
	sa := h.streamHooks()
	sa.TurnDone(context.Canceled, fantasy.Usage{})

	rows, err := h.DB.LogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	var toolRow store.LogEntry
	for _, r := range rows {
		if r.Kind == store.KindTool {
			toolRow = r
		}
	}
	if toolRow.Text != `{"command":"sleep 30","about":"slow4"}` {
		t.Fatalf("the log holds %q, want the raw args the live rail's label came from", toolRow.Text)
	}
	var rebuilt ChatMessage
	for _, m := range logRows(rows) {
		if m.Kind == RowTool {
			rebuilt = m
		}
	}
	if rebuilt.Label != "slow4" {
		t.Fatalf("rebuilt label %q, want the live one", rebuilt.Label)
	}
}

// TestTurnDoneLogsTheCutThought: a step's reasoning reaches the log only at
// its finish, so esc mid-thought left every thought row live-only - the
// stretches it flushed and the open buffer log here, in draw order. The scan
// reads this step's rows alone: an earlier turn's unnamed thought never
// re-logs.
func TestTurnDoneLogsTheCutThought(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: "an earlier turn.", Stat: &TurnStat{ThoughtMS: 900}})
	sa := h.streamHooks()
	sa.StepDone(agent.Step{}) // the previous step settled...
	sa.StepStart()            // ...and this one began: its rows are unlogged
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: "thinking it over.", Stat: &TurnStat{ThoughtMS: 1100}})
	h.agents.mu.Lock()
	h.agents.reasonBuf = "still thinking."
	h.agents.thought = 400 * time.Millisecond
	h.agents.mu.Unlock()
	sa.TurnDone(context.Canceled, fantasy.Usage{})

	rows, err := h.DB.LogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range rows {
		if r.Kind == store.KindThought {
			texts = append(texts, r.Text)
		}
	}
	if len(texts) != 2 || texts[0] != "thinking it over." || texts[1] != "still thinking." {
		t.Fatalf("the log holds thoughts %q, want this step's two and not the earlier turn's", texts)
	}
	tr := logRows(rows)
	var ms []int64
	for _, m := range tr {
		if m.Kind == RowReasoning {
			ms = append(ms, m.Stat.ThoughtMS)
		}
	}
	if len(ms) != 2 || ms[0] != 1100 || ms[1] != 400 {
		t.Fatalf("the rebuilt thoughts carry ms %v, want [1100 400]", ms)
	}
}

// TestTurnDoneLogsTheCutReply: a reply cut mid-stream draws its partial text
// live and dies with the turn otherwise - the log holds it, so a resumed
// window draws the line the user read. The stretch a call's open flushed
// above the rail logs with it, in draw order; an earlier turn's reply never
// re-logs.
func TestTurnDoneLogsTheCutReply(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "an earlier turn's reply"})
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "a queued line"})
	sa := h.streamHooks()
	sa.StepDone(agent.Step{})
	sa.StepStart()
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: "a thought.", Stat: &TurnStat{}})
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "words before the call"})
	h.agents.mu.Lock()
	h.agents.reasonBuf, h.agents.thought = "a thought.", 300*time.Millisecond
	h.agents.textBuf = "done, here"
	h.agents.mu.Unlock()
	sa.TurnDone(context.Canceled, fantasy.Usage{})

	rows, err := h.DB.LogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range rows {
		if r.Kind == store.KindAgent {
			texts = append(texts, r.Text)
		}
	}
	if len(texts) != 2 || texts[0] != "words before the call" || texts[1] != "done, here" {
		t.Fatalf("the log holds replies %q, want this step's two and neither earlier row", texts)
	}
}

// TestStreamDoneStoresLastIn: a step's context is stored where a resume's
// meter reads it, and a dropped chat clears it with the chat it belonged to.
func TestStreamDoneStoresLastIn(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	sa := h.streamHooks()
	sa.StreamDone(fantasy.Usage{InputTokens: 900, CacheReadTokens: 100})
	s, err := h.DB.GetSession(store.MainSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if want := billedInput(fantasy.Usage{InputTokens: 900, CacheReadTokens: 100}); s.LastIn != want {
		t.Fatalf("stored last_in %d, want %d", s.LastIn, want)
	}
	h.chatDropped()
	if s, err := h.DB.GetSession(store.MainSessionID); err != nil || s.LastIn != 0 {
		t.Fatalf("after a drop: last_in %d err %v, want 0", s.LastIn, err)
	}
}

// TestCloseStopsTheQueueDrain: a host closed inside the idle-drain's window
// stops the timer, so no drain fires a turn against closed stores.
func TestCloseStopsTheQueueDrain(t *testing.T) {
	t.Parallel()
	h, clock := newLoopHost(t)
	replyStub(t, h, "ok")
	h.agents.mu.Lock()
	scope := h.claim()
	h.agents.mu.Unlock()
	if err := h.AgentSend("queued"); err != nil {
		t.Fatal(err)
	}
	scope.cancel()
	h.nextTurn(scope, nil)
	if clock.armed() != 1 {
		t.Fatalf("setup: %d timers, want the drain armed", clock.armed())
	}
	h.Close()
	if clock.armed() != 0 {
		t.Fatalf("close left the drain armed (%d timers)", clock.armed())
	}
}

// TestTurnClockIsTheHostClock: a turn's stamps and the snapshot that reads
// them share one clock, so a zemi on a clock behind the wall one (a plugin
// test's) counts no negative elapsed.
func TestTurnClockIsTheHostClock(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	clock := &fakeClock{now: time.Unix(1_000_000_000, 0)}
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, Clock: clock})
	h := NewHost(r, log, config.NewStore(), config.Dirs{StateHome: t.TempDir(), ProjectDir: t.TempDir()})
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	h.SetUIHooks(&fakeUI{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			if s := h.AgentStream(); s.Elapsed < 0 || s.Elapsed > time.Minute {
				t.Errorf("a live turn on a clock behind the wall one counts %v", s.Elapsed)
			}
			return textParts("done")
		}}, nil
	}
	if err := h.AgentSend("hi"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	tr := h.Session().Transcript
	if last := tr[len(tr)-1]; last.Stat != nil && (last.Stat.ElapsedMS < 0 || last.Stat.ElapsedMS > 60_000) {
		t.Fatalf("the settled turn counts %d ms", last.Stat.ElapsedMS)
	}
}

// TestTurnEndSettlesTodos: an item left in_progress when the claim ends goes
// back to pending, so no row of the list reads as running while the agent is
// idle; the rest of the model's list is kept as it wrote it.
func TestTurnEndSettlesTodos(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
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
		todos, err := h.DB.Todos(store.MainSessionID)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(todos, func(td store.Todo) bool { return td.Status == store.TodoInProgress }) {
			return errors.New("an item is still in progress")
		}
		return nil
	})
	todos, err := h.DB.Todos(store.MainSessionID)
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

// TestTurnPricedWithItsModel: a turn records the model it was built
// with, so its price is not read off the session default.
func TestTurnPricedWithItsModel(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	replyStub(t, h, "ok")
	runUser(t, r, `
		zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://x", models = { { id = "cheap" } } })
		zemi.model.set("p/cheap")
	`)
	if err := h.AgentSend("hi"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	h.agents.mu.Lock()
	got := h.agents.turnModel
	h.agents.mu.Unlock()
	if got != "p/cheap" {
		t.Fatalf("turn model = %q, want the conversation's pick", got)
	}
}

// TestProviderModelNumbers: a model entry's numeric fields are checked at the
// boundary, so a quoted one is refused here instead of reading as 0 and
// leaving /health to blame a field the user did set.
func TestProviderModelNumbers(t *testing.T) {
	for _, c := range []struct {
		name, models string
		want         int64
		err          string
	}{
		{name: "numbers", models: `{ id = "m", context_window = 200000 }`, want: 200000},
		{name: "a quoted number", models: `{ id = "m", context_window = "200000" }`, err: "models[1].context_window must be a number"},
		{name: "a quoted cost", models: `{ id = "m", pricing = { input = "1.5" } }`, err: "models[1].pricing.input must be a number"},
		{name: "a missing one", models: `{ id = "m" }`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, r := newTestHost(t)
			err := r.ExecuteChunk(t.Context(), `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = { `+c.models+` } })`, "p.lua", lua.OriginUser)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("quoted field: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			spec, ok := h.Providers.Lookup("a")
			if !ok || spec.Models[0].ContextWindow != c.want {
				t.Fatalf("context window = %+v, want %d", spec.Models, c.want)
			}
		})
	}
}

// TestIdleStreamUsesItsModel: between turns the context window and the
// image check follow the conversation's model (models.default); compaction
// and a subagent without a model follow models.default too.
func TestIdleStreamUsesItsModel(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = {
			{ id = "big", context_window = 200000, capabilities = { vision = false } },
			{ id = "small", context_window = 32000 },
		} })
		zemi.model.set("a/big")
	`)
	if s := h.AgentStream(); s.Model != "a/big" || s.CtxWindow != 200000 {
		t.Fatalf("stream model %q window %d, want the conversation's", s.Model, s.CtxWindow)
	}
	if h.PipelineFor().Images() {
		t.Fatal("the conversation's model is text-only")
	}
	runUser(t, r, `assert(zemi.agent.current().model == "a/big")`)
	if id := h.roleModel("compact"); id != "a/big" {
		t.Fatalf("summary model %q, want models.default", id)
	}
	if id := h.subagentModelID(AgentDef{}); id != "a/big" {
		t.Fatalf("subagent model %q, want models.default", id)
	}
}

// TestEffortPicker: /effort lists the levels and the model's own, keys the
// stored pick, and says what each sends on the conversation's model.
func TestEffortPicker(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.Providers.Reset(agent.Catalog{Specs: []agent.Spec{{
		ID: "a", Kind: agent.KindOpenAI, Builtin: true,
		Models: []agent.Model{{ID: "big", Efforts: []string{"low", "high"}, Effort: "high"}},
	}}})
	if err := h.SetModel("a/big"); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("agent.effort", "medium"); err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.OpenEffortPicker(); err != nil {
		t.Fatal(err)
	}
	o := ui.opened[0]
	want := []struct{ text, desc, run, key string }{
		{"low", "sent as low", "effort low", ""},
		{"medium", "sent as low", "effort medium", "current"}, // the model lists no medium
		{"high", "sent as high", "effort high", ""},
		{"default", "the model's own: high", "effort default", ""},
	}
	if len(o.Items) != len(want) || o.Selected != 1 || o.Anchor != OverlayBottomRight {
		t.Fatalf("picker %+v", o)
	}
	for i, w := range want {
		row := o.Items[i]
		if row.Text != w.text || row.Desc != w.desc || row.Run != w.run || row.Tag != w.key {
			t.Errorf("row %d %+v, want %+v", i, row, w)
		}
	}
}

// TestModelPickerOdd: with nothing usable the picker opens by itself, unless
// init.lua failed, and ends with the keyless builtin providers by env var,
// explained only when nothing is usable; a configured id the provider does
// not list is the selected row, which the reload toast also names.
func TestModelPickerOdd(t *testing.T) {
	t.Parallel()
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
	runUser(t, r, `zemi.config.set("models.default", "deepseek/x")`)
	if h.OpenModelPicker("") != nil {
		t.Fatal("picker did not open")
	}
	for _, o := range ui.opened {
		if len(o.Items) != 1 || o.Items[0].Text != "deepseek" || o.Items[0].Copy != "export DEEPSEEK_API_KEY=" || o.Items[0].Run != "keys paste deepseek" || !strings.HasPrefix(o.Body, "No model yet: press enter") {
			t.Fatalf("empty picker: %+v body %q", o.Items, o.Body)
		}
	}
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })
		zemi.config.set("models.default", "a/typo")`)
	if h.OpenModelPicker("") != nil {
		t.Fatal("picker did not open")
	}
	o := ui.opened[2]
	row := o.Items[2]
	if o.Selected != 2 || len(o.Items) != 5 || row.Text != "a/typo" || row.Run != "model a/typo" || row.Tag != "current, not in the list" {
		t.Fatalf("unlisted id: %+v", o.Items)
	}
	// the keyless builtins still close the list, now with paste on enter
	if last := o.Items[4]; last.Text != "deepseek" || last.Run != "keys paste deepseek" || last.Verb != VerbPaste || last.Copy != "export DEEPSEEK_API_KEY=" || o.Body != "" {
		t.Fatalf("missing builtins after the models: %+v body %q", o.Items, o.Body)
	}
	if w := h.modelRow().Detail; !strings.HasPrefix(w, "a/typo: not in the provider's models list") {
		t.Fatalf("warning %q", w)
	}
	if h.OfferModel(); len(ui.opened) != 3 {
		t.Fatal("a model that runs opened the picker")
	}
}

// TestSetModel: /model refuses a provider nobody registered, and an id the
// provider does not list is taken and warned about, mid-turn too.
func TestSetModel(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "main" }, { id = "cheap" } } })
		zemi.config.set("models.default", "a/main")`)
	for _, c := range []struct {
		name    string
		do      func() error
		wantErr string
	}{
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
	} {
		err := c.do()
		if (c.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.wantErr)
		}
	}
}

// TestConcurrentSendsQueue: two sends racing to start a turn run one after
// the other, so the history keeps both messages.
func TestConcurrentSendsQueue(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "ok")
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
	t.Parallel()
	h, _ := newTestHostDB(t)
	run, _ := h.newRun(t.Context(), builtinAgents[0], "d", "p", false)
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
		c.hooks.ToolCallEnd("c0", "read", out(""), "")
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

// TestRunRetryDropsItsThought: the retry of a subagent's step drops the
// thinking the failed attempt streamed, as it drops its text, so the thought
// that settles is the retried step's own.
func TestRunRetryDropsItsThought(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	run, _ := h.newRun(t.Context(), builtinAgents[0], "d", "p", false)
	hk := &subagentHooks{h: h, def: builtinAgents[0], run: run}
	hk.StepStart()
	hk.ReasoningDelta("half a thought")
	hk.ReasoningDelta(" and the rest of it")
	hk.Retrying(errors.New("overloaded"), 3*time.Second)
	run.mu.Lock()
	status := run.status
	run.mu.Unlock()
	if status != RunRunning {
		t.Fatalf("status = %q, want the run kept running through the retry", status)
	}
	_, _, live, _ := run.view(h.AgentStream())
	if !live.Retrying || live.RetryIn <= 0 || live.RetryIn > 3*time.Second || live.Retries != 1 {
		t.Fatalf("view = retrying %v in %v (n=%d), want the retry the hook reported", live.Retrying, live.RetryIn, live.Retries)
	}
	hk.ReasoningDelta("the real one")
	run.mu.Lock()
	run.flush()
	got := slices.Clone(run.tr[1:])
	run.mu.Unlock()
	if len(got) != 1 || got[0].Kind != RowReasoning || got[0].Text != "the real one" {
		t.Fatalf("rows = %+v, want the retried step's thought alone", got)
	}
	if _, _, live, _ := run.view(h.AgentStream()); live.Retrying {
		t.Fatal("a streamed part left the run marked retrying")
	}
}

func TestMaxOutput(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.add({ id = "a", kind = "anthropic", models = { { id = "big" }, { id = "old", max_output = 4096, capabilities = { vision = false } } } })
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

// TestHeadlessToolsetDropsPrompts: with no UI hooks nobody can answer a
// question or approve a plan, so those tools never ship.
func TestHeadlessToolsetDropsPrompts(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	names := func() map[string]bool {
		out := map[string]bool{}
		toolset := h.mainTools()
		for _, tool := range toolset {
			out[tool.Info().Name] = true
		}
		return out
	}
	for name := range headlessExcluded {
		if names()[string(name)] {
			t.Errorf("the toolset still ships %q", name)
		}
	}

	h.SetUIHooks(&fakeUI{})
	for name := range headlessExcluded {
		if !names()[string(name)] {
			t.Errorf("the TUI toolset lost %q", name)
		}
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
	if h.roleModel("default") != "a/big" || h.roleModel("agents") != "a/big" || !h.Providers.HasKey("a") {
		t.Fatalf("catalog defaults: %s %s", h.roleModel("default"), h.roleModel("agents"))
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
	if h.roleModel("default") != "a/m" || h.roleModel("agents") != "a/m" {
		t.Fatalf("user defaults: %s %s", h.roleModel("default"), h.roleModel("agents"))
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
	if err := r.ExecuteChunk(t.Context(), `zemi.provider.del("a")`, "p.lua", lua.OriginProject); err != nil {
		t.Fatalf("project provider del: %v", err)
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
	t.Parallel()
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

	billed, _ := h.DB.GetSession(store.MainSessionID)
	answer, err := h.btw(t.Context(), "what failed?")
	if err != nil || answer != "the build" {
		t.Fatalf("btw = %q, %v", answer, err)
	}
	after, _ := h.DB.GetSession(store.MainSessionID)
	if todos, _ := h.DB.Todos(store.MainSessionID); len(todos) > 0 {
		t.Fatalf("a side question ran a tool: %+v", todos)
	}
	if after.CompletionTokens != billed.CompletionTokens+7 {
		t.Fatalf("completion tokens %d -> %d, want the aside's 7 on the anchor row", billed.CompletionTokens, after.CompletionTokens)
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
		if label, cancellable := h.BusyNow(); label != "Answering the side question" || !cancellable {
			return fmt.Errorf("busy %q %v", label, cancellable)
		}
		return nil
	})
	h.CancelBusy()
	waitFor(t, func() error {
		if label, _ := h.BusyNow(); label != "" {
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
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := replyStub(t, h, "fixing the build;\n\n  next: run  make check\nignored")
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
		{"off in config", func() { _, _ = h.Settings.SetQuiet("session.recap", "0") }, ""},
		{"a reply-only turn is its own recap", func() { _, _ = h.Settings.SetQuiet("session.recap", "2m"); turn(false)() }, ""},
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
	t.Parallel()
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
		hk.ToolCallEnd("t1", "grep", out(""), "")
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
			run, _ := h.newRun(t.Context(), builtinAgents[0], "d", "p", false)
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

// TestTypedSendMarksOnlyAQueuedMessage: the handoff signal belongs to a
// message that joined a running turn (Host.AgentSendUser). One that starts a
// turn marks nothing, so a foreground call in that turn waits as usual - which
// is what the composer's send does every time it is idle.
func TestTypedSendMarksOnlyAQueuedMessage(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	gate := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(string, string, bool) []fantasy.StreamPart {
			<-gate // the turn holds until the test releases it
			return textParts("ok")
		}}, nil
	}
	if err := h.AgentSendUser("start a turn"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		ag := h.agents
		ag.mu.Lock()
		defer ag.mu.Unlock()
		if !ag.active {
			return errors.New("the turn never started")
		}
		return nil
	})
	select {
	case <-h.userSpoke():
		t.Fatal("a message that started the turn marked it: a foreground call hands off at once")
	default:
	}
	if err := h.AgentSendUser("and this one joins it"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.userSpoke():
	default:
		t.Fatal("a message that joined the turn did not mark it")
	}
	close(gate)
	waitIdle(t, h)
}

// TestFlushKeepsJob: a shell row that settles keeps the job pointer its live
// row carried, so the chat still opens the job after the turn ends.
func TestFlushKeepsJob(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SeedStream(StreamSnapshot{Active: true, Tools: []StreamTool{
		{ID: "t1", Name: "shell", Input: "make build", Status: CallOK, Job: "job-1", Output: "still running"},
	}})
	h.agents.mu.Lock()
	h.flushOutput(TurnStat{})
	h.agents.mu.Unlock()
	last := h.Session().Transcript[len(h.Session().Transcript)-1]
	if last.Kind != RowTool || last.Job != "job-1" {
		t.Fatalf("settled row = %+v, want the job pointer kept", last)
	}
}

// TestInterruptedQueueDrains: esc stops the turn and leaves the reports
// queued; the drain works them after idleResumeWait, as a clean end would
// have, and an interrupt with nothing queued arms nothing.
func TestInterruptedQueueDrains(t *testing.T) {
	t.Parallel()
	h, clock := newLoopHost(t)
	replyStub(t, h, "ok")
	report := agent.AttachMarker + "\n<agent-report run=\"a1\" agent=\"explore\" status=\"finished\" description=\"find x\">\nfound\n</agent-report>\n</attached-files>"
	h.agents.mu.Lock()
	scope := h.claim()
	h.agents.mu.Unlock()
	if err := h.AgentSend(report); err != nil {
		t.Fatal(err)
	}
	scope.cancel()
	h.nextTurn(scope, nil)
	h.agents.mu.Lock()
	n := len(h.agents.steering)
	h.agents.mu.Unlock()
	if n != 1 || clock.armed() != 1 {
		t.Fatalf("queue %d, timers %d; want the report queued and the drain armed", n, clock.armed())
	}
	f, ok := clock.due(clock.Now().Add(idleResumeWait))
	if !ok {
		t.Fatal("no drain timer due")
	}
	f()
	waitFor(t, func() error {
		h.agents.mu.Lock()
		defer h.agents.mu.Unlock()
		if len(h.agents.steering) != 0 {
			return errors.New("the queue did not drain")
		}
		return nil
	})
	waitIdle(t, h)
	tr := h.Session().Transcript
	if len(tr) == 0 || tr[len(tr)-1].Role != fantasy.MessageRoleAssistant {
		t.Fatalf("the drain ran no turn: %+v", tr)
	}
	// with nothing queued, an interrupt arms nothing
	h.agents.mu.Lock()
	scope = h.claim()
	h.agents.mu.Unlock()
	scope.cancel()
	h.nextTurn(scope, nil)
	if n := clock.armed(); n != 0 {
		t.Fatalf("%d timers armed with an empty queue", n)
	}
}

// TestInterruptHandsTheForegroundRunOff: esc interrupts the turn a foreground
// agent call waits in, not the run - the run detaches the way one a message
// took does, keeps working, and its report lands in the conversation.
func TestInterruptHandsTheForegroundRunOff(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{}) // a detached run needs the TUI: headless it runs inline
	release := make(chan struct{})
	var calls atomic.Int64
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				<-release
				return textParts("found the thing")
			case toolResult:
				return textParts("noted")
			case calls.Add(1) == 1:
				return toolCallParts("c1", "agent", `{"description":"slow dig","prompt":"find it","agent":"explore"}`)
			}
			return textParts("done")
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if n := h.tasks.runningFor(h.AgentSessionID()); n != 1 {
			return fmt.Errorf("the foreground run is not going: %d", n)
		}
		return nil
	})
	h.AgentCancel() // the UI interrupt's call
	close(release)
	waitFor(t, func() error {
		if n := h.tasks.runningFor(h.AgentSessionID()); n != 0 {
			return fmt.Errorf("the run did not finish after its handoff: %d running", n)
		}
		for _, m := range h.Session().Transcript {
			if m.Kind == "notice" && m.Run != "" && strings.Contains(m.Text, "found the thing") {
				return nil
			}
		}
		return fmt.Errorf("the detached run's report never landed: %+v", h.Session().Transcript)
	})
}
