package lua

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"
)

// Sandbox builds a sandboxed LState: curated stdlib, frozen shared tables,
// per-chunk environments with protected names (DESIGN §5).
type Sandbox struct {
	L         *glua.LState
	frozen    *glua.LTable
	protected map[string]struct{}
	integrity []namespaceCheck
	originOf  func(*glua.LState) Origin
	now       func() time.Time
	start     time.Time
	guard     Guard
}

// Guard runs before a guarded Go function with the member it belongs to
// ("config.set", "ui.widget.add", "win:send") and raises to refuse.
type Guard func(L *glua.LState, member string)

// SetGuard sets the guard every namespace member runs through; it is read
// per call, so it may be set after registration.
func (s *Sandbox) SetGuard(g Guard) { s.guard = g }

// GuardFuncs wraps every Go function in tb and its sub-tables so guard runs
// first; member names are prefix + key, sub-tables joined with ".".
func GuardFuncs(L *glua.LState, tb *glua.LTable, prefix string, guard Guard) {
	guardFuncs(L, tb, prefix, guard, map[*glua.LTable]bool{})
}

func guardFuncs(L *glua.LState, tb *glua.LTable, prefix string, guard Guard, seen map[*glua.LTable]bool) {
	if seen[tb] {
		return
	}
	seen[tb] = true
	wraps := map[glua.LValue]glua.LValue{}
	tb.ForEach(func(k, v glua.LValue) {
		switch v := v.(type) {
		case *glua.LFunction:
			if v.IsG {
				wraps[k] = guardFunc(L, v, prefix+k.String(), guard)
			} else if inner, ok := suspended(v); ok {
				v.Upvalues[0].SetValue(guardFunc(L, inner, prefix+k.String(), guard))
			}
		case *glua.LTable:
			guardFuncs(L, v, prefix+k.String()+".", guard, seen)
		}
	})
	for k, v := range wraps {
		tb.RawSet(k, v)
	}
}

func guardFunc(L *glua.LState, fn *glua.LFunction, member string, guard Guard) *glua.LFunction {
	w := L.NewFunction(func(L *glua.LState) int {
		guard(L, member)
		return fn.GFunction(L)
	})
	// a Go closure reads upvalues through the running function
	w.Upvalues = fn.Upvalues
	return w
}

type namespaceCheck struct {
	name    string
	table   *glua.LTable
	members map[string]glua.LValue
}

