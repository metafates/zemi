package lua

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"

	"github.com/metafates/zemi/internal/plugin"
)

// Runtime owns the interpreter lifecycle: one live generation at a time, a
// hot flag, and the actor queue contract (DESIGN §4). Only the dispatcher
// goroutine of the current generation touches the LState.
type Runtime struct {
	log   *slog.Logger
	paths PathClasses
	rfs   fs.FS // embedded runtime/ tree (lua/ under it)

	hot     atomic.Bool
	hotMu   sync.Mutex // orders goHot's generation check against retiring cur
	booting atomic.Bool
	// reloading marks the window where the old generation is retired and
	// the new one is not up, so requests read as ErrReloaded, not as a
	// runtime nobody ever booted.
	reloading atomic.Bool
	cur       atomic.Pointer[Generation]
	seq       atomic.Int64

	printHook  func(line string)
	reloadMu   sync.Mutex
	clock      Clock
	wholeTasks bool
}

// Clock is the time Lua sees: os.time, os.date and os.clock read it, and
// the host runs zemi.timer on it. zemi plugin test gives its zemi a fake one.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) AfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

// frame is one entry of the origin stack: the origin and the file path of
// the chunk currently running on the interpreter goroutine.
type frame struct {
	origin Origin
	path   string
}

// RuntimeOptions configures a Runtime.
type RuntimeOptions struct {
	Log       *slog.Logger
	PathClass PathClasses
	RuntimeFS fs.FS // embedded FS rooted at runtime/ (containing lua/...)
	Clock     Clock // nil is the wall clock
	// WholeTasks makes StartTask wait for the whole task, as RunTask does:
	// a test zemi drives one thing at a time and reads what it left.
	WholeTasks bool
}

