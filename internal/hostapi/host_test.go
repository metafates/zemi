package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// newTestHost boots a fresh runtime with the full host API installed.
func newTestHost(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir(), DataHome: t.TempDir()})
	t.Cleanup(h.Close) // the actor goroutine pins the Lua state: release both
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// newTestHostUI is newTestHost with a state dir; a clock given moves only
// when the test moves it.
func newTestHostUI(t *testing.T, clock ...lua.Clock) *Host {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, RuntimeFS: zemiruntime.FS, Clock: cmp.Or(clock...)})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir()})
	h.ReleaseBuilder() // tests stub the model and boot no config
	t.Cleanup(h.Close)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	return h
}

// out is a tool result as the stream hook receives it: the row's mark reads
// this type, so a test builds one the way fantasy does.
func out(s string) fantasy.ToolResultOutputContent {
	return fantasy.ToolResultOutputContentText{Text: s}
}

// outErr is a call that answered an error: a tool that did not do what it
// was asked answers with one.
func outErr(s string) fantasy.ToolResultOutputContent {
	return fantasy.ToolResultOutputContentError{Error: errors.New(s)}
}

// TestToolStatus: the mark comes from the outcome the tool reported and the
// result's own type, never from the result's words: a call that refused, or
// a job_read of a job that died, draws a failure and never a check.
func TestToolStatus(t *testing.T) {
	for _, c := range []struct {
		name      string
		result    fantasy.ToolResultOutputContent
		cancelled bool
		outcome   tools.Outcome
		want      CallStatus
	}{
		{"a finished call", out("package a"), false, tools.Outcome{}, CallOK},
		{"a failed call", outErr("boom: exit status 1"), false, tools.Outcome{}, CallErr},
		{"a job that died", outErr("job-3 failed: exit status 1"), false, tools.Outcome{}, CallErr},
		{"an ask the user denied", outErr("permission denied (user): fs.write /p/b.go"), false, tools.Outcome{Stopped: true}, CallStopped},
		{"a question the user dismissed", outErr("The user dismissed the questions. Stop and wait for them to say how to proceed."), false, tools.Outcome{Stopped: true}, CallStopped},
		{"an interrupted turn", outErr("context canceled"), true, tools.Outcome{}, CallStopped},
		{"a background agent", out("run=1"), false, tools.Outcome{Run: "1"}, CallBG},
		{"a shell that handed off", out("still running"), false, tools.Outcome{Job: "job-3"}, CallBG},
		{"a job that started", out("started job-2 (npm run dev)"), false, tools.Outcome{Job: "job-2"}, CallBG},
		{"a workflow that warn-finished", out("finished"), false, tools.Outcome{Warn: true}, CallWarn},
		// the words alone decide nothing: a result naming a job is no hand-off
		{"a shell that named a job but exited", out(tools.JobHandoff + "job-3"), false, tools.Outcome{}, CallOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := toolStatus(c.result, c.cancelled, c.outcome); got != c.want {
				t.Fatalf("toolStatus = %s, want %s", got, c.want)
			}
		})
	}
}