// NewSandbox creates a state with the §5 sandbox applied. zemi.*
// namespaces are registered afterwards, before any chunk runs. now is the
// os library's clock; nil is the wall clock.
func NewSandbox(originOf func(*glua.LState) Origin, now func() time.Time) (*Sandbox, error) {
	if now == nil {
		now = time.Now
	}
	L := glua.NewState(glua.Options{
		SkipOpenLibs:  true,
		CallStackSize: 512,
		// grown on demand: every coroutine gets its own registry, and the
		// full 1<<20 slots up front are 16 MB each
		RegistrySize:     1 << 12,
		RegistryMaxSize:  1 << 20,
		RegistryGrowStep: 1 << 16,
	})

	// No package module: require is Go-owned, and package.loaded,
	// package.seeall and package.loaders hand out the unfrozen stdlib and
	// globals and run files from package.path (DESIGN §5).
	libs := []struct {
		name string
		fn   glua.LGFunction
	}{
		{"", glua.OpenBase},
		{glua.TabLibName, glua.OpenTable},
		{glua.StringLibName, glua.OpenString},
		{glua.MathLibName, glua.OpenMath},
		{glua.CoroutineLibName, glua.OpenCoroutine},
	}
	for _, lib := range libs {
		L.Push(L.NewFunction(lib.fn))
		L.Push(glua.LString(lib.name))
		if err := L.PCall(1, 0, nil); err != nil {
			return nil, fmt.Errorf("open stdlib %s: %w", lib.name, err)
		}
	}

	global := L.Get(glua.GlobalsIndex).(*glua.LTable)

	// Strip the unsafe surface: raw-file execution, environment swapping,
	// freeze bypasses, and the Lua-side module system (require is Go-owned).
	for _, name := range []string{
		"dofile", "loadfile", "getfenv", "setfenv", "rawset", "rawequal",
		"module", "_printregs", "print", "require",
	} {
		global.RawSetString(name, glua.LNil)
	}

	s := &Sandbox{
		L:         L,
		protected: map[string]struct{}{},
		originOf:  originOf,
		now:       now,
		start:     now(),
	}

	global.RawSetString("os", s.makeOsTable())
	dbg := L.NewTable()
	dbg.RawSetString("traceback", L.NewFunction(s.debugTraceback))
	global.RawSetString("debug", dbg)

	// The stdlib makes the string module the metatable of every string with
	// __index pointing at itself, so ("").__index would hand out the real
	// table; strings get a locked metatable over the frozen proxy below.
	global.RawGetString("string").(*glua.LTable).RawSetString("__index", glua.LNil)
	FixStringLib(L)

	// A thread keeps the context it was created under: unless resume rebinds
	// it, a coroutine made in one request raises "context canceled" in a
	// later one, and one made at boot ignores cancellation (DESIGN §4).
	if co, ok := global.RawGetString("coroutine").(*glua.LTable); ok {
		resume := co.RawGetString("resume").(*glua.LFunction).GFunction
		wrap := co.RawGetString("wrap").(*glua.LFunction).GFunction
		co.RawSetString("resume", L.NewFunction(func(L *glua.LState) int {
			th := L.CheckThread(1)
			// the driver alone resumes a task, with what it waited on (DESIGN §4);
			// wrap resumes only the thread it made
			if g := genOf(L); g != nil && g.tasks[th] != nil {
				L.RaiseError("coroutine.resume: a task's thread resumes only when what it waits on is done")
			}
			bindContext(L, th)
			return resume(L)
		}))
		co.RawSetString("wrap", L.NewFunction(func(L *glua.LState) int {
			wrap(L)
			aux := L.Get(-1).(*glua.LFunction)
			L.Pop(1)
			// aux reads the thread from the running closure's upvalue, so
			// this closure carries the same one
			L.Push(L.NewClosure(func(L *glua.LState) int {
				bindContext(L, L.ToThread(glua.UpvalueIndex(1)))
				return aux.GFunction(L)
			}, aux.Upvalues[0].Value()))
			return 1
		}))
	}

	global.RawSetString("setmetatable", L.NewFunction(s.setMetatable))
	next := global.RawGetString("next")
	global.RawSetString("pairs", L.NewFunction(func(L *glua.LState) int {
		tb := L.CheckTable(1)
		var real *glua.LTable
		if mt, ok := tb.Metatable.(*glua.LTable); ok {
			real, _ = mt.RawGetString(realKey).(*glua.LTable)
		}
		if real == nil {
			L.Push(next)
			L.Push(tb)
			L.Push(glua.LNil)
			return 3
		}
		// a namespace proxy is empty; its members come from a Go iterator, so
		// the real table, which raw writes would reach, never does
		L.Push(L.NewFunction(func(L *glua.LState) int {
			k, v := real.Next(L.Get(2))
			if k == glua.LNil {
				L.Push(glua.LNil)
				return 1
			}
			L.Push(k)
			L.Push(v)
			return 2
		}))
		L.Push(tb)
		L.Push(glua.LNil)
		return 3
	}))

	// load/loadstring stay (Lua source only, DESIGN §5) but the base
	// versions bind compiled chunks to the interpreter's environment; the
	// sandbox versions inherit the loading chunk's env and origin.
	global.RawSetString("load", s.makeLoader(false))
	global.RawSetString("loadstring", s.makeLoader(true))

	// Freeze the shared surface: every remaining global and the stdlib
	// tables move into `frozen`, which per-chunk environments read through
	// __index. Assignments land in the chunk env or raise, never here.
	frozen := L.NewTable()
	type namedTable struct {
		name string
		tb   *glua.LTable
	}
	var toFreeze []namedTable
	global.ForEach(func(key, value glua.LValue) {
		name, ok := key.(glua.LString)
		if !ok {
			return
		}
		if string(name) == "_G" {
			return
		}
		frozen.RawSet(key, value)
		s.protected[string(name)] = struct{}{}
		if tb, isTable := value.(*glua.LTable); isTable {
			toFreeze = append(toFreeze, namedTable{name: string(name), tb: tb})
		}
	})
	frozen.RawSetString("_G", glua.LNil)
	s.frozen = frozen
	s.freezeTable(frozen, "stdlib surface")
	// The zemi binding itself is write-guarded from the start (DESIGN §5):
	// namespace registrations mark their dotted names, but the root name is
	// protected even before any namespace exists.
	s.protected["zemi"] = struct{}{}
	for _, e := range toFreeze {
		frozen.RawSetString(e.name, s.protectTable(e.tb, "stdlib table "+e.name))
	}
	strMeta := L.NewTable()
	strMeta.RawSetString("__index", frozen.RawGetString("string"))
	strMeta.RawSetString("__metatable", glua.LString("zemi:protected"))
	L.SetMetatable(glua.LString(""), strMeta)
	L.Env = frozen

	return s, nil
}

// handlersKey marks a metatable setMetatable copied; true when the original
// had no __metatable lock, so setmetatable may replace it.
const handlersKey = "__zemi_handlers"

