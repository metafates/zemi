package lua

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metafates/zemi/internal/logging"
	glua "github.com/yuin/gopher-lua"
)

// Runtime owns the interpreter lifecycle: one live generation at a time, a
// hot flag, and the actor queue contract (DESIGN §4). Only the dispatcher
// goroutine of the current generation touches the LState.
type Runtime struct {
	log   *logging.Logger
	paths PathClasses
	rfs   fs.FS // embedded runtime/ tree (lua/ under it)

	hot     atomic.Bool
	hotMu   sync.Mutex // orders goHot's generation check against retiring cur
	booting atomic.Bool
	cur     atomic.Pointer[Generation]
	seq     atomic.Int64

	printHook func(origin, line string)
	reloadMu  sync.Mutex
}

// frame is one entry of the origin stack: the origin and the file path of
// the chunk currently running on the interpreter goroutine.
type frame struct {
	origin Origin
	path   string
}

// RuntimeOptions configures a Runtime.
type RuntimeOptions struct {
	Log       *logging.Logger
	PathClass PathClasses
	RuntimeFS fs.FS // embedded FS rooted at runtime/ (containing lua/...)
}

func NewRuntime(opts RuntimeOptions) *Runtime {
	r := &Runtime{
		log:   opts.Log,
		paths: opts.PathClass,
		rfs:   opts.RuntimeFS,
	}
	if r.log == nil {
		r.log = logging.New("")
	}
	return r
}

