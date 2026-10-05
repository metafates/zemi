// plugintest.go is zemi.test, the namespace only a zemi plugins test boot
// registers (DESIGN §5): one throwaway zemi per case, on a fake clock, whose
// gated calls only test.stub answers, and drivers that run what the plugin
// registered the way zemi runs it.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// testEpoch is where a test zemi's clock starts, in UTC so os.date reads
// the same on every machine.
var testEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// PluginTest is one boot of zemi plugins test: the test file it evaluates,
// the case it runs, and what that case left for the report.
type PluginTest struct {
	// Cases are the file's zemi.test calls in order, filled as it runs.
	Cases []TestCase

	h      *Host
	clock  *fakeClock
	ui     *testUI
	plugin string
	file   string
	run    int             // the case this boot runs; -1 runs none
	fn     *glua.LFunction // its function

	goes    goes
	mu      sync.Mutex
	stubs   []*testStub // interpreter only
	errs    []errEntry  // what zemi recorded in /errors, in order
	log     []string    // driver errors and notes for a failure's report
	pending []string    // the running driver's notes, logged after its error
	prints  []string
	at      string // the running driver and its call, for a timeout
}

// TestCase is one zemi.test(name, fn) of a file.
type TestCase struct {
	Name string
	Line int
}

type testStub struct {
	subject, pattern string
	reply            glua.LValue
	requests         *glua.LTable
}

// NewPluginTest builds the zemi for one boot of a test file of a plugin
// under dirs: a runtime over classes on a fake clock that runs every task
// whole, and a host whose overlays wait for test.answer. It runs case run of
// the file, none when run is -1. Boot it with Step where init.lua would
// run, then Run it and Close the host.
func NewPluginTest(log *slog.Logger, classes lua.PathClasses, dirs config.Dirs, file string, run int) (*Host, *PluginTest) {
	clock := &fakeClock{now: testEpoch}
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes, RuntimeFS: zemiruntime.FS, Clock: clock, WholeTasks: true})
	h := NewHost(rt, log, config.NewStore(), dirs)
	t := &PluginTest{h: h, clock: clock, file: file, run: run, plugin: h.PluginOf(file)}
	t.ui = &testUI{h: h}
	h.test = t
	h.SetUIHooks(t.ui)
	rt.SetPrintHook(func(_, line string) {
		t.mu.Lock()
		t.prints = append(t.prints, line)
		t.mu.Unlock()
	})
	return h, t
}

// Step registers zemi.test and runs the test file as its plugin's code;
// with no file it only registers.
func (t *PluginTest) Step() lua.BootStep {
	return lua.BootStep{Name: "test file", Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
		tb := g.L.NewTable()
		for name, fn := range map[string]glua.LGFunction{"stub": t.stub, "overlay": t.overlay} {
			tb.RawSetString(name, g.L.NewFunction(fn))
		}
		asserts, err := r.Require(g, g.L, "zemi.test.assert")
		if err != nil {
			return err
		}
		rel := g.L.NewFunction(func(L *glua.LState) int {
			L.Push(glua.LString(t.Rel(L.CheckString(1))))
			return 1
		})
		if err := g.L.CallByParam(glua.P{Fn: asserts, NRet: 1, Protect: true}, rel); err != nil {
			return err
		}
		g.L.Get(-1).(*glua.LTable).ForEach(tb.RawSet)
		g.L.Pop(1)
		for name, fn := range map[string]glua.LGFunction{
			"run": t.runCmd, "keys": t.keys, "tool": t.tool, "render": t.render, "click": t.click,
			"emit": t.emit, "answer": t.answer, "advance": t.advance, "complete": t.complete, "set": t.set,
		} {
			tb.RawSetString(name, lua.Suspending(g.L, g.L.NewFunction(func(L *glua.LState) int {
				if t.h.RT.Booting() {
					raise(L, "test.%s: drivers run inside a case: test(\"name\", function() ... end)", name)
				}
				return fn(L)
			})))
		}
		if err := g.SB.RegisterNamespace("zemi.test", tb, callable(g.L, t.declare)); err != nil || t.file == "" {
			return err
		}
		return r.EvalFileInline(ctx, g.L, t.file, lua.OriginPlugin)
	}}
}

