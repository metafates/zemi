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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// secretEnvShape matches env var names zemi refuses to expose or pass to
// children (case-insensitive).
var secretEnvShape = []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "API_KEY", "PRIVATE_KEY"}

func envIsSecret(name string) bool {
	up := strings.ToUpper(name)
	for _, s := range secretEnvShape {
		if strings.Contains(up, s) {
			return true
		}
	}
	return false
}

// secretPathShape matches secret-shaped filesystem paths (§9); reads ask.
var secretPathShape = []string{".env", ".env.*", ".envrc", "*.pem", "*.key", "id_rsa*", "*credential*", "*secret*", ".netrc"}

func pathIsSecret(p string) bool {
	base := filepath.Base(p)
	for _, pat := range secretPathShape {
		if ok, _ := filepath.Match(strings.ToLower(pat), strings.ToLower(base)); ok {
			return true
		}
	}
	return false
}

// inJail reports whether path is inside one of the jail roots.
func (h *Host) inJail(path string, roots ...string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	// Compare resolved paths, so a symlink inside a root cannot lead out
	// (and a root reached through a symlink, like macOS /var, still matches).
	real := realPath(abs)
	for _, r := range roots {
		if r == "" {
			continue
		}
		rr := realPath(r)
		if real == rr || strings.HasPrefix(real, rr+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
	return []string{h.Dirs.ProjectDir, h.Dirs.ConfigHome, h.Dirs.ProjectConfigDir(),
		h.builtinSkillsDir(), h.learnedSkillsDir()}
}

func (h *Host) writeJails() []string {
	return []string{h.Dirs.ProjectDir}
}

// fsDefault implements the §9 built-in defaults for fs subjects: reads
// inside the jail allow except secret-shaped paths; everything else asks.
func (h *Host) fsDefault(subject, arg string) perm.Decision {
	switch subject {
	case "fs.read", "lua.fs.read":
		if pathIsSecret(arg) {
			return perm.Ask
		}
		if h.inJail(arg, h.readJails()...) {
			return perm.Allow
		}
	case "fs.write", "lua.fs.write":
		if h.inJail(arg, h.writeJails()...) && !pathIsSecret(arg) {
			return perm.Ask // write stays ask inside the jail too
		}
	}
	return perm.Ask
}

// luaGate runs one Lua-boundary permission check. Asks fail closed with
// the §6.5 contract ("permission ask required"): host-API calls from Lua
// never open the interactive overlay, so the asker is stripped from the
// pipeline copy and an unresolved ask stays an ask for the gate to refuse.
func (h *Host) luaGate(L *glua.LState, subject, arg string) error {
	pipe := h.PipelineFor()
	pipe.Ask = nil
	v := pipe.Check(tools.Request{Subject: subject, Arg: arg, Reason: "Lua call"})
	switch v.Decision {
	case perm.Allow:
		return nil
	case perm.Ask:
		h.EmitAsync("permission.request", map[string]any{
			"subject": subject, "arg": arg, "post_hoc": true,
		})
		L.RaiseError("%s %s: permission ask required", subject, arg)
		return fmt.Errorf("ask")
	default:
		L.RaiseError("%s %s: permission denied (%s)", subject, arg, v.DecidedBy)
		return fmt.Errorf("deny")
	}
}

// ---------------------------------------------------------------------------
// zemi.env

func (h *Host) envNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		if envIsSecret(name) {
			L.RaiseError("zemi.env.get: %q matches the secret denylist", name)
			return 0
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
			L.RaiseError("zemi.state.get: store unavailable")
			return 0
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
		v, err := luaToGoChecked(L, 2, "zemi.state.put")
		if err != nil {
			return 0
		}
		if h.GlobalDB == nil {
			L.RaiseError("zemi.state.put: store unavailable")
			return 0
		}
		b, err := json.Marshal(v)
		if err != nil {
			L.RaiseError("zemi.state.put: %v", err)
			return 0
		}
		if err := h.GlobalDB.SetKV(key, string(b)); err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LTrue)
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

type timerReg struct {
	mu   sync.Mutex
	next int
	m    map[int]*timerHandle
}

func newTimerReg() *timerReg { return &timerReg{m: map[int]*timerHandle{}} }

func (r *timerReg) add(t *timerHandle) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	r.m[r.next] = t
	return r.next
}

func (r *timerReg) purge() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.m {
		t.stopIt()
	}
	r.m = map[int]*timerHandle{}
}