// SetPrintHook routes Lua print() to the host (origin-tagged).
func (r *Runtime) SetPrintHook(fn func(origin, line string)) {
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

// CurrentOrigin returns the origin of the chunk currently running on L's
// interpreter (host entries push the registration origin).
func (r *Runtime) CurrentOrigin(L *glua.LState) Origin {
	return genOf(L).top().origin
}

// CurrentPath returns the file path of the chunk running on L, or "".
func (r *Runtime) CurrentPath(L *glua.LState) string {
	return genOf(L).top().path
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
	r.log.Error("lua", "runtime marked hot: "+cause.Error(), nil)
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
	if old := r.current(); old != nil {
		old.drain(ErrReloaded)
		close(old.stop)
		r.hotMu.Lock()
		r.cur.Store(nil)
		r.hotMu.Unlock()
		r.log.Info("lua", "generation retired for reload", map[string]any{"gen": old.ID})
	}
	if purge != nil {
		purge()
	}
	r.hot.Store(false)
	gen, err := r.boot(ctx, steps)
	if err != nil {
		return err
	}
	r.log.Info("lua", "generation booted", map[string]any{"gen": gen.ID})
	return nil
}

func (r *Runtime) boot(ctx context.Context, steps []BootStep) (*Generation, error) {
	r.booting.Store(true)
	defer r.booting.Store(false)
	var gen *Generation
	sb, err := NewSandbox(func() Origin { return gen.top().origin })
	if err != nil {
		return nil, err
	}
	sb.SetPrint(func(origin, line string) {
		if r.printHook != nil {
			r.printHook(origin, line)
		}
	})
	gen = &Generation{
		ID:      r.seq.Add(1),
		rt:      r,
		SB:      sb,
		L:       sb.L,
		modules: map[string]glua.LValue{},
		signal:  make(chan struct{}, 1),
		stop:    make(chan struct{}),
	}
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
		if err := step.Run(ctx, r, gen); err != nil {
			err = fmt.Errorf("boot step %s: %w", step.Name, err)
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

// Do enqueues fn on the current generation and waits for the result.
// ceiling == 0 selects the class default.
func (r *Runtime) Do(ctx context.Context, class Class, ceiling, grace time.Duration, fn Fn) ([]any, error) {
	if r.hot.Load() {
		return nil, ErrHot
	}
	gen := r.current()
	if gen == nil {
		return nil, errors.New("zemi: lua runtime not booted")
	}
	return gen.do(ctx, class, ceiling, grace, fn)
}

func (g *Generation) do(ctx context.Context, class Class, ceiling, grace time.Duration, fn Fn) ([]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r := newRequest(class, fn, ctx, ceiling, grace)
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
		if res.err != nil && !errors.Is(res.err, ErrCanceled) && !errors.Is(res.err, ErrAbandoned) {
			return nil, fmt.Errorf("%w (ctx: %v)", res.err, ctx.Err())
		}
		return nil, context.Canceled
	}
}

// Call runs a registered Lua callback with Go arguments. It is the standard
// host -> Lua entry point for UI-class work.
func (r *Runtime) Call(ctx context.Context, origin Origin, fn *glua.LFunction, args ...any) ([]any, error) {
	return r.Do(ctx, ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		defer r.PushFrame(L, origin, "callback")()
		base := L.GetTop()
		L.Push(fn)
		for _, a := range args {
			L.Push(FromGo(L, a))
		}
		if err := L.PCall(len(args), glua.MultRet, nil); err != nil {
			L.SetTop(base)
			return nil, err
		}
		nres := L.GetTop() - base
		out := make([]any, 0, nres)
		for i := 1; i <= nres; i++ {
			v, err := ToGo(L.Get(base + i))
			if err != nil {
				L.SetTop(base)
				return nil, err
			}
			out = append(out, v)
		}
		L.SetTop(base)
		return out, nil
	})
}

// ExecuteChunk compiles and runs src as a chunk of the given origin on the
// interpreter goroutine.
func (r *Runtime) ExecuteChunk(ctx context.Context, src, rel string, origin Origin) error {
	_, err := r.Do(ctx, ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		return nil, r.runChunk(L, src, rel, origin)
	})
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
// already does: a Lua error carries its chunk name and line.
func chunkErr(rel string, err error) error {
	if strings.Contains(err.Error(), rel) {
		return err
	}
	return fmt.Errorf("%s: %w", rel, err)
}

func (r *Runtime) runChunk(L *glua.LState, src, rel string, origin Origin) error {
	sb, err := r.sandboxOf(L)
	if err != nil {
		return err
	}
	fn, err := sb.CompileChunk(src, origin.ChunkName(rel))
	if err != nil {
		return chunkErr(rel, err)
	}
	defer r.PushFrame(L, origin, rel)()
	L.Push(fn)
	if err := L.PCall(0, 0, nil); err != nil {
		return chunkErr(rel, err)
	}
	return nil
}

// Eval runs src as a chunk of origin on the interpreter and returns its
// results. print in the chunk (and functions it defines) goes to out
// instead of the print hook; the chunk's globals stay in its own env.
func (r *Runtime) Eval(ctx context.Context, src, rel string, origin Origin, ceiling time.Duration, out func(string)) ([]any, error) {
	return r.Do(ctx, ClassIO, ceiling, time.Second, func(L *glua.LState) ([]any, error) {
		sb, err := r.sandboxOf(L)
		if err != nil {
			return nil, err
		}
		fn, err := sb.CompileChunk(src, origin.ChunkName(rel))
		if err != nil {
			return nil, chunkErr(rel, err)
		}
		// CompileChunk already gave the chunk a fresh env carrying the
		// protected-global guard. A raw print here shadows the frozen one
		// for this chunk alone; wrapping the env in a plain __index
		// metatable instead would drop that guard.
		fn.Env.RawSetString("print", L.NewFunction(func(L *glua.LState) int {
			out(joinArgs(L, "\t"))
			return 0
		}))
		defer r.PushFrame(L, origin, rel)()
		base := L.GetTop()
		L.Push(fn)
		if err := L.PCall(0, glua.MultRet, nil); err != nil {
			L.SetTop(base)
			return nil, err
		}
		var vals []any
		for i := base + 1; i <= L.GetTop(); i++ {
			v, err := ToGo(L.Get(i))
			if err != nil {
				// not ToStringMeta: a raising __tostring would fail the chunk
				// after it already ran
				v = L.Get(i).String()
			}
			vals = append(vals, v)
		}
		L.SetTop(base)
		return vals, nil
	})
}

// EvalFileInline loads and runs a file on the caller's interpreter turn:
// for use from inside a namespace call, which already runs on the
// interpreter goroutine (Do would deadlock there). The path is
// symlink-resolved and must classify to a known origin; the origin
// argument must match the classification, so a caller cannot launder a
// chunk through a higher-trust origin.
func (r *Runtime) EvalFileInline(L *glua.LState, path string, origin Origin) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if class := r.paths.Classify(resolved); class != origin {
		return fmt.Errorf("refusing %s: resolves to %q origin, not %q", resolved, class, origin)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return err
	}
	return r.runChunk(L, string(data), resolved, origin)
}

func (r *Runtime) installRequire(gen *Generation) {
	L := gen.L
	gen.SB.SetRequire(L.NewClosure(func(L *glua.LState) int {
		name := L.CheckString(1)
		val, err := r.requireModule(gen, name)
		if err != nil {
			L.RaiseError("require: %v", err)
			return 0
		}
		L.Push(val)
		return 1
	}))
}