// outsideTemp is a directory that is none of zemi's roots - not the project,
// not the pad, not a data dir - for a fixture whose point is a path outside
// them. t.TempDir() alone would do, except a test may chdir into the project,
// so it sits beside this file instead of in the working directory.
func outsideTemp(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir, err := os.MkdirTemp(filepath.Dir(file), ".zemi-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return perm.RealPath(dir)
}

// runChunk runs src as a user chunk off the test goroutine and returns its
// values and error: a slice gets 30s, not the interpreter's own ceiling, which
// a loaded machine reaches on a chunk that starts processes. The select guard
// turns an interpreter deadlock into a loud failure instead of a hung test
// binary.
func runChunk(t *testing.T, r *lua.Runtime, src string) ([]any, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type chunk struct {
		vals []any
		err  error
	}
	done := make(chan chunk, 1)
	go func() {
		vals, err := r.Eval(ctx, src, "test.lua", lua.OriginUser, 30*time.Second, nil)
		done <- chunk{vals: vals, err: err}
	}()
	select {
	case c := <-done:
		return c.vals, c.err
	case <-time.After(30 * time.Second):
		t.Fatal("chunk did not complete within 30s (interpreter deadlock?)")
		return nil, nil
	}
}

// runUser executes src as a user-origin chunk and fails the test on error.
func runUser(t *testing.T, r *lua.Runtime, src string) {
	t.Helper()
	if _, err := runChunk(t, r, src); err != nil {
		t.Fatalf("user chunk: %v", err)
	}
}

// askEverything registers the ask-everything permission policy a plugin's
// tool.pre handler makes: every action routes to the overlay.
// _askEverythingOff turns it off again.
const _askEverythingOff = `zemi.event.off("ask-everything")`

func askEverything(t *testing.T, r *lua.Runtime) {
	t.Helper()
	runUser(t, r, `zemi.event.on("tool.pre", function() return { decision = "ask" } end, { id = "ask-everything" })`)
}

// waitFor polls cond until it returns nil and fails the test with its last
// error after 10s: parallel hosts on a loaded machine stretch every
// transition past a shorter deadline.
func waitFor(t *testing.T, cond func() error) {
	waitForWithin(t, 10*time.Second, cond)
}

// waitForWithin waits for cond past d: a test whose work is O(hundreds) of
// stubbed calls under -race raises the deadline instead of failing a loaded
// machine.
func waitForWithin(t *testing.T, d time.Duration, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
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

// TestNamespaceVersion pins zemi.api's version and keeps zemi.test to a
// plugin test boot; TestTypesCoverAPI, TestTypesDeclareNothingExtra and
// TestTypesEnums pin every member.
func TestNamespaceVersion(t *testing.T) {
	t.Parallel()
	_, r := newTestHost(t)
	runUser(t, r, `
		assert(zemi.api.version == "0.159.0", "version=" .. tostring(zemi.api.version))
		assert(not pcall(function() return zemi.test end), "zemi.test outside zemi plugin test")
		assert(zemi.project.trusted() == false and zemi.project.root() ~= "", "zemi.project")
		assert(zemi.ui.width("日本") == 4, "zemi.ui.width counts cells")
	`)
}

// TestReloadConfig: config asking for a reload while it loads would replay
// the very steps it runs in, over and over; after boot the reload waits for
// its Lua caller, which it would cancel.
func TestReloadConfig(t *testing.T) {
	for _, c := range []struct {
		name    string
		booting bool
	}{
		{"refused while config loads", true},
		{"runs once its caller returned", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := slog.New(slog.DiscardHandler)
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
			h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
			t.Cleanup(h.Close)
			h.SetUIHooks(&fakeUI{})
			var reloads atomic.Int32
			var mouse any
			h.SetReload(func() error {
				mouse, _ = h.Settings.Get("ui.mouse")
				reloads.Add(1)
				return nil
			}, nil)
			h.Commands.Add(&Command{Name: "reload", GoFn: func(h *Host, _ string, confirm bool) error { return h.ReloadConfig() }, Origin: lua.OriginBuiltin})
			// A boot step is where config code runs. It cannot go through the
			// queue: boot starts the dispatcher only after the steps.
			cfg := lua.BootStep{Name: "config", Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
				if err := h.ReloadConfig(); err == nil {
					t.Error("a reload asked for while config loads must be refused")
				}
				return nil
			}}
			steps := []lua.BootStep{h.NamespacesStep()}
			if c.booting {
				steps = append(steps, cfg)
			}
			if err := r.Boot(t.Context(), steps); err != nil {
				t.Fatal(err)
			}
			if c.booting {
				if n := reloads.Load(); n != 0 {
					t.Fatalf("boot ran %d reloads", n)
				}
				return
			}
			runUser(t, r, `
				assert(zemi.command.run("reload"))
				local t = os.clock()
				while os.clock() - t < 0.05 do end
				zemi.config.set("ui.mouse", false)
			`)
			waitFor(t, func() error {
				if n := reloads.Load(); n != 1 {
					return fmt.Errorf("reloads=%d, want 1", n)
				}
				return nil
			})
			if mouse != false {
				t.Fatalf("the reload ran before its caller returned: ui.mouse = %v", mouse)
			}
		})
	}
}

