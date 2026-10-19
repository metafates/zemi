// io_lua.go is the Lua I/O surface (DESIGN §6.5, §9): zemi.env/state/
// timer/fs/http/job. Every side-effectful call passes a `lua.<ns.call>`
// permission gate at the Lua boundary; asks fail closed (host-API calls
// from Lua never open the interactive overlay, §6.5).

package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/glob"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// inRoots reports whether path, resolved, is inside one of roots.
func (h *Host) inRoots(path string, roots ...string) bool {
	// Compare resolved paths, so a symlink inside a root cannot lead out
	// (and a root reached through a symlink, like macOS /var, still matches).
	real := perm.CanonPath(path)
	if real == "" {
		return false
	}
	for _, r := range roots {
		if _, ok := perm.Under(perm.CanonPath(r), real); ok {
			return true
		}
	}
	return false
}

// luaDataRoot holds the zemi.fs.data_dir dirs. It sits outside the config
// tree, so a write there never reloads the config.
func (h *Host) luaDataRoot() string {
	if h.Dirs.DataHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.DataHome, "lua")
}

// luaGate runs one Lua-boundary permission check. The gate settles every
// call: a read-only agent's writes deny, everything else runs (a plugin's
// own call it declared above already returned); a plugin calling past its
// manifest was refused there. req describes the call for a test zemi, where
// only a test.stub passes one of the calls that do IO: its reply is
// returned and stands in for the transport, a call none answers raises
// naming the stub to add, and a false reply is refused; req is a func so no
// other zemi builds it.
func (h *Host) luaGate(L *glua.LState, subject, arg string, req func() map[string]any) *stubReply {
	// NOTE: a raise at a config file's top level ends the file, so an inert
	// boot misses what it registers after the call; a handle that failed
	// through its callback would keep the rest.
	if h.Inert && subject != "lua.fs.read" {
		L.RaiseError("%s %s: permission denied (inert)", subject, arg)
	}
	// a plugin calls nothing its manifest does not declare: the manifest is
	// what the user reviewed when they switched it on
	name := ""
	if h.RT.CurrentOrigin(L) == lua.OriginPlugin {
		name = h.PluginOf(h.RT.CurrentPath(L))
	}
	if cap := pluginCap(subject); name != "" && cap != "" && !h.pluginDeclares(name, cap) {
		L.RaiseError("%s %s: plugin %s declares no %q; add %q to uses in %s", subject, arg, name, cap, cap, plugin.ManifestFile)
	}
	if h.test != nil {
		var reply *stubReply
		var stubbed bool
		if reply, stubbed = h.test.reply(L, subject, arg, req); reply != nil {
			return reply
		}
		if stubbed {
			// a false stub refuses the call whatever the gate settles
			L.RaiseError("%s %s: permission ask required: a host-API call from Lua never opens the interactive overlay", subject, arg)
		}
	}
	// no veto here: it runs on the actor goroutine, where its own
	// interpreter round-trip would deadlock, and the ask path has no
	// overlay to offer a Lua call anyway
	pipe := h.PipelineFor()
	pipe.Ask = nil
	if v := pipe.Check(tools.Request{Subject: subject, Arg: arg, Reason: "Lua call"}); !v.Allowed {
		L.RaiseError("%s %s: permission denied (%s)", subject, arg, v.DecidedBy)
	}
	return nil
}

// gateAsk is the origin gate both ask paths share: project code may not call a
// model at all, and a plugin's call goes through luaGate, which is where its
// manifest's declaration is checked. The stub a test zemi answers with comes
// back for the caller to stand in for the transport.
func (h *Host) gateAsk(L *glua.LState, api, subject, id string, payload func() map[string]any) *stubReply {
	switch o := h.RT.CurrentOrigin(L); {
	case o.Rank() <= lua.OriginProject.Rank():
		L.RaiseError("%s: %s code may not call a model", api, o)
	case o == lua.OriginPlugin:
		return h.luaGate(L, subject, id, payload)
	}
	return nil
}

// pluginCaps is the capability each gated subject needs declared, keyed by
// subject prefix: every lua.http.<method> and every tool.<name> share one row.
var pluginCaps = []struct{ prefix, cap string }{
	{"lua.model.ask", "model"},
	{"lua.classify.ask", "model"},
	{"lua.agent.send", "agent"},
	{"lua.fs.write", "fs.write"},
	{"lua.http.", "http"},
	{"tool.", "tool"},
}

// pluginCap is the capability a plugin must declare for a gated subject, ""
// when no capability covers it (lua.fs.read, zemi.state.*, zemi.env.get): a
// subject nothing declares keeps its own gate.
func pluginCap(subject string) string {
	if subject == "lua.job.start" {
		return "job"
	}
	for _, c := range pluginCaps {
		if strings.HasPrefix(subject, c.prefix) {
			return c.cap
		}
	}
	return ""
}