// setMetatable is setmetatable for tables only: gopher-lua's would set the
// metatable of every number, boolean or function. Code below builtin may
// install only Lua functions of its own origin or lower as handlers: a Go or
// builtin function leaves no frame of that origin, so an event user code
// triggers (obj.setup, obj(), obj == x) would run it as user (DESIGN §5).
// The handlers are copied, so a later write to mt cannot swap one in, and
// getmetatable still returns mt.
// A Go function handed out as a plain value would run as its caller too,
// which is why a plugin module may not export one (exportsGo).
func (s *Sandbox) setMetatable(L *glua.LState) int {
	obj := L.CheckTable(1)
	L.CheckTypes(2, glua.LTNil, glua.LTTable)
	if cur, ok := obj.Metatable.(*glua.LTable); ok && cur.RawGetString("__metatable") != glua.LNil && cur.RawGetString(handlersKey) != glua.LTrue {
		L.RaiseError("cannot change a protected metatable")
	}
	mt := L.Get(2)
	if tb, ok := mt.(*glua.LTable); ok && s.originOf != nil {
		if o := s.originOf(L); o.Rank() > 0 && o.Rank() < OriginBuiltin.Rank() {
			mt = copyHandlers(L, tb, o)
		}
	}
	L.SetMetatable(obj, mt)
	L.SetTop(1)
	return 1
}

// copyHandlers copies mt's events, refusing a handler that would not run as o
// or, for plugin code, as the setter's own plugin.
func copyHandlers(L *glua.LState, mt *glua.LTable, o Origin) *glua.LTable {
	cp := L.NewTable()
	bad := ""
	g := genOf(L)
	own := ""
	if g != nil {
		own = g.pluginRoot(g.rt.CurrentPath(L))
	}
	mt.ForEach(func(k, v glua.LValue) {
		event, ok := k.(glua.LString)
		if !ok || !strings.HasPrefix(string(event), "__") {
			return
		}
		if fn, ok := v.(*glua.LFunction); ok {
			fo, path := OriginHost, ""
			if !fn.IsG {
				fo, path = splitChunkName(fn.Proto.SourceName)
			}
			if fo.Rank() == 0 || fo.Rank() > o.Rank() || g != nil && g.pluginRoot(path) != own {
				bad = string(event)
			}
		}
		cp.RawSet(k, v)
	})
	if bad != "" {
		L.RaiseError("setmetatable: %s must be a Lua function of this %s's own code, not a Go, builtin, more trusted or other plugin's one, which would run as the code that triggers it", bad, o)
	}
	lock := mt.RawGetString("__metatable")
	cp.RawSetString(handlersKey, glua.LBool(lock == glua.LNil))
	if lock == glua.LNil {
		lock = mt
	}
	cp.RawSetString("__metatable", lock)
	return cp
}

// bindContext gives thread th the context of L, the thread resuming it.
func bindContext(L, th *glua.LState) {
	if ctx := L.Context(); ctx != nil {
		th.SetContext(ctx)
	} else {
		th.RemoveContext()
	}
}

func (s *Sandbox) makeOsTable() *glua.LTable {
	L := s.L
	osTab := L.NewTable()
	L.SetFuncs(osTab, map[string]glua.LGFunction{
		"clock": func(L *glua.LState) int {
			L.Push(glua.LNumber(s.now().Sub(s.start).Seconds()))
			return 1
		},
		"time": func(L *glua.LState) int {
			if L.GetTop() == 0 {
				L.Push(glua.LNumber(s.now().Unix()))
				return 1
			}
			tb := L.CheckTable(1)
			sec := tableInt(tb, "sec", 0)
			mm := tableInt(tb, "min", 0)
			hh := tableInt(tb, "hour", 12)
			day := tableInt(tb, "day", -1)
			mon := tableInt(tb, "month", -1)
			year := tableInt(tb, "year", -1)
			if day < 0 || mon < 0 || year < 0 {
				L.RaiseError("os.time: incomplete date table")
			}
			L.Push(glua.LNumber(time.Date(year, time.Month(mon), day, hh, mm, sec, 0, s.now().Location()).Unix()))
			return 1
		},
		"date": s.date,
	})
	return osTab
}