func NewRuntime(opts RuntimeOptions) *Runtime {
	r := &Runtime{
		log:        opts.Log,
		paths:      opts.PathClass,
		rfs:        opts.RuntimeFS,
		clock:      cmp.Or[Clock](opts.Clock, wallClock{}),
		wholeTasks: opts.WholeTasks,
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	return r
}

// Clock is the clock Lua reads.
func (r *Runtime) Clock() Clock { return r.clock }

// SetPrintHook routes Lua print() to the host.
func (r *Runtime) SetPrintHook(fn func(line string)) {
	r.printHook = fn
}

// genKey is the registry slot holding an LState's *Generation. Lua cannot
// reach the registry (no debug.getregistry), so it cannot forge it.
const genKey = "zemi.generation"

// genOf is the generation L belongs to, retired or live, or nil. Frames
// live on the generation so a retired generation's goroutine, still
// running after ForceReload, only touches its own stack (DESIGN §4).
func genOf(L *glua.LState) *Generation {
	if L == nil {
		return nil
	}
	ud, _ := L.G.Registry.RawGetString(genKey).(*glua.LUserData)
	if ud == nil {
		return nil
	}
	g, _ := ud.Value.(*Generation)
	return g
}

// PushFrame pushes the origin of the chunk about to run on L's generation
// and returns its pop, for `defer r.PushFrame(L, ...)()`.
func (r *Runtime) PushFrame(L *glua.LState, origin Origin, path string) func() {
	g := genOf(L)
	if g == nil {
		return func() {}
	}
	return g.pushFrame(origin, path)
}

// CurrentOrigin returns the origin of the code running on L: the innermost
// Lua file on the call stack, so a plugin's setup() that user init.lua calls
// still runs as the plugin.
func (r *Runtime) CurrentOrigin(L *glua.LState) Origin {
	return r.effective(L).origin
}

// CurrentPath returns the file of the code CurrentOrigin names, or "".
func (r *Runtime) CurrentPath(L *glua.LState) string {
	return r.effective(L).path
}

// effective names the code running on L: the innermost Lua file on L's stack
// or on the threads that resumed it, since a chunk keeps the origin it was
// compiled with. A chunk whose source names no file (a generated one, an
// embedded module's relative path) keeps its origin and takes the path of the
// frame the host entry pushed.
// NOTE: O(stack depth) per call; caching per frame push is the upgrade.
func (r *Runtime) effective(L *glua.LState) frame {
	top := genOf(L).top()
	for th := L; th != nil; th = th.Parent {
		for lvl := 0; ; lvl++ {
			dbg, ok := th.GetStack(lvl)
			if !ok {
				break
			}
			if _, err := th.GetInfo("S", dbg, glua.LNil); err != nil {
				continue
			}
			o, path := splitChunkName(dbg.Source)
			if o == "" || o == OriginHost {
				continue
			}
			if r.paths.Classify(path) != o {
				path = top.path // no file of its own: name it by the entry's
			}
			return frame{o, path}
		}
	}
	return top
}

// The stack is touched only by the goroutine running the generation's
// Lua (boot steps, then its dispatcher), so it needs no lock.
func (g *Generation) pushFrame(origin Origin, path string) func() {
	g.frames = append(g.frames, frame{origin: origin, path: path})
	depth := len(g.frames)
	return func() {
		if len(g.frames) >= depth {
			g.frames = g.frames[:depth-1]
		}
	}
}

func (g *Generation) top() frame {
	if g == nil || len(g.frames) == 0 {
		return frame{origin: OriginHost}
	}
	return g.frames[len(g.frames)-1]
}

// goHot marks the runtime wedged: every Lua request fails fast until
// ForceReload (DESIGN §4).
func (r *Runtime) goHot(g *Generation, cause error) {
	r.hotMu.Lock()
	// a retired generation's watchdog must not wedge its replacement
	stale := r.current() != g || r.hot.Swap(true)
	r.hotMu.Unlock()
	if stale {
		return
	}
	r.log.Error("runtime marked hot: "+cause.Error(), "origin", "lua")
	g.drain(ErrHot)
}

func (r *Runtime) IsHot() bool { return r.hot.Load() }

func (r *Runtime) current() *Generation { return r.cur.Load() }

// Boot builds the first generation and runs the boot steps through its
// queue. steps run in order; each sees a fresh generation.
func (r *Runtime) Boot(ctx context.Context, steps []BootStep) error {
	// a reload during boot would boot a second generation over this one
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	_, err := r.boot(ctx, steps)
	return err
}

// ForceReload tears down the current generation (wedged or not), purges
// its registrations via the callback, and boots a fresh one (DESIGN §4).
// The reload trigger is Go-reserved and never routed through Lua.
func (r *Runtime) ForceReload(ctx context.Context, steps []BootStep, purge func()) error {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	r.reloading.Store(true)
	defer r.reloading.Store(false)
	r.retire()
	if purge != nil {
		purge()
	}
	r.hot.Store(false)
	gen, err := r.boot(ctx, steps)
	if err != nil {
		return err
	}
	r.log.Info("generation booted", "origin", "lua", "gen", gen.ID)
	return nil
}

// Close retires the live generation: its queued and running requests end,
// its tasks' waits end and its dispatcher stops, so it holds nothing.
func (r *Runtime) Close() {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	r.retire()
}

func (r *Runtime) retire() {
	old := r.current()
	if old == nil {
		return
	}
	old.drain(ErrReloaded)
	close(old.stop)
	r.hotMu.Lock()
	r.cur.Store(nil)
	r.hotMu.Unlock()
	r.log.Info("generation retired", "origin", "lua", "gen", old.ID)
}

func (r *Runtime) boot(ctx context.Context, steps []BootStep) (*Generation, error) {
	r.booting.Store(true)
	defer r.booting.Store(false)
	prefixResolution.Clear()
	sb, err := NewSandbox(r.CurrentOrigin, r.clock.Now)
	if err != nil {
		return nil, err
	}
	sb.SetPrint(func(line string) {
		if r.printHook != nil {
			r.printHook(line)
		}
	})
	gen := &Generation{
		ID:      r.seq.Add(1),
		rt:      r,
		SB:      sb,
		L:       sb.L,
		modules: map[string]glua.LValue{},
		signal:  make(chan struct{}, 1),
		stop:    make(chan struct{}),
		tasks:   map[*glua.LState]*taskRun{},
	}
	gen.ctx, gen.cancel = context.WithCancelCause(context.Background())
	ud := sb.L.NewUserData()
	ud.Value = gen
	sb.L.G.Registry.RawSetString(genKey, ud)
	r.installRequire(gen)

	// r.cur is stored before the steps so early callers reach the
	// generation and wait in do(); the dispatcher starts only after the
	// steps, so boot chunks and queued requests never run concurrently
	// on one LState (DESIGN §4 single writer).
	r.cur.Store(gen)

	for _, step := range steps {
		started := time.Now()
		err := step.Run(ctx, r, gen)
		r.log.Debug("boot step", "origin", "boot", "name", step.Name, "elapsed", time.Since(started).String())
		if err != nil {
			err = &StepError{step.Name, err}
			// no dispatcher will run this generation, so its requests,
			// queued and future, must fail fast until a reload succeeds
			r.goHot(gen, err)
			return gen, err
		}
	}

	go gen.loop()
	return gen, nil
}

// Booting reports whether boot steps are running. Config chunks run inside
// them, so a reload asked for by one would replay the steps it is running
// in, over and over.
func (r *Runtime) Booting() bool { return r.booting.Load() }

// Reloading reports the window where the old generation is retired and the
// new one is not up: its registrations are purged, so a screen drawn here
// would lose its widgets and theme.
func (r *Runtime) Reloading() bool { return r.reloading.Load() }

// Do enqueues fn on the current generation and waits for the result.
// ceiling == 0 selects the class default; name is what fn runs, for the
// soft-budget warn and the abandon error.
func (r *Runtime) Do(ctx context.Context, class Class, ceiling, grace time.Duration, name string, fn Fn) ([]any, error) {
	if r.hot.Load() {
		return nil, ErrHot
	}
	gen := r.current()
	if gen == nil {
		if r.reloading.Load() {
			return nil, ErrReloaded
		}
		return nil, errors.New("zemi: lua runtime not booted")
	}
	return gen.do(ctx, class, ceiling, grace, name, fn)
}

func (g *Generation) do(ctx context.Context, class Class, ceiling, grace time.Duration, name string, fn Fn) ([]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r := newRequest(ctx, class, ceiling, grace, name, fn)
	g.enqueue(r)
	select {
	case res := <-r.reply:
		if res.err != nil {
			return nil, res.err
		}
		return res.vals, nil
	case <-ctx.Done():
		r.cancel()
		// Wait for the executor, watchdog or drain to settle the request,
		// so an error other than cancel or abandon still reaches the caller.
		res := <-r.reply
		if res.err != nil && !errors.Is(res.err, context.Canceled) && !errors.Is(res.err, ErrAbandoned) {
			return nil, fmt.Errorf("%w (ctx: %v)", res.err, ctx.Err()) //nolint:errorlint // res.err is the cause; a wrapped ctx error would read as a plain cancel
		}
		return nil, context.Canceled
	}
}

// ExecuteChunk compiles and runs src as a chunk of the given origin, as a
// task.
func (r *Runtime) ExecuteChunk(ctx context.Context, src, rel string, origin Origin) error {
	_, err := r.Eval(ctx, src, rel, origin, 0, nil)
	return err
}

// sandboxOf is the sandbox of L's generation; a request that outlived its
// generation (ForceReload retired it mid-run) gets ErrReloaded.
func (r *Runtime) sandboxOf(L *glua.LState) (*Sandbox, error) {
	if g := r.current(); g != nil && g.L.G == L.G {
		return g.SB, nil
	}
	return nil, ErrReloaded
}

// chunkErr names the file a chunk error came from, unless the error
// already does: a Lua error carries its chunk name and line, shown without
// the origin tag the chunk name carries. A syntax error gets the same
// file:line: shape (gopher-lua's own puts the line in the middle and "at
// EOF" for an unclosed block), and a traceback that holds only the main
// chunk and Go frames is dropped: it says nothing the message does not.
func chunkErr(rel, src string, origin Origin, err error) error {
	api, ok := errors.AsType[*glua.ApiError](err)
	if !ok {
		if strings.Contains(err.Error(), rel) {
			return err
		}
		return fmt.Errorf("%s: %w", rel, err)
	}
	if pe, ok := errors.AsType[*parse.Error](api.Cause); ok {
		return SyntaxErr(rel, src, pe)
	}
	untag := func(s string) string { return strings.ReplaceAll(s, origin.ChunkName(rel), rel) }
	msg := untag(api.Object.String())
	if _, ok := api.Object.(*glua.LTable); ok {
		// error({ code = 1 }) prints as its address otherwise
		if v, err := ToGo(api.Object); err == nil {
			if data, err := json.Marshal(v, json.Deterministic(true)); err == nil {
				msg = string(data)
			}
		}
	}
	if !strings.Contains(msg, rel) {
		msg = rel + ": " + msg
	}
	if usefulTrace(api.StackTrace) {
		msg += "\n" + untag(api.StackTrace)
	}
	return errors.New(msg)
}

// SyntaxErr is a parse error of src, the file rel, as rel:line: message.
func SyntaxErr(rel, src string, pe *parse.Error) error {
	if pe.Pos.Line == parse.EOF {
		return fmt.Errorf("%s:%d: %s at end of file", rel, strings.Count(strings.TrimRight(src, "\n"), "\n")+1, pe.Message)
	}
	return fmt.Errorf("%s:%d: %s near '%s'", rel, pe.Pos.Line, pe.Message, pe.Token)
}

// usefulTrace reports a traceback with a Lua frame other than the main
// chunk: the message already names the main chunk's line. gopher-lua prints
// the chunk itself as "in function <name:0>" and labels only the Go PCall
// boundary "in main chunk".
func usefulTrace(trace string) bool {
	for line := range strings.SplitSeq(trace, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "[") && !strings.HasPrefix(line, "stack traceback") && !strings.HasSuffix(line, ":0>") {
			return true
		}
	}
	return false
}

