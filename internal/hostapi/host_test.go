package hostapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	glua "github.com/yuin/gopher-lua"
)

// newTestHost boots a fresh runtime with the full host API installed.
func newTestHost(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// runUser executes src as a user-origin chunk and fails the test on error.
// The select guard turns an interpreter deadlock into a loud failure
// instead of a hung test binary (the watch-delivery regression in
// TestConfigWatchNoDeadlock used to wedge the queue permanently).
func runUser(t *testing.T, r *lua.Runtime, src string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.ExecuteChunk(ctx, src, "test.lua", lua.OriginUser) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("user chunk: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("user chunk did not complete within 3s (interpreter deadlock?)")
	}
}

// waitFor polls cond until it returns nil and fails the test with its last
// error after 3s.
func waitFor(t *testing.T, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := cond()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNamespaceVersion pins the frozen API surface: the zemi root and the
// boot-installed namespaces resolve from user chunks, and zemi.api reports
// the pinned version.
func TestNamespaceVersion(t *testing.T) {
	_, r := newTestHost(t)
	runUser(t, r, `
		assert(type(zemi) == "table", "zemi root missing")
		assert(type(zemi.api) == "table", "zemi.api missing")
		assert(zemi.api.version == "0.20.0", "version=" .. tostring(zemi.api.version))
		assert(type(zemi.event.log) == "function", "zemi.event.log missing")
		assert(type(zemi.event.stats) == "function", "zemi.event.stats missing")
		assert(type(zemi.notify.list) == "function", "zemi.notify.list missing")
		assert(type(zemi.health.register) == "function", "zemi.health.register missing")
		assert(type(zemi.errors.list) == "function", "zemi.errors.list missing")
		assert(type(zemi.config.reload) == "function", "zemi.config.reload missing")
		assert(zemi.provider.kind.OPENAI_COMPAT == "openai_compat" and zemi.provider.kind.ANTHROPIC == "anthropic" and zemi.provider.kind.OPENAI == "openai", "zemi.provider.kind missing")
		for _, fn in ipairs({ "add", "remove", "list" }) do
			assert(type(zemi.permission.mode[fn]) == "function", "zemi.permission.mode." .. fn .. " missing")
		end
		assert(type(zemi.config.get) == "function", "zemi.config.get missing")
		assert(type(zemi.config.set) == "function", "zemi.config.set missing")
		for _, fn in ipairs({ "set", "del", "get", "list" }) do
			assert(type(zemi.keymap[fn]) == "function", "zemi.keymap." .. fn .. " missing")
		end
		assert(type(zemi.command.add) == "function", "zemi.command.add missing")
		assert(type(zemi.event.emit) == "function", "zemi.event.emit missing")
		assert(type(zemi.log) == "table", "zemi.log missing")
		assert(type(zemi.notify) == "table", "zemi.notify missing")
		assert(type(zemi.ui.theme.set) == "function", "zemi.ui.theme.set missing")
		assert(type(zemi.ui.markdown) == "function", "zemi.ui.markdown missing")
		assert(type(zemi.ui.select) == "function", "zemi.ui.select missing")
		assert(type(zemi.ui.input) == "function", "zemi.ui.input missing")
		assert(type(zemi.ui.hl) == "function", "zemi.ui.hl missing")
		local writable = {}
		for _, s in ipairs(zemi.config.schema()) do writable[s.path] = s.writable end
		assert(writable["ui.theme"] == true, "ui.theme must be writable")
		assert(writable["memory.autonomy"] == false, "autonomy knobs must not look writable")
		assert(type(zemi.skills.list) == "function", "zemi.skills.list missing")
		assert(type(zemi.skills.enable) == "function", "zemi.skills.enable missing")
		assert(type(zemi.skills.disable) == "function", "zemi.skills.disable missing")
		assert(type(zemi.learn.proposals) == "function", "zemi.learn.proposals missing")
		assert(type(zemi.learn.decide) == "function", "zemi.learn.decide missing")
		for _, fn in ipairs({ "encode", "decode" }) do
			assert(type(zemi.json[fn]) == "function", "zemi.json." .. fn .. " missing")
		end
		for _, fn in ipairs({ "get", "set", "insert", "submit" }) do
			assert(type(zemi.composer[fn]) == "function", "zemi.composer." .. fn .. " missing")
		end
		assert(type(zemi.event.off) == "function", "zemi.event.off missing")
		assert(type(zemi.http.request) == "function", "zemi.http.request missing")
		for _, fn in ipairs({ "del", "run" }) do
			assert(type(zemi.command[fn]) == "function", "zemi.command." .. fn .. " missing")
		end
		for _, fn in ipairs({ "list", "stat", "exists", "mkdir", "remove" }) do
			assert(type(zemi.fs[fn]) == "function", "zemi.fs." .. fn .. " missing")
		end
		assert(type(zemi.mcp.add) == "function" and type(zemi.mcp.list) == "function", "zemi.mcp missing")
		assert(type(zemi.lsp.add) == "function" and type(zemi.lsp.list) == "function" and type(zemi.lsp.disable) == "function", "zemi.lsp missing")
		for _, fn in ipairs({ "list", "new", "open", "todos", "rewind" }) do
			assert(type(zemi.session[fn]) == "function", "zemi.session." .. fn .. " missing")
		end
		for _, fn in ipairs({ "run", "list" }) do
			assert(type(zemi.agent[fn]) == "function", "zemi.agent." .. fn .. " missing")
			assert(type(zemi.workflow[fn]) == "function", "zemi.workflow." .. fn .. " missing")
		end
	`)
}

// TestReloadDuringBootRefused: config calling zemi.config.reload() while it
// is loading would replay the very steps it runs in, over and over.
func TestReloadDuringBootRefused(t *testing.T) {
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	// A boot step is where config code runs. It cannot go through the queue
	// here: boot starts the dispatcher only after the steps, so a chunk run
	// from inside one would wait on a loop that has not started.
	cfg := lua.BootStep{Name: "config", Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
		if err := h.ReloadConfig(); err == nil {
			t.Error("a reload asked for while config loads must be refused")
		}
		return nil
	}}
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), cfg}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if ui.reloads != 0 {
		t.Fatalf("boot dispatched %d reloads", ui.reloads)
	}
	if err := h.ReloadConfig(); err != nil { // once boot is done it works
		t.Fatalf("reload after boot: %v", err)
	}
	if ui.reloads != 1 {
		t.Fatalf("reloads=%d, want 1", ui.reloads)
	}
}

