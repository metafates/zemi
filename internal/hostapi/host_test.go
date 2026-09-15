package hostapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/logging"
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

// TestNamespaceVersion pins the frozen API surface: the zemi root and the
// boot-installed namespaces resolve from user chunks, and zemi.api reports
// the M0 version.
func TestNamespaceVersion(t *testing.T) {
	_, r := newTestHost(t)
	runUser(t, r, `
		assert(type(zemi) == "table", "zemi root missing")
		assert(type(zemi.api) == "table", "zemi.api missing")
		assert(zemi.api.version == "0.1.0-m1", "version=" .. tostring(zemi.api.version))
		assert(type(zemi.config.get) == "function", "zemi.config.get missing")
		assert(type(zemi.config.set) == "function", "zemi.config.set missing")
		assert(type(zemi.keymap.bind) == "function", "zemi.keymap.bind missing")
		assert(type(zemi.command.add) == "function", "zemi.command.add missing")
		assert(type(zemi.event.emit) == "function", "zemi.event.emit missing")
		assert(type(zemi.log) == "table", "zemi.log missing")
		assert(type(zemi.notify) == "table", "zemi.notify missing")
		assert(type(zemi.ui.theme.set) == "function", "zemi.ui.theme.set missing")
	`)
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

// TestKeymapBindFromLua covers bind-time RHS resolution: function RHS lands
// in the registry with the caller's origin, string RHS must be a known
// command or host action, the Go-reserved action is refused, and <leader>
// expands to the space prefix.
func TestKeymapBindFromLua(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.keymap.bind("normal", "gm", function() end)
		zemi.keymap.bind("normal", "<leader>x", function() end)
		local ok, err = pcall(zemi.keymap.bind, "normal", "r1", "zemi.reload")
		assert(not ok, "Go-reserved rhs accepted")
		assert(tostring(err):find("Go-reserved", 1, true) ~= nil, "err=" .. tostring(err))
		local ok2, err2 = pcall(zemi.keymap.bind, "normal", "r2", "zemi.nope")
		assert(not ok2, "unknown rhs accepted")
		assert(tostring(err2):find("neither a command nor a host action", 1, true) ~= nil,
			"err2=" .. tostring(err2))
	`)
	b := h.Keymaps.Lookup("normal", "gm")
	if b == nil {
		t.Fatal("gm binding missing from the registry")
	}
	if b.Origin != lua.OriginUser {
		t.Fatalf("gm binding origin = %v, want user", b.Origin)
	}
	if h.Keymaps.Lookup("normal", "spacex") == nil {
		t.Fatal("<leader>x did not expand to spacex")
	}
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
				zemi.keymap.bind("normal", "zz", function() end)
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
	if h.Keymaps.Lookup("normal", "zz") != nil {
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

	runUser(t, r, `
		assert(zemi.api.version == "0.1.0-m1", "interpreter wedged after veto timeout")
	`)
}

// TestConfigSetGating pins the config write gate: after the boot window
// closes, autonomy knobs are refused everywhere, non-writable keys are
// refused, and a writable key applies to the store.
func TestConfigSetGating(t *testing.T) {
	h, r := newTestHost(t)
	h.Settings.CloseBootWindow()
	runUser(t, r, `
		local ok, err = pcall(zemi.config.set, "skills.autonomy", "yolo")
		assert(not ok, "autonomy knob accepted after boot")
		assert(tostring(err):find("autonomy knob", 1, true) ~= nil, "err=" .. tostring(err))

		local ok2, err2 = pcall(zemi.config.set, "reflector.every_n_sessions", 5)
		assert(not ok2, "non-writable key accepted")
		assert(tostring(err2):find("not writable", 1, true) ~= nil, "err2=" .. tostring(err2))

		zemi.config.set("ui.sidebar.width", 42)
	`)
	if v, ok := h.Settings.Get("ui.sidebar.width"); !ok || v != float64(42) {
		t.Fatalf("ui.sidebar.width = %v, want 42", v)
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
		zemi.config.watch("ui.sidebar.open", function(nv, ov)
			assert(ov == false, "old=" .. tostring(ov))
			assert(nv == true, "new=" .. tostring(nv))
			zemi.config.set("ui.theme", "watched")
		end)
		zemi.config.set("ui.sidebar.open", true)
	`)
	if v, ok := h.Settings.Get("ui.theme"); !ok || v != "watched" {
		t.Fatalf("watch callback side effect missing: ui.theme = %v, want watched", v)
	}
}

// TestToggleSidebarFromLua exercises the interpreter-side toggle path: the
// Lua closure passes the LState, so the fired watch is delivered inline on
// the same goroutine instead of round-tripping the queue.
func TestToggleSidebarFromLua(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.config.watch("ui.sidebar.open", function(nv, ov)
			assert(nv == true, "new=" .. tostring(nv))
			zemi.config.set("ui.theme", "toggled")
		end)
		zemi.ui.toggle_sidebar()
	`)
	if !h.SidebarOpen() {
		t.Fatal("sidebar did not open")
	}
	if v, _ := h.Settings.Get("ui.theme"); v != "toggled" {
		t.Fatalf("toggle watch side effect missing: ui.theme = %v", v)
	}
}