func (r *Runtime) runChunk(ctx context.Context, L *glua.LState, src, rel string, origin Origin) error {
	sb, err := r.sandboxOf(L)
	if err != nil {
		return err
	}
	fn, err := sb.CompileChunk(src, origin.ChunkName(rel))
	if err != nil {
		return chunkErr(rel, src, origin, err)
	}
	defer r.PushFrame(L, origin, rel)()
	if L.Context() == nil {
		// a boot step's chunk ends with ctx, as a request's code does with its own
		L.SetContext(ctx)
		defer L.RemoveContext()
	}
	L.Push(fn)
	if err := L.PCall(0, 0, nil); err != nil {
		return chunkErr(rel, src, origin, err)
	}
	return nil
}

// Eval runs src as a chunk of origin, as a task, and returns its results.
// With out set, print in the chunk (and functions it defines) goes to out
// instead of the print hook; the chunk's globals stay in its own env.
func (r *Runtime) Eval(ctx context.Context, src, rel string, origin Origin, ceiling time.Duration, out func(string)) ([]any, error) {
	vals, err := r.RunTask(ctx, Task{
		Class: ClassIO, Ceiling: ceiling, Origin: origin, Path: rel,
		Start: func(L *glua.LState) (*glua.LFunction, []glua.LValue, error) {
			sb, err := r.sandboxOf(L)
			if err != nil {
				return nil, nil, err
			}
			fn, err := sb.CompileChunk(src, origin.ChunkName(rel))
			if err != nil {
				return nil, nil, chunkErr(rel, src, origin, err)
			}
			if out != nil {
				// CompileChunk already gave the chunk a fresh env carrying the
				// protected-global guard. A raw print here shadows the frozen
				// one for this chunk alone; wrapping the env in a plain __index
				// metatable instead would drop that guard.
				fn.Env.RawSetString("print", L.NewFunction(func(L *glua.LState) int {
					out(joinArgs(L, "\t"))
					return 0
				}))
			}
			return fn, nil, nil
		},
		Done: func(L *glua.LState, rets []glua.LValue) ([]any, error) {
			// a resume reports a chunk that returned nothing as one nil
			for len(rets) > 0 && rets[len(rets)-1] == glua.LNil {
				rets = rets[:len(rets)-1]
			}
			var vals []any
			for _, lv := range rets {
				v, err := ToGo(lv)
				if err != nil {
					// not ToStringMeta: a raising __tostring would fail the chunk
					// after it already ran
					v = lv.String()
				}
				vals = append(vals, v)
			}
			return vals, nil
		},
	})
	if _, ok := errors.AsType[*glua.ApiError](err); ok {
		err = chunkErr(rel, src, origin, err)
	}
	return vals, err
}

