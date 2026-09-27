// plugintest.go is zemi.test, the namespace only a zemi plugins test boot
// registers (DESIGN §5): one throwaway zemi per case, on a fake clock, whose
// gated calls only test.stub answers, and drivers that run what the plugin
// registered the way zemi runs it.
package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
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
func NewPluginTest(log *logging.Logger, classes lua.PathClasses, dirs config.Dirs, file string, run int) (*Host, *PluginTest) {
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
		for name, fn := range map[string]glua.LGFunction{"eq": t.eq, "stub": t.stub, "overlay": t.overlay} {
			tb.RawSetString(name, g.L.NewFunction(fn))
		}
		for name, fn := range map[string]glua.LGFunction{
			"run": t.runCmd, "keys": t.keys, "tool": t.tool, "render": t.render, "click": t.click,
			"emit": t.emit, "answer": t.answer, "advance": t.advance,
		} {
			tb.RawSetString(name, lua.Suspending(g.L, g.L.NewFunction(fn)))
		}
		if err := g.SB.RegisterCallableNamespace("zemi.test", tb, g.L.NewFunction(t.declare)); err != nil || t.file == "" {
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
		lines := slices.Concat([]string{msg}, t.log, t.pending)
		for _, p := range t.prints {
			lines = append(lines, "print: "+p)
		}
		for i, l := range lines {
			lines[i] = t.Rel(l)
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
	if t.settle(ctx) != nil {
		return timedOut()
	}
	if msg := t.unowned(0, "", "during boot"); msg != "" {
		return fail(msg)
	}
	mark := t.recordedLen()
	_, err := t.h.RT.RunTask(ctx, lua.Task{
		Class: lua.ClassIO, Origin: lua.OriginPlugin, Path: t.file,
		Start: func(*glua.LState) (*glua.LFunction, []glua.LValue, error) { return t.fn, nil, nil },
	})
	switch {
	case ctx.Err() != nil:
		return timedOut()
	case err != nil:
		return fail(luaErrText(err))
	case t.settle(ctx) != nil:
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
	name, fn := L.CheckString(2), L.CheckFunction(3)
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

// ---------------------------------------------------------------------------
// quiet and the clock

// settle waits until zemi is quiet: no host goroutine runs and no timer is
// due at the fake now, the due ones fired in order; never once ctx is done.
func (t *PluginTest) settle(ctx context.Context) error {
	for {
		if err := cmp.Or(ctx.Err(), t.h.goes.wait(ctx)); err != nil {
			return err
		}
		f, ok := t.clock.due(t.clock.Now())
		if !ok {
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
		until := t.clock.Now().Add(time.Duration(ms * float64(time.Millisecond)))
		for t.settle(ctx) == nil {
			f, ok := t.clock.due(until)
			if !ok {
				t.clock.set(until)
				break
			}
			f()
		}
		return none, nil
	})
}

// fakeClock is a test zemi's lua.Clock: time moves only when test.advance
// moves it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    int
	timers []*fakeTimer
}

type fakeTimer struct {
	at  time.Time
	seq int
	f   func()
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
	c.seq++
	t := &fakeTimer{c.now.Add(d), c.seq, f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		n := len(c.timers)
		c.timers = slices.DeleteFunc(c.timers, func(o *fakeTimer) bool { return o == t })
		return len(c.timers) < n
	}
}

// due takes the earliest timer due by until, the first set of those due
// together, and moves the clock to its time.
func (c *fakeClock) due(until time.Time) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := -1
	for j, t := range c.timers {
		if !t.at.After(until) && (i < 0 || t.at.Before(c.timers[i].at) || t.at.Equal(c.timers[i].at) && t.seq < c.timers[i].seq) {
			i = j
		}
	}
	if i < 0 {
		return nil, false
	}
	t := c.timers[i]
	c.timers = slices.Delete(c.timers, i, i+1)
	c.now = t.at
	return t.f, true
}

// ---------------------------------------------------------------------------
// drivers

// raiseErr is an act's error that fails the case at the driver's call
// instead of reaching it as nil, err.
type raiseErr string

func (e raiseErr) Error() string { return string(e) }

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
		if t.settle(ctx) != nil {
			return none // timed out: the case does not resume
		}
		r, raised := errors.AsType[raiseErr](err)
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

func (t *PluginTest) recordedLen() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.errs)
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

// note adds a line to the report, after the error of the driver running.
func (t *PluginTest) note(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.at != "" {
		t.pending = append(t.pending, line)
	} else {
		t.log = append(t.log, line)
	}
}

// luaErrText is a Lua error's message without its traceback.
func luaErrText(err error) string {
	if ae, ok := errors.AsType[*glua.ApiError](err); ok {
		return ae.Object.String()
	}
	return err.Error()
}

func (t *PluginTest) mine(path string) bool { return t.h.PluginOf(path) == t.plugin }

// refuse raises for a target the plugin under test did not register,
// naming the ones it did.
func (t *PluginTest) refuse(L *glua.LState, driver, target, kind string, own []string) {
	has := "none"
	if len(own) > 0 {
		has = strings.Join(own, ", ")
	}
	raise(L, "test.%s: %s is not a %s of plugin %s; it registered %s", driver, target, kind, t.plugin, has)
}

func (t *PluginTest) command(name string) *Command {
	if c := t.h.Commands.Lookup(name); c != nil && c.GoFn == nil && t.mine(c.Path) {
		return c
	}
	return nil
}

func (t *PluginTest) commands() (out []string) {
	for _, c := range t.h.Commands.List() {
		if t.command(c.Name) != nil {
			out = append(out, "/"+c.Name)
		}
	}
	return out
}

// runCmd is test.run(name [, args]): the plugin's Lua command as typed.
func (t *PluginTest) runCmd(L *glua.LState) int {
	name, args := strings.TrimPrefix(L.CheckString(1), "/"), L.OptString(2, "")
	if t.command(name) == nil {
		t.refuse(L, "run", "/"+name, "command", t.commands())
	}
	return t.drive(L, "run", "", func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		return nil, errText(t.h.RunCommand(ctx, name, args))
	})
}