// FixStringLib caps string.rep results at MaxStringBytes: an out-of-memory
// kills the whole process, and string.rep reaches one in a single call.
// NOTE: .. in a loop, gsub and table.concat still grow strings uncapped;
// gopher-lua has no allocation hook to cap them all.
// It also gives string.format Lua 5.1's argument checks (checkFormat).
func FixStringLib(L *glua.LState) {
	lib := L.GetGlobal(glua.StringLibName).(*glua.LTable)
	lib.RawSetString("rep", L.NewFunction(func(L *glua.LState) int {
		s, n := L.CheckString(1), L.CheckInt(2)
		if n > 0 && len(s) > 0 && n > MaxStringBytes/len(s) {
			L.RaiseError("string.rep: result exceeds the %d byte cap", MaxStringBytes)
		}
		L.Push(glua.LString(strings.Repeat(s, max(n, 0))))
		return 1
	}))
	format := lib.RawGetString("format")
	lib.RawSetString("format", L.NewFunction(func(L *glua.LState) int {
		f, args := checkFormat(L)
		L.Push(format)
		L.Push(glua.LString(f))
		for _, a := range args {
			L.Push(a)
		}
		L.Call(len(args)+1, 1)
		return 1
	}))
}

// checkFormat raises where Lua 5.1's string.format does (an unknown option,
// a missing argument, a non-number under a numeric option) and returns the
// format and arguments gopher-lua's may print: it hands them to Go's fmt,
// which writes "%!d(MISSING)", 0 for a word under %d and a numeric string's
// bytes in hex under %x.
func checkFormat(L *glua.LState) (string, []glua.LValue) {
	f := []byte(L.CheckString(1))
	var args []glua.LValue
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		if i++; i < len(f) && f[i] == '%' {
			continue
		}
		for i < len(f) && strings.IndexByte("-+ #0123456789.", f[i]) >= 0 {
			i++
		}
		if i == len(f) || strings.IndexByte("cdiouxXeEfgGsq", f[i]) < 0 {
			L.RaiseError("invalid option '%%%s' to 'format'", f[i:min(i+1, len(f))])
		}
		n := len(args) + 2
		switch {
		case n > L.GetTop():
			L.ArgError(n, "no value")
		case f[i] == 's' || f[i] == 'q':
			args = append(args, L.Get(n))
		default:
			args = append(args, L.CheckNumber(n))
		}
		if f[i] == 'u' { // Go's fmt has no %u
			f[i] = 'd'
		}
	}
	return string(f), args
}

func tableInt(tb *glua.LTable, name string, def int) int {
	if v, ok := tb.RawGetString(name).(glua.LNumber); ok {
		return int(v)
	}
	return def
}

// date implements the subset of os.date zemi needs: %c, %x, %X and the
// individual specifiers, with optional Unix time argument, in the clock's
// zone. A specifier outside it raises: printing its bare letter is a wrong
// date, not a missing feature.
func (s *Sandbox) date(L *glua.LState) int {
	format := L.OptString(1, "%c")
	t := s.now()
	if L.Get(2) != glua.LNil {
		t = time.Unix(L.CheckInt64(2), 0).In(t.Location())
	}
	format, utc := strings.CutPrefix(format, "!")
	if utc {
		t = t.UTC()
	}
	if format == "*t" {
		tb := L.NewTable()
		tb.RawSetString("year", glua.LNumber(t.Year()))
		tb.RawSetString("month", glua.LNumber(int(t.Month())))
		tb.RawSetString("day", glua.LNumber(t.Day()))
		tb.RawSetString("hour", glua.LNumber(t.Hour()))
		tb.RawSetString("min", glua.LNumber(t.Minute()))
		tb.RawSetString("sec", glua.LNumber(t.Second()))
		tb.RawSetString("wday", glua.LNumber(int(t.Weekday())+1))
		tb.RawSetString("yday", glua.LNumber(t.YearDay()))
		tb.RawSetString("isdst", glua.LFalse)
		L.Push(tb)
		return 1
	}
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'p':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'c':
			b.WriteString(t.Format(time.ANSIC))
		case 'x':
			b.WriteString(t.Format("01/02/06"))
		case 'X':
			b.WriteString(t.Format(time.TimeOnly))
		case '%':
			b.WriteByte('%')
		default:
			L.RaiseError("os.date: unsupported format specifier %%%c", format[i])
		}
	}
	L.Push(glua.LString(b.String()))
	return 1
}

// SetPrint installs the host-routed print: output goes to the log sink
// with the currently running origin (DESIGN §5).
func (s *Sandbox) SetPrint(fn func(origin, line string)) {
	printFn := s.L.NewFunction(func(L *glua.LState) int {
		origin := OriginHost
		if s.originOf != nil {
			origin = s.originOf(L)
		}
		fn(string(origin), joinArgs(L, " "))
		return 0
	})
	s.frozen.RawSetString("print", printFn)
	s.protected["print"] = struct{}{}
}

// joinArgs renders the call's arguments with tostring, the way print does.
func joinArgs(L *glua.LState, sep string) string {
	parts := make([]string, L.GetTop())
	for i := range parts {
		parts[i] = L.ToStringMeta(L.Get(i + 1)).String()
	}
	return strings.Join(parts, sep)
}

