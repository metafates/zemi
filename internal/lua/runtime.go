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
	rfs   fs.FS // embedded runtime/lua tree

	hot atomic.Bool
	cur atomic.Pointer[Generation]
	seq atomic.Int64

	originMu sync.Mutex
	frames   []frame
	epoch    uint64 // bumped on reload; stamped into every pop

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

// PushFrame pushes the origin of the chunk about to run and returns its
// pop, for `defer r.PushFrame(...)()`. The pop carries the epoch and depth
// it pushed at: a request the watchdog abandoned can return long after a
// reload, and popping then would take a frame of the live generation and
// misattribute its origin (DESIGN §4).
func (r *Runtime) PushFrame(origin Origin, path string) func() {
	r.originMu.Lock()
	r.frames = append(r.frames, frame{origin: origin, path: path})
	epoch, depth := r.epoch, len(r.frames)
	r.originMu.Unlock()
	return func() {
		r.originMu.Lock()
		defer r.originMu.Unlock()
		if r.epoch != epoch || len(r.frames) < depth {
			return
		}
		r.frames = r.frames[:depth-1]
	}
}

// CurrentOrigin returns the origin of the chunk currently running on the
// interpreter goroutine (host entries push the registration origin).
func (r *Runtime) CurrentOrigin() Origin {
	f, ok := r.topFrame()
	if !ok {
		return OriginHost
	}
	return f.origin
}

// CurrentPath returns the file path of the currently running chunk, or "".
func (r *Runtime) CurrentPath() string {
	f, ok := r.topFrame()
	if !ok {
		return ""
	}
	return f.path
}

func (r *Runtime) topFrame() (frame, bool) {
	r.originMu.Lock()
	defer r.originMu.Unlock()
	if len(r.frames) == 0 {
		return frame{}, false
	}
	return r.frames[len(r.frames)-1], true
}

// goHot marks the runtime wedged: every Lua request fails fast until
// ForceReload (DESIGN §4).
func (r *Runtime) goHot(cause error) {
	if r.hot.Swap(true) {
		return
	}
	r.log.Error("lua", "runtime marked hot: "+cause.Error(), nil)
	if g := r.current(); g != nil {
		g.drainWith(ErrHot)
	}
}

func (r *Runtime) IsHot() bool { return r.hot.Load() }

func (r *Runtime) current() *Generation { return r.cur.Load() }

// Boot builds the first generation and runs the boot steps through its
// queue. steps run in order; each sees a fresh generation.
func (r *Runtime) Boot(ctx context.Context, steps []BootStep) error {
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
		old.drain()
		close(old.stop)
		r.cur.Store(nil)
		r.log.Info("lua", "generation retired for reload", map[string]any{"gen": old.ID})
	}
	if purge != nil {
		purge()
	}
	// An abandoned request never pops its frame: a stale frame left on the
	// stack would change require resolution and registration origins for
	// the fresh generation. The epoch bump disarms its pop, should it
	// return later.
	r.originMu.Lock()
	r.frames = nil
	r.epoch++
	r.originMu.Unlock()
	r.hot.Store(false)
	gen, err := r.boot(ctx, steps)
	if err != nil {
		return err
	}
	r.log.Info("lua", "generation booted", map[string]any{"gen": gen.ID})
	return nil
}

