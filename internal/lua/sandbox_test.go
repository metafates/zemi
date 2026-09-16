package lua

import (
	"fmt"
	"strings"
	"testing"

	glua "github.com/yuin/gopher-lua"
)

func newTestSandbox(t *testing.T) *Sandbox {
	t.Helper()
	sb, err := NewSandbox(nil)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	t.Cleanup(sb.L.Close)
	return sb
}

// runLua compiles src as a test chunk and runs it; a non-nil error means the
// chunk raised (assertion failures inside the snippet surface here).
func runLua(t *testing.T, sb *Sandbox, src string) error {
	t.Helper()
	fn, err := sb.CompileChunk(src, "@test:chunk")
	if err != nil {
		return err
	}
	sb.L.Push(fn)
	return sb.L.PCall(0, 0, nil)
}

func mustRun(t *testing.T, sb *Sandbox, src string) {
	t.Helper()
	if err := runLua(t, sb, src); err != nil {
		t.Fatalf("chunk failed: %v", err)
	}
}

func wantRaise(t *testing.T, sb *Sandbox, src, want string) {
	t.Helper()
	err := runLua(t, sb, src)
	if err == nil {
		t.Fatalf("expected chunk to raise %q, it succeeded", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error containing %q, got: %v", want, err)
	}
}

func TestSandboxStrippedSurface(t *testing.T) {
	sb := newTestSandbox(t)
	sb.SetPrint(func(origin, line string) {})
	mustRun(t, sb, `
		assert(rawset == nil, "rawset present")
		assert(rawequal == nil, "rawequal present")
		assert(setfenv == nil, "setfenv present")
		assert(getfenv == nil, "getfenv present")
		assert(dofile == nil, "dofile present")
		assert(loadfile == nil, "loadfile present")
		assert(module == nil, "module present")
		assert(io == nil, "io present")
		assert(os.execute == nil, "os.execute present")
		assert(os.getenv == nil, "os.getenv present")
		assert(os.remove == nil, "os.remove present")
		assert(os.rename == nil, "os.rename present")
		assert(os.exit == nil, "os.exit present")
		assert(os.tmpname == nil, "os.tmpname present")
		assert(package == nil, "package present: its loaded, seeall and loaders reach the unfrozen stdlib and disk")
		assert(load ~= nil, "load missing")
		assert(loadstring ~= nil, "loadstring missing")
		assert(os.time ~= nil, "os.time missing")
		assert(os.date ~= nil, "os.date missing")
		assert(os.clock ~= nil, "os.clock missing")
		assert(debug.traceback ~= nil, "debug.traceback missing")
		assert(debug.getinfo == nil, "debug.getinfo present")
		assert(print ~= nil, "print missing")
	`)
}

func TestSandboxFrozenTables(t *testing.T) {
	sb := newTestSandbox(t)
	want := "cannot modify frozen"
	// Writes into shared stdlib tables raise, also over an existing key
	// (gopher-lua's fast path skips __newindex there).
	wantRaise(t, sb, `string.format = "x"`, want)
	wantRaise(t, sb, `table.insert = nil`, want)
	wantRaise(t, sb, `math.pi = 3`, want)
	// setmetatable on a frozen table is refused via the __metatable lock.
	wantRaise(t, sb, `setmetatable(string, {})`, "protected metatable")
	// The metatable itself is unreachable: getmetatable returns the lock,
	// and indexing the lock string raises.
	mustRun(t, sb, `
		assert(getmetatable(string) == "zemi:protected",
			"metatable lock not returned to Lua")
		assert(getmetatable("") == "zemi:protected",
			"string metatable not locked")
	`)
	wantRaise(t, sb, `getmetatable("").format = function() end`,
		"attempt to index a non-table")
}

func TestSandboxChunkEnv(t *testing.T) {
	sb := newTestSandbox(t)
	// Ordinary new globals land in the chunk environment unblocked.
	mustRun(t, sb, `
		MyThing = MyThing or {}
		MyThing.x = 1
		assert(MyThing.x == 1, "chunk global did not persist")
		_G.y = 5
		assert(y == 5, "_G write did not land in chunk env")
	`)
	// The frozen surface reads through __index.
	mustRun(t, sb, `
		assert(type(string) == "table", "string not visible from chunk")
		assert(string.upper("a") == "A", "string.upper broken")
		assert(_G ~= nil, "_G missing")
	`)
}

func TestSandboxProtectedGlobals(t *testing.T) {
	sb := newTestSandbox(t)
	ns := sb.L.NewTable()
	ns.RawSetString("hello", glua.LString("world"))
	if err := sb.RegisterNamespace("zemi.ui", ns); err != nil {
		t.Fatal(err)
	}
	want := "assignment to protected global"
	wantRaise(t, sb, `zemi = "fake"`, want)
	wantRaise(t, sb, `string = {}`, want)
	wantRaise(t, sb, `os = {}`, want)
	wantRaise(t, sb, `pcall = function() end`, want)
	// A stripped name is not part of the frozen surface, so assigning it is
	// an ordinary new global in the chunk env; it cannot reach the sandbox
	// (frozen reads never see chunk-env writes).
	mustRun(t, sb, `print = function() end`)
	// Writing INTO a namespace raises like any frozen table, including
	// overwriting an existing member (the gopher-lua existing-key path)
	// and nested subtables.
	wantRaise(t, sb, `zemi.ui.hello = "stolen"`, "cannot modify frozen")
	// Reads still work after the refused writes.
	mustRun(t, sb, `
		assert(type(zemi) == "table", "zemi root is not a table")
		assert(zemi.ui.hello == "world", "zemi.ui member not readable")
	`)
}

func TestSandboxDeepNamespaceProtection(t *testing.T) {
	sb := newTestSandbox(t)
	ui := sb.L.NewTable()
	widget := sb.L.NewTable()
	widget.RawSetString("register", sb.L.NewFunction(func(L *glua.LState) int { return 0 }))
	ui.RawSetString("widget", widget)
	if err := sb.RegisterNamespace("zemi.ui", ui); err != nil {
		t.Fatal(err)
	}
	mustRun(t, sb, `
		assert(type(zemi.ui.widget) == "table", "nested namespace not readable")
		assert(type(zemi.ui.widget.register) == "function", "nested member not readable")
	`)
	wantRaise(t, sb, `zemi.ui.widget.register = "fake"`, "cannot modify frozen")
	wantRaise(t, sb, `zemi.ui.widget.new = 1`, "cannot modify frozen")
	mustRun(t, sb, `
		assert(type(zemi.ui.widget.register) == "function",
			"nested member changed despite refused write")
	`)
	if err := sb.CheckIntegrity(); err != nil {
		t.Fatalf("integrity check failed: %v", err)
	}
}

func TestSandboxCallableNamespace(t *testing.T) {
	sb := newTestSandbox(t)
	var got []string
	tbl := sb.L.NewTable()
	tbl.RawSetString("info", sb.L.NewFunction(func(L *glua.LState) int {
		got = append(got, "info:"+L.CheckString(1))
		return 0
	}))
	call := sb.L.NewFunction(func(L *glua.LState) int {
		// __call dispatch: the callable table itself is argument 1.
		got = append(got, "call:"+L.CheckString(2))
		return 0
	})
	if err := sb.RegisterCallableNamespace("zemi.log", tbl, call); err != nil {
		t.Fatal(err)
	}
	mustRun(t, sb, `
		zemi.log("via call")
		zemi.log.info("via member")
	`)
	if len(got) != 2 || got[0] != "call:via call" || got[1] != "info:via member" {
		t.Fatalf("unexpected dispatch log: %v", got)
	}
}

func TestSandboxProtectedValue(t *testing.T) {
	sb := newTestSandbox(t)
	api := sb.L.NewTable()
	api.RawSetString("version", glua.LString("0.1.0-m0"))
	if err := sb.RegisterProtectedValue("zemi.api", api); err != nil {
		t.Fatal(err)
	}
	mustRun(t, sb, `
		assert(zemi.api.version == "0.1.0-m0", "zemi.api.version not readable")
	`)
	// The api table is frozen like every zemi.* surface.
	wantRaise(t, sb, `zemi.api.version = "9.9.9"`, "cannot modify frozen")
	// Duplicate registration is refused.
	if err := sb.RegisterProtectedValue("zemi.api", api); err == nil {
		t.Fatal("duplicate protected value accepted")
	}
}

func TestSandboxRegisterNamespaceRules(t *testing.T) {
	sb := newTestSandbox(t)
	tbl := sb.L.NewTable()
	tbl.RawSetString("fn", sb.L.NewFunction(func(L *glua.LState) int { return 0 }))
	if err := sb.RegisterNamespace("zemi.ui", tbl); err != nil {
		t.Fatal(err)
	}
	if err := sb.RegisterNamespace("zemi.ui", sb.L.NewTable()); err == nil {
		t.Fatal("duplicate namespace accepted")
	}
	if err := sb.RegisterNamespace("config", sb.L.NewTable()); err == nil {
		t.Fatal("non-zemi namespace accepted")
	}
	if err := sb.RegisterNamespace("zemi.", sb.L.NewTable()); err == nil {
		t.Fatal("trailing-dot namespace accepted")
	}
	// A namespace colliding with an existing non-table path is refused.
	if err := sb.RegisterProtectedValue("zemi.seal", glua.LString("x")); err != nil {
		t.Fatal(err)
	}
	if err := sb.RegisterNamespace("zemi.seal.deep", sb.L.NewTable()); err == nil {
		t.Fatal("namespace under a non-table path accepted")
	}
	if err := sb.CheckIntegrity(); err != nil {
		t.Fatalf("integrity check failed: %v", err)
	}
}

func TestSandboxLookupPath(t *testing.T) {
	sb := newTestSandbox(t)
	ns := sb.L.NewTable()
	ns.RawSetString("hello", glua.LString("world"))
	if err := sb.RegisterNamespace("zemi.ui", ns); err != nil {
		t.Fatal(err)
	}
	got := sb.LookupPath("zemi.ui")
	if got == glua.LNil {
		t.Fatal("LookupPath did not resolve zemi.ui")
	}
	// The resolved value is the protecting proxy, not the real table:
	// Go consumers must see what Lua sees.
	tb, ok := got.(*glua.LTable)
	if !ok {
		t.Fatalf("LookupPath returned a non-table: %v", got)
	}
	if v := sb.L.GetField(tb, "hello"); v != glua.LString("world") {
		t.Fatalf("zemi.ui member not readable through LookupPath: %v", v)
	}
	if got := sb.LookupPath("zemi.nope"); got != glua.LNil {
		t.Fatalf("LookupPath resolved unknown name: %v", got)
	}
	if got := sb.LookupPath("nope"); got != glua.LNil {
		t.Fatalf("LookupPath resolved non-zemi name: %v", got)
	}
}

func TestSandboxLoadInheritsChunkEnv(t *testing.T) {
	sb := newTestSandbox(t)
	// load/loadstring chunks inherit the loading chunk's environment:
	// new globals land there, frozen writes still raise.
	mustRun(t, sb, `
		local f = loadstring("Loaded2 = 7; return Loaded2")
		assert(f and f() == 7, "loadstring chunk lost the chunk env")
		assert(Loaded2 == 7, "loaded global did not land in the loading chunk env")
		local e = loadstring("error('boom')")
		local ok, err = pcall(e)
		assert(not ok, "loadstring error chunk succeeded")
		assert(err:find("@test:", 1, true) ~= nil,
			"loadstring chunk lost the origin tag: " .. tostring(err))
		local g = loadstring("zemi = 'fake'")
		local ok2, err2 = pcall(g)
		assert(not ok2, "loadstring chunk wrote a protected global: " .. tostring(err2))
		local h = loadstring("return string.upper('a')")
		assert(h() == "A", "load chunk lost the frozen surface")
		local n = 0
		local r = load(function()
			n = n + 1
			if n == 1 then return "return " .. 4 end
			return nil
		end)
		assert(r and r() == 4, "load with a reader function failed")
	`)
}

// TestSandboxLoadInCoroutine: load inside a coroutine binds the env of the
// function calling it there, not of whoever resumed the coroutine.
func TestSandboxLoadInCoroutine(t *testing.T) {
	sb := newTestSandbox(t)
	other, err := sb.CompileChunk(`return function() return loadstring("return SECRET")() end`, "@plugin:other")
	if err != nil {
		t.Fatal(err)
	}
	sb.L.Push(other)
	if err := sb.L.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	peek := sb.L.Get(-1)
	sb.L.Pop(1)
	fn, err := sb.CompileChunk(`
		SECRET = "user secret"
		local peek = ...
		assert(coroutine.wrap(peek)() == nil, "a coroutine's load read the resumer's globals")
	`, "@user:init")
	if err != nil {
		t.Fatal(err)
	}
	sb.L.Push(fn)
	sb.L.Push(peek)
	if err := sb.L.PCall(1, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxLoadReaderSizeCap(t *testing.T) {
	sb := newTestSandbox(t)
	mustRun(t, sb, `
		local calls = 0
		local f, err = load(function()
			calls = calls + 1
			if calls <= 100 then return string.rep("x", 1000000) end
			return nil
		end)
		assert(f == nil, "oversized reader source loaded")
		assert(err:find("cap", 1, true) ~= nil,
			"cap error not reported: " .. tostring(err))
		assert(calls <= 20, "reader kept being called past the cap: " .. calls)
		local delivered = false
		local small, serr = load(function()
			if delivered then return nil end
			delivered = true
			return string.rep("-- pad\n", 500000)
		end)
		assert(small and small() == nil, "under-cap reader source must load: " .. tostring(serr))
	`)
}

func TestSandboxLoadRefusesBytecode(t *testing.T) {
	sb := newTestSandbox(t)
	mustRun(t, sb, `
		local ok, err = pcall(string.dump, function() return 1 end)
		assert(not ok, "string.dump produced bytecode")
		local f, err2 = loadstring("\x1bLua not real bytecode")
		assert(f == nil, "binary chunk loaded")
	`)
}

// BenchmarkCheckIntegrity sizes the namespace verification against a
// surface the size of the real one: it decides whether the check can run
// on every request or only when the request class changes (DESIGN §4).
func BenchmarkCheckIntegrity(b *testing.B) {
	sb, err := NewSandbox(nil)
	if err != nil {
		b.Fatal(err)
	}
	defer sb.L.Close()
	for i := range 28 {
		tbl := sb.L.NewTable()
		for j := range 8 {
			tbl.RawSetString(fmt.Sprintf("fn%d", j), sb.L.NewFunction(func(*glua.LState) int { return 0 }))
		}
		if err := sb.RegisterNamespace(fmt.Sprintf("zemi.ns%d", i), tbl); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if err := sb.CheckIntegrity(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSandboxTracebackCollapsesFrames(t *testing.T) {
	// debug.traceback exists and returns a string; full redaction is a
	// UI concern, here we pin the API shape.
	sb := newTestSandbox(t)
	mustRun(t, sb, `
		assert(type(debug.traceback("msg")) == "string", "traceback not a string")
		assert(debug.traceback("msg"):find("msg", 1, true), "traceback lost the message")
	`)
}
