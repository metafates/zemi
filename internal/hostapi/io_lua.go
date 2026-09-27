// io_lua.go is the Lua I/O surface (DESIGN §6.5, §9): zemi.env/state/
// timer/fs/http/job. Every side-effectful call passes a `lua.<ns.call>`
// permission gate at the Lua boundary; asks fail closed (host-API calls
// from Lua never open the interactive overlay, §6.5).
package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
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
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// secretPath matches the §9 secret shapes on the path as written and on the
// resolved one, so an innocently named symlink cannot hide a secret, and
// the home paths the sandbox hides (secretDirs), whose names say nothing.
func (h *Host) secretPath(p string) bool {
	if perm.PathIsSecret(p) {
		return true
	}
	real := perm.CanonPath(p)
	return real == "" || perm.PathIsSecret(real) || slices.ContainsFunc(homeSecrets(), func(root string) bool {
		_, ok := perm.Under(root, real)
		return ok
	})
}

// homeSecrets are the secretDirs under $HOME, resolved once: home does not
// move under a running zemi.
var homeSecrets = sync.OnceValue(func() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	roots := make([]string, len(secretDirs))
	for i, s := range secretDirs {
		roots[i] = perm.CanonPath(filepath.Join(home, s))
	}
	return roots
})

// inJail reports whether path is inside one of the jail roots.
func (h *Host) inJail(path string, roots ...string) bool {
	// Compare resolved paths, so a symlink inside a root cannot lead out
	// (and a root reached through a symlink, like macOS /var, still matches).
	real := perm.CanonPath(path)
	if real == "" {
		return false
	}
	for _, r := range roots {
		rr := perm.CanonPath(r)
		if r == "" || rr == "" {
			continue
		}
		// a root of / already ends in the separator
		sep := string(filepath.Separator)
		if real == rr || strings.HasPrefix(real, strings.TrimSuffix(rr, sep)+sep) {
			return true
		}
	}
	return false
}

func (h *Host) readJails() []string {
	// Builtin skill bodies live under the data dir; reading them must not ask.
	// The log dir is not: MCP and LSP server stderr is logged as written and
	// may carry tokens. The types dir holds the API annotations and the
	// shipped Lua UI, which the agent reads before restyling or extending it.
	// UserTrees: a symlinked one (skills -> ~/.agents/skills) resolves
	// outside the config home, so it needs its own root.
	roots := append([]string{
		h.Dirs.ProjectDir, h.Dirs.ConfigHome, h.Dirs.ProjectConfigDir(),
		h.builtinSkillsDir(), h.ScratchDir(), h.TypesDir(),
	},
		h.Dirs.UserTrees()...)
	if h.ProjectTrusted() {
		// the decision covers the repository, so starting in a subdirectory
		// must not ask for every sibling package the task reaches
		roots = append(roots, h.Dirs.RepoRoot(), h.recipesDir())
	}
	return roots
}

func (h *Host) writeJails() []string {
	return []string{h.Dirs.ProjectDir}
}

// luaDataRoot holds the zemi.fs.data_dir dirs. It sits outside the config
// tree, so a write there never reloads the config.
func (h *Host) luaDataRoot() string {
	if h.Dirs.DataHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.DataHome, "lua")
}

// luaJails are the roots a zemi.fs call resolves in: the tools' jails, plus
// the data root for user and builtin code only, so the model's file tools,
// its unapproved Lua tool scripts and less trusted Lua never reach it.
func (h *Host) luaJails(L *glua.LState, write bool) []string {
	roots := h.readJails()
	if write {
		roots = h.writeJails()
	}
	tool, approved := h.modelLuaRun(L)
	if o := h.RT.CurrentOrigin(L); (o == lua.OriginUser || o == lua.OriginBuiltin) && (!tool || approved) {
		roots = append(roots, h.luaDataRoot())
	}
	return roots
}