// Run settles what boot started, runs the case this boot is for within
// timeout, and returns its failure as report lines, nil when it passed.
// Paths in them are the plugin's own.
func (t *PluginTest) Run(ctx context.Context, timeout time.Duration) []string {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(msg string) []string {
		t.mu.Lock()
		defer t.mu.Unlock()
		// a log line goes when a line before it already holds its fact: a
		// driver's error, or a note whole
		lines := []string{t.Rel(msg)}
		for _, l := range slices.Concat(t.log, t.pending) {
			l = t.Rel(l)
			fact := l
			if _, err, ok := strings.Cut(l, " returned: "); ok && strings.HasPrefix(l, "test.") {
				fact = err
			}
			if !slices.ContainsFunc(lines, func(k string) bool { return strings.Contains(k, fact) }) {
				lines = append(lines, l)
			}
		}
		for _, p := range t.prints {
			lines = append(lines, "print: "+t.Rel(p))
		}
		return lines
	}
	timedOut := func() []string {
		t.mu.Lock()
		msg := "timed out after " + timeout.String()
		if t.at != "" {
			msg += " in " + t.at
		}
		t.mu.Unlock()
		return fail(msg)
	}
	if t.settle(ctx, t.clock.Now()) != nil {
		return timedOut()
	}
	if msg := t.unowned(0, "", "during boot"); msg != "" {
		return fail(msg)
	}
	t.mu.Lock()
	mark := len(t.errs)
	t.mu.Unlock()
	_, err := t.h.RT.RunTask(ctx, lua.Task{
		Class: lua.ClassIO, Origin: lua.OriginPlugin, Path: t.file,
		Start: func(*glua.LState) (*glua.LFunction, []glua.LValue, error) { return t.fn, nil, nil },
	})
	switch {
	case ctx.Err() != nil:
		return timedOut()
	case err != nil:
		return fail(luaErrText(err))
	case t.settle(ctx, t.clock.Now()) != nil:
		return timedOut()
	}
	if msg := t.unowned(mark, "", "after the case"); msg != "" {
		return fail(msg)
	}
	return nil
}

// Rel writes the paths of the plugin copy relative to the plugin and the
// test zemi's project as <project>, so a report names no temp dir.
func (t *PluginTest) Rel(s string) string {
	root := filepath.Dir(filepath.Dir(t.file))
	s = strings.ReplaceAll(s, "@plugin:"+root+"/", "")
	s = strings.ReplaceAll(s, root+"/", "")
	return strings.ReplaceAll(s, t.h.Dirs.ProjectDir, "<project>")
}

// declare is zemi.test(name, fn).
func (t *PluginTest) declare(L *glua.LState) int {
	name, fn := L.CheckString(1), L.CheckFunction(2)
	if !t.h.RT.Booting() {
		L.RaiseError("zemi.test: declare cases at the top level of the test file")
	}
	for _, c := range t.Cases {
		if c.Name == name {
			L.RaiseError("duplicate case name %q, first at line %d", name, c.Line)
		}
	}
	if len(t.Cases) == t.run {
		t.fn = fn
	}
	line := 0
	if dbg, ok := L.GetStack(1); ok {
		if _, err := L.GetInfo("l", dbg, glua.LNil); err == nil {
			line = dbg.CurrentLine
		}
	}
	t.Cases = append(t.Cases, TestCase{name, line})
	return 0
}

// quiet and the clock

// settle fires the timers due by until in order, waiting after each until
// no host goroutine runs, and moves the clock to until: zemi is quiet then.
// It stops once ctx is done.
func (t *PluginTest) settle(ctx context.Context, until time.Time) error {
	for {
		if err := cmp.Or(ctx.Err(), t.goes.wait(ctx)); err != nil {
			return err
		}
		f, ok := t.clock.due(until)
		if !ok {
			t.clock.set(until)
			return nil
		}
		f()
	}
}

func (t *PluginTest) advance(L *glua.LState) int {
	ms := float64(L.CheckNumber(1))
	if ms < 0 || math.IsNaN(ms) {
		raise(L, "test.advance: ms must not be negative")
	}
	// a Duration holds some 292 years; past that the conversion wraps on amd64
	ms = min(ms, float64(math.MaxInt64/int64(time.Millisecond)))
	return t.drive(L, "advance", "", func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		return none, t.settle(ctx, t.clock.Now().Add(time.Duration(ms*float64(time.Millisecond))))
	})
}

// goes counts the goroutines GoSafe runs in a test zemi: every async
// callback and handler reaches Lua through one, so it is quiet when none runs.
type goes struct {
	mu   sync.Mutex
	n    int
	none chan struct{} // closed when n drops to zero
}

func (g *goes) add() {
	g.mu.Lock()
	if g.n == 0 {
		g.none = make(chan struct{})
	}
	g.n++
	g.mu.Unlock()
}

func (g *goes) done() {
	g.mu.Lock()
	if g.n--; g.n == 0 {
		close(g.none)
	}
	g.mu.Unlock()
}