// EvalFileInline loads and runs a file on the caller's interpreter turn:
// for use from inside a namespace call, which already runs on the
// interpreter goroutine (Do would deadlock there). The path is
// symlink-resolved; origin only labels the chunk. ctx stops it at its next
// opcode.
func (r *Runtime) EvalFileInline(ctx context.Context, L *glua.LState, path string, origin Origin) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return err
	}
	return r.runChunk(ctx, L, string(data), resolved, origin)
}

func (r *Runtime) installRequire(gen *Generation) {
	L := gen.L
	gen.SB.SetRequire(L.NewClosure(func(L *glua.LState) int {
		name := L.CheckString(1)
		val, err := r.Require(gen, L, name)
		if err != nil {
			L.RaiseError("require: %v", err)
			return 0
		}
		L.Push(val)
		return 1
	}))
}

// Require is require(name) called on thread L, whose stack names the
// requiring file.
func (r *Runtime) Require(gen *Generation, L *glua.LState, name string) (glua.LValue, error) {
	if ns := gen.SB.LookupPath(name); ns != glua.LNil {
		return ns, nil
	}
	if strings.HasPrefix(name, "zemi.") {
		// zemi.* resolves to the Go preload or the embedded stdlib only;
		// userland cannot shadow it (DESIGN §5).
		return r.loadModule(gen, "embedded:"+name, name, OriginBuiltin, func() ([]byte, string, error) {
			return r.readEmbedded(name)
		})
	}
	path, origin, err := r.findModule(gen, L, name)
	if err != nil {
		return glua.LNil, err
	}
	return r.loadModule(gen, path, name, origin, func() ([]byte, string, error) {
		data, err := os.ReadFile(path)
		return data, path, err
	})
}

