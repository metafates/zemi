// lsp.go feeds language server diagnostics to the model: after write, edit
// or multiedit changes a file, its server's errors and warnings are appended
// to the tool result, and the diagnostics tool checks any file. Servers
// start lazily on the first matching file: the builtin set when its binary
// is installed and the project has its marker file (lsp.auto), plus servers
// user config adds with zemi.lsp.add.
package hostapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lsp"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

type lspDef struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	Exts    []string // ".go"
	Markers []string // files at the repository root that enable a builtin
}

var builtinLSP = []lspDef{
	{Name: "gopls", Command: "gopls", Exts: []string{".go"}, Markers: []string{"go.mod", "go.work"}},
	{Name: "pyright", Command: "pyright-langserver", Args: []string{"--stdio"}, Exts: []string{".py"}, Markers: []string{"pyproject.toml", "setup.py", "requirements.txt", "pyrightconfig.json"}},
	{Name: "rust-analyzer", Command: "rust-analyzer", Exts: []string{".rs"}, Markers: []string{"Cargo.toml"}},
	{Name: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}, Exts: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}, Markers: []string{"package.json", "tsconfig.json"}},
	{Name: "clangd", Command: "clangd", Exts: []string{".c", ".h", ".cc", ".cpp", ".hpp", ".cxx"}, Markers: []string{"compile_commands.json", "CMakeLists.txt", ".clangd"}},
}

var languageIDs = map[string]string{
	".go": "go", ".py": "python", ".rs": "rust", ".ts": "typescript", ".tsx": "typescriptreact",
	".js": "javascript", ".jsx": "javascriptreact", ".mjs": "javascript", ".cjs": "javascript",
	".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp", ".cxx": "cpp",
}

type lspProc struct {
	h      *Host
	def    lspDef
	ready  chan struct{} // closed when the start attempt ends
	client *lsp.Client
	err    error
}

type lspReg struct {
	mu      sync.Mutex
	user    []lspDef
	running map[string]*lspProc
}

func newLSPReg() *lspReg { return &lspReg{running: map[string]*lspProc{}} }

// stopAll stops every server and forgets user definitions (reload replays
// them).
func (r *lspReg) stopAll() {
	r.mu.Lock()
	procs := r.running
	r.running, r.user = map[string]*lspProc{}, nil
	r.mu.Unlock()
	for _, p := range procs {
		p.close()
	}
}

// close stops the server once its start attempt ends.
func (p *lspProc) close() {
	p.h.GoSafe("lsp stop "+p.def.Name, func() {
		<-p.ready
		if p.client != nil {
			p.client.Close()
		}
	})
}

// lspFor picks the server for path: user definitions first, then an
// installed builtin whose marker exists at the repository root.
func (h *Host) lspFor(path string) (lspDef, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	h.lsp.mu.Lock()
	user := append([]lspDef(nil), h.lsp.user...)
	h.lsp.mu.Unlock()
	for _, d := range user {
		if slices.Contains(d.Exts, ext) {
			return d, true
		}
	}
	if v, ok := h.Settings.Get("lsp.auto"); ok && v == false {
		return lspDef{}, false
	}
	root := h.Dirs.RepoRoot()
	for _, d := range builtinLSP {
		if !slices.Contains(d.Exts, ext) {
			continue
		}
		if _, err := exec.LookPath(d.Command); err != nil {
			continue
		}
		for _, m := range d.Markers {
			if _, err := os.Stat(filepath.Join(root, m)); err == nil {
				return d, true
			}
		}
	}
	return lspDef{}, false
}