func (h *Host) timerNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	set := func(repeat bool) *glua.LFunction {
		return L.NewFunction(func(L *glua.LState) int {
			if h.refuseDuringRender(L, "zemi.timer") {
				return 0
			}
			ms := L.CheckNumber(1)
			fn := L.CheckFunction(2)
			// set(0, fn) runs fn on the next queue turn (vim.schedule); a
			// repeating timer needs a real interval.
			if ms < 0 || (repeat && ms == 0) {
				L.RaiseError("zemi.timer: interval must be positive")
				return 0
			}
			d := time.Duration(ms) * time.Millisecond
			gen := h.currentGen()
			handle := &timerHandle{stop: make(chan struct{})}
			h.timers.add(handle)
			h.GoSafe("zemi.timer", func() {
				t := time.NewTimer(d)
				defer t.Stop()
				for {
					select {
					case <-handle.stop:
						return
					case <-t.C:
					}
					_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
						if h.currentGen() != gen {
							return nil, nil
						}
						base := L.GetTop()
						L.Push(fn)
						if err := L.PCall(0, 0, nil); err != nil {
							L.SetTop(base)
							return nil, err
						}
						return nil, nil
					})
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

func (h *Host) fsNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("read", L.NewFunction(func(L *glua.LState) int {
		path := L.CheckString(1)
		resolved, err := h.resolveJailed(path, h.readJails())
		if err != nil {
			L.RaiseError("zemi.fs.read: %v", err)
			return 0
		}
		if err := h.luaGate(L, "lua.fs.read", resolved); err != nil {
			return 0
		}
		b, err := os.ReadFile(resolved)
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LString(b))
		return 1
	}))

	tb.RawSetString("write", L.NewFunction(func(L *glua.LState) int {
		path := L.CheckString(1)
		content := L.CheckString(2)
		resolved, err := h.resolveJailed(path, h.writeJails())
		if err != nil {
			L.RaiseError("zemi.fs.write: %v", err)
			return 0
		}
		if err := h.luaGate(L, "lua.fs.write", resolved); err != nil {
			return 0
		}
		err = os.MkdirAll(filepath.Dir(resolved), 0o755)
		if err == nil {
			err = os.WriteFile(resolved, []byte(content), 0o644)
		}
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LTrue)
		return 1
	}))

	tb.RawSetString("glob", L.NewFunction(func(L *glua.LState) int {
		pattern := L.CheckString(1)
		if err := h.luaGate(L, "lua.fs.read", h.Dirs.ProjectDir); err != nil {
			return 0
		}
		matches, err := h.jailedGlob(pattern)
		if err != nil {
			L.RaiseError("zemi.fs.glob: %v", err)
			return 0
		}
		out := L.NewTable()
		for _, m := range matches {
			out.Append(glua.LString(m))
		}
		L.Push(out)
		return 1
	}))

	h.fsExtraFns(L, tb)

	tb.RawSetString("grep", L.NewFunction(func(L *glua.LState) int {
		pattern := L.CheckString(1)
		if err := h.luaGate(L, "lua.fs.read", h.Dirs.ProjectDir); err != nil {
			return 0
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			L.RaiseError("zemi.fs.grep: %v", err)
			return 0
		}
		out := L.NewTable()
		for _, hit := range h.jailedGrep(re) {
			row := L.NewTable()
			row.RawSetString("path", glua.LString(hit.path))
			row.RawSetString("line", glua.LNumber(hit.line))
			row.RawSetString("text", glua.LString(hit.text))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))

	return tb
}

// resolveJailed resolves path against the project dir and enforces the
// jail (no escaping the roots, no symlinks out).
func (h *Host) resolveJailed(path string, roots []string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
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

// jailedGlob matches project-relative paths: * and ? stay within one path
// segment, ** spans directories ("**/*.go" matches at any depth).
func (h *Host) jailedGlob(pattern string) ([]string, error) {
	re, err := globPathRegexp(pattern)
	if err != nil {
		return nil, err
	}
	var out []string
	root := h.Dirs.ProjectDir
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		if re.MatchString(filepath.ToSlash(rel)) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func globPathRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

type grepHit struct {
	path string
	line int
	text string
}

func (h *Host) jailedGrep(re *regexp.Regexp) []grepHit {
	var out []grepHit
	root := h.Dirs.ProjectDir
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && strings.HasPrefix(d.Name(), ".") && p != root {
				return filepath.SkipDir
			}
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
			return 0
		}
		if hs, ok := spec.RawGetString("headers").(*glua.LTable); ok {
			hs.ForEach(func(k, v glua.LValue) {
				req.headers[k.String()] = v.String()
			})
		}
		if h.RT.CurrentOrigin() != lua.OriginUser {
			for k := range req.headers {
				switch strings.ToLower(k) {
				case "authorization", "proxy-authorization", "cookie":
					L.RaiseError("zemi.http.request: only user config may set the %s header", k)
					return 0
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
	err  string
}

// httpStart gates the request, runs it off the interpreter and pushes the
// handle.
func (h *Host) httpStart(L *glua.LState, req httpReq, onDone *glua.LFunction) int {
	subject := "lua.http." + strings.ToLower(req.method)
	if err := h.luaGate(L, subject, req.url); err != nil {
		return 0
	}
	hnd := &httpHandle{done: make(chan struct{})}
	gen := h.currentGen()
	h.GoSafe(subject, func() {
		res, err := h.httpFetch(subject, req)
		hnd.res = res
		if err != nil {
			hnd.err = err.Error()
		}
		close(hnd.done)
		if onDone != nil {
			payload := map[string]any{"url": req.url}
			if err != nil {
				payload["error"] = err.Error()
			} else {
				maps.Copy(payload, res)
			}
			h.callLuaAsync(gen, onDone, payload)
		}
	})
	handle := L.NewTable()
	handle.RawSetString("wait", L.NewFunction(func(L *glua.LState) int {
		select {
		case <-hnd.done:
		case <-time.After(120 * time.Second):
			L.RaiseError("zemi.http: wait timed out")
			return 0
		}
		if hnd.err != "" {
			L.Push(glua.LNil)
			L.Push(glua.LString(hnd.err))
			return 2
		}
		L.Push(lua.FromGo(L, hnd.res))
		return 1
	}))
	L.Push(handle)
	return 1
}

// httpFetch performs the request with size and time caps; redirects re-gate
// through the permission store under the same subject (deny/ask refuses the
// hop, §6.5).
func (h *Host) httpFetch(subject string, r httpReq) (map[string]any, error) {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			o := h.Perm.Evaluate(subject, req.URL.String(), "user")
			if o.Decision != perm.Allow {
				return fmt.Errorf("redirect to %s refused by the permission gate", req.URL)
			}
			return nil
		},
	}
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

// callLuaAsync runs fn(args...) on the interpreter from a host goroutine,
// dropping the call when the generation changed; a failing callback is
// logged and shown in /errors.
func (h *Host) callLuaAsync(gen Gen, fn *glua.LFunction, args ...any) {
	_, err := h.RT.Do(context.Background(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		if h.currentGen() != gen {
			return nil, nil
		}
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

// jobEnvFilters builds the child environment: parent minus secret-shaped
// names plus Lua-supplied extras (§6.5).
func jobEnvFilters(extras map[string]any) ([]string, error) {
	var out []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && !envIsSecret(name) {
			out = append(out, kv)
		}
	}
	for k, v := range extras {
		if envIsSecret(k) || perm.EnvAssignmentGate(k) {
			return nil, fmt.Errorf("env var %q is refused by the environment gate", k)
		}
		out = append(out, k+"="+fmt.Sprint(v))
	}
	return out, nil
}

// lineSink batches output lines for a Lua callback: a line waits at most
// 30ms, and one delivery runs at a time, so a chatty process cannot flood
// the queue.
type lineSink struct {
	h         *Host
	gen       Gen
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
			s.h.callLuaAsync(s.gen, s.fn, lines)
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

func (h *Host) jobNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	// start runs spec = { cmd, args, env, on_stdout(lines), on_stderr(lines),
	// on_exit(result) }; output is also buffered (1 MiB) for wait().
	start := func(L *glua.LState) *jobHandle {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.job.start: spec must be a table")
			return nil
		}
		cmdName := rawString(spec, "cmd")
		if cmdName == "" {
			L.RaiseError("zemi.job.start: cmd is required")
			return nil
		}
		if err := h.luaGate(L, "lua.job.start", cmdName); err != nil {
			return nil
		}
		var args []string
		if list, ok := spec.RawGetString("args").(*glua.LTable); ok {
			list.ForEach(func(_, v glua.LValue) {
				if s, ok := v.(glua.LString); ok {
					args = append(args, string(s))
				}
			})
		}
		if perm.Normalize(cmdName).ForceAsk != "" {
			L.RaiseError("zemi.job.start: command is refused by the environment gate")
			return nil
		}
		extras := map[string]any{}
		if envT, ok := spec.RawGetString("env").(*glua.LTable); ok {
			if v, err := lua.ToGo(envT); err == nil {
				if m, ok := v.(map[string]any); ok {
					extras = m
				}
			}
		}
		env, err := jobEnvFilters(extras)
		if err != nil {
			L.RaiseError("zemi.job.start: %v", err)
			return nil
		}
		// own session and group (proc): kill stops its children too, and a
		// background grandchild holding stdout does not keep the job alive
		cmd := proc.Command(cmdName, args...)
		cmd.Dir = h.Dirs.ProjectDir
		cmd.Env = env
		jh := &jobHandle{id: jobSeq.Add(1), cmd: cmd, done: make(chan struct{})}
		gen := h.currentGen()
		sinkOf := func(key string) *lineSink {
			if fn, ok := spec.RawGetString(key).(*glua.LFunction); ok {
				return &lineSink{h: h, gen: gen, fn: fn}
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
				return nil
			}
			jh.stdin = stdin
		}
		if err := proc.Start(cmd); err != nil {
			L.RaiseError("zemi.job.start: %v", err)
			return nil
		}
		onExit, _ := spec.RawGetString("on_exit").(*glua.LFunction)
		h.GoSafe("job wait", func() {
			err := proc.Wait(cmd)
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
				h.callLuaAsync(gen, onExit, jh.snapshot())
			}
			h.EmitAsync("job.exit", map[string]any{"id": jh.id, "cmd": cmdName, "code": jh.code})
		})
		return jh
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
		jh := start(L)
		if jh == nil {
			return 0
		}
		handle := L.NewTable()
		handle.RawSetString("id", glua.LNumber(jh.id))
		// wait([timeout_ms]) blocks the interpreter: keep it for tools and
		// short commands, and prefer on_exit elsewhere
		handle.RawSetString("wait", L.NewFunction(func(L *glua.LState) int {
			timeout := 30 * time.Second
			if ms := L.OptInt(1, 0); ms > 0 {
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
				return 0
			}
			if _, err := io.WriteString(jh.stdin, L.CheckString(1)); err != nil {
				L.Push(glua.LNil)
				L.Push(glua.LString(err.Error()))
				return 2
			}
			L.Push(glua.LTrue)
			return 1
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
		jh := start(L)
		if jh == nil {
			return 0
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