// TestErrorsAndReloadFromLua: the agent reads zemi's own failures and
// applies config it just wrote, both through namespaces the lua tool can
// reach, so debugging zemi does not depend on the user reading the screen.
func TestErrorsAndReloadFromLua(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.RecordError("boot", `init.lua:1: zemi.provider.register: provider "x" already registered`)
	runUser(t, r, `
		local rows = zemi.errors.list()
		assert(#rows == 1, "rows=" .. #rows)
		assert(rows[1].source == "boot", "source=" .. tostring(rows[1].source))
		assert(rows[1].message:find("already registered", 1, true) ~= nil, "message=" .. tostring(rows[1].message))
		zemi.config.reload()
	`)
	if ui.reloads != 1 {
		t.Fatalf("reload dispatched %d times, want 1", ui.reloads)
	}
}

// TestOverlayFromRetiredGenerationIsDropped: an overlay still open across a
// reload must not run its on_select, which belongs to the retired LState
// (DESIGN §4).
func TestOverlayFromRetiredGenerationIsDropped(t *testing.T) {
	h, r := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)

	called := false
	var onSelect *glua.LFunction
	if _, err := r.Do(context.Background(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		onSelect = L.NewFunction(func(*glua.LState) int { called = true; return 0 })
		return nil, nil
	}); err != nil {
		t.Fatalf("build callback: %v", err)
	}
	o := Overlay{ID: "x", Origin: lua.OriginUser, Gen: h.currentGen(), OnSelect: onSelect, Items: []any{"a"}}

	h.SelectOverlay(context.Background(), o, 0)
	if !called {
		t.Fatal("a live overlay did not run on_select")
	}

	called = false
	if err := r.ForceReload(context.Background(), []lua.BootStep{h.NamespacesStep()}, h.PurgeGeneration); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	h.SelectOverlay(context.Background(), o, 0)
	if called {
		t.Fatal("a stale overlay ran a retired generation's on_select")
	}
	// The UI closes the overlay before the callback, which may reopen the id.
	if len(ui.closed) != 0 {
		t.Fatalf("SelectOverlay closed %v after on_select", ui.closed)
	}
}