// SetRequire installs the Go-owned require into the frozen surface so every
// chunk resolves it through its environment chain; package.* stays inert
// (DESIGN §5: require resolution is Go-side, never Lua-side).
func (s *Sandbox) SetRequire(fn *glua.LFunction) {
	s.frozen.RawSetString("require", fn)
	s.protected["require"] = struct{}{}
}

// freezeTable locks a shared table: writes through it raise, setmetatable
// on it is refused via the __metatable lock, and Lua cannot get at the real
// metatable to swap events. The stdlib surface and each registered namespace
// root get it; Lua reaches namespaces only through protectNamespace proxies.
func (s *Sandbox) freezeTable(tb *glua.LTable, what string) {
	s.L.SetMetatable(tb, s.lockedMeta(what))
}

// lockedMeta is a metatable whose writes raise and that Lua cannot read.
func (s *Sandbox) lockedMeta(what string) *glua.LTable {
	mt := s.L.NewTable()
	mt.RawSetString("__newindex", s.L.NewFunction(func(L *glua.LState) int {
		L.RaiseError("zemi: cannot modify frozen %s (write refused)", what)
		return 0
	}))
	mt.RawSetString("__metatable", glua.LString("zemi:protected"))
	return mt
}

// protectTable wraps a shared table in an empty proxy: reads fall through
// __index to the real table, every write raises, and getmetatable returns
// the lock string. The proxy matters because gopher-lua raw-sets table keys
// that already exist without consulting __newindex, so a plain frozen
// metatable would still allow `string.format = fake`; an empty proxy never
// takes the existing-key fast path. Returns the proxy to store in place of
// the real table.
func (s *Sandbox) protectTable(tb *glua.LTable, what string) *glua.LTable {
	mt := s.lockedMeta(what)
	mt.RawSetString("__index", tb)
	proxy := s.L.NewTable()
	s.L.SetMetatable(proxy, mt)
	return proxy
}

// protectNamespace is protectTable for a zemi.* namespace: reading a member
// it does not have raises and lists the ones it does, and pairs lists them
// through the sandbox's own pairs.
func (s *Sandbox) protectNamespace(tb *glua.LTable, what, path string) *glua.LTable {
	mt := s.lockedMeta(what)
	// realTableOf reads it: __index is a function here, not the table
	mt.RawSetString(realKey, tb)
	mt.RawSetString("__index", s.L.NewFunction(func(L *glua.LState) int {
		key := L.Get(2)
		if v := tb.RawGet(key); v != glua.LNil {
			L.Push(v)
			return 1
		}
		var names []string
		tb.ForEach(func(k, _ glua.LValue) { names = append(names, k.String()) })
		slices.Sort(names)
		L.RaiseError("%s has no member %q (%s)", path, key.String(), strings.Join(names, ", "))
		return 0
	}))
	proxy := s.L.NewTable()
	s.L.SetMetatable(proxy, mt)
	return proxy
}

// deepProtect replaces every table-valued member of tbl (recursively) with
// its protected proxy, so nested namespaces like zemi.ui.widget are frozen
// too. Go keeps its references to the real tables.
func (s *Sandbox) deepProtect(tbl *glua.LTable, path string) {
	type entry struct {
		key glua.LValue
		tb  *glua.LTable
	}
	var swaps []entry
	tbl.ForEach(func(k, v glua.LValue) {
		if mt, ok := v.(*glua.LTable); ok {
			swaps = append(swaps, entry{key: k, tb: mt})
		}
	})
	for _, e := range swaps {
		joined := path + "." + e.key.String()
		s.deepProtect(e.tb, joined)
		tbl.RawSet(e.key, s.protectNamespace(e.tb, "namespace "+joined, joined))
	}
}

// readOnlyView is protectTable applied lazily down a tree: every table read
// through it comes back as a proxy too, and the real tables stay writable
// for the code that owns them.
func (s *Sandbox) readOnlyView(tb *glua.LTable, what string) *glua.LTable {
	proxies := map[*glua.LTable]*glua.LTable{}
	var wrap func(*glua.LTable) *glua.LTable
	wrap = func(real *glua.LTable) *glua.LTable {
		if p, ok := proxies[real]; ok {
			return p
		}
		mt := s.lockedMeta(what)
		mt.RawSetString("__index", s.L.NewFunction(func(L *glua.LState) int {
			v := L.GetTable(real, L.Get(2))
			if t, ok := v.(*glua.LTable); ok {
				v = wrap(t)
			}
			L.Push(v)
			return 1
		}))
		p := s.L.NewTable()
		s.L.SetMetatable(p, mt)
		proxies[real] = p
		return p
	}
	return wrap(tb)
}