// zemi.env

func (h *Host) envNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		// unset is nil, never "": an empty api_key registers a provider that
		// fails much later, so `zemi.env.get("X") or error(...)` must work
		v, ok := os.LookupEnv(name)
		if !ok {
			L.Push(glua.LNil)
			return 1
		}
		L.Push(glua.LString(v))
		return 1
	}))
	return tb
}

// zemi.state (persisted kv, global scope)

// principal names the calling code for state keys and attribution: its
// origin, and for a plugin which one ("plugin:<name>"). Plugin code with no
// plugin file on the stack (a user module a plugin required, a load chunk,
// called from user code) raises: a nameless principal would share one state
// space with every plugin.
func (h *Host) principal(L *glua.LState) string {
	if o := h.RT.CurrentOrigin(L); o != lua.OriginPlugin {
		return string(o)
	}
	name := h.PluginOf(h.RT.CurrentPath(L))
	if name == "" {
		L.RaiseError("plugin code running with no file of its plugin on the stack has no principal")
	}
	return perm.PluginPrincipal(name)
}

// stateScope is the prefix a chunk's keys are stored under: each plugin and
// project code get their own space, so none reads or clobbers another's or
// user state (DESIGN §6.5); user and builtin see the whole store.
func (h *Host) stateScope(L *glua.LState) string {
	if o := h.RT.CurrentOrigin(L); o.Rank() < lua.OriginBuiltin.Rank() {
		return h.principal(L) + ":"
	}
	return ""
}

var errNoStateStore = errors.New("state store unavailable")

func (h *Host) stateNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// get(key) -> value; a key nobody set is nil with no error, a store or
	// decode failure is nil, err
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		key := h.stateScope(L) + L.CheckString(1)
		if h.GlobalDB == nil {
			return luaResult(L, errNoStateStore)
		}
		v, err := h.GlobalDB.GetKV(key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			L.Push(glua.LNil)
			return 1
		case err != nil:
			return luaResult(L, err)
		}
		var decoded any
		if err := json.Unmarshal([]byte(v), &decoded); err != nil {
			return luaResult(L, err)
		}
		L.Push(lua.FromGo(L, decoded))
		return 1
	}))
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		key := h.stateScope(L) + L.CheckString(1)
		v := luaToGoChecked(L, 2, "zemi.state.set")
		if h.GlobalDB == nil {
			return luaResult(L, errNoStateStore)
		}
		b, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			L.RaiseError("zemi.state.set: %v", err)
		}
		return luaResult(L, h.GlobalDB.SetKV(key, string(b)))
	}))
	// del(key) -> whether there was one; a config that writes state can
	// clear it again
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		key := h.stateScope(L) + L.CheckString(1)
		if h.GlobalDB == nil {
			return luaResult(L, errNoStateStore)
		}
		removed, err := h.GlobalDB.DelKV(key)
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(glua.LBool(removed))
		return 1
	}))
	// keys(prefix?) -> the keys set, sorted
	tb.RawSetString("keys", L.NewFunction(func(L *glua.LState) int {
		if h.GlobalDB == nil {
			return luaResult(L, errNoStateStore)
		}
		scope := h.stateScope(L)
		keys, err := h.GlobalDB.KeysKV(scope + L.OptString(1, ""))
		if err != nil {
			return luaResult(L, err)
		}
		out := L.NewTable()
		for _, k := range keys {
			out.Append(glua.LString(strings.TrimPrefix(k, scope)))
		}
		L.Push(out)
		return 1
	}))
	return tb
}

// zemi.timer

// timerHandle is one zemi.timer on the runtime's clock (lua.Clock).
type timerHandle struct {
	mu      sync.Mutex
	stopped bool
	stop    func() bool // the pending tick's
}

func (t *timerHandle) arm(c lua.Clock, d time.Duration, fire func()) {
	t.mu.Lock()
	if !t.stopped {
		t.stop = c.AfterFunc(d, fire)
	}
	t.mu.Unlock()
}

func (t *timerHandle) stopIt() {
	t.mu.Lock()
	if !t.stopped && t.stop != nil {
		t.stop()
	}
	t.stopped = true
	t.mu.Unlock()
}

// stopReg holds how to stop the live generation's timers, jobs and HTTP
// requests, which a reload stops before replay (DESIGN §4).
type stopReg struct {
	mu   sync.Mutex
	next int
	m    map[int]func()
}

func newStopReg() *stopReg { return &stopReg{m: map[int]func(){}} }