// TestOverlayItemsRenderOnOpen: render_item and format_item run when the
// overlay opens, so the UI never calls Lua to draw it.
func TestOverlayItemsRenderOnOpen(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `
		zemi.ui.overlay.open({ id = "o", items = { "a", "b" }, render_item = function(s)
			if s == "b" then error("boom") end
			return s:upper()
		end })
		zemi.ui.select({ 1 }, { format_item = function(n) return "#" .. n end }, function() end)
	`)
	var got []string
	for _, o := range ui.opened {
		got = append(got, strings.Join(o.ItemText, ","))
	}
	if strings.Join(got, ";") != "A,b;#1" {
		t.Fatalf("rendered items: %q", got)
	}
}

// TestEventRoundTrip covers the user.* event path end to end: register from
// Lua, emit from Lua, payload attribution (from = emitting origin), and the
// host-owned-prefix emit refusal.
func TestEventRoundTrip(t *testing.T) {
	_, r := newTestHost(t)
	runUser(t, r, `
		captured = nil
		zemi.event.on("user.ping", function(ev) captured = ev end)
		zemi.event.emit("user.ping", {x = 1})
		assert(captured ~= nil, "handler did not fire")
		assert(captured.name == "user.ping", "name=" .. tostring(captured.name))
		assert(captured.from == "user", "from=" .. tostring(captured.from))
		assert(captured.x == 1, "x=" .. tostring(captured.x))
	`)
	runUser(t, r, `
		local ok, err = pcall(zemi.event.emit, "app.ready", {})
		assert(not ok, "host-owned event emitted from Lua")
		assert(tostring(err):find("only user.", 1, true) ~= nil, "err=" .. tostring(err))
	`)
}

// TestKeymapFromLua covers set/get/del/list: a function action lands with
// the caller's origin, string actions must be a known command or host
// action (the Go-reserved one is refused), key names normalize, and user
// config can remove a shipped default.
func TestKeymapFromLua(t *testing.T) {
	h, r := newTestHost(t)
	if err := h.Keymaps.Set(&KeyBinding{Key: "tab", RHS: RHS{API: "zemi.ui.help"}, Origin: lua.OriginBuiltin}); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `
		zemi.keymap.set("Ctrl+G", function() end, { desc = "go" })
		local b = zemi.keymap.get("ctrl+g")
		assert(b and b.desc == "go" and b.origin == "user" and b.action == "<function>", "get after set")
		local ok, err = pcall(zemi.keymap.set, "f1", "zemi.reload")
		assert(not ok and tostring(err):find("Go-reserved", 1, true), "err=" .. tostring(err))
		ok, err = pcall(zemi.keymap.set, "f2", "zemi.nope")
		assert(not ok and tostring(err):find("neither a command nor a host action", 1, true), "err=" .. tostring(err))
		assert(#zemi.keymap.list() == 2, "list=" .. #zemi.keymap.list())
		zemi.keymap.del("tab")
		assert(zemi.keymap.get("tab") == nil, "user could not remove a shipped key")
		assert(not pcall(zemi.keymap.del, "tab"), "deleting an unbound key must raise")
	`)
	if b := h.Keymaps.Lookup("ctrl+g"); b == nil || b.Origin != lua.OriginUser {
		t.Fatalf("ctrl+g = %+v", b)
	}
}