// errText drops a Lua error's traceback, which names Go and temp frames.
func errText(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(luaErrText(err))
}

// keys is test.keys(seq): what the plugin bound a global key sequence to.
// NOTE: a { focus = id } binding is not reachable; keys needs a focused
// node for it, which only a drawn layout has.
func (t *PluginTest) keys(L *glua.LState) int {
	seq, err := t.h.keySeq(L.CheckString(1), false)
	if err != nil {
		raise(L, "test.keys: %v", err)
	}
	b := t.h.Keymaps.Lookup("", seq)
	if b == nil || !t.mine(b.Path) || b.RHS.API != "" || b.RHS.Command != "" && t.command(b.RHS.Command) == nil {
		var own []string
		for _, k := range t.h.Keymaps.List() {
			if k.Focus == "" && t.mine(k.Path) && k.RHS.API == "" {
				own = append(own, k.Key)
			}
		}
		t.refuse(L, "keys", seq, "Lua key binding", own)
	}
	return t.drive(L, "keys", "keymap "+b.Key, func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		return nil, errText(t.h.RunRHS(ctx, b))
	})
}

// tool is test.tool(name, input): the model's call of the plugin's Lua tool,
// allowed as the user would allow it, tool.pre and tool.post included.
func (t *PluginTest) tool(L *glua.LState) int {
	name := L.CheckString(1)
	input := luaToGoChecked(L, 2, "test.tool")
	var lt *luaTool
	var own []string
	for _, x := range t.h.luaTools.list() {
		if t.mine(x.Path) {
			own = append(own, x.Name)
			if x.Name == name {
				lt = x
			}
		}
	}
	if lt == nil {
		t.refuse(L, "tool", name, "tool", own)
	}
	if input == nil {
		input = map[string]any{}
	}
	raw, err := json.Marshal(input)
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
	id := L.CheckString(1)
	w := t.h.Widgets.Get(id)
	if w == nil || !t.mine(w.Path) {
		var own []string
		for _, x := range t.h.Widgets.List() {
			if t.mine(x.Path) {
				own = append(own, x.ID)
			}
		}
		t.refuse(L, driver, id, "widget", own)
	}
	return w
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
		if n, ok := over.RawGetString("width").(glua.LNumber); ok {
			width = int(n)
		}
		if n, ok := over.RawGetString("height").(glua.LNumber); ok {
			height = int(n)
		}
	}
	return t.drive(L, "render", "", func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		lines, _, err := t.h.renderWidget(ctx, w, width, height, UIState{Time: t.h.now().Format("15:04")}, over)
		if err != nil {
			return nil, raiseErr("render " + w.ID + ": " + luaErrText(err))
		}
		return func(L *glua.LState) []glua.LValue {
			text := L.CreateTable(len(lines), 0)
			for _, line := range lines {
				var b strings.Builder
				for _, s := range line {
					b.WriteString(s.Text)
				}
				text.Append(glua.LString(b.String()))
			}
			return []glua.LValue{text, linesTable(L, lines)}
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
	mine := func(hs []*EventHandler) []*EventHandler {
		return slices.DeleteFunc(hs, func(hd *EventHandler) bool { return !t.mine(hd.Path) })
	}
	handlers := mine(t.h.Events.Handlers(name))
	if name == "tool.pre" || name == "tool.post" {
		handlers = mine(t.h.hookHandlers(name, tool))
	}
	if len(handlers) == 0 {
		var own []string
		for e := range eventCatalogue {
			if len(mine(t.h.Events.Handlers(e))) > 0 {
				own = append(own, e)
			}
		}
		slices.Sort(own)
		t.refuse(L, "emit", name, "handled event", own)
	}
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
			out := t.h.postChain(ctx, handlers, "", tool, input, ev["ok"] != false, output, changed, time.Duration(ms)*time.Millisecond)
			return func(*glua.LState) []glua.LValue { return []glua.LValue{glua.LString(out)} }, nil
		case "tool.pre":
			d, reason, rewritten, _ := t.h.toolPre(tool, input, handlers)
			return veto(string(d), reason, rewritten), nil
		case "permission.request", "app.quit", "composer.submit":
			if res := t.h.emitVetoable(ctx, name, handlers, ev, lua.VetoDeadline); res.Denied {
				return veto("deny", res.Reason, nil), nil
			}
			return veto("", "", nil), nil
		}
		_, err := t.h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			t.h.recordEvent(name, "host", ev)
			t.h.emitSync(L, handlers, name, ev, "host")
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
	items := L.CreateTable(len(o.Items), 0)
	for i := range o.Items {
		items.Append(glua.LString(itemText(o, i)))
	}
	tb := L.CreateTable(0, 5)
	tb.RawSetString("id", glua.LString(o.ID))
	tb.RawSetString("title", glua.LString(o.Title))
	tb.RawSetString("body", glua.LString(o.Body))
	tb.RawSetString("items", items)
	tb.RawSetString("input", glua.LBool(o.Input))
	L.Push(tb)
	return 1
}