// wait returns once no GoSafe goroutine runs, or with ctx's error.
func (g *goes) wait(ctx context.Context) error {
	g.mu.Lock()
	n, none := g.n, g.none
	g.mu.Unlock()
	if n == 0 {
		return nil
	}
	select {
	case <-none:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fakeClock is a test zemi's lua.Clock: time moves only when test.advance
// moves it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer // by when they are due, then as they were set
}

type fakeTimer struct {
	at time.Time
	f  func()
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c.now.Add(d), f}
	i := slices.IndexFunc(c.timers, func(o *fakeTimer) bool { return o.at.After(t.at) })
	if i < 0 {
		i = len(c.timers)
	}
	c.timers = slices.Insert(c.timers, i, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		n := len(c.timers)
		c.timers = slices.DeleteFunc(c.timers, func(o *fakeTimer) bool { return o == t })
		return len(c.timers) < n
	}
}

// due takes the first timer due by until and moves the clock to its time.
func (c *fakeClock) due(until time.Time) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) == 0 || c.timers[0].at.After(until) {
		return nil, false
	}
	t := c.timers[0]
	c.timers = c.timers[1:]
	c.now = t.at
	return t.f, true
}

// drivers

// raiseError is an act's error that fails the case at the driver's call
// instead of reaching it as nil, err.
type raiseError string

func (e raiseError) Error() string { return string(e) }

// none is a driver's result with no values.
func none(*glua.LState) []glua.LValue { return nil }

// raise refuses misuse of a Suspending driver at the case's line: level 3
// passes the driver and its wrapper.
func raise(L *glua.LState, format string, args ...any) {
	L.Error(glua.LString(fmt.Sprintf(format, args...)), 3)
}

// drive runs a driver the case called: act runs off the interpreter while
// the case waits, then zemi settles. What act returns goes back as its
// values, true for none, or nil, err; an error zemi recorded meanwhile
// under any source but own, the one act's own failure goes under, fails
// the case at the call.
func (t *PluginTest) drive(L *glua.LState, name, own string, act func(ctx context.Context) (func(*glua.LState) []glua.LValue, error)) int {
	site := strings.TrimSuffix(L.Where(2), ":")
	call := "test." + name
	if s, ok := L.Get(1).(glua.LString); ok {
		call += "(" + luaLiteral(string(s)) + ")"
	} else if n, ok := L.Get(1).(glua.LNumber); ok {
		call += "(" + n.String() + ")"
	}
	return lua.Suspend(L, call, func(ctx context.Context) func(*glua.LState) []glua.LValue {
		t.mu.Lock()
		t.at, t.pending = call+" at "+site, nil
		mark := len(t.errs)
		t.mu.Unlock()
		push, err := act(ctx)
		if t.settle(ctx, t.clock.Now()) != nil {
			return none // timed out: the case does not resume
		}
		r, raised := errors.AsType[raiseError](err)
		t.mu.Lock()
		if err != nil && !raised {
			t.log = append(t.log, "test."+name+" at "+site+" returned: "+t.Rel(err.Error()))
		}
		t.log, t.pending, t.at = append(t.log, t.pending...), nil, ""
		t.mu.Unlock()
		msg := t.unowned(mark, own, "during test."+name+" at "+site)
		if raised && msg == "" {
			msg = site + ": " + string(r)
		}
		switch {
		case msg != "":
			return func(*glua.LState) []glua.LValue { return []glua.LValue{glua.LNil, glua.LNil, glua.LString(t.Rel(msg))} }
		case err != nil:
			return func(*glua.LState) []glua.LValue { return []glua.LValue{glua.LNil, glua.LString(t.Rel(err.Error()))} }
		case push == nil:
			return func(*glua.LState) []glua.LValue { return []glua.LValue{glua.LTrue} }
		}
		return push
	})
}

func (t *PluginTest) recorded(source, msg string) {
	t.mu.Lock()
	t.errs = append(t.errs, errEntry{source: source, msg: msg})
	t.mu.Unlock()
}

// unowned is the failure of the first error zemi recorded since mark under
// a source own does not name, or "".
func (t *PluginTest) unowned(mark int, own, when string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.errs[mark:] {
		if own == "" || !strings.HasPrefix(e.source, own) {
			first, _, _ := strings.Cut(e.msg, "\n")
			return "error in " + e.source + " " + when + ":\n" + first
		}
	}
	return ""
}

// note adds a line to the report, after the error of the driver running,
// once: a call a loop repeats says the same thing each time.
func (t *PluginTest) note(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case slices.Contains(t.log, line) || slices.Contains(t.pending, line):
	case t.at != "":
		t.pending = append(t.pending, line)
	default:
		t.log = append(t.log, line)
	}
}

func (t *PluginTest) mine(path string) bool { return t.h.PluginOf(path) == t.plugin }