// TestRenderGuardOnlyWhileRendering: a render queued behind other Lua does
// not make that Lua look like render code (project config registering a
// widget during the trust prompt was refused).
func TestRenderGuardOnlyWhileRendering(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.ui.widget.register({ id = "w", region = "header", render = function() return {} end })`)
	w := h.Widgets.List()[0]
	rendered := make(chan struct{})
	if _, err := r.Do(context.Background(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		go func() {
			defer close(rendered)
			h.RenderWidget(context.Background(), w, 40, UIState{})
		}()
		time.Sleep(50 * time.Millisecond) // the render is now queued behind this request
		h.renderMu.Lock()
		defer h.renderMu.Unlock()
		if h.renderSize != 0 {
			t.Error("render guard raised while the render only waited in the queue")
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	<-rendered
}

// TestRenderRefusesMutations pins the render-purity guard (DESIGN §6.6):
// a widget render that mutates state fails with the guard error and leaves
// no side effects, while a clean render comes back sanitized.
func TestRenderRefusesMutations(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.ui.widget.register({
			id = "mutating", region = "header",
			render = function(ctx)
				zemi.keymap.set("zz", function() end)
				return {}
			end,
		})
		zemi.ui.widget.register({
			id = "clean", region = "header",
			render = function(ctx)
				return {{{text = "a\27[31mb", hl = "Comment"}}}
			end,
		})
	`)
	var mutating, clean *Widget
	for _, w := range h.Widgets.List() {
		switch w.ID {
		case "mutating":
			mutating = w
		case "clean":
			clean = w
		}
	}
	if mutating == nil || clean == nil {
		t.Fatalf("widgets not registered (mutating=%v clean=%v)", mutating, clean)
	}

	if _, err := h.RenderWidget(context.Background(), mutating, 40, UIState{}); err == nil ||
		!strings.Contains(err.Error(), "not allowed during widget render") {
		t.Fatalf("mutation during render: err = %v", err)
	}
	if h.Keymaps.Lookup("zz") != nil {
		t.Fatal("refused bind still landed in the keymap registry")
	}

	lines, err := h.RenderWidget(context.Background(), clean, 40, UIState{})
	if err != nil {
		t.Fatalf("clean render: %v", err)
	}
	if len(lines) != 1 || len(lines[0]) != 1 {
		t.Fatalf("clean render shape = %#v", lines)
	}
	seg := lines[0][0]
	if seg.Text != "ab" || strings.ContainsRune(seg.Text, '\x1b') {
		t.Fatalf("segment text = %q (escape not sanitized)", seg.Text)
	}
	if seg.HL != "Comment" {
		t.Fatalf("segment hl = %q, want Comment", seg.HL)
	}
}

// TestEmitVetoable covers the veto contract (DESIGN §4): a deny decision
// aggregates with its reason, a wedged handler times out at the veto
// deadline without wedging the interpreter, and the host default applies.
func TestEmitVetoable(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.event.on("composer.submit", function(ev)
			return {decision = "deny", reason = "no"}
		end)
	`)
	res := h.EmitVetoable(context.Background(), "composer.submit", nil, lua.VetoDeadline)
	if !res.Denied || !res.Handled {
		t.Fatalf("veto result = %+v, want Denied/Handled", res)
	}
	if res.Reason != "no" {
		t.Fatalf("veto reason = %q, want no", res.Reason)
	}
	if res.TimedOut {
		t.Fatal("deny path must not report TimedOut")
	}

	runUser(t, r, `
		zemi.event.on("tool.pre", function(ev) while true do end end)
	`)
	start := time.Now()
	res2 := h.EmitVetoable(context.Background(), "tool.pre", nil, lua.VetoDeadline)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("veto deadline not honored: %s", elapsed)
	}
	if !res2.TimedOut {
		t.Fatalf("wedged handler result = %+v, want TimedOut", res2)
	}
	if res2.Denied {
		t.Fatal("timed-out handler must not deny")
	}

	// the point is that the interpreter still answers, not which version it
	// reports: TestNamespaceVersion pins the string
	runUser(t, r, `
		assert(type(zemi.api.version) == "string", "interpreter wedged after veto timeout")
	`)
}

// TestConfigSetGating pins the config write gate: after the boot window
// closes, autonomy knobs are refused everywhere, non-writable keys are
// refused, and a writable key applies to the store.
func TestConfigSetGating(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		local ok, err = pcall(zemi.config.set, "memory.autonomy", "auto")
		assert(not ok, "autonomy knob accepted")
		assert(tostring(err):find("not writable", 1, true) ~= nil, "err=" .. tostring(err))

		local ok2, err2 = pcall(zemi.config.set, "reflector.every_n_sessions", 5)
		assert(not ok2, "non-writable key accepted")
		assert(tostring(err2):find("not writable", 1, true) ~= nil, "err2=" .. tostring(err2))

		zemi.config.set("ui.composer.history", 42)
	`)
	if v, ok := h.Settings.Get("ui.composer.history"); !ok || v != 42 {
		t.Fatalf("ui.composer.history = %v, want 42", v)
	}
}