// loadModule runs a module once per generation, keyed by its resolved file,
// so "app.util" and "./util" naming one file share one instance.
func (r *Runtime) loadModule(gen *Generation, key, name string, origin Origin, read func() ([]byte, string, error)) (glua.LValue, error) {
	if cached, ok := gen.modules[key]; ok {
		return cached, nil
	}
	if _, loading := gen.modules["<loading:"+key+">"]; loading {
		return glua.LNil, fmt.Errorf("circular require of %q", name)
	}
	gen.modules["<loading:"+key+">"] = glua.LTrue
	defer delete(gen.modules, "<loading:"+key+">")
	src, rel, err := read()
	if err != nil {
		return glua.LNil, err
	}
	mod, err := r.evalModule(gen, src, rel, origin)
	if err != nil {
		return glua.LNil, err
	}
	gen.modules[key] = mod
	return mod, nil
}

func (r *Runtime) readEmbedded(name string) ([]byte, string, error) {
	if r.rfs != nil {
		base := strings.ReplaceAll(name, ".", "/")
		for _, rel := range []string{base + ".lua", base + "/init.lua"} {
			if data, err := fs.ReadFile(r.rfs, "lua/"+rel); err == nil {
				return data, rel, nil
			}
		}
	}
	return nil, "", fmt.Errorf("module %q not found in embedded runtime", name)
}

// cand is one place a module may sit: its base path, and the plugin dir the
// file must be in ("" for any other origin).
type cand struct{ base, root string }

// findModule resolves a module name to a symlink-resolved file. Relative
// names ("./util", "../lib/x") resolve against the requiring file and must
// stay inside its tree; dotted names search the runtimepath's code dirs. A
// plugin's file resolves only from its own enabled plugin, so neither ".."
// nor a symlink reaches another plugin's code or a disabled one's.
func (r *Runtime) findModule(gen *Generation, L *glua.LState, name string) (string, Origin, error) {
	var cands []cand
	var from string
	relative := strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../")
	if relative {
		from = r.callerFile(L)
		if !filepath.IsAbs(from) {
			return "", "", fmt.Errorf("relative require %q: the caller is not a file", name)
		}
		cands = []cand{{filepath.Join(filepath.Dir(from), filepath.FromSlash(strings.TrimSuffix(name, ".lua"))), gen.pluginRoot(from)}}
	} else {
		rel := filepath.FromSlash(strings.ReplaceAll(name, ".", "/"))
		for _, c := range r.runtimepath(gen, L, rel) {
			cands = append(cands, cand{filepath.Join(c.base, rel), c.root})
		}
	}
	for _, c := range cands {
		for _, p := range []string{c.base + ".lua", filepath.Join(c.base, "init.lua")} {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			origin := r.paths.Classify(resolved)
			switch {
			case origin == "":
				return "", "", fmt.Errorf("module %q at %s is outside known origins", name, resolved)
			case relative && origin != r.paths.Classify(from):
				return "", "", fmt.Errorf("relative require %q reaches %s, outside the %s tree", name, resolved, r.paths.Classify(from))
			case gen.pluginRoot(resolved) != c.root || origin == OriginPlugin && c.root == "":
				where := "the " + string(r.paths.Classify(c.base)) + " tree"
				if c.root != "" {
					where = "plugin " + filepath.Base(c.root)
				}
				return "", "", fmt.Errorf("module %q at %s is outside %s", name, resolved, where)
			}
			return resolved, origin, nil
		}
	}
	if relative {
		return "", "", fmt.Errorf("module %q not found next to %s", name, r.callerFile(L))
	}
	return "", "", fmt.Errorf("module %q not found on runtimepath", name)
}