// itemText is the row the user reads for item i.
func itemText(o Overlay, i int) string {
	if i < len(o.ItemText) && o.ItemText[i] != nil {
		var b strings.Builder
		for _, s := range o.ItemText[i] {
			b.WriteString(s.Text)
		}
		return b.String()
	}
	p, _ := ParseOverlayItem(o.Items[i])
	return p.Text
}

// answer is test.answer(choice [, action]): enter on an item (its index or
// text) or on a detail action (its id or text), the input's text, or esc
// for nil. The overlay closes first, as the UI closes it.
func (t *PluginTest) answer(L *glua.LState) int {
	o, ok := t.ui.top()
	if !ok {
		raise(L, "test.answer: no overlay is open")
	}
	var item, action any
	what, fn := "on_select", o.OnSelect
	choice := L.Get(1)
	switch c := choice.(type) {
	case *glua.LNilType:
		what, fn = "on_close", o.OnClose
	case glua.LString:
		if o.Input {
			item = string(c)
			break
		}
		i := -1
		for j := range o.Items {
			if itemText(o, j) == string(c) {
				i = j
				break
			}
		}
		if i < 0 {
			texts := make([]string, len(o.Items))
			for j := range o.Items {
				texts[j] = luaLiteral(itemText(o, j))
			}
			raise(L, "test.answer: no item %q in %s; it lists %s", string(c), o.ID, strings.Join(texts, ", "))
		}
		item = o.Items[i]
	case glua.LNumber:
		i := int(c) - 1
		if o.Input || i < 0 || i >= len(o.Items) {
			raise(L, "test.answer: %s has no item %d", o.ID, int(c))
		}
		item = o.Items[i]
	default:
		raise(L, "test.answer: choice must be an item's index or text, the input's text, or nil for esc")
	}
	if a := L.Get(2); a != glua.LNil && choice != glua.LNil {
		p, _ := ParseOverlayItem(item)
		want := a.String()
		i := slices.IndexFunc(p.Actions, func(x any) bool {
			ap, _ := ParseOverlayItem(x)
			return ap.ID == want || ap.Text == want
		})
		if i < 0 {
			raise(L, "test.answer: the item has no action %q", want)
		}
		what, fn, action = "on_action", o.OnAction, p.Actions[i]
	}
	if fn == nil && what != "on_close" {
		// NOTE: a row's run, a list key and a toggle list's reload are not driven here
		raise(L, "test.answer: overlay %s has no %s; test.run runs what a row's run names", o.ID, what)
	}
	return t.drive(L, "answer", "overlay "+o.ID, func(ctx context.Context) (func(*glua.LState) []glua.LValue, error) {
		t.ui.close(o.ID)
		args := []any{item}
		switch {
		case what == "on_close":
			args = nil
		case action != nil:
			args = append(args, action)
		}
		return nil, errText(t.h.callOverlay(ctx, o, what, fn, args...))
	})
}

