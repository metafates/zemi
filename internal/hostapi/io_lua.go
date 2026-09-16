// io_lua.go is the Lua I/O surface (DESIGN §6.5, §9): zemi.env/state/
// timer/fs/http/job. Every side-effectful call passes a `lua.<ns.call>`
// permission gate at the Lua boundary; asks fail closed (host-API calls
// from Lua never open the interactive overlay, §6.5).
package hostapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metafates/zemi/internal/glob"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// secretPath matches the §9 secret shapes on the path as written and on the
// resolved one, so an innocently named symlink cannot hide a secret.
func (h *Host) secretPath(p string) bool {
	if perm.PathIsSecret(p) {
		return true
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return true
	}
	return perm.PathIsSecret(realPath(abs))
}

// inJail reports whether path is inside one of the jail roots.
func (h *Host) inJail(path string, roots ...string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	// Compare resolved paths, so a symlink inside a root cannot lead out
	// (and a root reached through a symlink, like macOS /var, still matches).
	real := foldPath(realPath(abs))
	for _, r := range roots {
		if r == "" {
			continue
		}
		rr := foldPath(realPath(r))
		if real == rr || strings.HasPrefix(real, rr+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// foldPath case-folds where the filesystem does: EvalSymlinks keeps the
// case it was given, so a jail root and a path under it can differ only by
// case and still be the same directory.
func foldPath(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// realPath resolves symlinks in the longest existing prefix of p, so a file
// about to be created is judged by its real parent directory.
func realPath(p string) string {
	suffix := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, suffix)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, suffix)
		}
		suffix = filepath.Join(filepath.Base(p), suffix)
		p = parent
	}
}

func (h *Host) readJails() []string {
	// Skill bodies (builtin and learned) live under the data dir; reading
	// them must not ask.
	// The log dir too: reading its own log is how the agent debugs zemi,
	// and the records are sanitized and secret-scrubbed on the way in.
	// The types dir holds the API annotations and the shipped Lua UI, which
	// the agent reads before restyling or extending it.
	return []string{h.Dirs.ProjectDir, h.Dirs.ConfigHome, h.Dirs.ProjectConfigDir(),
		h.builtinSkillsDir(), h.learnedSkillsDir(), h.ScratchDir(), h.Dirs.LogDir(), h.TypesDir()}
}

func (h *Host) writeJails() []string {
	return []string{h.Dirs.ProjectDir}
}

// fsDefault implements the §9 built-in defaults for fs subjects: reads
// inside the jail allow except secret-shaped paths, writes allow only in the
// session scratchpad; everything else asks.
func (h *Host) fsDefault(subject, arg string) perm.Decision {
	switch subject {
	case "fs.read", "lua.fs.read":
		if h.secretPath(arg) {
			return perm.Ask
		}
		if h.inJail(arg, h.readJails()...) {
			return perm.Allow
		}
	case "fs.write", "lua.fs.write":
		if subject == "fs.write" && h.inScratch(arg) && !h.secretPath(arg) {
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
	pipe := h.PipelineFor()
	pipe.Ask = nil
	// rules are attributed to the calling code's origin, not to the model
	pipe.Principal = string(h.RT.CurrentOrigin(L))
	var v tools.Verdict
	if subject == "lua.job.start" {
		// a command line, normalized like bash so rules can scope arguments
		// and a second command cannot ride an allow
		o := pipe.Store.CheckCommandFor(subject, arg, pipe.Principal)
		v = tools.Verdict{Decision: o.Decision, Reason: o.Reason, DecidedBy: "default"}
		if o.Rule != nil {
			v.DecidedBy = "rule"
		}
	} else {
		v = pipe.Check(tools.Request{Subject: subject, Arg: arg, Reason: "Lua call"})
	}
	switch v.Decision {
	case perm.Allow:
	case perm.Ask:
		h.EmitAsync("permission.request", map[string]any{
			"subject": subject, "arg": arg, "post_hoc": true,
		})
		L.RaiseError("%s %s: permission ask required", subject, arg)
	default:
		L.RaiseError("%s %s: permission denied (%s)", subject, arg, v.DecidedBy)
	}
	return v
}

// ---------------------------------------------------------------------------
// zemi.env

func (h *Host) envNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		// user config reads its own keys (api_key = zemi.env.get(...));
		// plugins, projects and learned code never see secret-shaped names
		if o := h.RT.CurrentOrigin(L); perm.EnvIsSecret(name) && o != lua.OriginUser && o != lua.OriginBuiltin {
			L.RaiseError("zemi.env.get: %q matches the secret denylist", name)
		}
		L.Push(lua.FromGo(L, os.Getenv(name)))
		return 1
	}))
	return tb
}

// ---------------------------------------------------------------------------
// zemi.state (persisted kv, global scope)