// callerFile is the file of the Lua function calling require, from its
// chunk name ("@origin:/abs/path", set by Go at compile time), so a
// function defined in a module still resolves "./x" next to that module
// when it runs later as a callback. Falls back to the running frame.
func (r *Runtime) callerFile(L *glua.LState) string {
	for level := range 2 {
		dbg, ok := L.GetStack(level)
		if !ok {
			break
		}
		if _, err := L.GetInfo("S", dbg, glua.LNil); err != nil {
			continue
		}
		if _, path, ok := strings.Cut(strings.TrimPrefix(dbg.Source, "@"), ":"); ok && filepath.IsAbs(path) {
			return path
		}
	}
	return r.CurrentPath(L)
}

func (r *Runtime) evalModule(gen *Generation, src []byte, rel string, origin Origin) (glua.LValue, error) {
	sb := gen.SB
	fn, cerr := sb.CompileChunk(string(src), origin.ChunkName(rel))
	if cerr != nil {
		return glua.LNil, chunkErr(rel, string(src), origin, cerr)
	}
	L := gen.L
	defer gen.pushFrame(origin, rel)()
	L.Push(fn)
	if perr := L.PCall(0, 1, nil); perr != nil {
		return glua.LNil, chunkErr(rel, string(src), origin, perr)
	}
	out := L.Get(-1)
	if out == glua.LNil {
		out = glua.LTrue
	}
	L.Pop(1)
	return out, nil
}

// runtimepath lists the code directories a dotted require of rel searches, in
// order: the config home, then the enabled plugin named like rel's first
// segment, first for that plugin's own code. Any requirer searches every
// tree. A plugin keeps its modules with the rest of its Lua under CodeDir.
// The embedded runtime is consulted separately (DESIGN §5).
func (r *Runtime) runtimepath(gen *Generation, L *glua.LState, rel string) []cand {
	head, _, _ := strings.Cut(rel, string(filepath.Separator))
	var out []cand
	own := ""
	for _, root := range gen.Plugins {
		if filepath.Base(root) == head {
			own = root
		}
	}
	if own != "" && own == r.requirerRoot(gen, L) {
		out, own = append(out, cand{pluginCode(own), own}), ""
	}
	for _, c := range r.paths {
		if c.Origin == OriginUser && c.Prefix != "" {
			out = append(out, cand{filepath.Join(c.Prefix, "lua"), ""})
		}
	}
	if own != "" {
		out = append(out, cand{pluginCode(own), own})
	}
	return out
}

// pluginCode is where a plugin's modules live: with its manifest, under the
// code dir zemi loads it from.
func pluginCode(root string) string { return filepath.Join(root, plugin.CodeDir, "lua") }

// requirerRoot is the enabled plugin dir of the code calling require: the
// calling file's, else that of the plugin the call runs as.
func (r *Runtime) requirerRoot(gen *Generation, L *glua.LState) string {
	return cmp.Or(gen.pluginRoot(r.callerFile(L)), gen.pluginRoot(r.CurrentPath(L)))
}

// pluginRoot is the enabled plugin dir that is or holds path, or "".
func (g *Generation) pluginRoot(path string) string {
	for _, root := range g.Plugins {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return root
		}
	}
	return ""
}

// BootStep is one stage of the boot sequence, executed on the interpreter
// goroutine of each new generation.
type BootStep struct {
	Name string
	Run  func(ctx context.Context, r *Runtime, g *Generation) error
}

// StepError is the error of the boot step that stopped a boot.
type StepError struct {
	Step string
	Err  error
}

func (e *StepError) Error() string { return "boot step " + e.Step + ": " + e.Err.Error() }

func (e *StepError) Unwrap() error { return e.Err }

// LoadFileStep loads a Lua file from disk as the given origin; a missing
// file is skipped.
func LoadFileStep(name, path string, origin Origin) BootStep {
	return BootStep{
		Name: name,
		Run: func(ctx context.Context, r *Runtime, g *Generation) error {
			if path == "" {
				return nil
			}
			if err := r.EvalFileInline(ctx, g.L, path, origin); !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		},
	}
}

// RequireStep requires a module (e.g. the embedded shipped defaults).
func RequireStep(module string) BootStep {
	return BootStep{
		Name: "require " + module,
		Run: func(ctx context.Context, r *Runtime, g *Generation) error {
			_, err := r.Require(g, g.L, module)
			return err
		},
	}
}