func (r *stopReg) add(stop func()) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	r.m[r.next] = stop
	return r.next
}

func (r *stopReg) remove(id int) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

func (r *stopReg) purge() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, stop := range r.m {
		stop()
	}
	r.m = map[int]func(){}
}

func (h *Host) timerNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	once := func(repeat bool) *glua.LFunction {
		return L.NewFunction(func(L *glua.LState) int {
			ms := L.CheckNumber(1)
			fn := L.CheckFunction(2)
			// after(0, fn) runs fn on the next queue turn (vim.schedule); a
			// repeating timer needs a real interval.
			if ms < 0 || (repeat && ms == 0) {
				L.RaiseError("zemi.timer: interval must be positive")
			}
			d := time.Duration(ms) * time.Millisecond
			at := h.callerOf(L)
			handle := &timerHandle{}
			stopID := h.stops.add(handle.stopIt)
			var fire func() // the clock's call, on its goroutine
			tick := func() {
				_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
					if h.currentGen() != at.gen {
						return nil, nil
					}
					_, err := h.callFn(L, at, fn, 0)
					return nil, err
				})
				if !repeat {
					h.refresh() // a repeating timer redraws itself with zemi.ui.redraw
				}
				if err != nil && !lua.Dropped(err) {
					h.Log.Warn("timer callback failed", "origin", "lua", "error", err)
					h.RecordError("zemi.timer", err.Error())
				}
				if err != nil || !repeat {
					h.stops.remove(stopID)
					return
				}
				handle.arm(h.RT.Clock(), d, fire)
			}
			fire = func() { h.safe("zemi.timer", tick) }
			handle.arm(h.RT.Clock(), d, fire)
			handleTb := L.NewTable()
			handleTb.RawSetString("stop", L.NewFunction(func(L *glua.LState) int {
				handle.stopIt()
				h.stops.remove(stopID)
				return 0
			}))
			lua.GuardFuncs(L, handleTb, "timer:", h.guardLua)
			L.Push(handleTb)
			return 1
		})
	}
	tb.RawSetString("after", once(false))
	tb.RawSetString("every", once(true))
	return tb
}

// zemi.fs