func (r *Runtime) boot(ctx context.Context, steps []BootStep) (*Generation, error) {
	sb, err := NewSandbox(r.CurrentOrigin)
	if err != nil {
		return nil, err
	}
	sb.SetPrint(func(origin, line string) {
		if r.printHook != nil {
			r.printHook(origin, line)
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
	}
	r.installRequire(gen)

	// r.cur is stored before the steps so early callers reach the
	// generation and wait in do(); the dispatcher starts only after the
	// steps, so boot chunks and queued requests never run concurrently
	// on one LState (DESIGN §4 single writer).
	r.cur.Store(gen)

	for _, step := range steps {
		if err := step.Run(ctx, r, gen); err != nil {
			return gen, fmt.Errorf("boot step %s: %w", step.Name, err)
		}
	}

	go gen.loop()
	return gen, nil
}

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
		// Wait for the watchdog/executor to settle the request so the
		// reply channel is never leaked; the outcome is canceled.
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
		defer r.PushFrame(origin, "callback")()
		base := L.GetTop()
		L.Push(fn)
		for _, a := range args {
			L.Push(FromGo(L, a))
		}
		n := len(args)
		if err := L.PCall(n, glua.MultRet, nil); err != nil {
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

func (r *Runtime) runChunk(L *glua.LState, src, rel string, origin Origin) error {
	sb := r.current().SB
	fn, err := sb.CompileChunk(src, origin.ChunkName(rel))
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	defer r.PushFrame(origin, rel)()
	L.Push(fn)
	if err := L.PCall(0, 0, nil); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	return nil
}

// Eval runs src as a chunk of origin on the interpreter and returns its
// results. print in the chunk (and functions it defines) goes to out
// instead of the print hook; the chunk's globals stay in its own env.
func (r *Runtime) Eval(ctx context.Context, src, rel string, origin Origin, ceiling time.Duration, out func(string)) ([]any, error) {
	return r.Do(ctx, ClassIO, ceiling, time.Second, func(L *glua.LState) ([]any, error) {
		fn, err := r.current().SB.CompileChunk(src, origin.ChunkName(rel))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		// CompileChunk already gave the chunk a fresh env carrying the
		// protected-global guard. A raw print here shadows the frozen one
		// for this chunk alone; wrapping the env in a plain __index
		// metatable instead would drop that guard.
		fn.Env.RawSetString("print", L.NewFunction(func(L *glua.LState) int {
			parts := make([]string, L.GetTop())
			for i := range parts {
				parts[i] = L.ToStringMeta(L.Get(i + 1)).String()
			}
			out(strings.Join(parts, "\t"))
			return 0
		}))
		defer r.PushFrame(origin, rel)()
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
				v = L.ToStringMeta(L.Get(i)).String()
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
		return r.loadModule(gen, "embedded:"+name, name, func() ([]byte, string, Origin, error) {
			src, rel, err := r.readEmbedded(name)
			return src, rel, OriginBuiltin, err
		})
	}
	path, origin, err := r.findModule(gen.L, name)
	if err != nil {
		return glua.LNil, err
	}
	return r.loadModule(gen, path, name, func() ([]byte, string, Origin, error) {
		data, err := os.ReadFile(path)
		return data, path, origin, err
	})
}

// loadModule runs a module once per generation, keyed by its resolved file,
// so "app.util" and "./util" naming one file share one instance.
func (r *Runtime) loadModule(gen *Generation, key, name string, read func() ([]byte, string, Origin, error)) (glua.LValue, error) {
	if cached, ok := gen.modules[key]; ok {
		return cached, nil
	}
	if _, loading := gen.modules["<loading:"+key+">"]; loading {
		return glua.LNil, fmt.Errorf("circular require of %q", name)
	}
	gen.modules["<loading:"+key+">"] = glua.LTrue
	defer delete(gen.modules, "<loading:"+key+">")
	src, rel, origin, err := read()
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
	if r.rfs == nil {
		return nil, "", fmt.Errorf("module %q not found in embedded runtime", name)
	}
	rel := strings.ReplaceAll(name, ".", "/") + ".lua"
	data, err := fs.ReadFile(r.rfs, "lua/"+rel)
	if err == nil {
		return data, rel, nil
	}
	relInit := strings.ReplaceAll(name, ".", "/") + "/init.lua"
	data, err = fs.ReadFile(r.rfs, "lua/"+relInit)
	if err == nil {
		return data, relInit, nil
	}
	return nil, "", fmt.Errorf("module %q not found in embedded runtime", name)
}

// findModule resolves a module name to a symlink-resolved file. Relative
// names ("./util", "../lib/x") resolve against the requiring file and must
// stay inside its origin's tree, so a path cannot reach code of another
// trust class; dotted names search <root>/lua over the runtimepath.
func (r *Runtime) findModule(L *glua.LState, name string) (string, Origin, error) {
	var bases []string
	relative := strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../")
	if relative {
		from := r.callerFile(L)
		if !filepath.IsAbs(from) {
			return "", "", fmt.Errorf("relative require %q: the caller is not a file", name)
		}
		bases = []string{filepath.Join(filepath.Dir(from), filepath.FromSlash(strings.TrimSuffix(name, ".lua")))}
	} else {
		rel := filepath.FromSlash(strings.ReplaceAll(name, ".", "/"))
		for _, root := range r.runtimepath() {
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
			case relative && origin != r.CurrentOrigin():
				return "", "", fmt.Errorf("relative require %q reaches %s, outside the %s tree", name, resolved, r.CurrentOrigin())
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
	for level := 0; level <= 1; level++ {
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
	return r.CurrentPath()
}

func (r *Runtime) evalModule(gen *Generation, src []byte, rel string, origin Origin) (glua.LValue, error) {
	sb := gen.SB
	fn, cerr := sb.CompileChunk(string(src), origin.ChunkName(rel))
	if cerr != nil {
		return glua.LNil, fmt.Errorf("%s: %w", rel, cerr)
	}
	L := gen.L
	defer r.PushFrame(origin, rel)()
	L.Push(fn)
	if perr := L.PCall(0, 1, nil); perr != nil {
		return glua.LNil, fmt.Errorf("%s: %w", rel, perr)
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
func (r *Runtime) runtimepath() []string {
	var out []string
	for _, want := range []Origin{OriginProject, OriginUser, OriginPlugin} {
		if want == OriginProject && r.CurrentOrigin() != OriginProject {
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

// LoadFileStep loads a Lua file from disk as the given origin.
func LoadFileStep(name, path string, origin Origin) BootStep {
	return BootStep{
		Name: name,
		Run: func(ctx context.Context, r *Runtime, g *Generation) error {
			if path == "" {
				return nil
			}
			if _, err := os.Stat(path); err != nil {
				return nil
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				resolved = path
			}
			class := r.paths.Classify(resolved)
			if class == "" {
				return fmt.Errorf("refusing %s: outside known origins", resolved)
			}
			data, err := os.ReadFile(resolved)
			if err != nil {
				return err
			}
			// The chunk carries its file path so relative requires resolve.
			return r.runChunk(g.L, string(data), resolved, class)
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