// ---------------------------------------------------------------------------
// eq

// eq is test.eq(got, want): a deep compare of Lua values, functions and
// other references by identity.
func (t *PluginTest) eq(L *glua.LState) int {
	if L.GetTop() < 2 {
		L.RaiseError("test.eq: takes the value got and the value wanted")
	}
	got, want := L.Get(1), L.Get(2)
	path, g, w, same := diff(got, want, "", map[[2]*glua.LTable]bool{})
	if same {
		return 0
	}
	msg := "got " + literal(g, "", map[*glua.LTable]bool{}) + ", want " + literal(w, "", map[*glua.LTable]bool{})
	if gs, ok := g.(glua.LString); ok {
		if ws, ok := w.(glua.LString); ok && max(len(gs), len(ws)) > 40 {
			// Rel before the cut, which could leave half a temp path
			msg = window(t.Rel(string(gs)), t.Rel(string(ws)))
		}
	}
	if path != "" {
		msg = strings.TrimPrefix(path, ".") + ": " + msg
		gl, wl := literal(got, "", map[*glua.LTable]bool{}), literal(want, "", map[*glua.LTable]bool{})
		if strings.Count(gl, "\n") < 10 && strings.Count(wl, "\n") < 10 {
			msg += "\ngot  " + gl + "\nwant " + wl
		}
	}
	L.RaiseError("%s", msg)
	return 0
}