// fsDefault implements the §9 built-in defaults for fs subjects: reads
// inside the jail allow except secret-shaped paths, and the file tools read
// the plugins (their skills load through them) but not a clone's .git, whose
// config may hold a token in the remote URL; writes allow only in the
// session scratchpad; everything else asks.
func (h *Host) fsDefault(subject, arg string) perm.Decision {
	switch subject {
	case perm.SubjectRead, "lua.fs.read":
		if h.secretPath(arg) {
			return perm.Ask
		}
		if h.inJail(arg, h.readJails()...) || subject == "lua.fs.read" && h.inJail(arg, h.luaDataRoot()) ||
			subject == perm.SubjectRead && h.inJail(arg, h.pluginsRoot()) && !inDirNamed(arg, ".git") {
			return perm.Allow
		}
	case perm.SubjectWrite, "lua.fs.write":
		if subject == perm.SubjectWrite && h.inScratch(arg) && !h.secretPath(arg) {
			return perm.Allow
		}
		if h.inJail(arg, h.writeJails()...) && !h.secretPath(arg) {
			return perm.Ask // write stays ask inside the jail too
		}
	}
	return perm.Ask
}

// luaGate runs one Lua-boundary permission check. Asks fail closed with
// the §6.5 contract ("permission ask required"): host-API calls from Lua
// never open the interactive overlay, so the asker is stripped from the
// pipeline copy and an unresolved ask stays an ask for the gate to refuse.
func (h *Host) luaGate(L *glua.LState, subject, arg string) tools.Verdict {
	// a program the Lua tool starts runs unconfined, so the user approves it
	tool, approved := h.modelLuaRun(L)
	approved = subject == "lua.job.start" && tool && approved
	pipe := h.PipelineFor()
	pipe.Ask = nil
	// rules are attributed to the calling code, not to the model
	pipe.Principal = h.principal(L)
	var v tools.Verdict
	if subject == "lua.job.start" {
		// a command line, normalized like bash so rules can scope arguments
		// and a second command cannot ride an allow
		o := pipe.Store.CheckCommandFor(subject, perm.Normalize(arg), pipe.Principal)
		v = tools.Verdict{Decision: o.Decision, Reason: o.Reason, DecidedBy: tools.ByDefault}
		if o.Rule != nil {
			v.DecidedBy = tools.ByRule
		}
	} else {
		v = pipe.Check(tools.Request{Subject: subject, Arg: arg, Reason: "Lua call"})
	}
	switch {
	case v.Decision == perm.Allow:
	case v.Decision == perm.Ask && approved:
		v = tools.Verdict{Decision: perm.Allow, DecidedBy: tools.ByUser}
	case v.Decision == perm.Ask:
		h.EmitAsync("permission.request", map[string]any{
			"subject": subject, "arg": arg, "post_hoc": true,
		})
		// name the rule that settles it: a host-API call has no overlay, so
		// "ask required" is the whole problem and none of the fix
		principal := ""
		if _, ok := perm.CutPluginPrincipal(pipe.Principal); ok {
			principal = pipe.Principal
		}
		L.RaiseError("%s %s: permission ask required; allow it in config with %s",
			subject, arg, allowLine(subject, arg, arg, principal))
	default:
		L.RaiseError("%s %s: permission denied (%s)", subject, arg, v.DecidedBy)
	}
	return v
}

// allowLine is the init.lua line of an allow rule, match its example.
// NOTE: %q writes \u and \x escapes gopher-lua cannot read, so a pattern with
// a non-printable rune (a non-breaking space) needs editing by hand.
func allowLine(subject, pattern, match, principal string) string {
	scope := ""
	if principal != "" {
		scope = fmt.Sprintf(", principal = %q", principal)
	}
	return fmt.Sprintf(`zemi.permission.rule.add{ subject = %q, pattern = %q, decision = "allow", match = %q%s }`,
		subject, pattern, match, scope)
}

// ---------------------------------------------------------------------------
// zemi.env

func (h *Host) envNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		// user config reads its own keys (api_key = zemi.env.get(...));
		// plugins and projects never see secret-shaped names
		if o := h.RT.CurrentOrigin(L); perm.EnvIsSecret(name) && o != lua.OriginUser && o != lua.OriginBuiltin {
			L.RaiseError("zemi.env.get: %q matches the secret denylist", name)
		}
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

// ---------------------------------------------------------------------------
// zemi.state (persisted kv, global scope)