// fsNamespace is zemi.fs. Paths resolve against the project dir and are not
// confined: reads pass the lua.fs.read gate and changes lua.fs.write; runtime
// failures return nil, err.
func (h *Host) fsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// link: the call means a symlink itself, not what it points at
	resolve := func(L *glua.LState, api string, write, link bool) perm.Path {
		subject := "lua.fs.read"
		if write {
			subject = "lua.fs.write"
		}
		path := L.CheckString(1)
		resolved, err := h.projectResolve(path, link)
		if err != nil {
			L.RaiseError("zemi.fs.%s: %v", api, err)
		}
		h.luaGate(L, subject, h.projectAbs(path), func() map[string]any { return map[string]any{"path": path} })
		return resolved
	}
	kind := func(fi os.FileInfo) string {
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			return "link"
		case fi.IsDir():
			return "dir"
		}
		return "file"
	}

	tb.RawSetString("read", L.NewFunction(func(L *glua.LState) int {
		f, _, err := tools.OpenRegular(resolve(L, "read", false, false))
		if err != nil {
			return luaResult(L, err)
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(glua.LString(b))
		return 1
	}))

	tb.RawSetString("write", L.NewFunction(func(L *glua.LState) int {
		path := resolve(L, "write", true, false)
		data := []byte(L.CheckString(2))
		// the model's own script writes on a write call's terms; user and
		// plugin code keeps writing raw, as its own code does
		if h.modelLuaScript(L) != "" {
			return luaResult(L, h.PipelineFor().ScriptWrite(path, data, 0o644))
		}
		return luaResult(L, tools.WriteFile(path, data, 0o644))
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		f, err := resolve(L, "list", false, false).Open(os.O_RDONLY, 0)
		if err != nil {
			return luaResult(L, err)
		}
		defer f.Close()
		entries, err := f.ReadDir(-1)
		if err != nil {
			return luaResult(L, err)
		}
		slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		rows := []any{}
		for _, e := range entries {
			if fi, err := e.Info(); err == nil {
				rows = append(rows, map[string]any{"name": e.Name(), "type": kind(fi), "size": fi.Size()})
			}
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	tb.RawSetString("stat", L.NewFunction(func(L *glua.LState) int {
		// NOTE: by name, so a directory swapped after the check shows the
		// metadata of what it leads to; nothing is opened
		fi, err := os.Lstat(resolve(L, "stat", false, true).String())
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(lua.FromGo(L, map[string]any{"type": kind(fi), "size": fi.Size(), "mtime": fi.ModTime().UnixMilli()}))
		return 1
	}))

	tb.RawSetString("exists", L.NewFunction(func(L *glua.LState) int {
		_, err := os.Lstat(resolve(L, "exists", false, true).String())
		L.Push(glua.LBool(err == nil))
		return 1
	}))

	tb.RawSetString("mkdir", L.NewFunction(func(L *glua.LState) int {
		return luaResult(L, resolve(L, "mkdir", true, false).MkdirAll())
	}))

	// remove deletes one file or empty directory (never a tree).
	tb.RawSetString("remove", L.NewFunction(func(L *glua.LState) int {
		return luaResult(L, resolve(L, "remove", true, true).Remove())
	}))

	// data_dir(name) is where config code keeps its files; nothing is created.
	tb.RawSetString("data_dir", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "fs.data_dir")
		name := L.CheckString(1)
		if !filepath.IsLocal(name) || filepath.Base(name) != name {
			L.RaiseError("zemi.fs.data_dir: %q is not a plain directory name", name)
		}
		if h.luaDataRoot() == "" {
			L.RaiseError("zemi.fs.data_dir: no data dir")
		}
		L.Push(glua.LString(filepath.Join(h.luaDataRoot(), name)))
		return 1
	}))

	tb.RawSetString("glob", L.NewFunction(func(L *glua.LState) int {
		pattern := L.CheckString(1)
		h.luaGate(L, "lua.fs.read", h.Dirs.ProjectDir, nil)
		matches, err := h.projectGlob(cmp.Or(L.Context(), context.Background()), pattern)
		if err != nil {
			L.RaiseError("zemi.fs.glob: %v", err)
		}
		L.Push(lua.FromGo(L, matches))
		return 1
	}))

	tb.RawSetString("grep", L.NewFunction(func(L *glua.LState) int {
		pattern := L.CheckString(1)
		dir, include, unknown := "", "", ""
		if opts := L.OptTable(2, nil); opts != nil {
			opts.ForEach(func(k, v glua.LValue) {
				switch k.String() {
				case "path":
					dir = v.String()
				case "include":
					include = v.String()
				default:
					unknown = k.String()
				}
			})
		}
		if unknown != "" {
			// silently ignoring an option reads as a filter that ran
			L.RaiseError("zemi.fs.grep: unknown option %q (path, include)", unknown)
		}
		root, rel := h.Dirs.ProjectDir, ""
		if dir != "" {
			p, err := h.projectResolve(dir, true)
			if err != nil {
				L.RaiseError("zemi.fs.grep: %v", err)
			}
			root = p.String()
			rel, _ = filepath.Rel(h.Dirs.ProjectDir, root)
		}
		h.luaGate(L, "lua.fs.read", root, nil)
		re, err := regexp.Compile(pattern)
		if err != nil {
			L.RaiseError("zemi.fs.grep: %v", err)
		}
		hits, err := h.projectGrep(cmp.Or(L.Context(), context.Background()), re, rel, include)
		if err != nil {
			L.RaiseError("zemi.fs.grep: %v", err)
		}
		rows := []any{}
		for _, hit := range hits {
			rows = append(rows, map[string]any{"path": hit.Rel, "line": hit.Line, "text": hit.Text})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	return tb
}

// projectResolve resolves path against the project dir. With link the last
// component stays as named (perm.Lresolve).
func (h *Host) projectResolve(path string, link bool) (perm.Path, error) {
	if path == "" {
		return perm.Path{}, errors.New("empty path")
	}
	resolve := perm.Resolve
	if link {
		resolve = perm.Lresolve
	}
	return resolve(h.projectAbs(path))
}

// projectAbs is path made absolute against the project dir; not Join,
// which cleans .. before a symlink is seen.
func (h *Host) projectAbs(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return h.Dirs.ProjectDir + string(filepath.Separator) + path
}

// projectGlob matches project-relative paths in the glob tool's dialect,
// anchored at the root so "*.go" stays in the top directory.
func (h *Host) projectGlob(ctx context.Context, pattern string) ([]string, error) {
	match, err := glob.Match("/" + strings.TrimPrefix(pattern, "/"))
	if err != nil {
		return nil, err
	}
	files, _, err := tools.ListFiles(ctx, h.Dirs.ProjectDir, tools.ListLimit)
	return slices.DeleteFunc(files, func(rel string) bool { return !match("/" + rel) }), err
}

// projectGrep searches the project's files, under dir and in the files the
// include glob names (both optional), and answers paths project-relative.
func (h *Host) projectGrep(ctx context.Context, re *regexp.Regexp, dir, include string) ([]tools.Hit, error) {
	root := h.Dirs.ProjectDir
	match := func(string) bool { return true }
	var err error
	if include != "" {
		if match, err = glob.Match(include); err != nil {
			return nil, err
		}
	}
	dir = strings.Trim(filepath.ToSlash(dir), "/")
	if dir == "." {
		dir = ""
	}
	files, _, err := tools.ListFiles(ctx, root, tools.ListLimit)
	if err != nil {
		return nil, err
	}
	files = slices.DeleteFunc(files, func(rel string) bool {
		return !match(rel) || dir != "" && rel != dir && !strings.HasPrefix(rel, dir+"/")
	})
	return tools.Grep(ctx, root, files, re, math.MaxInt)
}

// zemi.http

func (h *Host) httpNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// request({ method, url, headers, body, timeout_ms, on_done }) -> handle
	// with wait(); the result is { status, body, headers } or, from wait(),
	// nil, err. Each method is its own gate subject (lua.http.<method>).
	tb.RawSetString("request", L.NewFunction(func(L *glua.LState) int {
		spec := L.CheckTable(1)
		req := httpReq{
			method:  cmp.Or(strings.ToUpper(rawString(L, spec, "method")), "GET"),
			url:     rawString(L, spec, "url"),
			body:    rawString(L, spec, "body"),
			headers: map[string]string{},
		}
		if req.url == "" {
			L.RaiseError("zemi.http.request: url is required")
		}
		if hs, ok := opt[*glua.LTable](L, spec, "headers"); ok {
			hs.ForEach(func(k, v glua.LValue) {
				req.headers[k.String()] = v.String()
			})
		}
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			for k := range req.headers {
				switch strings.ToLower(k) {
				case "authorization", "proxy-authorization", "cookie":
					L.RaiseError("zemi.http.request: only user config may set the %s header", k)
				}
			}
		}
		if ms, ok := opt[glua.LNumber](L, spec, "timeout_ms"); ok && ms > 0 {
			req.timeout = time.Duration(ms) * time.Millisecond
		}
		onDone, _ := opt[*glua.LFunction](L, spec, "on_done")
		return h.httpStart(L, req, onDone)
	}))
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		var onDone *glua.LFunction
		if opts, ok := arg[*glua.LTable](L, 2, "zemi.http.get"); ok {
			onDone, _ = opt[*glua.LFunction](L, opts, "on_done")
		}
		return h.httpStart(L, httpReq{method: "GET", url: L.CheckString(1), headers: map[string]string{}}, onDone)
	}))
	return tb
}