// diff finds the first path where got and want differ, tables walked in key
// order with the pairs already compared taken as equal, so cycles end.
func diff(got, want glua.LValue, path string, seen map[[2]*glua.LTable]bool) (string, glua.LValue, glua.LValue, bool) {
	gt, gok := got.(*glua.LTable)
	wt, wok := want.(*glua.LTable)
	if gok && wok {
		if seen[[2]*glua.LTable{gt, wt}] {
			return "", nil, nil, true
		}
		seen[[2]*glua.LTable{gt, wt}] = true
		for _, k := range sortedKeys(gt, wt) {
			if p, g, w, ok := diff(gt.RawGet(k), wt.RawGet(k), path+keyPath(k), seen); !ok {
				return p, g, w, false
			}
		}
		return "", nil, nil, true
	}
	if gn, ok := got.(glua.LNumber); ok {
		if wn, ok := want.(glua.LNumber); ok && math.IsNaN(float64(gn)) && math.IsNaN(float64(wn)) {
			return "", nil, nil, true
		}
	}
	return path, got, want, got == want
}

// sortedKeys is the keys of both tables: numbers ascending, then strings,
// then the rest by how they print.
func sortedKeys(tabs ...*glua.LTable) []glua.LValue {
	set := map[glua.LValue]bool{}
	for _, tb := range tabs {
		tb.ForEach(func(k, _ glua.LValue) { set[k] = true })
	}
	rank := func(v glua.LValue) int {
		switch v.(type) {
		case glua.LNumber:
			return 0
		case glua.LString:
			return 1
		}
		return 2
	}
	keys := make([]glua.LValue, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b glua.LValue) int {
		if c := cmp.Compare(rank(a), rank(b)); c != 0 {
			return c
		}
		if an, ok := a.(glua.LNumber); ok {
			return cmp.Compare(float64(an), float64(b.(glua.LNumber)))
		}
		return cmp.Compare(a.String(), b.String())
	})
	return keys
}

var luaIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func keyPath(k glua.LValue) string {
	if s, ok := k.(glua.LString); ok && luaIdent.MatchString(string(s)) {
		return "." + string(s)
	}
	return "[" + literal(k, "", map[*glua.LTable]bool{}) + "]"
}

// literal writes v as Lua source, a table on one line when it fits.
func literal(v glua.LValue, indent string, seen map[*glua.LTable]bool) string {
	switch v := v.(type) {
	case glua.LString:
		return luaLiteral(string(v))
	case glua.LNumber:
		switch f := float64(v); {
		case math.IsNaN(f):
			return "0/0"
		case math.IsInf(f, 1):
			return "math.huge"
		case math.IsInf(f, -1):
			return "-math.huge"
		}
		return v.String()
	case *glua.LTable:
		if seen[v] {
			return "<cycle>"
		}
		seen[v] = true
		defer delete(seen, v)
		inner := indent + "  "
		var items []string
		n := v.Len()
		for i := 1; i <= n; i++ {
			items = append(items, literal(v.RawGetInt(i), inner, seen))
		}
		for _, k := range sortedKeys(v) {
			if i, ok := k.(glua.LNumber); ok && float64(i) == math.Trunc(float64(i)) && i >= 1 && int(i) <= n {
				continue
			}
			key := "[" + literal(k, inner, seen) + "]"
			if s, ok := k.(glua.LString); ok && luaIdent.MatchString(string(s)) {
				key = string(s)
			}
			items = append(items, key+" = "+literal(v.RawGet(k), inner, seen))
		}
		if len(items) == 0 {
			return "{}"
		}
		if line := "{ " + strings.Join(items, ", ") + " }"; len(line) <= 60 && !strings.Contains(line, "\n") {
			return line
		}
		return "{\n" + inner + strings.Join(items, ",\n"+inner) + "\n" + indent + "}"
	}
	return v.String()
}

// window shows two long strings around their first difference.
func window(got, want string) string {
	g, w := []rune(got), []rune(want)
	i := 0
	for i < len(g) && i < len(w) && g[i] == w[i] {
		i++
	}
	cut := func(r []rune) string {
		from, to := max(0, i-10), min(len(r), i+40)
		s := luaLiteral(string(r[from:to]))
		if from > 0 {
			s = `"...` + s[1:]
		}
		if to < len(r) {
			s = s[:len(s)-1] + `..."`
		}
		return s
	}
	return fmt.Sprintf("got %s, want %s (first difference at char %d)", cut(g), cut(w), i+1)
}