// lspClient returns the running client for def, starting it once; a failed
// start is remembered until /lsp restart.
func (h *Host) lspClient(ctx context.Context, def lspDef) (*lsp.Client, error) {
	h.lsp.mu.Lock()
	p, ok := h.lsp.running[def.Name]
	if !ok {
		p = &lspProc{h: h, def: def, ready: make(chan struct{})}
		h.lsp.running[def.Name] = p
	}
	h.lsp.mu.Unlock()
	if !ok {
		h.GoSafe("lsp start "+def.Name, func() {
			defer close(p.ready)
			env, _ := perm.ChildEnv(nil)
			for k, v := range def.Env {
				env = append(env, k+"="+v)
			}
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.client, p.err = lsp.Start(sctx, lsp.Config{
				Command: def.Command, Args: def.Args, Env: env, Root: h.Dirs.RepoRoot(),
				Stderr: serverLogWriter{h: h, source: "lsp", server: def.Name},
			})
			if p.err != nil {
				h.Log.Warn("lsp", "server failed to start", map[string]any{"server": def.Name, "error": p.err.Error()})
			}
		})
	}
	select {
	case <-p.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.err != nil {
		return nil, p.err
	}
	select {
	case <-p.client.Done():
		return nil, fmt.Errorf("%s exited", def.Name)
	default:
	}
	return p.client, nil
}

// lspCheck syncs path to its server and formats diagnostics with severity
// <= maxSeverity (1 errors ... 4 all); ok is false when no server covers the
// file or it did not answer.
func (h *Host) lspCheck(path string, maxSeverity int) (out string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	def, ok := h.lspFor(abs)
	if !ok {
		return "", false
	}
	// NOTE: a cold gopls or rust-analyzer can take longer than this on a
	// big workspace; the edit then returns without diagnostics.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := h.lspClient(ctx, def)
	if err != nil {
		return "", false
	}
	diags, err := c.Touch(ctx, abs, languageIDs[strings.ToLower(filepath.Ext(abs))])
	if err != nil && len(diags) == 0 {
		return "", false
	}
	rel := abs
	if r, err := filepath.Rel(h.Dirs.ProjectDir, abs); err == nil && !strings.HasPrefix(r, "..") {
		rel = r
	}
	var lines []string
	for _, d := range diags {
		if d.Severity > maxSeverity {
			continue
		}
		if len(lines) == 30 {
			lines = append(lines, "...")
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s:%d:%d: %s", lsp.SeverityName(d.Severity), rel, d.Line, d.Col, strings.TrimSpace(d.Message)))
	}
	if len(lines) == 0 {
		return fmt.Sprintf("%s reports no problems in %s", def.Name, rel), true
	}
	return fmt.Sprintf("<diagnostics source=%q>\n%s\n</diagnostics>", def.Name, strings.Join(lines, "\n")), true
}

// lspAfterWrite is the pipeline's AfterWrite hook: errors and warnings for
// the file just changed.
func (h *Host) lspAfterWrite(path string) string {
	if out, ok := h.lspCheck(path, 2); ok {
		return "\n\n" + out
	}
	return ""
}

// lspDiagnostics backs the diagnostics tool.
func (h *Host) lspDiagnostics(path string) (string, error) {
	if out, ok := h.lspCheck(path, 3); ok {
		return out, nil
	}
	if _, ok := h.lspFor(path); !ok {
		return "", fmt.Errorf("no language server for %s files (install one, or add it with zemi.lsp.add)", filepath.Ext(path))
	}
	return "", errors.New("the language server did not answer in time")
}

func (h *Host) lspNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// add(name, { command, args, env, filetypes = { "go" } }): user config only.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name, opts := L.CheckString(1), L.CheckTable(2)
		if h.RT.CurrentOrigin() != lua.OriginUser {
			L.RaiseError("zemi.lsp.add: only user config may add language servers")
			return 0
		}
		def := lspDef{Name: name, Command: rawString(opts, "command"), Env: map[string]string{}}
		if def.Command == "" {
			L.RaiseError("zemi.lsp.add: command is required")
			return 0
		}
		each := func(key string, fn func(k, v glua.LValue)) {
			if t, ok := opts.RawGetString(key).(*glua.LTable); ok {
				t.ForEach(fn)
			}
		}
		each("args", func(_, v glua.LValue) { def.Args = append(def.Args, v.String()) })
		each("env", func(k, v glua.LValue) { def.Env[k.String()] = v.String() })
		each("filetypes", func(_, v glua.LValue) { def.Exts = append(def.Exts, "."+strings.TrimPrefix(v.String(), ".")) })
		if len(def.Exts) == 0 {
			L.RaiseError("zemi.lsp.add: filetypes is required")
			return 0
		}
		h.lsp.mu.Lock()
		h.lsp.user = append(h.lsp.user, def)
		h.lsp.mu.Unlock()
		return 0
	}))
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, row := range h.lspRows() {
			out.Append(lua.FromGo(L, row))
		}
		L.Push(out)
		return 1
	}))
	return tb
}

// lspRows lists started servers: name, status, error.
func (h *Host) lspRows() []map[string]any {
	h.lsp.mu.Lock()
	defer h.lsp.mu.Unlock()
	var out []map[string]any
	for name, p := range h.lsp.running {
		row := map[string]any{"name": name, "command": p.def.Command, "status": "starting", "error": ""}
		select {
		case <-p.ready:
			row["status"] = "running"
			if p.err != nil {
				row["status"], row["error"] = "failed", p.err.Error()
			} else {
				select {
				case <-p.client.Done():
					row["status"] = "exited"
				default:
				}
			}
		default:
		}
		out = append(out, row)
	}
	return out
}

// LSPCommandsStep registers /lsp.
func (h *Host) LSPCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "lsp commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "lsp", NArgs: "*", Desc: "language servers zemi started; restart <name>", Hidden: true,
				GoFn: func(h *Host, args string) error {
					if name, ok := strings.CutPrefix(strings.TrimSpace(args), "restart "); ok {
						name = strings.TrimSpace(name)
						h.lsp.mu.Lock()
						p := h.lsp.running[name]
						delete(h.lsp.running, name)
						h.lsp.mu.Unlock()
						if p == nil {
							return fmt.Errorf("no language server %q", name)
						}
						p.close()
						return nil
					}
					rows := h.lspRows()
					ui := h.hooks()
					if ui == nil || len(rows) == 0 {
						return errors.New("no language server started yet: they start when the agent edits a file (gopls, pyright, rust-analyzer, typescript-language-server, clangd)")
					}
					items := make([]any, 0, len(rows))
					for _, row := range rows {
						items = append(items, map[string]any{
							"text": fmt.Sprintf("%-14s %-9s %s", row["name"], row["status"], truncRunes(fmt.Sprint(row["error"]), 60)),
							"run":  fmt.Sprintf("lsp restart %s", row["name"]),
						})
					}
					ui.OpenOverlay(Overlay{ID: "lsp", Title: "Language servers (enter restarts)", Origin: lua.OriginBuiltin, Items: items})
					return nil
				},
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			return err
		},
	}
}