type httpReq struct {
	method, url, body string
	headers           map[string]string
	timeout           time.Duration
}

type httpHandle struct {
	done chan struct{}
	res  map[string]any
	err  error
}

// httpStart gates the request, runs it off the interpreter and pushes the
// handle.
func (h *Host) httpStart(L *glua.LState, req httpReq, onDone *glua.LFunction) int {
	subject := "lua.http." + strings.ToLower(req.method)
	stub := h.luaGate(L, subject, req.url, func() map[string]any {
		return map[string]any{"method": req.method, "url": req.url, "headers": req.headers, "body": req.body}
	})
	fetch := func(ctx context.Context) (map[string]any, error) {
		return h.httpFetch(ctx, req)
	}
	if stub != nil {
		fetch = func(context.Context) (map[string]any, error) {
			if _, err := tools.ParseNetURL(req.url); err != nil {
				return nil, err
			}
			return stub.fields, stub.err
		}
	}
	hnd := &httpHandle{done: make(chan struct{})}
	at := h.callerOf(L)
	ctx, cancel := context.WithCancel(context.Background())
	stopID := h.stops.add(cancel) // a reload cancels it, like a job
	h.GoSafe(subject, func() {
		defer cancel()
		defer h.stops.remove(stopID)
		res, err := fetch(ctx)
		hnd.res, hnd.err = res, err
		close(hnd.done)
		if onDone != nil {
			payload := map[string]any{"url": req.url}
			if err != nil {
				payload["error"] = err.Error()
			} else {
				maps.Copy(payload, res)
			}
			h.callLuaAsync("zemi.http on_done", at, onDone, payload)
		}
	})
	handle := L.NewTable()
	handle.RawSetString("wait", lua.Suspending(L, L.NewFunction(func(L *glua.LState) int {
		return waitHandle(L, "http:wait", hnd.done, 120*time.Second, nil, func(L *glua.LState, err error) []glua.LValue {
			if err == nil { // hnd.err is set only once done has closed
				err = hnd.err
			}
			if err != nil {
				return []glua.LValue{glua.LNil, glua.LString(err.Error())}
			}
			return []glua.LValue{lua.FromGo(L, hnd.res)}
		})
	})))
	lua.GuardFuncs(L, handle, "http:", h.guardLua)
	L.Push(handle)
	return 1
}