// ---------------------------------------------------------------------------
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
	"http":  {"status": glua.LTNumber, "body": glua.LTString, "headers": glua.LTTable},
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
	for _, k := range sortedKeys(tb) {
		want, ok := fields[k.String()]
		if _, str := k.(glua.LString); !str || !ok {
			takes := "no fields"
			if len(fields) > 0 {
				takes = strings.Join(slices.Sorted(maps.Keys(fields)), ", ")
			}
			return fmt.Sprintf("%s is not a field of a %s reply, which takes %s", k, kind, takes)
		}
		if got := tb.RawGet(k).Type(); got != want {
			return fmt.Sprintf("%s must be a %s, not a %s", k, want, got)
		}
	}
	return ""
}

// reply is the reply of the newest stub matching a gated call, with the
// defaults filled in, and whether the call is one a stub answers; a call
// none answers gets a note naming the stub that would.
// NOTE: a stub answers at once, so a handle's timeout goes untested; a
// reply that waits for test.advance would reach it.
func (t *PluginTest) reply(L *glua.LState, subject, arg string, req map[string]any) (map[string]any, bool) {
	kind := stubKind(subject)
	if kind == "" {
		return nil, false
	}
	for _, s := range slices.Backward(t.stubs) {
		if s.subject != subject || !perm.MatchExample(s.pattern, arg) {
			continue
		}
		reqTb := lua.FromGo(L, req)
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
		case "model":
			if s, ok := reply.(glua.LString); ok {
				return map[string]any{"text": string(s)}, true
			}
		}
		if tb, ok := reply.(*glua.LTable); ok {
			fields, err := lua.ToGo(tb)
			if m, ok := fields.(map[string]any); ok && err == nil {
				maps.Copy(out, m)
			}
		}
		return out, true
	}
	t.note("no stub answered it; answer it with " + stubLine(kind, subject, arg, req))
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
		return fmt.Sprintf(`test.stub(%q, %q, { status = 200, body = "..." })`, subject, pattern)
	case "job":
		pattern, _ := req["cmd"].(string)
		if args, _ := req["args"].([]string); len(args) > 0 {
			pattern += " *"
		}
		return fmt.Sprintf(`test.stub("lua.job.start", %q, { stdout = "..." })`, pattern)
	case "model":
		return `test.stub("lua.model.ask", "*", "...")`
	}
	return fmt.Sprintf("test.stub(%q)", subject)
}

// ---------------------------------------------------------------------------
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

func (u *testUI) close(id string) (Overlay, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	i := slices.IndexFunc(u.overlays, func(o Overlay) bool { return o.ID == id })
	if i < 0 {
		return Overlay{}, false
	}
	o := u.overlays[i]
	u.overlays = slices.Delete(u.overlays, i, i+1)
	return o, true
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

func (*testUI) Notify(logging.Level, string, string)          {}
func (*testUI) Reload()                                       {}
func (*testUI) Refresh()                                      {}
func (*testUI) Scroll(ScrollAction, string)                   {}
func (*testUI) LayoutRects() map[string]Rect                  { return nil }
func (*testUI) AgentStream()                                  {}
func (*testUI) TurnDone(string, bool)                         {}
func (*testUI) AskPermission(PermRequest) (bool, string)      { return false, tools.ByUser }
func (*testUI) ReviewMemories([]MemoryRow) []MemoryRow        { return nil }
func (*testUI) Screenshot() string                            { return "" }
func (*testUI) AskQuestion([]tools.Question) ([]string, bool) { return nil, false }
func (*testUI) AskPlan(string, string) (tools.PlanVerdict, bool) {
	return tools.PlanVerdict{}, false
}
func (*testUI) ComposerEdit(ComposerOp, string, int) {}
func (*testUI) EditFile(context.Context, EditRequest) error {
	return errors.New("a test zemi has no editor")
}
func (*testUI) Copy(string) {}