// realTableOf resolves a protected proxy to the table its __index reads.
func realTableOf(tb *glua.LTable) *glua.LTable {
	mt, ok := tb.Metatable.(*glua.LTable)
	if !ok {
		return tb
	}
	if real, ok := mt.RawGetString(realKey).(*glua.LTable); ok {
		return real
	}
	idx, ok := mt.RawGetString("__index").(*glua.LTable)
	if !ok || idx == tb {
		return tb
	}
	return idx
}

// realKey holds a namespace proxy's real table on its metatable, which
// Lua cannot reach: __metatable answers "zemi:protected".
const realKey = "__zemi_real"

// rawLookup reads key from tbl, falling through a protected proxy's
// __index (LookupPath walks proxies and real tables alike).
func rawLookup(tb *glua.LTable, key string) glua.LValue {
	if v := tb.RawGetString(key); v != glua.LNil {
		return v
	}
	return realTableOf(tb).RawGetString(key)
}

// RegisterNamespace installs a Go-backed zemi.* table: frozen, integrity
// checked on every request class, and visible through the chunk env chain.
// A non-nil call makes it callable (zemi.log, zemi.notify): `tbl(...)`
// dispatches to call.
func (s *Sandbox) RegisterNamespace(name string, tbl *glua.LTable, call *glua.LFunction) error {
	if !strings.HasPrefix(name, "zemi.") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("namespace %q must be a dotted zemi.* name", name)
	}
	if s.LookupPath(name) != glua.LNil {
		return fmt.Errorf("namespace %q already registered", name)
	}
	short := strings.TrimPrefix(name, "zemi.")
	guard := func(L *glua.LState, member string) {
		if s.guard != nil {
			s.guard(L, member)
		}
	}
	GuardFuncs(s.L, tbl, short+".", guard)
	if call != nil && call.IsG {
		call = guardFunc(s.L, call, short, guard)
	}
	s.freezeTable(tbl, "namespace "+name)
	s.deepProtect(tbl, name)
	proxy := s.protectNamespace(tbl, "namespace "+name, name)
	if call != nil {
		mt := proxy.Metatable.(*glua.LTable)
		mt.RawSetString("__call", call)
	}
	parent, err := s.ensureParent(strings.Split(name, "."))
	if err != nil {
		return err
	}
	members := map[string]glua.LValue{}
	tbl.ForEach(func(k, v glua.LValue) {
		members[k.String()] = v
	})
	s.integrity = append(s.integrity, namespaceCheck{name: name, table: tbl, members: members})
	s.protected[name] = struct{}{}
	parent.RawSetString(name[strings.LastIndex(name, ".")+1:], proxy)
	return nil
}

// LookupPath resolves a dotted name against the frozen surface. The zemi.*
// namespaces hang under a zemi root table, so chunk environments (whose
// __index is the frozen surface) resolve `zemi.ui` as ordinary table reads.
func (s *Sandbox) LookupPath(name string) glua.LValue {
	var v glua.LValue = s.frozen
	for p := range strings.SplitSeq(name, ".") {
		tb, ok := v.(*glua.LTable)
		if !ok {
			return glua.LNil
		}
		v = rawLookup(tb, p)
	}
	return v
}

// ensureParent walks the frozen surface along all but the last segment of a
// dotted path, creating and freezing missing intermediate tables (the zemi
// root included) and marking them protected so chunk-env assignments to
// those names raise (DESIGN §5).
func (s *Sandbox) ensureParent(parts []string) (*glua.LTable, error) {
	tbl := s.frozen
	for i := range len(parts) - 1 {
		joined := strings.Join(parts[:i+1], ".")
		cur := rawLookup(tbl, parts[i])
		switch next := cur.(type) {
		case *glua.LTable:
			tbl = realTableOf(next)
		case *glua.LNilType:
			real := s.L.NewTable()
			tbl.RawSetString(parts[i], s.protectNamespace(real, "namespace "+joined, joined))
			s.protected[joined] = struct{}{}
			tbl = real
		default:
			return nil, fmt.Errorf("frozen path %q is registered as a non-table", joined)
		}
	}
	return tbl, nil
}

// RegisterProtectedValue adds a read-only value to the frozen surface,
// creating (and freezing) intermediate tables as needed (e.g. zemi.api).
func (s *Sandbox) RegisterProtectedValue(path string, v glua.LValue) error {
	if s.LookupPath(path) != glua.LNil {
		return fmt.Errorf("protected value %q already registered", path)
	}
	if tb, ok := v.(*glua.LTable); ok {
		s.freezeTable(tb, "protected value "+path)
		s.deepProtect(tb, path)
		v = s.protectNamespace(tb, "protected value "+path, path)
	}
	parent, err := s.ensureParent(strings.Split(path, "."))
	if err != nil {
		return err
	}
	s.protected[path] = struct{}{}
	parent.RawSetString(path[strings.LastIndex(path, ".")+1:], v)
	return nil
}