// own is what of all the plugin registered under name, key naming each
// entry and whether it is the plugin's; for any other name it raises,
// listing the plugin's.
func (t *PluginTest) own[T any](L *glua.LState, driver, kind, name string, all []T, key func(T) (string, bool)) T {
	var names []string
	for _, x := range all {
		if k, ok := key(x); ok && k == name {
			return x
		} else if ok {
			names = append(names, k)
		}
	}
	raise(L, "test.%s: %s is not a %s of plugin %s; it registered %s", driver, name, kind, t.plugin, cmp.Or(strings.Join(names, ", "), "none"))
	var zero T
	return zero
}

// command is the plugin's Lua command name, which driver runs.
func (t *PluginTest) command(L *glua.LState, driver, name string) *Command {
	return t.own(L, driver, "command", "/"+name, t.h.Commands.List(), func(c *Command) (string, bool) {
		return "/" + c.Name, c.GoFn == nil && t.mine(c.Path)
	})
}

// runCmd is test.run(name [, args]): the plugin's Lua command as typed.
func (t *PluginTest) runCmd(L *glua.LState) int {
	c, args := t.command(L, "run", strings.TrimPrefix(L.CheckString(1), "/")), L.OptString(2, "")
	return t.drive(L, "run", "", func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		return nil, errText(t.h.RunCommand(ctx, c.Name, args))
	})
}

// set is test.set(key, value): one of the plugin's settings, named as its
// code names it, changed as the user changes it in /config, its watches
// firing.
func (t *PluginTest) set(L *glua.LState) int {
	s := t.own(L, "set", "setting", t.h.settingPath(L, L.CheckString(1)), t.h.Settings.Schema(), func(s config.Setting) (string, bool) {
		return s.Path, settingPlugin(s.Path) == t.plugin
	})
	v := luaToGoChecked(L, 2, "test.set")
	return t.drive(L, "set", "", func(context.Context) (func(*glua.LState) []glua.LValue, error) {
		return nil, t.h.SetConfig(s.Path, v)
	})
}

// keys is test.keys(seq): what the plugin bound a global key sequence to.
// NOTE: a { focus = id } binding is not reachable; keys needs a focused
// node for it, which only a drawn layout has.
func (t *PluginTest) keys(L *glua.LState) int {
	seq, err := t.h.keySeq(L.CheckString(1), false)
	if err != nil {
		raise(L, "test.keys: %v", err)
	}
	b := t.own(L, "keys", "Lua key binding", seq, t.h.Keymaps.List(), func(k *KeyBinding) (string, bool) {
		c := t.h.Commands.Lookup(k.RHS.Command)
		return k.Key, k.Focus == "" && t.mine(k.Path) && k.RHS.API == "" && (k.RHS.Command == "" || c != nil && c.GoFn == nil && t.mine(c.Path))
	})
	return t.drive(L, "keys", "keymap "+b.Key, func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		return nil, errText(t.h.RunRHS(ctx, b))
	})
}

// tool is test.tool(name, input): the model's call of the plugin's Lua tool,
// allowed as the user would allow it, tool.pre and tool.post included.
func (t *PluginTest) tool(L *glua.LState) int {
	name := L.CheckString(1)
	input := luaToGoChecked(L, 2, "test.tool")
	lt := t.own(L, "tool", "tool", name, t.h.luaTools.list(), func(x *luaTool) (string, bool) { return x.Name, t.mine(x.Path) })
	if input == nil {
		input = map[string]any{}
	}
	raw, err := json.Marshal(input, json.Deterministic(true))
	if err != nil {
		raise(L, "test.tool: input: %v", err)
	}
	return t.drive(L, "tool", "tool "+name, func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		pipe := t.h.PipelineFor()
		pipe.Ask = func(tools.Request) (bool, string, string) { return true, tools.ByUser, "" }
		reply, _ := (&luaToolAdapter{h: t.h, t: lt, pipe: pipe}).Run(ctx, fantasy.ToolCall{ID: "test", Name: name, Input: string(raw)})
		if reply.IsError {
			return nil, errors.New(reply.Content)
		}
		return func(*glua.LState) []glua.LValue { return []glua.LValue{glua.LString(reply.Content)} }, nil
	})
}

func (t *PluginTest) widget(L *glua.LState, driver string) *Widget {
	return t.own(L, driver, "widget", L.CheckString(1), t.h.Widgets.List(), func(w *Widget) (string, bool) { return w.ID, t.mine(w.Path) })
}