func (r *Runtime) requireModule(gen *Generation, name string) (glua.LValue, error) {
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
	path, origin, err := r.findModule(gen.L, name)
	if err != nil {
		return glua.LNil, err
	}
	return r.loadModule(gen, path, name, origin, func() ([]byte, string, error) {
		data, err := os.ReadFile(path)
		return data, path, err
	})
}

// loadModule runs a module once per generation and trust class, keyed by
// its resolved file, so "app.util" and "./util" naming one file share one
// instance. A requirer below the file's origin gets its own instance run
// under the requirer's origin: a shared table would let lower code plant
// functions that higher-origin callbacks later call. Builtin modules are
// the exception: they keep no caller-supplied functions, and rerunning
// zemi.defaultui would re-register the UI and hit the cross-origin refusal,
// so lower origins share the builtin instance through a read-only view.
func (r *Runtime) loadModule(gen *Generation, key, name string, origin Origin, read func() ([]byte, string, error)) (glua.LValue, error) {
	req := r.CurrentOrigin(gen.L)
	lower := req != OriginHost && req.Rank() < origin.Rank()
	if lower && origin != OriginBuiltin {
		origin = req
	}
	key += "|" + string(origin)
	if !lower || origin != OriginBuiltin {
		return r.moduleInstance(gen, key, name, origin, read)
	}
	if cached, ok := gen.modules[key+"|view"]; ok {
		return cached, nil
	}
	mod, err := r.moduleInstance(gen, key, name, origin, read)
	if tb, ok := mod.(*glua.LTable); ok && err == nil {
		mod = gen.SB.readOnlyView(tb, "builtin module "+name)
		gen.modules[key+"|view"] = mod
	}
	return mod, err
}

func (r *Runtime) moduleInstance(gen *Generation, key, name string, origin Origin, read func() ([]byte, string, error)) (glua.LValue, error) {
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

// findModule resolves a module name to a symlink-resolved file. Relative
// names ("./util", "../lib/x") resolve against the requiring file and must
// stay inside its origin's tree, so a path cannot reach code of another
// trust class; dotted names search <root>/lua over the runtimepath.
func (r *Runtime) findModule(L *glua.LState, name string) (string, Origin, error) {
	var bases []string
	var from string
	relative := strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../")
	if relative {
		from = r.callerFile(L)
		if !filepath.IsAbs(from) {
			return "", "", fmt.Errorf("relative require %q: the caller is not a file", name)
		}
		bases = []string{filepath.Join(filepath.Dir(from), filepath.FromSlash(strings.TrimSuffix(name, ".lua")))}
	} else {
		rel := filepath.FromSlash(strings.ReplaceAll(name, ".", "/"))
		for _, root := range r.runtimepath(L) {
			bases = append(bases, filepath.Join(root, "lua", rel))
		}
	}
	for _, base := range bases {
		for _, p := range []string{base + ".lua", filepath.Join(base, "init.lua")} {
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
		return glua.LNil, chunkErr(rel, cerr)
	}
	L := gen.L
	defer gen.pushFrame(origin, rel)()
	L.Push(fn)
	if perr := L.PCall(0, 1, nil); perr != nil {
		return glua.LNil, chunkErr(rel, perr)
	}
	out := L.Get(-1)
	if out == glua.LNil {
		out = glua.LTrue
	}
	L.Pop(1)
	return out, nil
}

// runtimepath lists require roots in order: the project's .zemi for project
// code only (user and plugin requires never resolve into a repository's
// untrusted files), then config home and plugins; the embedded runtime is
// consulted separately (DESIGN §5).
func (r *Runtime) runtimepath(L *glua.LState) []string {
	var out []string
	for _, want := range []Origin{OriginProject, OriginUser, OriginPlugin} {
		if want == OriginProject && r.CurrentOrigin(L) != OriginProject {
			continue
		}
		for _, c := range r.paths {
			if c.Origin == want && c.Prefix != "" {
				out = append(out, c.Prefix)
			}
		}
	}
	return out
}

// BootStep is one stage of the boot sequence, executed on the interpreter
// goroutine of each new generation.
type BootStep struct {
	Name string
	Run  func(ctx context.Context, r *Runtime, g *Generation) error
}

// LoadFileStep loads a Lua file from disk as the given origin; a missing
// file is skipped.
func LoadFileStep(name, path string, origin Origin) BootStep {
	return BootStep{
		Name: name,
		Run: func(ctx context.Context, r *Runtime, g *Generation) error {
			if path == "" {
				return nil
			}
			if err := r.EvalFileInline(g.L, path, origin); !errors.Is(err, fs.ErrNotExist) {
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
			_, err := r.requireModule(g, module)
			return err
		},
	}
}