// httpFetch performs the request with size and time caps: http/https only,
// no userinfo, no redirects - the gate approved one url, and a hop elsewhere
// would ride its approval; request the location header to follow one.
func (h *Host) httpFetch(ctx context.Context, r httpReq) (map[string]any, error) {
	if _, err := tools.ParseNetURL(r.url); err != nil {
		return nil, err
	}
	client := tools.SafeClient(func(*url.URL) error {
		return errors.New("zemi.http does not follow redirects; request the location header yourself")
	})
	client.Timeout = cmp.Or(r.timeout, 30*time.Second)
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		return nil, err
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	headers := map[string]any{}
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	return map[string]any{
		"status":  resp.StatusCode,
		"body":    string(data),
		"headers": headers,
	}, nil
}

// luaCaller is where a Lua callback was handed over: a later call runs
// under that generation and origin, since attribution follows the
// registration, never the trigger (DESIGN §9).
type luaCaller struct {
	gen    Gen
	origin lua.Origin
	path   string
}

func (h *Host) callerOf(L *glua.LState) luaCaller {
	return luaCaller{gen: h.currentGen(), origin: h.RT.CurrentOrigin(L), path: h.RT.CurrentPath(L)}
}

// callFn runs fn(args...) on L with at's frame pushed and returns what the
// caller asks for, nret values (gopher-lua pads a missing one with nil and
// restores the stack itself on every path).
func (h *Host) callFn(L *glua.LState, at luaCaller, fn *glua.LFunction, nret int, args ...glua.LValue) ([]glua.LValue, error) {
	defer h.RT.PushFrame(L, at.origin, at.path)()
	base := L.GetTop()
	L.Push(fn)
	for _, a := range args {
		L.Push(a)
	}
	if err := L.PCall(len(args), nret, nil); err != nil {
		return nil, err
	}
	rets := make([]glua.LValue, nret)
	for i := range rets {
		rets[i] = L.Get(base + 1 + i)
	}
	L.SetTop(base)
	return rets, nil
}

// luaTask calls fn(args...) as a task under at; one handed over before a
// reload is dropped.
func (h *Host) luaTask(class lua.Class, at luaCaller, fn *glua.LFunction, args ...any) lua.Task {
	return lua.Task{
		Class: class, Origin: at.origin, Path: at.path,
		Start: func(L *glua.LState) (*glua.LFunction, []glua.LValue, error) {
			if h.currentGen() != at.gen {
				return nil, nil, lua.ErrReloaded
			}
			vals := make([]glua.LValue, len(args))
			for i, a := range args {
				vals[i] = lua.FromGo(L, a)
			}
			return fn, vals, nil
		},
	}
}

// callLuaAsync runs fn(args...), the callback what names, as a task from a
// host goroutine, dropping the call when the generation changed.
func (h *Host) callLuaAsync(what string, at luaCaller, fn *glua.LFunction, args ...any) {
	_, err := h.RT.RunTask(context.Background(), h.luaTask(lua.ClassIO, at, fn, args...))
	h.callbackDone(what, err)
}

// callbackDone shows what an async callback changed, and a failing one in
// the log and /errors.
func (h *Host) callbackDone(what string, err error) {
	h.refresh() // without waiting for another redraw
	if err != nil && !lua.Dropped(err) {
		h.Log.Warn("callback failed", "origin", "lua", "callback", what, "error", err)
		h.RecordError(what, err.Error())
	}
}

// zemi.job

type jobHandle struct {
	id     int64
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
	mu     sync.Mutex
	stdout strings.Builder
	stderr strings.Builder
	code   int
	err    string
}

// discard is a stubbed job's stdin: what the plugin writes goes nowhere.
type discard struct{ io.Writer }

func (discard) Close() error { return nil }

func (j *jobHandle) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"id": j.id, "stdout": j.stdout.String(), "stderr": j.stderr.String(),
		"code": j.code, "error": j.err,
	}
}

// lineSink batches output lines for a Lua callback: a line waits at most
// 30ms on the runtime's clock, or until the stream ends, and one delivery
// runs at a time, so a chatty process cannot flood the queue.
type lineSink struct {
	h    *Host
	at   luaCaller
	fn   *glua.LFunction
	what string // "zemi.job on_stdout", for /errors
	mu   sync.Mutex
	// pending waits for the drain stop stops, nil when none is scheduled
	pending []string
	stop    func() bool
	wg      sync.WaitGroup
}

func (s *lineSink) add(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, line)
	if s.stop == nil {
		s.wg.Add(1)
		s.stop = s.h.RT.Clock().AfterFunc(30*time.Millisecond, func() { s.h.safe("job output", s.drain) })
	}
}