// principal names the calling code for permission rules and state: its
// origin, and for a plugin which one ("plugin:<name>"). Plugin code with no
// plugin file on the stack (a user module a plugin required, a load chunk,
// called from user code) raises: a nameless principal would share one state
// space and slip past every rule scoped to a plugin.
func (h *Host) principal(L *glua.LState) string {
	if o := h.RT.CurrentOrigin(L); o != lua.OriginPlugin {
		return string(o)
	}
	name := h.PluginOf(h.RT.CurrentPath(L))
	if name == "" {
		L.RaiseError("plugin code running with no file of its plugin on the stack has no principal for rules or state")
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

func (h *Host) stateNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// get(key) -> value; a key nobody set is nil with no error, a store or
	// decode failure is nil, err
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		key := h.stateScope(L) + L.CheckString(1)
		fail := func(err error) int {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		if h.GlobalDB == nil {
			return fail(errors.New("store unavailable"))
		}
		v, err := h.GlobalDB.GetKV(key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			L.Push(glua.LNil)
			return 1
		case err != nil:
			return fail(err)
		}
		var decoded any
		if err := json.Unmarshal([]byte(v), &decoded); err != nil {
			return fail(err)
		}
		L.Push(lua.FromGo(L, decoded))
		return 1
	}))
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		key := h.stateScope(L) + L.CheckString(1)
		v := luaToGoChecked(L, 2, "zemi.state.set")
		if h.GlobalDB == nil {
			return luaResult(L, errors.New("store unavailable"))
		}
		b, err := json.Marshal(v)
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
			return luaResult(L, errors.New("store unavailable"))
		}
		removed, err := h.GlobalDB.DelKV(key)
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LBool(removed))
		return 1
	}))
	// keys(prefix?) -> the keys set, sorted
	tb.RawSetString("keys", L.NewFunction(func(L *glua.LState) int {
		if h.GlobalDB == nil {
			return luaResult(L, errors.New("store unavailable"))
		}
		scope := h.stateScope(L)
		keys, err := h.GlobalDB.KeysKV(scope + L.OptString(1, ""))
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
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

// ---------------------------------------------------------------------------
// zemi.timer

type timerHandle struct {
	stop chan struct{}
	once sync.Once
}

func (t *timerHandle) stopIt() { t.once.Do(func() { close(t.stop) }) }

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
			handle := &timerHandle{stop: make(chan struct{})}
			stopID := h.stops.add(handle.stopIt)
			h.GoSafe("zemi.timer", func() {
				defer h.stops.remove(stopID)
				t := time.NewTimer(d)
				defer t.Stop()
				for {
					select {
					case <-handle.stop:
						return
					case <-t.C:
					}
					_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
						if h.currentGen() != at.gen {
							return nil, nil
						}
						defer h.RT.PushFrame(L, at.origin, at.path)()
						base := L.GetTop()
						L.Push(fn)
						if err := L.PCall(0, 0, nil); err != nil {
							L.SetTop(base)
							return nil, err
						}
						return nil, nil
					})
					if !repeat {
						h.refresh() // a repeating timer redraws itself with zemi.ui.redraw
					}
					if err != nil && !isQuietLuaErr(err) {
						h.Log.Warn("lua", "timer callback failed", map[string]any{"error": err.Error()})
						h.RecordError("zemi.timer", err.Error())
					}
					if err != nil || !repeat {
						return
					}
					t.Reset(d)
				}
			})
			handleTb := L.NewTable()
			handleTb.RawSetString("stop", L.NewFunction(func(L *glua.LState) int {
				handle.stopIt()
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

// ---------------------------------------------------------------------------
// zemi.fs

// fsNamespace is zemi.fs. Paths resolve inside the jails, reads pass the
// lua.fs.read gate and changes lua.fs.write; runtime failures return nil, err.
func (h *Host) fsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// link: the call means a symlink itself, not what it points at
	resolve := func(L *glua.LState, api string, write, link bool) perm.Path {
		subject := "lua.fs.read"
		if write {
			subject = "lua.fs.write"
		}
		path := L.CheckString(1)
		resolved, err := h.resolveJailed(path, h.luaJails(L, write), link)
		if err != nil {
			L.RaiseError("zemi.fs.%s: %v", api, err)
		}
		// as written: the secret shapes match a secret-named symlink's name
		h.luaGate(L, subject, h.projectAbs(path))
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
		return luaResult(L, tools.WriteFile(path, []byte(L.CheckString(2)), 0o644))
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
		h.luaGate(L, "lua.fs.read", h.Dirs.ProjectDir)
		matches, err := h.jailedGlob(cmp.Or(L.Context(), context.Background()), pattern)
		if err != nil {
			L.RaiseError("zemi.fs.glob: %v", err)
		}
		L.Push(lua.FromGo(L, matches))
		return 1
	}))

	tb.RawSetString("grep", L.NewFunction(func(L *glua.LState) int {
		pattern := L.CheckString(1)
		h.luaGate(L, "lua.fs.read", h.Dirs.ProjectDir)
		re, err := regexp.Compile(pattern)
		if err != nil {
			L.RaiseError("zemi.fs.grep: %v", err)
		}
		hits, err := h.jailedGrep(cmp.Or(L.Context(), context.Background()), re)
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

// resolveJailed resolves path against the project dir and enforces the
// jail (no escaping the roots, no symlinks out). With link the last
// component stays as named (perm.Lresolve).
func (h *Host) resolveJailed(path string, roots []string, link bool) (perm.Path, error) {
	if path == "" {
		return perm.Path{}, errors.New("empty path")
	}
	resolve := perm.Resolve
	if link {
		resolve = perm.Lresolve
	}
	resolved, err := resolve(h.projectAbs(path))
	if err != nil {
		return perm.Path{}, err
	}
	if !h.inJail(resolved.String(), roots...) {
		return perm.Path{}, fmt.Errorf("%s is outside the jail", path)
	}
	return resolved, nil
}

// projectAbs is path made absolute against the project dir; not Join,
// which cleans .. before a symlink is seen.
func (h *Host) projectAbs(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return h.Dirs.ProjectDir + string(filepath.Separator) + path
}

// jailedGlob matches project-relative paths in the glob tool's dialect,
// anchored at the root so "*.go" stays in the top directory.
func (h *Host) jailedGlob(ctx context.Context, pattern string) ([]string, error) {
	match, err := glob.Match("/" + strings.TrimPrefix(pattern, "/"))
	if err != nil {
		return nil, err
	}
	files, _, err := tools.ListFiles(ctx, h.Dirs.ProjectDir, tools.ListLimit)
	return slices.DeleteFunc(files, func(rel string) bool { return !match("/" + rel) }), err
}

// jailedGrep never reads secret-shaped files, which the lua.fs.read gate
// would ask for.
func (h *Host) jailedGrep(ctx context.Context, re *regexp.Regexp) ([]tools.Hit, error) {
	root := h.Dirs.ProjectDir
	files, _, err := tools.ListFiles(ctx, root, tools.ListLimit)
	if err != nil {
		return nil, err
	}
	files = slices.DeleteFunc(files, func(rel string) bool { return perm.PathIsSecret(filepath.Join(root, rel)) })
	return tools.Grep(ctx, root, files, re, math.MaxInt, nil)
}

// ---------------------------------------------------------------------------
// zemi.http

func (h *Host) httpNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// request({ method, url, headers, body, timeout_ms, on_done }) -> handle
	// with wait(); the result is { status, body, headers } or, from wait(),
	// nil, err. Each method is its own gate subject (lua.http.<method>).
	tb.RawSetString("request", L.NewFunction(func(L *glua.LState) int {
		spec := L.CheckTable(1)
		req := httpReq{
			method:  strings.ToUpper(rawString(spec, "method")),
			url:     rawString(spec, "url"),
			body:    rawString(spec, "body"),
			headers: map[string]string{},
		}
		if req.method == "" {
			req.method = "GET"
		}
		if req.url == "" {
			L.RaiseError("zemi.http.request: url is required")
		}
		if hs, ok := spec.RawGetString("headers").(*glua.LTable); ok {
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
		if ms, ok := spec.RawGetString("timeout_ms").(glua.LNumber); ok && ms > 0 {
			req.timeout = time.Duration(ms) * time.Millisecond
		}
		onDone, _ := spec.RawGetString("on_done").(*glua.LFunction)
		return h.httpStart(L, req, onDone)
	}))
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		var onDone *glua.LFunction
		if opts, ok := L.Get(2).(*glua.LTable); ok {
			onDone, _ = opts.RawGetString("on_done").(*glua.LFunction)
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
	origin := h.RT.CurrentOrigin(L)
	v := h.luaGate(L, subject, req.url)
	// §9 localhost development opt-in: an explicit user allow rule for the
	// host overrides the resolved-address deny. Allow rules are user-only
	// (zemi.permission.rule refuses them elsewhere), and project callers
	// never get the override.
	allowPrivate := v.DecidedBy == tools.ByRule && origin.Rank() > lua.OriginProject.Rank()
	hnd := &httpHandle{done: make(chan struct{})}
	at := h.callerOf(L)
	ctx, cancel := context.WithCancel(context.Background())
	stopID := h.stops.add(cancel) // a reload cancels it, like a job
	h.GoSafe(subject, func() {
		defer cancel()
		defer h.stops.remove(stopID)
		res, err := h.httpFetch(ctx, subject, string(origin), allowPrivate, req)
		hnd.res, hnd.err = res, err
		close(hnd.done)
		if onDone != nil {
			payload := map[string]any{"url": req.url}
			if err != nil {
				payload["error"] = err.Error()
			} else {
				maps.Copy(payload, res)
			}
			h.callLuaAsync(at, onDone, payload)
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

// httpFetch performs the request with size and time caps under the same §9
// network rules as the fetch tool: http/https only, no userinfo, denied
// addresses refused at connect time, and every redirect hop re-gated under
// the same subject as the calling origin (deny/ask refuses the hop, §6.5).
func (h *Host) httpFetch(ctx context.Context, subject, principal string, allowPrivate bool, r httpReq) (map[string]any, error) {
	if _, err := tools.ParseNetURL(r.url); err != nil {
		return nil, err
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := tools.SafeClient(func(hop *url.URL) error {
		if _, err := tools.ParseNetURL(hop.String()); err != nil {
			return err
		}
		if o := h.Perm.Evaluate(subject, hop.String(), principal); o.Decision != perm.Allow {
			return fmt.Errorf("redirect to %s refused by the permission gate", hop)
		}
		return nil
	}, allowPrivate)
	client.Timeout = timeout
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

// callLuaAsync runs fn(args...) as a task from a host goroutine, dropping
// the call when the generation changed.
func (h *Host) callLuaAsync(at luaCaller, fn *glua.LFunction, args ...any) {
	_, err := h.RT.RunTask(context.Background(), h.luaTask(lua.ClassIO, at, fn, args...))
	h.callbackDone(err)
}

// callbackDone shows what an async callback changed, and a failing one in
// the log and /errors.
func (h *Host) callbackDone(err error) {
	h.refresh() // without waiting for another redraw
	if err != nil && !isQuietLuaErr(err) {
		h.Log.Warn("lua", "callback failed", map[string]any{"error": err.Error()})
		h.RecordError("lua callback", err.Error())
	}
}

func isQuietLuaErr(err error) bool {
	return errors.Is(err, lua.ErrHot) || errors.Is(err, lua.ErrReloaded) || errors.Is(err, lua.ErrCanceled)
}

// ---------------------------------------------------------------------------
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

func (j *jobHandle) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"id": j.id, "stdout": j.stdout.String(), "stderr": j.stderr.String(),
		"code": j.code, "error": j.err,
	}
}

// lineSink batches output lines for a Lua callback: a line waits at most
// 30ms, and one delivery runs at a time, so a chatty process cannot flood
// the queue.
type lineSink struct {
	h         *Host
	at        luaCaller
	fn        *glua.LFunction
	mu        sync.Mutex
	pending   []string
	scheduled bool
	wg        sync.WaitGroup
}

func (s *lineSink) add(line string) {
	s.mu.Lock()
	s.pending = append(s.pending, line)
	schedule := !s.scheduled
	s.scheduled = true
	s.mu.Unlock()
	if !schedule {
		return
	}
	s.wg.Add(1)
	s.h.GoSafe("job output", func() {
		defer s.wg.Done()
		time.Sleep(30 * time.Millisecond)
		for {
			s.mu.Lock()
			lines := s.pending
			s.pending = nil
			if len(lines) == 0 {
				s.scheduled = false
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
			s.deliver(lines)
		}
	})
}

// deliver is a plain call, not a task: a batch every 30ms would cost a
// task's thread each.
func (s *lineSink) deliver(lines []string) {
	_, err := s.h.RT.Do(context.Background(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		if s.h.currentGen() != s.at.gen {
			return nil, nil
		}
		defer s.h.RT.PushFrame(L, s.at.origin, s.at.path)()
		L.Push(s.fn)
		L.Push(lua.FromGo(L, lines))
		return nil, L.PCall(1, 0, nil)
	})
	s.h.callbackDone(err)
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

	// start runs spec = { cmd, args, env, cwd, sandbox, on_stdout(lines), on_stderr(lines),
	// on_exit(result) }; output is also buffered (1 MiB) for wait(). Misuse
	// raises; a process that cannot start is the returned error.
	start := func(L *glua.LState) (*jobHandle, error) {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.job.start: spec must be a table")
		}
		cmdName := rawString(spec, "cmd")
		if cmdName == "" {
			L.RaiseError("zemi.job.start: cmd is required")
		}
		args := rawStrings(spec, "args")
		h.luaGate(L, "lua.job.start", argvLine(cmdName, args))
		extras := map[string]any{}
		if envT, ok := spec.RawGetString("env").(*glua.LTable); ok {
			if v, err := lua.ToGo(envT); err == nil {
				if m, ok := v.(map[string]any); ok {
					extras = m
				}
			}
		}
		env, err := h.Perm.ChildEnv(extras)
		if err != nil {
			L.RaiseError("zemi.job.start: %v", err)
		}
		// own session and group (proc): kill stops its children too, and a
		// background grandchild holding stdout does not keep the job alive
		cmd := proc.Command(cmdName, args...)
		cmd.Dir = h.Dirs.ProjectDir
		if cwd := rawString(spec, "cwd"); cwd != "" {
			dir, err := h.resolveJailed(cwd, h.luaJails(L, false), false)
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
		guard := func() string { return "" }
		// a program that was not found is proc.Start's returned error, not a
		// sandbox failure
		if rawBool(spec, "sandbox") && cmd.Err == nil {
			pol := h.sandboxPolicy(h.readOnly())
			// never an unconfined run in its place
			if err := sandbox.Wrap(cmd, pol); err != nil {
				L.RaiseError("zemi.job.start: sandbox: %v", err)
			}
			// NOTE: on a namespace backend with a writable project this walks
			// the project (sandboxWalkMax entries) while holding the interpreter;
			// a snapshot taken off it would need start to return before the run.
			guard = h.guardSandbox(pol)
		}
		h.lspDirty()
		jh := &jobHandle{id: jobSeq.Add(1), cmd: cmd, done: make(chan struct{})}
		at := h.callerOf(L)
		sinkOf := func(key string) *lineSink {
			if fn, ok := spec.RawGetString(key).(*glua.LFunction); ok {
				return &lineSink{h: h, at: at, fn: fn}
			}
			return nil
		}
		outSink, errSink := sinkOf("on_stdout"), sinkOf("on_stderr")
		outW := &lineWriter{mu: &jh.mu, buf: &jh.stdout, sink: outSink}
		errW := &lineWriter{mu: &jh.mu, buf: &jh.stderr, sink: errSink}
		cmd.Stdout, cmd.Stderr = outW, errW
		// stdin stays closed unless asked for: cat, grep or rg would wait on it
		if rawBool(spec, "stdin") {
			stdin, err := cmd.StdinPipe()
			if err != nil {
				L.RaiseError("zemi.job.start: %v", err)
			}
			jh.stdin = stdin
		}
		if err := proc.Start(cmd); err != nil {
			guard()
			if strings.ContainsAny(cmdName, " \t") {
				return nil, fmt.Errorf("zemi.job.start: %w (cmd is the program alone; the rest goes in args = { ... })", err)
			}
			return nil, fmt.Errorf("zemi.job.start: %w", err)
		}
		stopID := h.stops.add(func() { proc.Kill(cmd) })
		onExit, _ := spec.RawGetString("on_exit").(*glua.LFunction)
		h.GoSafe("job wait", func() {
			err := proc.Wait(cmd)
			h.stops.remove(stopID)
			guard()
			outW.flush()
			errW.flush()
			jh.mu.Lock()
			var ee *exec.ExitError
			switch {
			case errors.As(err, &ee):
				jh.code = ee.ExitCode()
			case err != nil && !errors.Is(err, exec.ErrWaitDelay):
				jh.err = err.Error()
			}
			jh.mu.Unlock()
			close(jh.done)
			// on_exit follows every output callback
			for _, s := range []*lineSink{outSink, errSink} {
				if s != nil {
					s.wg.Wait()
				}
			}
			if onExit != nil {
				h.callLuaAsync(at, onExit, jh.snapshot())
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
			if ms, ok := spec.RawGetString("timeout_ms").(glua.LNumber); ok && ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
		}
		return wait(L, "zemi.job.wait", jh, timeout, func() { proc.Kill(jh.cmd) })
	})))

	return tb
}