// TestOverlayItemsRenderOnOpen: render_item and format_item run when the
// overlay opens, so the UI never calls Lua to draw it.
func TestOverlayItemsRenderOnOpen(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `
		zemi.ui.overlay.open({ id = "o", items = { "a", "b" }, render_item = function(s)
			if s == "b" then error("boom") end
			return s:upper()
		end })
		zemi.ui.select({ 1 }, { format_item = function(n) return "#" .. n end }, function() end)
		zemi.ui.overlay.open({ id = "d", body = "text", empty = "none", items = { { text = "a", keys = { { key = "g", desc = "Go", fill = "x" } } } } })
		zemi.ui.overlay.open({ id = "s", anchor = "bottom-right", selected = 2, items = { "x", "y" }, render_item = function(s)
			return { { text = s, hl = "Title" }, { text = "!" } }
		end })
		assert(not pcall(zemi.ui.overlay.open, { id = "t", anchor = "top" }), "an unknown anchor must raise")
		for _, hk in ipairs({ "j", "/", "ab" }) do
			assert(not pcall(zemi.ui.overlay.open, { id = "t", items = { { text = "a", keys = { { key = hk, desc = "b" } } } } }), "row key " .. hk)
		end
		assert(not pcall(zemi.ui.overlay.open, { id = "t", items = { { text = "a", keys = { { key = "o", run = "a" }, { key = "o", run = "b" } } } } }), "one letter, two actions")
		assert(not pcall(zemi.ui.select, { prompt = "p" }, {}, function() end), "select with the args swapped must raise")
	`)
	if s := ui.opened[3]; s.Anchor != "bottom-right" || s.Selected != 1 || s.Items[0].Row[0].HL != "Title" {
		t.Fatalf("segment overlay: %+v", s)
	}
	if d := ui.opened[2]; d.Body != "text" || d.Empty != "none" || len(d.Items[0].Keys) != 1 {
		t.Fatalf("detail overlay: %+v", d)
	}
	var got []string
	for _, o := range ui.opened {
		var rows []string
		for _, item := range o.Items {
			var row string
			for _, seg := range item.Row {
				row += seg.Text
			}
			rows = append(rows, row)
		}
		got = append(got, strings.Join(rows, ","))
	}
	if strings.Join(got, ";") != "A,b;#1;;x!,y!" {
		t.Fatalf("rendered items: %q", got)
	}
}