// CheckIntegrity verifies every namespace still holds exactly the members
// it was registered with. Called per request class; any mismatch is a
// zemi bug that must fail loud.
func (s *Sandbox) CheckIntegrity() error {
	for _, c := range s.integrity {
		n := 0
		c.table.ForEach(func(k, v glua.LValue) { n++ })
		if n != len(c.members) {
			return fmt.Errorf("integrity: namespace %s changed size (%d != %d)", c.name, n, len(c.members))
		}
		for name, want := range c.members {
			if got := c.table.RawGetString(name); got != want {
				return fmt.Errorf("integrity: namespace %s member %s was tampered with", c.name, name)
			}
		}
	}
	return nil
}

// NamespaceMembers lists every registered namespace with its member paths,
// descending into member tables ("widget.add" under zemi.ui). It is
// what the API surface actually is, so the shipped type annotations can be
// checked against it instead of against a hand-kept list.
func (s *Sandbox) NamespaceMembers() map[string][]string {
	out := make(map[string][]string, len(s.integrity))
	for _, c := range s.integrity {
		out[c.name] = tableMembers(c.table, "")
	}
	return out
}

func tableMembers(tb *glua.LTable, prefix string) []string {
	var out []string
	tb.ForEach(func(k, v glua.LValue) {
		name := prefix + k.String()
		if sub, ok := v.(*glua.LTable); ok {
			// sub-tables were swapped for proxies by deepProtect
			out = append(out, tableMembers(realTableOf(sub), name+".")...)
			return
		}
		out = append(out, name)
	})
	slices.Sort(out)
	return out
}

// NewChunkEnv builds the per-chunk environment: __index reads the frozen
// surface, __newindex raises for protected names and stores ordinary new
// globals in the chunk env itself, _G resolves to this env.
func (s *Sandbox) NewChunkEnv() *glua.LTable {
	L := s.L
	env := L.NewTable()
	env.RawSetString("_G", env)
	envGuard := L.NewFunction(func(L *glua.LState) int {
		key := L.Get(2)
		if str, ok := key.(glua.LString); ok {
			if _, prot := s.protected[string(str)]; prot {
				L.RaiseError("zemi: assignment to protected global '%s' is refused", string(str))
				return 0
			}
		}
		if tb, ok := L.Get(1).(*glua.LTable); ok {
			L.RawSet(tb, key, L.Get(3))
		}
		return 0
	})
	mt := L.NewTable()
	mt.RawSetString("__index", s.frozen)
	mt.RawSetString("__newindex", envGuard)
	mt.RawSetString("__metatable", glua.LString("zemi:protected"))
	L.SetMetatable(env, mt)
	return env
}

// CompileChunk compiles Lua source into a function bound to a fresh chunk
// environment. The chunkname is origin-tagged for traceback redaction.
func (s *Sandbox) CompileChunk(src, chunkname string) (*glua.LFunction, error) {
	fn, err := s.L.Load(strings.NewReader(src), chunkname)
	if err != nil {
		return nil, err
	}
	noTailCalls(fn.Proto)
	fn.Env = s.NewChunkEnv()
	return fn, nil
}

// noTailCalls turns every tail call into a plain call, which the RETURN the
// compiler emits after it completes: a tail call would drop its caller's
// frame, and the origin a call runs as is read from the frames (DESIGN §5).
// NOTE: tail recursion is bounded by CallStackSize.
func noTailCalls(p *glua.FunctionProto) {
	for i, in := range p.Code {
		if int(in>>26) == glua.OP_TAILCALL {
			p.Code[i] = in&(1<<26-1) | uint32(glua.OP_CALL)<<26
		}
	}
	for _, c := range p.FunctionPrototypes {
		noTailCalls(c)
	}
}