// render is test.render(id [, ctx]): the widget's lines as text, then as
// segments, drawn the way a frame draws them with ctx's fields over the
// test zemi's.
// NOTE: one widget at a time; the whole screen's layout is the goldens'.
func (t *PluginTest) render(L *glua.LState) int {
	w := t.widget(L, "render")
	over := L.OptTable(2, nil)
	width, height := 80, 1
	if over != nil {
		if n, ok := opt[glua.LNumber](L, over, "width"); ok {
			width = int(n)
		}
		if n, ok := opt[glua.LNumber](L, over, "height"); ok {
			height = int(n)
		}
	}
	return t.drive(L, "render", "", func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		lines, _, err := t.h.renderWidget(ctx, w, width, height, UIState{Time: t.h.now().Format(ClockLayout)}, over)
		if err != nil {
			return nil, raiseError("render " + w.ID + ": " + luaErrText(err))
		}
		return func(L *glua.LState) []glua.LValue {
			text := make([]string, len(lines))
			for i, line := range lines {
				text[i] = SegsText(line)
			}
			return []glua.LValue{lua.FromGo(L, text), linesTable(L, lines)}
		}, nil
	})
}

// click is test.click(id, click): the widget's on_click for a segment's
// click id, as a left click.
func (t *PluginTest) click(L *glua.LState) int {
	w := t.widget(L, "click")
	id := L.CheckString(2)
	if w.OnClick == nil {
		raise(L, "test.click: widget %s has no on_click", w.ID)
	}
	ev := map[string]any{"id": id, "x": 0, "y": 0, "clicks": 1, "button": "left", "mods": map[string]any{"shift": false, "ctrl": false, "alt": false}}
	return t.drive(L, "click", "", func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		return nil, errText(t.h.ClickWidget(ctx, w.ID, ev))
	})
}

// emit is test.emit(name [, ev]): a host event delivered to the plugin's
// own handlers the way the host delivers it.
func (t *PluginTest) emit(L *glua.LState) int {
	name := L.CheckString(1)
	ev, _ := luaToGoChecked(L, 2, "test.emit").(map[string]any)
	if !eventCatalogue[name] {
		raise(L, "test.emit: %q is not a host event; zemi.event.emit sends user.* events", name)
	}
	tool, _ := ev["tool"].(string)
	handlers := func(event string) []*EventHandler {
		hs := t.h.Events.Handlers(event)
		if event == "tool.pre" || event == "tool.post" {
			hs = t.h.hookHandlers(event, tool)
		}
		return slices.DeleteFunc(hs, func(hd *EventHandler) bool { return !t.mine(hd.Path) })
	}
	t.own(L, "emit", "handled event", name, slices.Sorted(maps.Keys(eventCatalogue)), func(e string) (string, bool) { return e, len(handlers(e)) > 0 })
	hs := handlers(name)
	input, _ := ev["input"].(map[string]any)
	return t.drive(L, "emit", "", func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		veto := func(decision, reason string, input map[string]any) func(*glua.LState) []glua.LValue {
			return func(L *glua.LState) []glua.LValue {
				if decision == "" {
					return []glua.LValue{glua.LNil}
				}
				return []glua.LValue{lua.FromGo(L, map[string]any{"decision": decision, "reason": reason, "input": input})}
			}
		}
		switch name {
		case "tool.post":
			var changed []string
			if list, ok := ev["changed"].([]any); ok {
				for _, c := range list {
					changed = append(changed, fmt.Sprint(c))
				}
			}
			output, _ := ev["output"].(string)
			ms, _ := ev["ms"].(float64)
			out := t.h.postChain(ctx, hs, "", tool, input, ev["ok"] != false, output, changed, time.Duration(ms)*time.Millisecond)
			return func(*glua.LState) []glua.LValue { return []glua.LValue{glua.LString(out)} }, nil
		case "tool.pre":
			d, reason, rewritten, _ := t.h.toolPre(tool, input, hs)
			return veto(string(d), reason, rewritten), nil
		case "permission.request", "app.quit", "composer.submit":
			if res := t.h.emitVetoable(ctx, name, hs, ev); res.Denied {
				return veto("deny", res.Reason, nil), nil
			}
			return veto("", "", nil), nil
		}
		_, err := t.h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			t.h.recordEvent(name, "host", ev)
			t.h.emitSync(L, hs, name, ev, "host")
			return nil, nil
		})
		return none, err
	})
}

// overlay is test.overlay(): the top open overlay, nil when none is.
func (t *PluginTest) overlay(L *glua.LState) int {
	o, ok := t.ui.top()
	if !ok {
		L.Push(glua.LNil)
		return 1
	}
	L.Push(lua.FromGo(L, map[string]any{"id": o.ID, "title": o.Title, "body": o.Body, "items": itemTexts(o), "input": o.Input}))
	return 1
}

// itemTexts are the rows the user reads.
func itemTexts(o Overlay) []string {
	texts := make([]string, len(o.Items))
	for i, item := range o.Items {
		if i < len(o.ItemText) && o.ItemText[i] != nil {
			texts[i] = SegsText(o.ItemText[i])
		} else {
			p, _ := ParseOverlayItem(item)
			texts[i] = p.Text
		}
	}
	return texts
}

