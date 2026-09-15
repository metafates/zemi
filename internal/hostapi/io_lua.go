// io_lua.go is the Lua I/O surface (DESIGN §6.5, §9): zemi.env/state/
// timer/fs/http/job. Every side-effectful call passes a `lua.<ns.call>`
// permission gate at the Lua boundary; asks fail closed (host-API calls
// from Lua never open the interactive overlay, §6.5).
package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
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

func (h *Host) jailedGlob(pattern string) ([]string, error) {
	var out []string
	root := h.Dirs.ProjectDir
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
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
		if ok, _ := filepath.Match(pattern, rel); ok {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
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

type httpHandle struct {
	done chan struct{}
	res  map[string]any
	err  string
}

func (h *Host) httpNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		url := L.CheckString(1)
		if err := h.luaGate(L, "lua.http.get", url); err != nil {
			return 0
		}
		hnd := &httpHandle{done: make(chan struct{})}
		onDone := glua.LNil
		if opts, ok := L.Get(2).(*glua.LTable); ok {
			onDone = opts.RawGetString("on_done")
		}
		gen := h.currentGen()
		go func() {
			res, err := h.httpFetch(url)
			hnd.res = res
			if err != nil {
				hnd.err = err.Error()
			}
			close(hnd.done)
			if fn, ok := onDone.(*glua.LFunction); ok {
				payload := map[string]any{"url": url}
				if err != nil {
					payload["error"] = err.Error()
				} else {
					for k, v := range res {
						payload[k] = v
					}
				}
				_, _ = h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
					if h.currentGen() != gen {
						return nil, nil
					}
					base := L.GetTop()
					L.Push(fn)
					L.Push(lua.FromGo(L, payload))
					if perr := L.PCall(1, 0, nil); perr != nil {
						L.SetTop(base)
						return nil, perr
					}
					return nil, nil
				})
			}
		}()
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
	}))
	return tb
}

// httpFetch performs the GET with size and time caps; redirects re-gate
// through the permission store (deny/ask refuses the hop, §6.5).
func (h *Host) httpFetch(url string) (map[string]any, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			o := h.Perm.Evaluate("lua.http.get", req.URL.String(), "user")
			if o.Decision != perm.Allow {
				return fmt.Errorf("redirect to %s refused by the permission gate", req.URL)
			}
			return nil
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status": resp.StatusCode,
		"body":   string(body),
	}, nil
}

// ---------------------------------------------------------------------------
// zemi.job

type jobHandle struct {
	done   chan struct{}
	stdout strings.Builder
	stderr strings.Builder
	code   int
	err    string
	mu     sync.Mutex
}

func (j *jobHandle) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"stdout": j.stdout.String(), "stderr": j.stderr.String(),
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

func (h *Host) jobNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	start := func(L *glua.LState) (*exec.Cmd, *jobHandle, string) {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.job.start: spec must be a table")
			return nil, nil, ""
		}
		cmdName := rawString(spec, "cmd")
		if cmdName == "" {
			L.RaiseError("zemi.job.start: cmd is required")
			return nil, nil, ""
		}
		if err := h.luaGate(L, "lua.job.start", cmdName); err != nil {
			return nil, nil, ""
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
			return nil, nil, ""
		}
		extras := map[string]any{}
		if envT, ok := spec.RawGetString("env").(*glua.LTable); ok {
			v, err := lua.ToGo(envT)
			if err == nil {
				if m, ok := v.(map[string]any); ok {
					extras = m
				}
			}
		}
		env, err := jobEnvFilters(extras)
		if err != nil {
			L.RaiseError("zemi.job.start: %v", err)
			return nil, nil, ""
		}
		cmd := exec.Command(cmdName, args...)
		cmd.Dir = h.Dirs.ProjectDir
		cmd.Env = env
		jh := &jobHandle{done: make(chan struct{})}
		stdout, errS := cmd.StdoutPipe()
		stderr, errE := cmd.StderrPipe()
		if errS != nil || errE != nil {
			L.RaiseError("zemi.job.start: pipes unavailable")
			return nil, nil, ""
		}
		if err := cmd.Start(); err != nil {
			L.RaiseError("zemi.job.start: %v", err)
			return nil, nil, ""
		}
		go func() {
			b, _ := io.ReadAll(io.LimitReader(stdout, 1<<20))
			jh.mu.Lock()
			jh.stdout.Write(b)
			jh.mu.Unlock()
		}()
		go func() {
			b, _ := io.ReadAll(io.LimitReader(stderr, 1<<20))
			jh.mu.Lock()
			jh.stderr.Write(b)
			jh.mu.Unlock()
		}()
		go func() {
			err := cmd.Wait()
			jh.mu.Lock()
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					jh.code = ee.ExitCode()
				} else {
					jh.err = err.Error()
				}
			}
			jh.mu.Unlock()
			close(jh.done)
			h.EmitAsync("job.exit", map[string]any{"cmd": cmdName, "code": jh.code})
		}()
		return cmd, jh, ""
	}

	tb.RawSetString("start", L.NewFunction(func(L *glua.LState) int {
		_, jh, _ := start(L)
		if jh == nil {
			return 0
		}
		handle := L.NewTable()
		handle.RawSetString("wait", L.NewFunction(func(L *glua.LState) int {
			<-jh.done
			L.Push(lua.FromGo(L, jh.snapshot()))
			return 1
		}))
		L.Push(handle)
		return 1
	}))

	tb.RawSetString("out", L.NewFunction(func(L *glua.LState) int {
		_, jh, _ := start(L)
		if jh == nil {
			return 0
		}
		<-jh.done
		L.Push(lua.FromGo(L, jh.snapshot()))
		return 1
	}))

	return tb
}