// makeLoader builds the sandbox load/loadstring: it compiles Lua source
// like the base library but pins the compiled function's environment to
// the loading chunk's environment, with the chunkname re-tagged to the
// caller's origin, so tracebacks of generated code redact correctly and a
// generated chunk never receives the interpreter's global table (DESIGN §5).
// With no Lua caller the chunk gets a fresh chunk env: the frozen surface
// itself would let its existing keys be overwritten, as gopher-lua skips
// __newindex for them.
func (s *Sandbox) makeLoader(loadstring bool) *glua.LFunction {
	L := s.L
	return L.NewFunction(func(L *glua.LState) int {
		var src, chunkname string
		if loadstring {
			src = L.CheckString(1)
			chunkname = L.OptString(2, "<string>")
		} else {
			chunkname = L.OptString(2, "?")
			reader, ok := L.Get(1).(*glua.LFunction)
			if !ok {
				L.CheckFunction(1)
				return 0
			}
			var b strings.Builder
			top := L.GetTop()
			for {
				L.SetTop(top)
				L.Push(reader)
				L.Call(0, 1)
				ret := L.Get(-1)
				if ret == glua.LNil {
					break
				}
				if !glua.LVCanConvToString(ret) {
					L.Push(glua.LNil)
					L.Push(glua.LString("reader function must return a string"))
					return 2
				}
				str := ret.String()
				if len(str) == 0 {
					break
				}
				b.WriteString(str)
				if b.Len() > MaxStringBytes {
					L.Push(glua.LNil)
					L.Push(glua.LString(fmt.Sprintf("load: source exceeds the %d byte cap", MaxStringBytes)))
					return 2
				}
			}
			src = b.String()
		}
		callerEnv, callerOrigin := s.callerChunk(L)
		if callerOrigin == "" && s.originOf != nil {
			// pcall(loadstring, src, "@user:/x") has a Go caller; the name
			// must still carry the running origin, not the forged one.
			callerOrigin = s.originOf(L)
		}
		if filepath.IsAbs(chunkname) {
			// an absolute name would pass as a file to relative require
			chunkname = "=" + chunkname
		}
		if callerOrigin != "" {
			chunkname = callerOrigin.ChunkName(chunkname)
		}
		fn, err := L.Load(strings.NewReader(src), chunkname)
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		noTailCalls(fn.Proto)
		if callerEnv == nil {
			callerEnv = s.NewChunkEnv()
		}
		fn.Env = callerEnv
		L.Push(fn)
		return 1
	})
}

// callerChunk returns the environment and origin of the Lua chunk that
// called the Go function running on thread L (a coroutine's own stack, not
// the resumer's). A Go caller, or none, yields a nil env and no origin.
func (s *Sandbox) callerChunk(L *glua.LState) (*glua.LTable, Origin) {
	dbg, ok := L.GetStack(1)
	if !ok {
		return nil, ""
	}
	v, err := L.GetInfo("f", dbg, glua.LNil)
	if err != nil {
		return nil, ""
	}
	caller, ok := v.(*glua.LFunction)
	if !ok || caller.IsG || caller.Env == s.frozen {
		return nil, ""
	}
	origin, _ := splitChunkName(caller.Proto.SourceName)
	return caller.Env, origin
}

// debugTraceback renders the stack with frames from other origins collapsed
// to their origin label (DESIGN §5).
func (s *Sandbox) debugTraceback(L *glua.LState) int {
	msg := L.OptString(1, "")
	var b strings.Builder
	if msg != "" {
		b.WriteString(msg)
		b.WriteString("\n")
	}
	b.WriteString("stack traceback:")
	cur := Origin("")
	if s.originOf != nil {
		cur = s.originOf(L)
	}
	for lvl := 1; ; lvl++ {
		dbg, ok := L.GetStack(lvl)
		if !ok {
			break
		}
		frameOrigin, detail := frameInfo(L, dbg)
		if frameOrigin != cur && frameOrigin != OriginHost && frameOrigin != "" {
			b.WriteString("\n\t[" + string(frameOrigin) + "]")
			continue
		}
		if detail != "" {
			b.WriteString("\n\t" + detail)
		}
	}
	L.Push(glua.LString(b.String()))
	return 1
}

func frameInfo(L *glua.LState, dbg *glua.Debug) (Origin, string) {
	fn, err := L.GetInfo("Snlf", dbg, glua.LNil)
	if err != nil {
		return "", ""
	}
	f, ok := fn.(*glua.LFunction)
	if !ok {
		return "", ""
	}
	if f.IsG {
		name := ""
		if dbg.Name != "" {
			name = " in Go function '" + dbg.Name + "'"
		}
		return OriginHost, "<go>" + name
	}
	origin, rest := splitChunkName(f.Proto.SourceName)
	lineStr := ""
	if dbg.CurrentLine > 0 {
		lineStr = fmt.Sprintf(":%d", dbg.CurrentLine)
	}
	name := dbg.Name
	if name == "" {
		name = "?"
	}
	return origin, fmt.Sprintf("%s%s in function '%s'", rest, lineStr, name)
}

func splitChunkName(source string) (Origin, string) {
	s := strings.TrimPrefix(source, "@")
	if before, after, ok := strings.Cut(s, ":"); ok {
		return Origin(before), after
	}
	return "", s
}
