package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// newTestHostUI is newTestHost with the shipped theme, widgets and keymaps
// loaded, and a state dir; a clock given moves only when the test moves it.
func newTestHostUI(t *testing.T, clock ...lua.Clock) *Host {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, RuntimeFS: zemiruntime.FS, Clock: cmp.Or(clock...)})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir()})
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), lua.RequireStep("zemi.tui")}); err != nil {
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

// TestToolStatus: the mark comes from the result's own type, so a call that
// refused, or a job_read of a job that died, draws a failure and never a
// check; a handoff and an interrupt keep their own marks.
func TestToolStatus(t *testing.T) {
	for _, c := range []struct {
		name      string
		tool      string
		result    fantasy.ToolResultOutputContent
		cancelled bool
		want      CallStatus
	}{
		{"a finished call", tools.ToolRead, out("package a"), false, CallOK},
		{"a dropped memory", tools.ToolMemory, outErr("dropped: quote not found in this session's messages"), false, CallErr},
		{"a near-duplicate memory", tools.ToolMemory, outErr("near-duplicate of m1 (project): \"deploy by hand\""), false, CallErr},
		{"a job that died", tools.ToolJobRead, outErr("job-3 failed: exit status 1"), false, CallErr},
		{"an ask the user denied", tools.ToolWrite, outErr("permission denied (user): fs.write /p/b.go"), false, CallStopped},
		{"an ask the user dismissed", tools.ToolQuestion, outErr("The user dismissed the questions."), false, CallStopped},
		{"an interrupted turn", tools.ToolRead, outErr("context canceled"), true, CallStopped},
		{"a background agent", tools.ToolAgent, out(bgAgentStarted + " run=1"), false, CallBG},
		{"a background workflow", tools.ToolWorkflow, out(bgWorkflowStarted + " run=1"), false, CallBG},
		{"a shell that handed off", tools.ToolShell, out("still running, so it " + tools.JobHandoff + "job-3"), false, CallBG},
		{"a job that started", tools.ToolJobStart, out("started job-2 (npm run dev)"), false, CallBG},
		{"a shell that named a job but exited", tools.ToolShell, out(tools.JobHandoff + "job-3"), false, CallOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := toolStatus(c.tool, c.result, c.cancelled); got != c.want {
				t.Fatalf("toolStatus(%s) = %s, want %s", c.tool, got, c.want)
			}
		})
	}
}