func (h *Host) stateNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		key := L.CheckString(1)
		if h.GlobalDB == nil {
			L.Push(glua.LNil)
			return 1
		}
		v, err := h.GlobalDB.GetKV(key)
		if err != nil {
			L.Push(glua.LNil)
			return 1
		}
		var decoded any
		if json.Unmarshal([]byte(v), &decoded) != nil {
			L.Push(glua.LNil)
			return 1
		}
		L.Push(lua.FromGo(L, decoded))
		return 1
	}))
	tb.RawSetString("put", L.NewFunction(func(L *glua.LState) int {
		key := L.CheckString(1)
		v := luaToGoChecked(L, 2, "zemi.state.put")
		if h.GlobalDB == nil {
			return luaResult(L, errors.New("store unavailable"))
		}
		b, err := json.Marshal(v)
		if err != nil {
			L.RaiseError("zemi.state.put: %v", err)
		}
		return luaResult(L, h.GlobalDB.SetKV(key, string(b)))
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

// stopReg holds how to stop the live generation's timers and jobs, which a
// reload stops before replay (DESIGN §4).
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
	set := func(repeat bool) *glua.LFunction {
		return L.NewFunction(func(L *glua.LState) int {
			h.refuseDuringRender(L, "zemi.timer")
			ms := L.CheckNumber(1)
			fn := L.CheckFunction(2)
			// set(0, fn) runs fn on the next queue turn (vim.schedule); a
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
			L.Push(handleTb)
			return 1
		})
	}
	tb.RawSetString("set", set(false))
	tb.RawSetString("every", set(true))
	return tb
}

// ---------------------------------------------------------------------------
// zemi.fs

// fsNamespace is zemi.fs. Paths resolve inside the jails, reads pass the
// lua.fs.read gate and changes lua.fs.write; runtime failures return nil, err.
func (h *Host) fsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	resolve := func(L *glua.LState, api string, write bool) string {
		roots, subject := h.readJails(), "lua.fs.read"
		if write {
			roots, subject = h.writeJails(), "lua.fs.write"
		}
		resolved, err := h.resolveJailed(L.CheckString(1), roots)
		if err != nil {
			L.RaiseError("zemi.fs.%s: %v", api, err)
		}
		h.luaGate(L, subject, resolved)
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
		path := resolve(L, "read", false)
		b, err := os.ReadFile(path)
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(glua.LString(b))
		return 1
	}))

	tb.RawSetString("write", L.NewFunction(func(L *glua.LState) int {
		path := resolve(L, "write", true)
		content := L.CheckString(2)
		err := os.MkdirAll(filepath.Dir(path), 0o755)
		if err == nil {
			err = os.WriteFile(path, []byte(content), 0o644)
		}
		return luaResult(L, err)
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		dir := resolve(L, "list", false)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return luaResult(L, err)
		}
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
		path := resolve(L, "stat", false)
		fi, err := os.Lstat(path)
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(lua.FromGo(L, map[string]any{"type": kind(fi), "size": fi.Size(), "mtime": fi.ModTime().UnixMilli()}))
		return 1
	}))

	tb.RawSetString("exists", L.NewFunction(func(L *glua.LState) int {
		path := resolve(L, "exists", false)
		_, err := os.Lstat(path)
		L.Push(glua.LBool(err == nil))
		return 1
	}))

	tb.RawSetString("mkdir", L.NewFunction(func(L *glua.LState) int {
		path := resolve(L, "mkdir", true)
		return luaResult(L, os.MkdirAll(path, 0o755))
	}))

	// remove deletes one file or empty directory (never a tree).
	tb.RawSetString("remove", L.NewFunction(func(L *glua.LState) int {
		path := resolve(L, "remove", true)
		return luaResult(L, os.Remove(path))
	}))

	tb.RawSetString("glob", L.NewFunction(func(L *glua.LState) int {
		pattern := L.CheckString(1)
		h.luaGate(L, "lua.fs.read", h.Dirs.ProjectDir)
		matches, err := h.jailedGlob(pattern)
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
		rows := []any{}
		for _, hit := range h.jailedGrep(re) {
			rows = append(rows, map[string]any{"path": hit.path, "line": hit.line, "text": hit.text})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	return tb
}

// resolveJailed resolves path against the project dir and enforces the
// jail (no escaping the roots, no symlinks out).
func (h *Host) resolveJailed(path string, roots []string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(h.Dirs.ProjectDir, resolved)
	}
	resolved = filepath.Clean(resolved)
	if !h.inJail(resolved, roots...) {
		return "", fmt.Errorf("%s is outside the jail", path)
	}
	return resolved, nil
}

// jailedGlob matches project-relative paths in the glob tool's dialect,
// anchored at the root so "*.go" stays in the top directory.
func (h *Host) jailedGlob(pattern string) ([]string, error) {
	match, err := glob.Match("/" + strings.TrimPrefix(pattern, "/"))
	if err != nil {
		return nil, err
	}
	var out []string
	root := h.Dirs.ProjectDir
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return skipHidden(root, p, d, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		if match("/" + rel) {
			out = append(out, rel)
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

// skipHidden is the walk result for a directory or unreadable entry: dot
// directories below root are skipped, unreadable entries ignored.
func skipHidden(root, p string, d os.DirEntry, err error) error {
	if err == nil && strings.HasPrefix(d.Name(), ".") && p != root {
		return filepath.SkipDir
	}
	return nil
}

type grepHit struct {
	path string
	line int
	text string
}

// jailedGrep never reads secret-shaped files, which the lua.fs.read gate
// would ask for, nor symlinks, which could lead out of the project.
func (h *Host) jailedGrep(re *regexp.Regexp) []grepHit {
	var out []grepHit
	root := h.Dirs.ProjectDir
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return skipHidden(root, p, d, err)
		}
		if d.Type()&os.ModeSymlink != 0 || perm.PathIsSecret(p) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 1<<20))
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				rel, _ := filepath.Rel(root, p)
				out = append(out, grepHit{path: rel, line: i + 1, text: line})
			}
		}
		return nil
	})
	return out
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
	// (zemi.permission.rule refuses them elsewhere), and project and
	// learned callers never get the override.
	allowPrivate := v.DecidedBy == "rule" && origin.Rank() > lua.OriginProject.Rank()
	hnd := &httpHandle{done: make(chan struct{})}
	at := h.callerOf(L)
	h.GoSafe(subject, func() {
		res, err := h.httpFetch(subject, string(origin), allowPrivate, req)
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
	handle.RawSetString("wait", L.NewFunction(func(L *glua.LState) int {
		select {
		case <-hnd.done:
		case <-time.After(120 * time.Second):
			return luaResult(L, errors.New("wait timed out"))
		}
		if hnd.err != nil {
			return luaResult(L, hnd.err)
		}
		L.Push(lua.FromGo(L, hnd.res))
		return 1
	}))
	L.Push(handle)
	return 1
}

// httpFetch performs the request with size and time caps under the same §9
// network rules as the fetch tool: http/https only, no userinfo, denied
// addresses refused at connect time, and every redirect hop re-gated under
// the same subject as the calling origin (deny/ask refuses the hop, §6.5).
func (h *Host) httpFetch(subject, principal string, allowPrivate bool, r httpReq) (map[string]any, error) {
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
	req, err := http.NewRequest(r.method, r.url, body)
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

// callLuaAsync runs fn(args...) on the interpreter from a host goroutine,
// dropping the call when the generation changed; a failing callback is
// logged and shown in /errors.
func (h *Host) callLuaAsync(at luaCaller, fn *glua.LFunction, args ...any) {
	_, err := h.RT.Do(context.Background(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		if h.currentGen() != at.gen {
			return nil, nil
		}
		defer h.RT.PushFrame(L, at.origin, at.path)()
		L.Push(fn)
		for _, a := range args {
			L.Push(lua.FromGo(L, a))
		}
		return nil, L.PCall(len(args), 0, nil)
	})
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
			s.h.callLuaAsync(s.at, s.fn, lines)
		}
	})
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

	// start runs spec = { cmd, args, env, on_stdout(lines), on_stderr(lines),
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
		env, err := perm.ChildEnv(extras)
		if err != nil {
			L.RaiseError("zemi.job.start: %v", err)
		}
		// own session and group (proc): kill stops its children too, and a
		// background grandchild holding stdout does not keep the job alive
		cmd := proc.Command(cmdName, args...)
		cmd.Dir = h.Dirs.ProjectDir
		cmd.Env = env
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
			return nil, err
		}
		stopID := h.stops.add(func() { proc.Kill(cmd) })
		onExit, _ := spec.RawGetString("on_exit").(*glua.LFunction)
		h.GoSafe("job wait", func() {
			err := proc.Wait(cmd)
			h.stops.remove(stopID)
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

	wait := func(L *glua.LState, jh *jobHandle, timeout time.Duration) int {
		select {
		case <-jh.done:
			L.Push(lua.FromGo(L, jh.snapshot()))
			return 1
		case <-time.After(timeout):
			L.Push(glua.LNil)
			L.Push(glua.LString("timeout"))
			return 2
		}
	}

	tb.RawSetString("start", L.NewFunction(func(L *glua.LState) int {
		jh, err := start(L)
		if err != nil {
			return luaResult(L, err)
		}
		handle := L.NewTable()
		handle.RawSetString("id", glua.LNumber(jh.id))
		// Methods are colon calls (DESIGN §6.0), so arguments start at 2.
		// wait([timeout_ms]) blocks the interpreter: keep it for tools and
		// short commands, and prefer on_exit elsewhere
		handle.RawSetString("wait", L.NewFunction(func(L *glua.LState) int {
			timeout := 30 * time.Second
			if ms := L.OptInt(2, 0); ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
			return wait(L, jh, timeout)
		}))
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
		L.Push(handle)
		return 1
	}))

	// out runs a command to completion (spec.timeout_ms, default 60s, kills
	// it); it blocks the interpreter, so it belongs in tools.
	tb.RawSetString("out", L.NewFunction(func(L *glua.LState) int {
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
		if n := wait(L, jh, timeout); n == 2 {
			proc.Kill(jh.cmd)
			return 2
		}
		return 1
	}))

	return tb
}