// answer is test.answer(choice [, action]): enter on an item (its index or
// text) or on a detail action (its id or text), the input's text, or esc
// for nil. The overlay closes first, as the UI closes it, and a row no
// callback takes runs its run, the plugin's command.
// NOTE: a list key and a toggle list's reload are not driven here.
func (t *PluginTest) answer(L *glua.LState) int {
	o, ok := t.ui.top()
	if !ok {
		raise(L, "test.answer: no overlay is open")
	}
	what, fn, args := "on_close", o.OnClose, []any(nil)
	switch c := L.Get(1).(type) {
	case *glua.LNilType:
	case glua.LString:
		texts := itemTexts(o)
		i := slices.Index(texts, string(c))
		switch {
		case o.Input:
			args = []any{string(c)}
		case i < 0:
			for j := range texts {
				texts[j] = luaLiteral(texts[j])
			}
			raise(L, "test.answer: no item %q in %s; it lists %s", string(c), o.ID, strings.Join(texts, ", "))
		default:
			args = []any{o.Items[i]}
		}
	case glua.LNumber:
		i := int(c) - 1
		if o.Input || i < 0 || i >= len(o.Items) {
			raise(L, "test.answer: %s has no item %d", o.ID, int(c))
		}
		args = []any{o.Items[i]}
	default:
		raise(L, "test.answer: choice must be an item's index or text, the input's text, or nil for esc")
	}
	var row OverlayItem
	if args != nil {
		what, fn = "on_select", o.OnSelect
		row, _ = ParseOverlayItem(args[0])
	}
	if a := L.Get(2); a != glua.LNil && args != nil {
		i := slices.IndexFunc(row.Actions, func(x any) bool {
			ap, _ := ParseOverlayItem(x)
			return ap.ID == a.String() || ap.Text == a.String()
		})
		if i < 0 {
			raise(L, "test.answer: the item has no action %q", a.String())
		}
		args = append(args, row.Actions[i])
		row, _ = ParseOverlayItem(row.Actions[i])
		what, fn = "on_action", o.OnAction
		if row.ID == "" {
			fn = nil // the UI sends on_action only an action with an id
		}
	}
	act := func(ctx context.Context) error { return t.h.callOverlay(ctx, o, what, fn, args...) }
	if fn == nil && args != nil {
		if row.Run == "" {
			raise(L, "test.answer: overlay %s has no %s, and the row no run", o.ID, what)
		}
		name, cmdArgs := SplitCommand(row.Run)
		c := t.command(L, "answer", name)
		act = func(ctx context.Context) error { return t.h.RunCommand(ctx, c.Name, cmdArgs) }
	}
	return t.drive(L, "answer", "overlay "+o.ID, func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		t.ui.close(o.ID)
		return nil, errText(act(ctx))
	})
}

// complete is test.complete(line): the texts of the rows the plugin offers
// for a draft line, cursor at its end, as the palette and the completion
// popup ask for them. A "/command" line asks the plugin's command and the
// sources for it; any other line the source whose trigger starts its last word.
func (t *PluginTest) complete(L *glua.LState) int {
	line := L.CheckString(1)
	lead := line[strings.LastIndexAny(line, " \t\n")+1:]
	trigger, command := "", ""
	if name, ok := strings.CutPrefix(line, "/"); ok {
		name, _, _ = strings.Cut(name, " ")
		command = t.command(L, "complete", name).Name
	} else {
		t.h.completions.mu.Lock()
		srcs := slices.Clone(t.h.completions.list)
		t.h.completions.mu.Unlock()
		r, size := utf8.DecodeRuneInString(lead)
		trigger = t.own(L, "complete", "completion trigger", string(r), srcs, func(s completionSource) (string, bool) {
			return s.trigger, s.trigger != "" && t.mine(s.path)
		}).trigger
		lead = lead[size:]
	}
	return t.drive(L, "complete", "", func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		var items []CompletionItem
		if command != "" {
			items = t.h.CompleteCommand(ctx, command, lead, line)
		}
		items = append(items, t.h.Complete(ctx, trigger, command, lead, line)...)
		texts := []string{}
		for _, it := range items {
			texts = append(texts, it.Text)
		}
		return func(L *glua.LState) []glua.LValue { return []glua.LValue{lua.FromGo(L, texts)} }, nil
	})
}

// stubs

var httpSubject = regexp.MustCompile(`^lua\.http\.[a-z]+$`)

// stubKind is the kind of gated call subject names that test.stub
// answers, or "".
func stubKind(subject string) string {
	switch {
	case httpSubject.MatchString(subject):
		return "http"
	case subject == "lua.job.start":
		return "job"
	case subject == "lua.model.ask":
		return "model"
	case subject == "lua.agent.send":
		return "send"
	case subject == "lua.fs.write":
		return "write"
	}
	return ""
}