// TestJobOf: only a call that started or handed off to a job names one, from
// the phrase its own tool wrote, so the wording around it belongs to the tool.
func TestJobOf(t *testing.T) {
	for _, c := range []struct {
		tool   string
		status CallStatus
		out    string
		want   string
	}{
		{tools.ToolShell, CallBG, "still running, so it moved to the background as job-3: read the rest", "job-3"},
		{tools.ToolShell, CallBG, "job-7 of the build\nso it moved to the background as job-3: read the rest", "job-3"},
		{tools.ToolJobStart, CallBG, "started job-2 (npm run dev)", "job-2"},
		{tools.ToolShell, CallOK, "moved to the background as job-3", ""}, // a shell that exited names no job
		{tools.ToolJobStart, CallBG, "job-2", ""},                         // only the tool's own phrasing counts
		{tools.ToolShell, CallBG, "no id here", ""},
	} {
		if got := jobOf(c.tool, c.status, c.out); got != c.want {
			t.Errorf("jobOf(%s, %s, %q) = %q, want %q", c.tool, c.status, c.out, got, c.want)
		}
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
// error: a slice gets 30s, not the interpreter's own ceiling, which a loaded
// machine reaches on a chunk that starts processes. The select guard turns an
// interpreter deadlock into a loud failure instead of a hung test binary.
func runChunk(t *testing.T, r *lua.Runtime, src string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.Eval(ctx, src, "test.lua", lua.OriginUser, 30*time.Second, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("chunk did not complete within 30s (interpreter deadlock?)")
		return nil
	}
}

// runUser executes src as a user-origin chunk and fails the test on error.
func runUser(t *testing.T, r *lua.Runtime, src string) {
	t.Helper()
	if err := runChunk(t, r, src); err != nil {
		t.Fatalf("user chunk: %v", err)
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

// TestNamespaceVersion pins zemi.api's version and keeps zemi.test to a
// plugin test boot; TestTypesCoverAPI, TestTypesDeclareNothingExtra and
// TestTypesEnums pin every member.
func TestNamespaceVersion(t *testing.T) {
	_, r := newTestHost(t)
	runUser(t, r, `
		assert(zemi.api.version == "0.129.0", "version=" .. tostring(zemi.api.version))
		assert(not pcall(function() return zemi.test end), "zemi.test outside zemi plugins test")
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
			h.SetUIHooks(&fakeUI{})
			var reloads atomic.Int32
			var mouse any
			h.SetReload(func() error {
				mouse, _ = h.Settings.Get("ui.mouse")
				reloads.Add(1)
				return nil
			}, nil)
			h.Commands.Add(&Command{Name: "reload", GoFn: func(h *Host, _ string) error { return h.ReloadConfig() }, Origin: lua.OriginBuiltin})
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

// TestErrorsFromLua: the agent reads zemi's own failures through the lua
// tool, so debugging zemi does not depend on the user reading the screen.
func TestErrorsFromLua(t *testing.T) {
	h, r := newTestHost(t)
	h.RecordError("boot", `init.lua:1: zemi.provider.add: provider "x" already registered`)
	runUser(t, r, `
		local rows = zemi.errors.list()
		assert(#rows == 1, "rows=" .. #rows)
		assert(rows[1].source == "boot", "source=" .. tostring(rows[1].source))
		assert(rows[1].message:find("already registered", 1, true) ~= nil, "message=" .. tostring(rows[1].message))
	`)
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
	if _, err := r.Do(t.Context(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		onSelect = L.NewFunction(func(*glua.LState) int { called = true; return 0 })
		return nil, nil
	}); err != nil {
		t.Fatalf("build callback: %v", err)
	}
	o := Overlay{ID: "x", Origin: lua.OriginUser, Gen: h.currentGen(), OnSelect: onSelect, Items: []any{"a"}}

	h.SelectOverlay(t.Context(), o, 0)
	if !called {
		t.Fatal("a live overlay did not run on_select")
	}

	called = false
	if err := r.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}, h.PurgeGeneration); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	h.SelectOverlay(t.Context(), o, 0)
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
		zemi.ui.overlay.open({ id = "d", body = "text", empty = "none", items = { { text = "a", actions = { { text = "Go", fill = "x" } } } } })
		zemi.ui.overlay.open({ id = "s", anchor = "bottom-right", selected = 2, items = { "x", "y" }, render_item = function(s)
			return { { text = s, hl = "Title" }, { text = "!" } }
		end })
		assert(not pcall(zemi.ui.overlay.open, { id = "t", anchor = "top" }), "an unknown anchor must raise")
		for _, hk in ipairs({ "j", "/", "ab" }) do
			assert(not pcall(zemi.ui.overlay.open, { id = "t", items = { { text = "a", hotkey = hk } } }), "item hotkey " .. hk)
			assert(not pcall(zemi.ui.overlay.open, { id = "t", items = { { text = "a", actions = { { text = "b", hotkey = hk } } } } }), "action hotkey " .. hk)
		end
		assert(not pcall(zemi.ui.select, { prompt = "p" }, {}, function() end), "select with the args swapped must raise")
	`)
	if s := ui.opened[3]; s.Anchor != "bottom-right" || s.Selected != 1 || s.ItemText[0][0].HL != "Title" {
		t.Fatalf("segment overlay: %+v", s)
	}
	if d := ui.opened[2]; d.Body != "text" || d.Empty != "none" || len(d.Items[0].(map[string]any)["actions"].([]any)) != 1 {
		t.Fatalf("detail overlay: %+v", d)
	}
	var got []string
	for _, o := range ui.opened {
		var rows []string
		for _, segs := range o.ItemText {
			var row string
			for _, seg := range segs {
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
		assert(zemi.keymap.del("tab") == false, "unbinding twice reports false, not an error")
		for _, bad in ipairs({ "control+q", "ctrl-q", "ctrl+", "pgupp", "Ctrl+C", "ctrl+c x", "ctrl+x ctrl+c" }) do
			assert(not pcall(zemi.keymap.set, bad, function() end), "bound " .. bad)
			assert(not pcall(zemi.keymap.set, bad, function() end, { focus = "sidebar" }), "bound focused " .. bad)
		end
		for _, good in ipairs({ "shift+pgup", "alt+f12", "ctrl+alt+x", "esc" }) do
			zemi.keymap.set(good, function() end)
		end
		local ok2, err2 = pcall(zemi.keymap.set, "+", function() end)
		assert(not ok2 and tostring(err2):find("printable key", 1, true), "err=" .. tostring(err2))
		zemi.keymap.set("+", function() end, { focus = "sidebar" })

		zemi.command.add("hello", function() end)
		zemi.keymap.set("ctrl+y", "hello")
		local cmd = zemi.keymap.get("ctrl+y").action
		assert(cmd == "/hello", "action=" .. cmd)
		zemi.keymap.set("ctrl+n", cmd) -- what get hands back rebinds as is
		assert(zemi.keymap.get("ctrl+n").action == "/hello", "command binding does not round-trip")

		zemi.keymap.set("ctrl+j", "down", { list = true })
		local a = zemi.keymap.get("ctrl+j", { list = true })
		assert(a and a.list and a.action == "down" and zemi.keymap.get("ctrl+j") == nil, "a list alias is its own binding")
		for _, bad in ipairs({
			{ "ctrl+x j", "down", { list = true }, "one key" },
			{ "d", "down", { list = true }, "printable keys filter" },
			{ "ctrl+k", "help", { list = true }, "is a move" },
			{ "ctrl+k", function() end, { list = true }, "is a move" },
			{ "ctrl+k", "down", { list = true, focus = "sidebar" }, "do not combine" },
			{ "enter", "down", { list = true }, "lists answer with" },
		}) do
			local ok, err = pcall(zemi.keymap.set, bad[1], bad[2], bad[3])
			assert(not ok and tostring(err):find(bad[4], 1, true), "alias " .. bad[1] .. ": " .. tostring(err))
		end
		assert(zemi.keymap.del("ctrl+j", { list = true }) and zemi.keymap.get("ctrl+j", { list = true }) == nil, "del an alias")
	`)
	if err := r.ExecuteChunk(t.Context(), `zemi.keymap.set("ctrl+k", "up", { list = true })`, "p.lua", lua.OriginPlugin); err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("a plugin bound a list alias: %v", err)
	}
	if b := h.Keymaps.Lookup("", "ctrl+g"); b == nil || b.Origin != lua.OriginUser {
		t.Fatalf("ctrl+g = %+v", b)
	}
}

// waitedCtx says when somebody starts waiting on it: Runtime.Do reads Done
// only after its request is queued.
type waitedCtx struct {
	context.Context
	waited chan struct{}
}

func (c *waitedCtx) Done() <-chan struct{} {
	select {
	case c.waited <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

// TestRenderGuardOnlyWhileRendering: a render queued behind other Lua does
// not make that Lua look like render code (project config registering a
// widget during the trust prompt was refused).
func TestRenderGuardOnlyWhileRendering(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.ui.widget.add({ id = "w", region = "header", render = function() return {} end })`)
	w := h.Widgets.List()[0]
	rendered := make(chan struct{})
	ctx := &waitedCtx{Context: t.Context(), waited: make(chan struct{}, 1)}
	if _, err := r.Do(t.Context(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		go func() {
			defer close(rendered)
			h.RenderWidget(ctx, w, 40, 10, UIState{})
		}()
		<-ctx.waited // the render is now queued behind this request
		if h.rendering.Load() {
			t.Error("render guard raised while the render only waited in the queue")
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	<-rendered
}

// TestRenderAllowlist pins the render guard (DESIGN §4): a widget render
// may call the luaReadOnly readers, and any other call, a wait or a disk
// read raises naming it; a clean render comes back sanitized.
func TestRenderAllowlist(t *testing.T) {
	h, r := newTestHost(t)
	cases := []struct{ call, refused string }{
		{`zemi.config.get("ui.mouse")`, ""},
		{`zemi.perm.ask({ subject = "fs.write" })`, "perm.ask"},
		{`zemi.agent.run({ prompt = "x" })`, "agent.run"},
		{`zemi.memory.add("x")`, "memory.add"},
		{`zemi.skills.enable("x")`, "skills.enable"},
		{`zemi.config.watch("ui.mouse", function() end)`, "config.watch"},
		{`zemi.ui.scroll("bottom")`, "ui.scroll"},
		{`zemi.fs.read("x")`, "fs.read"},
		{`job:wait(1000)`, "job:wait"},
	}
	src := `local job = zemi.job.start({ cmd = "true" })` + "\n"
	for i, c := range cases {
		src += fmt.Sprintf("zemi.ui.widget.add({ id = %q, region = \"header\", render = function() %s return {} end })\n", strconv.Itoa(i), c.call)
	}
	src += `zemi.ui.widget.add({ id = "clean", region = "header", render = function()
		return {{{text = "a\27[31mb", hl = "Dim"}, {text = "chrome", hl = "Host:BtnSel"}}}
	end })`
	runUser(t, r, src)
	widgets := map[string]*Widget{}
	for _, w := range h.Widgets.List() {
		widgets[w.ID] = w
	}
	for i, c := range cases {
		_, _, err := h.RenderWidget(t.Context(), widgets[strconv.Itoa(i)], 40, 10, UIState{})
		if want := "zemi." + c.refused + " is not allowed during widget render"; c.refused == "" && err != nil ||
			c.refused != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: err = %v", c.call, err)
		}
	}

	lines, _, err := h.RenderWidget(t.Context(), widgets["clean"], 40, 10, UIState{})
	if err != nil {
		t.Fatalf("clean render: %v", err)
	}
	if len(lines) != 1 || len(lines[0]) != 2 {
		t.Fatalf("clean render shape = %#v", lines)
	}
	if hl := lines[0][1].HL; hl != "" {
		t.Fatalf("Go chrome hl survived Lua: %q", hl)
	}
	seg := lines[0][0]
	if seg.Text != "ab" || strings.ContainsRune(seg.Text, '\x1b') {
		t.Fatalf("segment text = %q (escape not sanitized)", seg.Text)
	}
	if seg.HL != "Dim" {
		t.Fatalf("segment hl = %q, want Dim", seg.HL)
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

// TestConfigSetGating pins the config write gate: only user config sets,
// a value of the wrong kind for a key with choices names them, and the value
// applies to the store.
func TestConfigSetGating(t *testing.T) {
	h, r := newTestHost(t)
	if err := r.ExecuteChunk(t.Context(), `zemi.config.set("memory.autonomy", "auto")`, "p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("project config set memory.autonomy: %v", err)
	}
	runExpectErr(t, r, `zemi.config.set("ui.notify", false)`, "config key ui.notify expects auto, osc, native, bell or off")
	runUser(t, r, `
		zemi.config.set("memory.autonomy", "auto")

		zemi.config.set("ui.composer.history", 42)
	`)
	if v, ok := h.Settings.Get("ui.composer.history"); !ok || v != 42.0 {
		t.Fatalf("ui.composer.history = %v, want 42", v)
	}
}

// TestThemeCommand: /theme <name> sets a theme and refuses one nothing
// added; bare, it lists the families, each light twin in its row, and the
// list stays open over a pick with the current row moved.
func TestThemeCommand(t *testing.T) {
	h := newTestHostUI(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	theme := h.configCommands()[1]
	for _, c := range []struct{ args, want, err string }{
		{"nord", "nord", ""},
		{"nope", "nord", "unknown theme"},
		{"", "nord", ""},
	} {
		err := theme.GoFn(h, c.args)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) || h.Settings.Str("ui.theme") != c.want {
			t.Fatalf("/theme %s: %v, ui.theme %q", c.args, err, h.Settings.Str("ui.theme"))
		}
	}
	opened := ui.overlays()
	if len(opened) != 1 || opened[0].Reload == nil {
		t.Fatalf("opened %d overlays, want the pick as a toggle list", len(opened))
	}
	row := func(items []any, name string) map[string]any {
		i := slices.IndexFunc(items, func(it any) bool { return it.(map[string]any)["text"] == name })
		if i < 0 {
			return nil
		}
		return items[i].(map[string]any)
	}
	items := opened[0].Items
	if row(items, "catppuccin-light") != nil || row(items, "catppuccin")["desc"] != "light: catppuccin-light" || row(items, "nord")["key"] != "current" {
		t.Fatalf("rows: %v", items)
	}
	if err := h.configCommand("set ui.theme dracula"); err != nil {
		t.Fatal(err)
	}
	if items, _ := opened[0].Reload(); row(items, "dracula")["key"] != "current" || row(items, "nord")["key"] != nil {
		t.Fatalf("after a pick: %v", items)
	}
}

// TestConfigSet: /config set takes the words /config and the pickers show
// for a value, and /config lua writes a number as typed.
func TestConfigSet(t *testing.T) {
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
		{"agent.profile", "high", "", `"default"`, ""},
		{"agent.context_window", 0.0, "1000000", "1000000", ""},
		{"agent.context_window", 0.0, "84.5", "84.5", ""},
		{"agent.context_window", 0.0, "abc", "0", "agent.context_window is a number"},
		{"agent.context_window", 0.0, "NaN", "0", "config key agent.context_window expects number"},
	} {
		h.Settings.SetQuiet(c.path, c.from)
		// a refused value is a usage hint, not an error
		err := h.configSet(c.path, c.value)
		got, _ := h.Settings.Get(c.path)
		if fmt.Sprint(err) != cmp.Or(c.err, "<nil>") || err != nil && !errors.Is(err, ErrUsage) || luaLiteral(got) != c.want {
			t.Errorf("%s %q: %s, %v; want %s, %q", c.path, c.value, luaLiteral(got), err, c.want, c.err)
		}
	}
	// every other row changes on a screen of its own, which /config edit is
	// unless the key has a picker
	for path, run := range map[string]string{"editor.command": "config edit editor.command", "ui.theme": "theme", "models.chat": "model chat"} {
		items := h.configItems()
		row := items[slices.IndexFunc(items, func(it any) bool { return it.(map[string]any)["text"] == path })].(map[string]any)
		if act := row["actions"].([]any)[0].(map[string]any); act["run"] != run || act["text"] != "Change" {
			t.Errorf("%s: first action %v, want Change running %q", path, act, run)
		}
	}
	// a bool row draws its switch after its source, and its description is
	// the preview
	h.cfgNow = map[string][]any{"ui.animations": {false}}
	for _, c := range []struct {
		step func()
		on   bool
		desc string
	}{
		{func() { h.Settings.SetQuiet("ui.animations", false) }, false, "init.lua"},
		{func() { h.configSet("ui.animations", "on") }, true, "this run"},
		{func() { h.cfgNow = nil }, true, "default"},
	} {
		c.step()
		items := h.configItems()
		row, _ := items[slices.IndexFunc(items, func(it any) bool { return it.(map[string]any)["text"] == "ui.animations" })].(map[string]any)
		if desc, _ := row["desc"].(string); row["on"] != c.on || desc != c.desc || !strings.Contains(row["preview"].(string), "animate") {
			t.Errorf("bool row: on %v, desc %q, preview %q; want %v, %q", row["on"], desc, row["preview"], c.on, c.desc)
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
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.config.watch("ui.completion.files", function(nv, ov)
			assert(ov == true, "old=" .. tostring(ov))
			assert(nv == false, "new=" .. tostring(nv))
			zemi.config.set("editor.command", "watched")
		end)
		zemi.config.set("ui.completion.files", false)
		assert(not pcall(zemi.config.watch, "agent.mdoel", function() end), "a watch on an unknown key must raise")
	`)
	if v, ok := h.Settings.Get("editor.command"); !ok || v != "watched" {
		t.Fatalf("watch callback side effect missing: editor.command = %v, want watched", v)
	}
}

func TestSetModelFiresWatches(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.config.watch("models.chat", function(nv) zemi.state.set("picked", nv) end)
		zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://x", api_key = "k", models = { { id = "m" }, { id = "o" } } })`)
	check := func(want string) {
		t.Helper()
		waitFor(t, func() error {
			if v, _ := h.GlobalDB.GetKV("picked"); v != `"`+want+`"` {
				return fmt.Errorf("models.chat watch never saw %q", want)
			}
			return nil
		})
	}
	if err := h.SetModel("p/m"); err != nil {
		t.Fatal(err)
	}
	check("p/m")
	runUser(t, r, `zemi.model.set("p/o")`)
	check("p/o")
}

// TestScrollRefusedBelowUser: code below user config may not scroll the
// user's screen, while shipped code may.
func TestScrollRefusedBelowUser(t *testing.T) {
	_, r := newTestHostDB(t)
	for _, o := range []lua.Origin{lua.OriginPlugin, lua.OriginProject} {
		if err := r.ExecuteChunk(t.Context(), `zemi.ui.scroll("bottom")`, "x.lua", o); err == nil || !strings.Contains(err.Error(), "only user config") {
			t.Errorf("%s code ran zemi.ui.scroll: %v", o, err)
		}
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.ui.scroll("bottom", "chat")`, "x.lua", lua.OriginBuiltin); err != nil {
		t.Fatal(err)
	}
}

// TestEventLogFromLua: emissions are logged and counted with no handler
// registered, and notifications are listed, newest first.
func TestEventLogFromLua(t *testing.T) {
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
		local log = zemi.event.log({ name = "user.ping" })
		assert(#log == 2, "log=" .. #log)
		assert(log[1].n == 2 and log[1].from == "user" and log[1].at:match("^%d%d:%d%d:%d%d$"), "newest first: " .. tostring(log[1].n))
		assert(#zemi.event.log({ limit = 1 }) == 1, "limit")
		local stats = zemi.event.stats()
		assert(stats["user.ping"] == 2, "ping=" .. tostring(stats["user.ping"]))
		assert(stats["agent.text_delta"] == 1, "delta counted")
		assert(#zemi.event.log({ name = "agent.text_delta" }) == 0, "delta not logged")
		local notes = zemi.notify.list()
		assert(#notes == 4 and notes[4].level == "info" and notes[3].level == "error", "notes=" .. #notes)
		assert(notes[2].title == "third" and notes[2].level == "info", "level stuck: " .. notes[2].level)
		assert(notes[1].title == "fourth" and notes[1].body == "four" and notes[1].level == "warn", "string form")
		assert(not pcall(zemi.notify, 42), "a number is not a spec")
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
	pdb, err := store.Open(store.ProjectDBPath(h.Dirs.StateHome, h.trustRoot()))
	if err != nil {
		t.Fatal(err)
	}
	defer pdb.Close()
	if _, err := pdb.GetSession("s1"); err != nil {
		t.Fatalf("session not in the project store: %v", err)
	}
}

// TestStoreKeyedOnRepo: zemi started in a subdirectory is the same project
// as one started at the root, so its sessions and memories are there.
func TestStoreKeyedOnRepo(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	sub := filepath.Join(root, "internal")
	for _, dir := range []string{filepath.Join(root, ".git"), sub} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	host := func(dir string) *Host {
		log := slog.New(slog.DiscardHandler)
		h := NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{StateHome: state, ProjectDir: dir})
		t.Cleanup(func() { h.DB.Close(); h.GlobalDB.Close() })
		return h
	}
	if err := host(root).DB.CreateSession("s1", "", "t", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := host(sub).DB.GetSession("s1"); err != nil {
		t.Fatalf("a subdirectory opened another project's store: %v", err)
	}
}

func TestClickWidget(t *testing.T) {
	const widget = `zemi.ui.widget.add({
		id = "w", region = "statusline.left",
		render = function() return {{{ text = "go", click = "probe" }, { text = " plain" }}} end,
		on_click = function(ev) assert(zemi.command.run(ev.id, ev.button)) end,
	})`
	for _, tc := range []struct {
		name    string
		origin  lua.Origin
		stale   bool
		wantErr string
	}{
		{"user handler runs with the id", lua.OriginUser, false, ""},
		{"project handler keeps its origin", lua.OriginProject, false, "project code may not run /probe"},
		{"reload drops the click", lua.OriginUser, true, "reloaded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r := newTestHost(t)
			ran := make(chan string, 1)
			h.Commands.Add(&Command{Name: "probe", NArgs: "?", GoFn: func(_ *Host, args string) error { ran <- args; return nil }, Origin: lua.OriginBuiltin})
			if err := r.ExecuteChunk(t.Context(), widget, "w.lua", tc.origin); err != nil {
				t.Fatal(err)
			}
			lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("w"), 40, 10, UIState{})
			if err != nil {
				t.Fatal(err)
			}
			if got := lines[0]; got[0].Click != "probe" || got[0].Widget != "w" || got[1].Click != "" || got[1].Widget != "" {
				t.Fatalf("segments = %+v", got)
			}
			if tc.stale {
				h.gen.Add(1)
			}
			err = h.ClickWidget(t.Context(), "w", map[string]any{"id": "probe", "button": "left"})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if args := <-ran; args != "left" {
				t.Fatalf("command args = %q", args)
			}
		})
	}
}

// TestShortOutput: a long result keeps its first lines, which say what
// failed, and its last ones, with the drop counted between them.
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
}

// TestOverlayCallbackErrorsRecorded: a raising on_select or on_close reaches
// /errors, like a timer's or an event handler's.
func TestOverlayCallbackErrorsRecorded(t *testing.T) {
	h, r := newTestHost(t)
	var boom *glua.LFunction
	if _, err := r.Do(t.Context(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		boom = L.NewFunction(func(L *glua.LState) int { L.RaiseError("nope"); return 0 })
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	o := Overlay{ID: "x", Origin: lua.OriginUser, Gen: h.currentGen(), OnSelect: boom, OnClose: boom, Items: []any{"a"}}
	for source, run := range map[string]func(){
		"overlay x on_select": func() { h.SelectOverlay(t.Context(), o, 0) },
		"overlay x on_close":  func() { h.CancelOverlay(t.Context(), o) },
	} {
		h.errs.entries = nil
		run()
		if len(h.errs.entries) != 1 || h.errs.entries[0].source != source {
			t.Errorf("%s: errors = %+v", source, h.errs.entries)
		}
	}
}

// TestWorkingRuleNamesTheBusyJob: the composer rule names the newest job the
// user waits on, and a narrow one drops that name before the key that stops it
// and the clock that shows it alive.
func TestWorkingRuleNamesTheBusyJob(t *testing.T) {
	clock := &fakeClock{now: testEpoch}
	h := newTestHostUI(t, clock)
	defer h.Busy("Reflecting", func() {})()
	// the rule keeps its block away for the first 300ms of a job
	clock.set(clock.Now().Add(4 * time.Second))
	row := func(w int) string {
		lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("working"), w, 3, UIState{})
		if err != nil {
			t.Fatal(err)
		}
		return SegsText(lines[0])
	}
	if got := row(80); !strings.Contains(got, "reflecting · esc interrupt · 4s") {
		t.Errorf("the rule does not name the busy job: %q", got)
	}
	if got := row(34); strings.Contains(got, "reflecting") || !strings.Contains(got, "esc interrupt · 4s") {
		t.Errorf("the narrow rule lost the key or the clock: %q", got)
	}
}