// TestConfigWatchNoDeadlock is the regression for the watch-delivery
// deadlock: a Lua config.set inside a running interpreter request used to
// deliver watches via a blocking RT.Do on the same (busy) queue, wedging
// the interpreter forever. The set must deliver the watch inline, the
// callback must run attributed to its registration origin (its own
// zemi.config.set would otherwise be refused), and the chunk must finish.
func TestConfigWatchNoDeadlock(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.config.watch("ui.completion.files", function(nv, ov)
			assert(ov == true, "old=" .. tostring(ov))
			assert(nv == false, "new=" .. tostring(nv))
			zemi.config.set("ui.theme", "watched")
		end)
		zemi.config.set("ui.completion.files", false)
	`)
	if v, ok := h.Settings.Get("ui.theme"); !ok || v != "watched" {
		t.Fatalf("watch callback side effect missing: ui.theme = %v, want watched", v)
	}
}

func TestModelPickerFiresWatches(t *testing.T) {
	h, r := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{pickID: "p/m", pickOK: true})
	runUser(t, r, `zemi.config.watch("agent.model", function(nv) zemi.state.put("picked", nv) end)`)
	check := func(want string) {
		t.Helper()
		if v, _ := h.GlobalDB.GetKV("picked"); v != `"`+want+`"` {
			t.Fatalf("agent.model watch saw %q, want %q", v, want)
		}
	}
	h.pickAndStoreModel()
	check("p/m")
	h.SetUIHooks(&fakeUI{pickID: "p/n", pickOK: true})
	runUser(t, r, `zemi.model.pick()`)
	check("p/n")
	runUser(t, r, `zemi.provider.register({ id = "p", kind = "openai_compat", base_url = "http://x", api_key = "k", models = { { id = "o" } } })
		zemi.model.set("p/o")`)
	check("p/o")
}

// TestEventLogFromLua: emissions are logged and counted with no handler
// registered, and notifications are listed, newest first.
func TestEventLogFromLua(t *testing.T) {
	h, r := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	h.EmitAsync("agent.text_delta", map[string]any{"text": "x"})
	h.Notify("info", "first", "one")
	runUser(t, r, `
		zemi.event.emit("user.ping", { n = 1 })
		zemi.event.emit("user.ping", { n = 2 })
		zemi.notify({ title = "second", body = "two", level = "error" })
		zemi.notify({ title = "third", body = "three" })
		zemi.notify.warn("four", "fourth")
		local log = zemi.event.log({ name = "user.ping" })
		assert(#log == 2, "log=" .. #log)
		assert(log[1].n == 2 and log[1].from == "user" and log[1].at:match("^%d%d:%d%d:%d%d$"), "newest first: " .. tostring(log[1].n))
		assert(#zemi.event.log({ limit = 1 }) == 1, "limit")
		local stats = zemi.event.stats()
		assert(stats["user.ping"] == 2, "ping=" .. tostring(stats["user.ping"]))
		assert(stats["agent.text_delta"] == 1, "delta counted")
		assert(#zemi.event.log({ name = "agent.text_delta" }) == 0, "delta not logged")
		local notes = zemi.notify.list()
		assert(#notes == 4 and notes[4].level == "info" and notes[3].level == "ERROR", "notes=" .. #notes)
		assert(notes[2].title == "third" and notes[2].level == "INFO", "level stuck: " .. notes[2].level)
		assert(notes[1].title == "fourth" and notes[1].body == "four" and notes[1].level == "WARN", "string form")
	`)
}

// Sessions belong to the per-project store and user memories to the
// global one (DESIGN §7); the two open together and are easy to swap.
func TestStoresByScope(t *testing.T) {
	h, _ := newTestHostDB(t)
	if h.memoryDB("user") != h.GlobalDB || h.memoryDB("project") != h.DB {
		t.Fatal("memory scope maps to the wrong store")
	}
	if err := h.DB.CreateSession("s1", "", "t", ""); err != nil {
		t.Fatal(err)
	}
	pdb, err := store.Open(store.ProjectDBPath(h.Dirs.StateHome, h.Dirs.ProjectDir))
	if err != nil {
		t.Fatal(err)
	}
	defer pdb.Close()
	if _, err := pdb.GetSession("s1"); err != nil {
		t.Fatalf("session not in the project store: %v", err)
	}
}