// stub is test.stub(subject [, pattern [, reply]]).
func (t *PluginTest) stub(L *glua.LState) int {
	subject := L.CheckString(1)
	kind := stubKind(subject)
	if kind == "" {
		L.RaiseError("test.stub: %q is not a call a stub answers: lua.http.<method>, lua.job.start, lua.model.ask, lua.agent.send or lua.fs.write", subject)
	}
	pattern, reply := L.OptString(2, "*"), L.Get(3)
	switch reply.(type) {
	case *glua.LNilType, *glua.LTable, *glua.LFunction:
	case glua.LBool:
		if reply == glua.LTrue {
			L.RaiseError("test.stub: reply is its result fields, a function returning them, or false to refuse")
		}
	case glua.LString:
		if kind != "model" {
			L.RaiseError("test.stub: only a lua.model.ask reply may be a string")
		}
	default:
		L.RaiseError("test.stub: reply is its result fields, a function returning them, or false to refuse")
	}
	if msg := checkReply(kind, reply); msg != "" {
		L.RaiseError("test.stub: %s", msg)
	}
	s := &testStub{subject: subject, pattern: pattern, reply: reply, requests: L.NewTable()}
	t.stubs = append(t.stubs, s)
	L.Push(s.requests)
	return 1
}

// stubFields are the fields a reply to each kind of call takes, and their
// types: the rest would be dropped without a word, so a typo passes.
var stubFields = map[string]map[string]glua.LValueType{
	"http":  {"status": glua.LTNumber, "body": glua.LTString, "headers": glua.LTTable, "error": glua.LTString},
	"job":   {"code": glua.LTNumber, "stdout": glua.LTString, "stderr": glua.LTString},
	"model": {"text": glua.LTString, "error": glua.LTString},
	"send":  {},
	"write": {},
}

// checkReply is why a reply table cannot answer a call of kind, or "".
func checkReply(kind string, reply glua.LValue) string {
	tb, ok := reply.(*glua.LTable)
	if !ok {
		return ""
	}
	fields := stubFields[kind]
	takes := cmp.Or(strings.Join(slices.Sorted(maps.Keys(fields)), ", "), "no fields")
	var why []string // sorted, so a reply with two gets the same one each run
	tb.ForEach(func(k, v glua.LValue) {
		want, ok := fields[k.String()]
		if _, str := k.(glua.LString); !str || !ok {
			why = append(why, fmt.Sprintf("%s is not a field of a %s reply, which takes %s", k, kind, takes))
		} else if v.Type() != want {
			why = append(why, fmt.Sprintf("%s must be a %s, not a %s", k, want, v.Type()))
		}
	})
	if len(why) == 0 {
		return ""
	}
	slices.Sort(why)
	return why[0]
}

// stubReply stands in for a call's transport: the reply's fields over its
// kind's defaults, or the transport's failure its error field names.
type stubReply struct {
	fields map[string]any
	err    error
}

// reply is the reply of the newest stub matching a gated call, and whether
// the call is one a stub answers; nil and true is a false reply, which
// refuses like a missing rule. A call no stub answers raises naming the
// stub that would.
// NOTE: a stub answers at once, so a handle's timeout goes untested; a
// reply that waits for test.advance would reach it.
func (t *PluginTest) reply(L *glua.LState, subject, arg string, req func() map[string]any) (*stubReply, bool) {
	kind := stubKind(subject)
	if kind == "" {
		return nil, false
	}
	for _, s := range slices.Backward(t.stubs) {
		if s.subject != subject || !perm.Match(s.pattern, arg) {
			continue
		}
		reqTb := lua.FromGo(L, req())
		reply := s.reply
		if fn, ok := reply.(*glua.LFunction); ok {
			L.Push(fn)
			L.Push(reqTb)
			L.Call(1, 1)
			reply = L.Get(-1)
			L.Pop(1)
		}
		if reply == glua.LFalse {
			return nil, true
		}
		if msg := checkReply(kind, reply); msg != "" {
			t.note("test.stub reply: " + msg)
			L.RaiseError("test.stub reply: %s", msg)
		}
		s.requests.Append(reqTb)
		out := map[string]any{}
		switch kind {
		case "http":
			out = map[string]any{"status": 200.0, "body": "", "headers": map[string]any{}}
		case "job":
			out = map[string]any{"code": 0.0, "stdout": "", "stderr": ""}
		}
		switch r := reply.(type) {
		case glua.LString:
			out["text"] = string(r)
		case *glua.LTable:
			fields, _ := lua.ToGo(r)
			if m, ok := fields.(map[string]any); ok {
				maps.Copy(out, m)
			}
		}
		if e, _ := out["error"].(string); e != "" {
			return &stubReply{err: errors.New(e)}, true
		}
		return &stubReply{fields: out}, true
	}
	// noted too, so a pcall in the plugin that swallows the raise still
	// leaves the fix in the report
	msg := subject + " " + arg + ": no test.stub answers it; add " + stubLine(kind, subject, arg, req())
	t.note(msg)
	L.RaiseError("%s", msg)
	return nil, true
}