// TestEventRoundTrip covers the user.* event path end to end: register from
// Lua, emit from Lua, payload attribution (from = emitting origin), and the
// host-owned-prefix emit refusal.
func TestEventRoundTrip(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		local keys = {}
		for _, b in ipairs(zemi.keymap.list()) do keys[b.key] = true end
		assert(keys["tab"] and keys["ctrl+g"], "list")
		zemi.keymap.del("tab")
		assert(zemi.keymap.get("tab") == nil, "user could not remove a shipped key")
		assert(zemi.keymap.del("tab") == false, "unbinding twice reports false, not an error")
		for _, bad in ipairs({ "control+q", "ctrl-q", "ctrl+", "pgupp", "Ctrl+C", "ctrl+c x", "ctrl+x ctrl+c" }) do
			assert(not pcall(zemi.keymap.set, bad, function() end), "bound " .. bad)
		end
		for _, good in ipairs({ "shift+pgup", "alt+f12", "ctrl+alt+x", "esc" }) do
			zemi.keymap.set(good, function() end)
		end
		local ok2, err2 = pcall(zemi.keymap.set, "+", function() end)
		assert(not ok2 and tostring(err2):find("printable key", 1, true), "err=" .. tostring(err2))

		zemi.command.add("hello", function() end)
		zemi.keymap.set("ctrl+y", "hello")
		local cmd = zemi.keymap.get("ctrl+y").action
		assert(cmd == "/hello", "action=" .. cmd)
		zemi.keymap.set("ctrl+n", cmd) -- what get hands back rebinds as is
		assert(zemi.keymap.get("ctrl+n").action == "/hello", "command binding does not round-trip")

		zemi.keymap.set("ctrl+u", "down", { list = true })
		local a = zemi.keymap.get("ctrl+u", { list = true })
		assert(a and a.list and a.action == "down" and zemi.keymap.get("ctrl+u") == nil, "a list alias is its own binding")
		for _, bad in ipairs({
			{ "ctrl+x j", "down", { list = true }, "one key" },
			{ "d", "down", { list = true }, "printable key" },
			{ "ctrl+k", "help", { list = true }, "is a move" },
			{ "ctrl+k", function() end, { list = true }, "is a move" },
			{ "enter", "down", { list = true }, "lists answer with" },
		}) do
			local ok, err = pcall(zemi.keymap.set, bad[1], bad[2], bad[3])
			assert(not ok and tostring(err):find(bad[4], 1, true), "alias " .. bad[1] .. ": " .. tostring(err))
		end
		assert(zemi.keymap.del("ctrl+u", { list = true }) and zemi.keymap.get("ctrl+u", { list = true }) == nil, "del an alias")
	`)
	if err := r.ExecuteChunk(t.Context(), `zemi.keymap.set("ctrl+k", "up", { list = true })`, "p.lua", lua.OriginPlugin); err != nil {
		t.Fatalf("a plugin bound a list alias: %v", err)
	}
	if b := h.Keymaps.Lookup("ctrl+g"); b == nil || b.Origin != lua.OriginUser {
		t.Fatalf("ctrl+g = %+v", b)
	}
}

// TestEmitVetoable covers the veto contract (DESIGN §4): a deny decision
// aggregates with its reason, a wedged handler times out at the veto
// deadline without wedging the interpreter, and the host default applies.
func TestEmitVetoable(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.event.on("composer.submit", function(ev)
			return {decision = "deny", reason = "no"}
		end)
	`)
	res := h.EmitVetoable(t.Context(), "composer.submit", nil)
	if !res.Denied {
		t.Fatalf("veto result = %+v, want Denied", res)
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
	res2 := h.EmitVetoable(t.Context(), "tool.pre", nil)
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

// TestConfigSetAnyOrigin pins the config write: any origin sets, a value of
// the wrong kind for a key with choices is flagged and leaves the default,
// and a good value applies to the store.
func TestConfigSetAnyOrigin(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	if err := r.ExecuteChunk(t.Context(), `zemi.config.set("agent.effort", "low")`, "p.lua", lua.OriginProject); err != nil {
		t.Fatalf("project config set agent.effort: %v", err)
	}
	runUser(t, r, `zemi.config.set("ui.notify", false)`)
	if v, _ := h.Settings.Get("ui.notify"); v != config.NotifyOSC {
		t.Errorf("ui.notify = %v after a refused set, want the default", v)
	}
	if !h.Recorded(`config key ui.notify expects osc, native or bell`) {
		t.Error("the refused value is not flagged in /errors")
	}
	runUser(t, r, `zemi.config.set("agent.max_background", 42)`)
	if v, ok := h.Settings.Get("agent.max_background"); !ok || v != 42.0 {
		t.Fatalf("agent.max_background = %v, want 42", v)
	}
}

// TestConfigSet: /config set takes the words /config and the pickers show
// for a value, and /config lua writes a number as typed.
func TestConfigSet(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	for _, c := range []struct {
		path  string
		from  any
		value string
		want  string // the value after, as /config lua writes it
		err   string
	}{
		{"agent.effort", "high", "default", `""`, ""},
		{"agent.effort", "high", "turbo", `"high"`, `config key agent.effort expects low, medium, high or default, not "turbo"`},
		{"agent.max_background", 4.0, "2", "2", ""},
		{"agent.max_background", 4.0, "many", "4", "agent.max_background is a number"},
	} {
		key := config.Key(c.path)
		h.Settings.SetQuiet(key, c.from)
		// a refused value is a usage hint, not an error
		err := h.configSet(key, c.value)
		got, _ := h.Settings.Get(key)
		if fmt.Sprint(err) != cmp.Or(c.err, "<nil>") || err != nil && !errors.Is(err, ErrUsage) || luaLiteral(got) != c.want {
			t.Errorf("%s %q: %s, %v; want %s, %q", c.path, c.value, luaLiteral(got), err, c.want, c.err)
		}
	}
	// every other row changes on a screen of its own, which /config edit is
	// unless the key has a picker
	for path, section := range map[string]string{"editor.command": "editor", "models.compact": "models"} {
		items := h.configItems(section)
		row := items[slices.IndexFunc(items, func(it OverlayItem) bool { return it.Text == path })]
		if act := row.Keys[0]; act.Run != configPicker(config.Key(path)) || act.Desc != "change" {
			t.Errorf("%s: first action %+v, want change running its picker", path, act)
		}
	}
	// /config's root is one row per section, each falling into its keys
	sections := h.configSections()
	root := slices.IndexFunc(sections, func(it OverlayItem) bool { return it.Text == "Session" })
	if root < 0 || sections[root].Run != "config session" || sections[root].Verb != VerbOpen {
		t.Fatalf("the session row = %+v, want open running config session", sections[root])
	}
	if sections[root].Desc == "" || sections[root].Preview != "recap" {
		t.Errorf("the session row reads %q, %q; want a description over its keys", sections[root].Desc, sections[root].Preview)
	}
	if n := len(h.configItems("session")); n != 1 {
		t.Errorf("the session section holds %d keys, want recap", n)
	}
	if !h.configSection("models") || h.configSection("set") {
		t.Error("configSection matches a schema segment only")
	}
	// an empty input's ghost is what a bare enter would set
	for _, c := range []struct {
		path, want string
	}{
		{"session.recap", "default 3m"},
		{"agent.max_background", "default 4"},
		{"editor.command", ""},
	} {
		s, err := h.setting(config.Key(c.path))
		if err != nil {
			t.Fatal(err)
		}
		if got := settingGhost(s); got != c.want {
			t.Errorf("%s ghost %q, want %q", c.path, got, c.want)
		}
	}
	// a bool row draws its switch after its source, and its description is
	// the preview
	h.cfgNow = map[config.Key][]any{config.KeyUIMouse: {false}}
	for _, c := range []struct {
		step func()
		on   bool
		desc string
	}{
		{func() { h.Settings.SetQuiet(config.KeyUIMouse, false) }, false, "init.lua"},
		{func() { h.configSet(config.KeyUIMouse, "on") }, true, "this run"},
		{func() { h.cfgNow = nil }, true, "default"},
	} {
		c.step()
		items := h.configItems("ui")
		row := items[slices.IndexFunc(items, func(it OverlayItem) bool { return it.Text == "ui.mouse" })]
		if row.On == nil || *row.On != c.on || row.Desc != c.desc || !strings.Contains(row.Preview, "drag to select") {
			t.Errorf("bool row: on %v, desc %q, preview %q; want %v, %q", row.On, row.Desc, row.Preview, c.on, c.desc)
		}
	}
}

// TestConfigEditList: a list key's edit prompt pre-fills the grammar
// configSet reads back, so enter on the shown value stores the same list.
func TestConfigEditList(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	key := config.KeyModelsFavorites
	ui := &fakeUI{}
	for _, want := range [][]string{{"acme/one", "acme/two"}, {}} {
		h.Settings.SetQuiet(key, want)
		if err := h.openConfigEdit(ui, key); err != nil {
			t.Fatal(err)
		}
		text := ui.overlays()[len(ui.opened)-1].Default
		if err := h.configSet(key, text); err != nil {
			t.Fatal(err)
		}
		got, _ := h.Settings.Get(key)
		list, ok := got.([]string)
		if !ok || !slices.Equal(list, want) {
			t.Errorf("enter on %q stored %v, want %v", text, got, want)
		}
	}
}

// TestLuaLiteral: a value /config lua shows reads back as itself in Lua.
func TestLuaLiteral(t *testing.T) {
	L := glua.NewState()
	defer L.Close()
	want := "q\" b\\ nl\n tab\t \x01\x7f9 é"
	if err := L.DoString("s = " + luaLiteral(want)); err != nil {
		t.Fatal(err)
	}
	if got := L.GetGlobal("s").String(); got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

// TestConfigWatchNoDeadlock: a Lua config.set inside a running interpreter
// request delivers the watch inline, since a blocking RT.Do on the busy queue
// would wedge it; the callback runs as its registration origin (its own
// zemi.config.set would otherwise be refused), and the chunk finishes.
func TestConfigWatchNoDeadlock(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.config.watch("ui.mouse", function(nv, ov)
			assert(ov == true, "old=" .. tostring(ov))
			assert(nv == false, "new=" .. tostring(nv))
			zemi.config.set("editor.command", "watched")
		end)
		zemi.config.set("ui.mouse", false)
		assert(not pcall(zemi.config.watch, "agent.mdoel", function() end), "a watch on an unknown key must raise")
	`)
	if v, ok := h.Settings.Get("editor.command"); !ok || v != "watched" {
		t.Fatalf("watch callback side effect missing: editor.command = %v, want watched", v)
	}
}

// TestLoadPicksDropsDeadKeys: a kept pick for a key nothing registers any
// more is deleted from the store once, instead of warning at every boot; a
// plugin- or model-shaped unknown pick may still register this boot.
func TestLoadPicksDropsDeadKeys(t *testing.T) {
	t.Parallel()
	var file bytes.Buffer
	h, _ := newTestHostDB(t)
	h.Log = slog.New(slog.NewTextHandler(&file, nil))
	for _, kv := range [][2]string{
		{"config:agent.max_steps", "500"},
		{"config:plugins.plug.thing", "1"},
		{"config:models.gpt9", `"big"`},
	} {
		if err := h.GlobalDB.SetKV(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	h.loadPicks()
	if _, err := h.GlobalDB.GetKV("config:agent.max_steps"); err == nil {
		t.Error("the dead pick row is still there")
	}
	if v, ok := h.picked["plugins.plug.thing"]; !ok || v != 1.0 {
		t.Errorf("plugin pick: %v, %v", v, ok)
	}
	if v, ok := h.picked["models.gpt9"]; !ok || v != "big" {
		t.Errorf("model pick: %v, %v", v, ok)
	}
	logs := file.String()
	if n := strings.Count(logs, "dropped dead pick"); n != 1 {
		t.Errorf("dropped dead pick logged %d times, want 1", n)
	}
}

// TestScroll: any origin scrolls the screen, and the region id is optional.
func TestScroll(t *testing.T) {
	t.Parallel()
	_, r := newTestHostDB(t)
	for _, o := range []lua.Origin{lua.OriginPlugin, lua.OriginProject, lua.OriginBuiltin} {
		if err := r.ExecuteChunk(t.Context(), `zemi.ui.scroll("bottom")`, "x.lua", o); err != nil {
			t.Errorf("%s code: %v", o, err)
		}
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.ui.scroll("bottom", "chat")`, "x.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
}

// TestEventLogFromLua: emissions are logged and counted with no handler
// registered, and notifications are listed, newest first.
func TestEventLogFromLua(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	h.EmitAsync("agent.text_delta", map[string]any{"text": "x"})
	h.Notify(slog.LevelInfo, "first", "one")
	runUser(t, r, `
		zemi.event.emit("user.ping", { n = 1 })
		zemi.event.emit("user.ping", { n = 2 })
		zemi.notify({ title = "second", body = "two", level = "error" })
		zemi.notify({ title = "third", body = "three" })
		zemi.notify.warn("four", "fourth")
		local log = zemi.event.list({ name = "user.ping" })
		assert(#log == 2, "log=" .. #log)
		assert(log[1].n == 2 and log[1].from == "user" and log[1].at:match("^%d%d:%d%d:%d%d$"), "newest first: " .. tostring(log[1].n))
		assert(#zemi.event.list({ limit = 1 }) == 1, "limit")
		local stats = zemi.event.stats()
		assert(stats["user.ping"] == 2, "ping=" .. tostring(stats["user.ping"]))
		assert(stats["agent.text_delta"] == 1, "delta counted")
		assert(#zemi.event.list({ name = "agent.text_delta" }) == 0, "delta not logged")
		local notes = zemi.notify.list()
		assert(#notes == 4 and notes[4].level == "info" and notes[3].level == "error", "notes=" .. #notes)
		assert(notes[2].title == "third" and notes[2].level == "info", "level stuck: " .. notes[2].level)
		assert(notes[1].title == "fourth" and notes[1].body == "four" and notes[1].level == "warn", "string form")
		assert(not pcall(zemi.notify, 42), "a number is not a spec")
	`)
}

func TestShortOutput(t *testing.T) {
	var lines []string
	for i := range 30 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	got := shortOutput(strings.Join(lines, "\n") + "\n")
	want := "line 0\nline 1\nline 2\n… 21 more lines\nline 24\nline 25\nline 26\nline 27\nline 28\nline 29"
	if got != want {
		t.Errorf("got:\n%s", got)
	}
	if got := shortOutput("a\nb"); got != "a\nb" {
		t.Errorf("short output changed: %q", got)
	}
	for i := 30; i < 3000; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	if got := shortOutput(tools.TruncOut(strings.Join(lines, "\n"), 10_000)); !strings.Contains(got, "\n… 2991 more lines\n") {
		t.Errorf("a cut result counts the lines it cut:\n%s", got)
	}
	if got := shortOutput("boom\nexit status 2\n\n" + tools.LeadReminder("shell", "about", "command")); strings.Contains(got, "system-reminder") {
		t.Errorf("the reminder leaked into the row: %q", got)
	}
}

// TestRowOutputDates: a date row reads the compact local stamp, and the
// already-compacted text a rebuild reads back passes through unchanged.
func TestRowOutputDate(t *testing.T) {
	const raw = "2026-10-29T11:03:12.487654321Z"
	want := time.Date(2026, 10, 29, 11, 3, 12, 487654321, time.UTC).Local().Format(_previewStamp)
	if got := rowOutput("date", raw); got != want || got == raw {
		t.Errorf("rowOutput(date) = %q, want %q", got, want)
	}
	if got := rowOutput("date", want); got != want {
		t.Errorf("a compacted stamp did not pass through: %q, want %q", got, want)
	}
	if got := rowOutput("date", "not a stamp"); got != "not a stamp" {
		t.Errorf("a malformed stamp did not pass through: %q", got)
	}
}

// TestProjectMark: the marker is a pure function of the root, and random
// roots spread over the pool.
func TestProjectMark(t *testing.T) {
	if got := ProjectMark(""); got != "" {
		t.Fatalf("no project drew %q", got)
	}
	seen := map[string]bool{}
	for range 200 {
		root := filepath.Join(t.TempDir(), store.ShortID())
		if got := ProjectMark(root); got != ProjectMark(root) || got == "" {
			t.Fatalf("%s derives %q", root, got)
		}
		seen[ProjectMark(root)] = true
	}
	if len(seen) < 10 {
		t.Fatalf("200 roots drew only %d markers", len(seen))
	}
}

// TestHistoryRowFacts: a resumed session rebuilds a call's row from the facts
// stored beside its message, never from the words it answered; a row older
// than that column reads as its result says.
func TestHistoryRowFacts(t *testing.T) {
	parts, err := json.Marshal(fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "t1", ToolName: string(tools.ToolShell), Input: `{"command":"make slow"}`},
		fantasy.ToolResultPart{ToolCallID: "t1", Output: fantasy.ToolResultOutputContentText{Text: "still running"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		meta string
		want rowMeta
	}{
		{"a hand-off", `{"t1":{"status":"bg","job":"job-3"}}`, rowMeta{Status: CallBG, Job: "job-3"}},
		{"a stop", `{"t1":{"status":"stopped"}}`, rowMeta{Status: CallStopped}},
		{"a row older than the column", "", rowMeta{Status: CallOK}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, tr := historyFrom(nil, []*store.Message{{Role: "assistant", PartsJSON: string(parts), RowMeta: c.meta}})
			if len(tr) != 1 {
				t.Fatalf("%d rows: %+v", len(tr), tr)
			}
			got := rowMeta{Status: tr[0].Status, Run: tr[0].Run, Job: tr[0].Job}
			if got != c.want {
				t.Fatalf("row = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestSavedTrustDecidesAtConstruction: the saved decision applies when the
// host is built, before any boot step, so a decided project never flashes
// the untrusted pill; an unsaved one waits for the boot's ask.
func TestSavedTrustDecidesAtConstruction(t *testing.T) {
	tmp := t.TempDir()
	for _, c := range []struct {
		name string
		mark string // "" saves nothing
		want bool
	}{
		{"trusted", "trusted", true},
		{"untrusted", "untrusted", false},
		{"unsaved", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dirs := config.Dirs{ProjectDir: t.TempDir(), ConfigHome: t.TempDir(), StateHome: filepath.Join(tmp, c.mark, "state")}
			if c.mark != "" {
				if err := dirs.WriteTrust(map[string]string{dirs.TrustRoot(): c.mark}); err != nil {
					t.Fatal(err)
				}
			}
			log := slog.New(slog.DiscardHandler)
			h := NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), dirs)
			t.Cleanup(h.Close)
			if got := h.ProjectTrusted(); got != c.want {
				t.Fatalf("ProjectTrusted = %v, want %v", got, c.want)
			}
		})
	}
}