// end delivers what waits now, the stream being over, and returns once
// every delivery ran.
func (s *lineSink) end() {
	s.mu.Lock()
	stop := s.stop
	s.mu.Unlock()
	if stop != nil && stop() {
		s.drain()
	}
	s.wg.Wait()
}

func (s *lineSink) drain() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		lines := s.pending
		s.pending = nil
		if len(lines) == 0 {
			s.stop = nil
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		s.deliver(lines)
	}
}

// deliver is a plain call, not a task: a batch every 30ms would cost a
// task's thread each.
func (s *lineSink) deliver(lines []string) {
	_, err := s.h.RT.Do(context.Background(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		if s.h.currentGen() != s.at.gen {
			return nil, nil
		}
		_, err := s.h.callFn(L, s.at, s.fn, 0, lua.FromGo(L, lines))
		return nil, err
	})
	s.h.callbackDone(s.what, err)
}

// lineWriter is a process stream: it keeps up to 1 MiB for wait() and hands
// complete lines to sink. exec writes each stream from one goroutine.
type lineWriter struct {
	mu      *sync.Mutex
	buf     *strings.Builder
	sink    *lineSink
	partial []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if room := 1<<20 - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	w.mu.Unlock()
	if w.sink == nil {
		return len(p), nil
	}
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.sink.add(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}
	if len(w.partial) > 1<<20 { // a line that never ends still arrives
		w.flush()
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if w.sink != nil && len(w.partial) > 0 {
		w.sink.add(string(w.partial))
		w.partial = nil
	}
}

var jobSeq atomic.Int64

// argvLine is an exec'd program as the shell line the gate checks: every
// word quoted where sh would split it, so a cmd of "ls; rm x" stays one word
// as it does in exec.
func argvLine(cmd string, args []string) string {
	var words []string
	for _, w := range append([]string{cmd}, args...) {
		words = append(words, tools.ShellQuote(w))
	}
	return strings.Join(words, " ")
}

func (h *Host) jobNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	// start runs spec = { cmd, args, env, cwd, on_stdout(lines), on_stderr(lines),
	// on_exit(result) }; output is also buffered (1 MiB) for wait(). Misuse
	// raises; a process that cannot start is the returned error.
	start := func(L *glua.LState) (*jobHandle, error) {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.job.start: spec must be a table")
		}
		cmdName := rawString(L, spec, "cmd")
		if cmdName == "" {
			L.RaiseError("zemi.job.start: cmd is required")
		}
		args := rawStrings(L, spec, "args")
		stub := h.luaGate(L, "lua.job.start", argvLine(cmdName, args), func() map[string]any {
			return map[string]any{"cmd": cmdName, "args": args, "cwd": rawString(L, spec, "cwd")}
		})
		extras := map[string]any{}
		if envT, ok := opt[*glua.LTable](L, spec, "env"); ok {
			if v, err := lua.ToGo(envT); err == nil {
				if m, ok := v.(map[string]any); ok {
					extras = m
				}
			}
		}
		env := append([]string{}, os.Environ()...)
		for k, v := range extras {
			env = append(env, k+"="+fmt.Sprint(v))
		}
		// own session and group (proc): kill stops its children too, and a
		// background grandchild holding stdout does not keep the job alive
		cmd := proc.Command(cmdName, args...)
		cmd.Dir = h.Dirs.ProjectDir
		if cwd := rawString(L, spec, "cwd"); cwd != "" {
			dir, err := h.projectResolve(cwd, false)
			if err != nil {
				L.RaiseError("zemi.job.start: cwd: %v", err)
			}
			cmd.Dir = dir.String()
			// exec blames the program for a directory it cannot enter
			if _, err := os.Stat(cmd.Dir); err != nil {
				return nil, fmt.Errorf("zemi.job.start: cwd: %w", err)
			}
		}
		cmd.Env = env
		jh := &jobHandle{id: jobSeq.Add(1), cmd: cmd, done: make(chan struct{})}
		at := h.callerOf(L)
		sinkOf := func(key string) *lineSink {
			if fn, _ := opt[*glua.LFunction](L, spec, key); fn != nil {
				return &lineSink{h: h, at: at, fn: fn, what: "zemi.job " + key}
			}
			return nil
		}
		outSink, errSink := sinkOf("on_stdout"), sinkOf("on_stderr")
		outW := &lineWriter{mu: &jh.mu, buf: &jh.stdout, sink: outSink}
		errW := &lineWriter{mu: &jh.mu, buf: &jh.stderr, sink: errSink}
		cmd.Stdout, cmd.Stderr = outW, errW
		// the process ends with its exit code, or with why it did not run; a
		// test zemi's stub stands in for it
		exit := func() (int, error) {
			if stub != nil {
				fmt.Fprint(outW, stub.fields["stdout"])
				fmt.Fprint(errW, stub.fields["stderr"])
				code, _ := stub.fields["code"].(float64)
				return int(code), nil
			}
			err := proc.Wait(cmd)
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				return ee.ExitCode(), nil
			}
			if errors.Is(err, exec.ErrWaitDelay) {
				return 0, nil
			}
			return 0, err
		}
		stopID := 0
		switch {
		case stub != nil:
			if rawBool(L, spec, "stdin") {
				jh.stdin = discard{io.Discard}
			}
		default:
			// stdin stays closed unless asked for: cat, grep or rg would wait on it
			if rawBool(L, spec, "stdin") {
				stdin, err := cmd.StdinPipe()
				if err != nil {
					L.RaiseError("zemi.job.start: %v", err)
				}
				jh.stdin = stdin
			}
			if err := proc.Start(cmd); err != nil {
				if strings.ContainsAny(cmdName, " \t") {
					return nil, fmt.Errorf("zemi.job.start: %w (cmd is the program alone; the rest goes in args = { ... })", err)
				}
				return nil, fmt.Errorf("zemi.job.start: %w", err)
			}
			stopID = h.stops.add(func() { proc.Kill(cmd) })
		}
		onExit, _ := opt[*glua.LFunction](L, spec, "on_exit")
		h.GoSafe("job wait", func() {
			code, err := exit()
			h.lspDirty()
			h.stops.remove(stopID)
			outW.flush()
			jh.mu.Lock()
			jh.code = code
			if err != nil {
				jh.err = err.Error()
			}
			jh.mu.Unlock()
			close(jh.done)
			// on_exit follows every output callback
			for _, s := range []*lineSink{outSink, errSink} {
				if s != nil {
					s.end()
				}
			}
			if onExit != nil {
				h.callLuaAsync("zemi.job on_exit", at, onExit, jh.snapshot())
			}
			h.EmitAsync("job.exit", map[string]any{"id": jh.id, "cmd": cmdName, "code": jh.code})
		})
		return jh, nil
	}

	wait := func(L *glua.LState, api string, jh *jobHandle, timeout time.Duration, stop func()) int {
		return waitHandle(L, api, jh.done, timeout, stop, func(L *glua.LState, err error) []glua.LValue {
			if err != nil {
				return []glua.LValue{glua.LNil, glua.LString(err.Error())}
			}
			return []glua.LValue{lua.FromGo(L, jh.snapshot())}
		})
	}

	tb.RawSetString("start", L.NewFunction(func(L *glua.LState) int {
		jh, err := start(L)
		if err != nil {
			return luaResult(L, err)
		}
		handle := L.NewTable()
		handle.RawSetString("id", glua.LNumber(jh.id))
		// Methods are colon calls (DESIGN §6.0), so arguments start at 2.
		handle.RawSetString("wait", lua.Suspending(L, L.NewFunction(func(L *glua.LState) int {
			timeout := 30 * time.Second
			if ms := L.OptInt(2, 0); ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
			return wait(L, "job:wait", jh, timeout, nil)
		})))
		handle.RawSetString("kill", L.NewFunction(func(L *glua.LState) int {
			proc.Kill(jh.cmd)
			return 0
		}))
		handle.RawSetString("write", L.NewFunction(func(L *glua.LState) int {
			if jh.stdin == nil {
				L.RaiseError("zemi.job: write needs the job started with stdin = true")
			}
			_, err := io.WriteString(jh.stdin, L.CheckString(2))
			return luaResult(L, err)
		}))
		handle.RawSetString("close_stdin", L.NewFunction(func(L *glua.LState) int {
			if jh.stdin != nil {
				_ = jh.stdin.Close()
			}
			return 0
		}))
		lua.GuardFuncs(L, handle, "job:", h.guardLua)
		L.Push(handle)
		return 1
	}))

	// wait starts the command and suspends the task until it finishes
	// (spec.timeout_ms, default 60s, then kills it).
	tb.RawSetString("wait", lua.Suspending(L, L.NewFunction(func(L *glua.LState) int {
		lua.CheckWait(L, "zemi.job.wait") // before the command starts
		jh, err := start(L)
		if err != nil {
			return luaResult(L, err)
		}
		timeout := 60 * time.Second
		if spec, ok := L.Get(1).(*glua.LTable); ok {
			if ms, ok := opt[glua.LNumber](L, spec, "timeout_ms"); ok && ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
		}
		return wait(L, "zemi.job.wait", jh, timeout, func() { proc.Kill(jh.cmd) })
	})))

	return tb
}