// stubLine is the test.stub call that would answer a call.
func stubLine(kind, subject, arg string, req map[string]any) string {
	switch kind {
	case "http":
		pattern := "*"
		if u, err := url.Parse(arg); err == nil && u.Host != "" {
			pattern = u.Scheme + "://" + u.Host + "/*"
		}
		return fmt.Sprintf(`test.stub(%s, %s, { status = 200, body = "..." })`, luaLiteral(subject), luaLiteral(pattern))
	case "job":
		pattern, _ := req["cmd"].(string)
		if args, _ := req["args"].([]string); len(args) > 0 {
			pattern += " *"
		}
		return `test.stub("lua.job.start", ` + luaLiteral(pattern) + `, { stdout = "..." })`
	case "model":
		return `test.stub("lua.model.ask", "*", "...")`
	}
	return "test.stub(" + luaLiteral(subject) + ")"
}

// the test zemi's UI

// testUI is the UIHooks of a test zemi: it keeps the overlays for
// test.overlay and test.answer and asks nobody anything.
type testUI struct {
	h        *Host
	mu       sync.Mutex
	overlays []Overlay
}

func (u *testUI) top() (Overlay, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.overlays) == 0 {
		return Overlay{}, false
	}
	return u.overlays[len(u.overlays)-1], true
}

func (u *testUI) close(id string) {
	u.mu.Lock()
	u.overlays = slices.DeleteFunc(u.overlays, func(o Overlay) bool { return o.ID == id })
	u.mu.Unlock()
}

func (u *testUI) OpenOverlay(o Overlay) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if i := slices.IndexFunc(u.overlays, func(x Overlay) bool { return x.ID == o.ID }); i >= 0 {
		u.overlays[i] = o
	} else if !o.Refresh {
		u.overlays = append(u.overlays, o)
	}
}

func (u *testUI) RefreshOverlay(id string, build func() Overlay) {
	u.mu.Lock()
	open := slices.ContainsFunc(u.overlays, func(o Overlay) bool { return o.ID == id })
	u.mu.Unlock()
	if open {
		o := build()
		o.Refresh = true
		u.OpenOverlay(o)
	}
}

// CloseOverlay runs the overlay's on_close off the interpreter, as the UI's
// worker does.
func (u *testUI) CloseOverlay(id string, origin lua.Origin) {
	u.mu.Lock()
	i := slices.IndexFunc(u.overlays, func(o Overlay) bool { return o.ID == id })
	if i < 0 || origin != lua.OriginUser && origin != lua.OriginBuiltin && origin != u.overlays[i].Origin {
		u.mu.Unlock()
		return
	}
	o := u.overlays[i]
	u.overlays = slices.Delete(u.overlays, i, i+1)
	u.mu.Unlock()
	u.h.GoSafe("overlay close", func() { u.h.CancelOverlay(context.Background(), o) })
}

func (u *testUI) CloseLuaOverlays() {
	u.mu.Lock()
	u.overlays = slices.DeleteFunc(u.overlays, func(o Overlay) bool { return o.Gen != 0 })
	u.mu.Unlock()
}

func (*testUI) Notify(slog.Level, string, string)                   {}
func (*testUI) Refresh()                                            {}
func (*testUI) Scroll(ScrollAction, string)                         {}
func (*testUI) LayoutRects() map[string]Rect                        { return nil }
func (*testUI) KeyHelp() ([][2]string, [][2]string)                 { return nil, nil }
func (*testUI) AgentStream()                                        {}
func (*testUI) TurnDone(string, bool)                               {}
func (*testUI) AskPermission(PermRequest) (bool, string)            { return false, tools.ByUser }
func (*testUI) Screenshot() string                                  { return "" }
func (*testUI) AskQuestion([]tools.Question) ([]tools.Answer, bool) { return nil, false }
func (*testUI) AskPlan(string, string) (tools.PlanVerdict, bool) {
	return tools.PlanVerdict{}, false
}
func (*testUI) ComposerEdit(ComposerOp, string, int) {}
func (*testUI) EditFile(context.Context, EditRequest) error {
	return errors.New("a test zemi has no editor")
}
func (*testUI) Copy(string)     {}
func (*testUI) Relaunch(string) {}
