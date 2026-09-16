package lua

import (
	"fmt"
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
	originOf  func() Origin
	start     time.Time
}

type namespaceCheck struct {
	name    string
	table   *glua.LTable
	members map[string]glua.LValue
}

// NewSandbox creates a state with the §5 sandbox applied. zemi.*
// namespaces are registered afterwards, before any chunk runs.
func NewSandbox(originOf func() Origin) (*Sandbox, error) {
	L := glua.NewState(glua.Options{
		SkipOpenLibs:  true,
		CallStackSize: 512,
		RegistrySize:  1 << 20,
	})

	libs := []struct {
		name string
		fn   glua.LGFunction
	}{
		{glua.LoadLibName, glua.OpenPackage},
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
		start:     time.Now(),
	}

	global.RawSetString("os", s.makeOsTable())
	dbg := L.NewTable()
	dbg.RawSetString("traceback", L.NewFunction(s.debugTraceback))
	global.RawSetString("debug", dbg)

	// The package table stays (require is Go-owned and never consults the
	// Lua-side module system), but its C-loading surface is stripped; its
	// subtables are frozen below together with every other shared table.
	if pkg, ok := global.RawGetString("package").(*glua.LTable); ok {
		pkg.RawSetString("loadlib", glua.LNil)
	}
	// The string module doubles as the metatable of every string value;
	// lock it so getmetatable("").format = f cannot patch string methods
	// behind the proxy (DESIGN §5).
	if strMod, ok := global.RawGetString("string").(*glua.LTable); ok {
		strMod.RawSetString("__metatable", glua.LString("zemi:protected"))
	}

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
	L.Env = frozen

	return s, nil
}

func (s *Sandbox) makeOsTable() *glua.LTable {
	L := s.L
	osTab := L.NewTable()
	L.SetFuncs(osTab, map[string]glua.LGFunction{
		"clock": func(L *glua.LState) int {
			L.Push(glua.LNumber(time.Since(s.start).Seconds()))
			return 1
		},
		"time": func(L *glua.LState) int {
			if L.GetTop() == 0 {
				L.Push(glua.LNumber(time.Now().Unix()))
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
			L.Push(glua.LNumber(time.Date(year, time.Month(mon), day, hh, mm, sec, 0, time.Local).Unix()))
			return 1
		},
		"date": luaDate,
	})
	return osTab
}

func tableInt(tb *glua.LTable, name string, def int) int {
	if v, ok := tb.RawGetString(name).(glua.LNumber); ok {
		return int(v)
	}
	return def
}

// luaDate implements the subset of os.date zemi needs: %c, %x, %X and the
// individual specifiers, with optional Unix time argument.
func luaDate(L *glua.LState) int {
	format := L.OptString(1, "%c")
	t := time.Now()
	if L.Get(2) != glua.LNil {
		t = time.Unix(L.CheckInt64(2), 0)
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
			b.WriteString(t.Format("Mon Jan _2 15:04:05 2006"))
		case 'x':
			b.WriteString(t.Format("01/02/06"))
		case 'X':
			b.WriteString(t.Format("15:04:05"))
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte(format[i])
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
			origin = s.originOf()
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
// metatable to swap events. Used for the frozen surface itself; shared leaf
// tables get protectTable instead (see below).
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
		tbl.RawSet(e.key, s.protectTable(e.tb, "namespace "+joined))
	}
}

// realTableOf resolves a protected proxy to the table its __index reads.
func realTableOf(tb *glua.LTable) *glua.LTable {
	mt, ok := tb.Metatable.(*glua.LTable)
	if !ok {
		return tb
	}
	idx, ok := mt.RawGetString("__index").(*glua.LTable)
	if !ok || idx == tb {
		return tb
	}
	return idx
}

// rawLookup reads key from tbl, falling through a protected proxy's
// __index (LookupPath walks proxies and real tables alike).
func rawLookup(tb *glua.LTable, key string) glua.LValue {
	if v := tb.RawGetString(key); v != glua.LNil {
		return v
	}
	return realTableOf(tb).RawGetString(key)
}

// FreezeTable locks a shared table from outside the package (hostapi uses
// it for nested namespace tables like zemi.ui.widget; RegisterNamespace
// additionally wraps the whole subtree in protected proxies).
func (s *Sandbox) FreezeTable(tb *glua.LTable, what string) { s.freezeTable(tb, what) }

// RegisterNamespace installs a Go-backed zemi.* table: frozen, integrity
// checked on every request class, and visible through the chunk env chain.
func (s *Sandbox) RegisterNamespace(name string, tbl *glua.LTable) error {
	return s.registerNamespace(name, tbl, nil)
}

// RegisterCallableNamespace installs a zemi.* callable table (zemi.log,
// zemi.notify): `tbl(...)` dispatches to call, members stay frozen.
func (s *Sandbox) RegisterCallableNamespace(name string, tbl *glua.LTable, call *glua.LFunction) error {
	return s.registerNamespace(name, tbl, call)
}

func (s *Sandbox) registerNamespace(name string, tbl *glua.LTable, call *glua.LFunction) error {
	if !strings.HasPrefix(name, "zemi.") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("namespace %q must be a dotted zemi.* name", name)
	}
	if s.LookupPath(name) != glua.LNil {
		return fmt.Errorf("namespace %q already registered", name)
	}
	s.freezeTable(tbl, "namespace "+name)
	s.deepProtect(tbl, name)
	proxy := s.protectTable(tbl, "namespace "+name)
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
			tbl.RawSetString(parts[i], s.protectTable(real, "namespace "+joined))
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
		v = s.protectTable(tb, "protected value "+path)
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
// descending into member tables ("widget.register" under zemi.ui). It is
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
	fn.Env = s.NewChunkEnv()
	return fn, nil
}

// makeLoader builds the sandbox load/loadstring: it compiles Lua source
// like the base library but pins the compiled function's environment to
// the loading chunk's environment, with the chunkname re-tagged to the
// caller's origin, so tracebacks of generated code redact correctly and a
// generated chunk never receives the interpreter's global table (DESIGN §5).
// With no Lua caller (Go-side use) the chunk gets the frozen surface:
// reads work, writes raise.
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
		callerEnv, callerOrigin := s.callerChunk()
		if callerOrigin != "" {
			chunkname = callerOrigin.ChunkName(chunkname)
		}
		fn, err := L.Load(strings.NewReader(src), chunkname)
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		fn.Env = callerEnv
		L.Push(fn)
		return 1
	})
}

// callerChunk returns the environment and origin of the Lua chunk that
// called the running Go function. Go-side callers (or a call chain made of
// Go functions only) yield the frozen surface and no origin.
func (s *Sandbox) callerChunk() (*glua.LTable, Origin) {
	L := s.L
	dbg, ok := L.GetStack(1)
	if !ok {
		return s.frozen, ""
	}
	v, err := L.GetInfo("f", dbg, glua.LNil)
	if err != nil {
		return s.frozen, ""
	}
	caller, ok := v.(*glua.LFunction)
	if !ok || caller.IsG || caller.Env == nil {
		return s.frozen, ""
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
		cur = s.originOf()
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
