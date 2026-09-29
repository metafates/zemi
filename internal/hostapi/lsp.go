// lsp.go feeds language server diagnostics to the model: after write or
// edit changes a file, its server's errors are appended to the tool result,
// and the diagnostics tool checks any file. Servers start lazily on the
// first matching file: the builtin set when its binary is installed and the
// project has its marker file (lsp.auto), plus servers user config adds with
// zemi.lsp.add, and lua-language-server for zemi's own Lua (zemiLua).

package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lsp"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/tools"
)

type lspDef struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	Exts    []string // ".go"
	Markers []string // files at the repository root that enable a builtin
	Init    any      // initializationOptions
	Config  any      // workspace settings
	// RunsCode marks a builtin that executes project code while it loads a
	// workspace, so it starts only confined
	RunsCode bool
	Root     string // the workspace; "" is the repository root
}

// builtinLSP start in a trusted repository with a marker. Servers run the
// repository's code (rust-analyzer runs build scripts and proc macros, and
// typescript would load node_modules), which is why no server starts in an
// untrusted one and a live backend confines them (serverPolicy); typescript
// still gets the tsserver on PATH.
var builtinLSP = []lspDef{
	{Name: "gopls", Command: "gopls", Exts: []string{".go"}, Markers: []string{"go.mod", "go.work"}},
	{Name: "pyright", Command: "pyright-langserver", Args: []string{"--stdio"}, Exts: []string{".py"}, Markers: []string{"pyproject.toml", "setup.py", "requirements.txt", "pyrightconfig.json"}},
	{Name: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}, Exts: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}, Markers: []string{"package.json", "tsconfig.json"}},
	{Name: "clangd", Command: "clangd", Exts: []string{".c", ".h", ".cc", ".cpp", ".hpp", ".cxx"}, Markers: []string{"compile_commands.json", "CMakeLists.txt", ".clangd"}},
	{Name: "rust-analyzer", Command: "rust-analyzer", Exts: []string{".rs"}, Markers: []string{"Cargo.toml"}, RunsCode: true},
	// It runs a workspace's Lua (Lua.runtime.plugin) only once the client
	// says yes, and zemi's never does (lsp.Client's server requests). With
	// workspace diagnostics on, it would publish a file's before zemi opens
	// it, and it never publishes an unchanged result again (lspCheck).
	{
		Name: "luals", Command: "lua-language-server", Exts: []string{".lua"},
		Markers: []string{".luarc.json", ".luarc.jsonc", ".stylua.toml", "stylua.toml", "selene.toml", ".luacheckrc"},
		Config:  map[string]any{"Lua": map[string]any{"diagnostics": map[string]any{"workspaceDelay": -1}}},
	},
}

// builtinCommands names the programs of the builtin servers, for the text
// that says what to install.
func builtinCommands() string {
	var cmds []string
	for _, d := range builtinLSP {
		cmds = append(cmds, d.Command)
	}
	return strings.Join(cmds[:len(cmds)-1], ", ") + " or " + cmds[len(cmds)-1]
}

var languageIDs = map[string]string{
	".go": "go", ".py": "python", ".rs": "rust", ".lua": "lua", ".ts": "typescript", ".tsx": "typescriptreact",
	".js": "javascript", ".jsx": "javascriptreact", ".mjs": "javascript", ".cjs": "javascript",
	".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp", ".cxx": "cpp",
}

type lspProc struct {
	h      *Host
	def    lspDef
	ready  chan struct{} // closed when the start attempt ends
	client *lsp.Client
	err    error
	stalls int // touches in a row the server answered nothing to
}

// lspMaxStarts caps the restarts of a server that keeps dying or hanging,
// so a crash loop cannot respawn it on every edit; after lspMaxStalls
// silent touches in a row a server that answers no request counts as dead.
const (
	lspMaxStarts = 3
	lspMaxStalls = 3
)

type lspReg struct {
	mu       sync.Mutex
	scanMu   sync.Mutex        // one scan at a time, in order
	scan     map[string]lspSig // last git status listing by path
	scanned  bool              // the baseline exists
	dirty    atomic.Bool       // something other than zemi's file tools ran
	user     []lspDef
	disabled map[string]bool // names zemi.lsp.del dropped
	running  map[string]*lspProc
	attempts map[string]int
}

func newLSPReg() *lspReg {
	return &lspReg{running: map[string]*lspProc{}, attempts: map[string]int{}, disabled: map[string]bool{}}
}

// stopAll stops every server and forgets user definitions (reload replays
// them). The servers stop on goroutines; wait returns once they all have.
func (r *lspReg) stopAll() (wait func()) {
	r.mu.Lock()
	procs := r.running
	r.running, r.user, r.attempts, r.disabled = map[string]*lspProc{}, nil, map[string]int{}, map[string]bool{}
	r.mu.Unlock()
	var stopped []<-chan struct{}
	for _, p := range procs {
		stopped = append(stopped, p.close())
	}
	return func() {
		for _, c := range stopped {
			<-c
		}
	}
}

// close stops the server once its start attempt ends, closing the channel
// it returns after.
func (p *lspProc) close() <-chan struct{} {
	done := make(chan struct{})
	p.h.GoSafe("lsp stop "+p.def.Name, func() {
		defer close(done)
		<-p.ready
		if p.client != nil {
			p.client.Close()
		}
	})
	return done
}

// lspFor picks the server for path: lua-language-server for zemi's own Lua
// (zemiLua), else the first configured one handling its extension
// (lspDefs order). A file outside the repository gets none, since a server
// answers from the workspace it was started in.
func (h *Host) lspFor(path string) (lspDef, bool) {
	path = h.projectAbs(path)
	if d, ok := h.zemiLua(path); ok {
		return d, true
	}
	if !h.inJail(path, h.Dirs.RepoRoot()) {
		return lspDef{}, false
	}
	ext := strings.ToLower(filepath.Ext(path))
	for _, d := range h.lspDefs(path) {
		if slices.Contains(d.Exts, ext) {
			return d, true
		}
	}
	return lspDef{}, false
}

// zemiLua is the lua-language-server for a .lua file of the user's config
// or a local plugin, rooted at that tree whatever the project's trust: it is
// the user's own code, and zemi writes the .luarc.json there, first, so a
// plugin made this turn has zemi's types when its server starts. A user
// zemi.lsp.add("luals", ...) replaces the builtin, and zemi.lsp.del or
// lsp.auto off without one leaves the file without.
// NOTE: a trusted project's .agents Lua is zemi's too, but zemi writes
// nothing into a repository, so it goes to the project's server without
// zemi's types.
func (h *Host) zemiLua(path string) (lspDef, bool) {
	if strings.ToLower(filepath.Ext(path)) != ".lua" {
		return lspDef{}, false
	}
	real := perm.RealPath(path) // as pluginsRoot is
	root, label := h.localPluginDir(real), "plugin "+h.PluginOf(real)
	if root == "" {
		if h.Dirs.ConfigHome == "" || !h.inJail(path, h.Dirs.ConfigHome) {
			return lspDef{}, false
		}
		root, label = h.Dirs.ConfigHome, "config"
	}
	h.lsp.mu.Lock()
	i := slices.IndexFunc(h.lsp.user, func(d lspDef) bool { return d.Name == "luals" })
	def, off := lspDef{}, h.lsp.disabled["luals"]
	if i >= 0 {
		def = h.lsp.user[i]
	}
	h.lsp.mu.Unlock()
	if i < 0 && h.Settings.Bool("lsp.auto") {
		def = builtinLSP[slices.IndexFunc(builtinLSP, func(d lspDef) bool { return d.Name == "luals" })]
		if _, err := exec.LookPath(def.Command); err != nil {
			return lspDef{}, false
		}
	}
	if off || !slices.Contains(def.Exts, ".lua") {
		return lspDef{}, false
	}
	if err := writeLuarc(root, h.TypesDir()); err != nil {
		h.Log.Warn("luarc not written", "origin", "lsp", "dir", root, "error", err)
	}
	def.Name, def.Root = "luals ("+label+")", root
	return def, true
}

// lspDefs lists the servers this project can use: user definitions first,
// then, unless lsp.auto is off, each installed builtin whose marker exists
// in path's directory or above it up to the repository root (a module in a
// subdirectory counts); disabled names are left out of both. path "" means
// the root alone. An untrusted project gets none: a user-added server runs
// the repository's code as much as a builtin does.
func (h *Host) lspDefs(path string) []lspDef {
	if !h.ProjectTrusted() {
		return nil
	}
	h.lsp.mu.Lock()
	defs := slices.Clone(h.lsp.user)
	disabled := maps.Clone(h.lsp.disabled)
	h.lsp.mu.Unlock()
	if h.Settings.Bool("lsp.auto") {
		root := h.Dirs.RepoRoot()
		dirs := []string{root}
		if abs, err := filepath.Abs(path); err == nil && path != "" {
			for dir := filepath.Dir(abs); dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
				dirs = append(dirs, dir)
			}
		}
		for _, d := range builtinLSP {
			if _, err := exec.LookPath(d.Command); err != nil {
				continue
			}
			// NOTE: without a backend every server runs unconfined; a user-added
			// one is the user's call, and of the builtins only rust-analyzer is
			// left out (typescript plugins and clangd drivers are not)
			if d.RunsCode && !h.sandboxLive() {
				continue
			}
			if d.Name == "typescript" {
				// without tsserver.path the server prefers node_modules/typescript
				ts := globalTSServer(root)
				if ts == "" {
					continue
				}
				d.Init = map[string]any{"tsserver": map[string]any{"path": ts}}
			}
			if slices.ContainsFunc(dirs, func(dir string) bool {
				return slices.ContainsFunc(d.Markers, func(m string) bool { _, err := os.Stat(filepath.Join(dir, m)); return err == nil })
			}) {
				defs = append(defs, d)
			}
		}
	}
	return slices.DeleteFunc(defs, func(d lspDef) bool { return disabled[d.Name] })
}

// globalTSServer is the tsserver.js of the typescript whose tsserver is on
// PATH, or "" when there is none or it lives inside root.
func globalTSServer(root string) string {
	bin, err := exec.LookPath("tsserver")
	if err != nil {
		return ""
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	js := filepath.Join(filepath.Dir(filepath.Dir(bin)), "lib", "tsserver.js")
	if _, err := os.Stat(js); err != nil || strings.HasPrefix(js, root+string(filepath.Separator)) {
		return ""
	}
	return js
}

// lspQuietTool lists the tools that change nothing behind a language
// server's back: they either only read, or write through zemi's own file
// tools, which open the document they touched. Anything else ran a program.
var lspQuietTool = map[string]bool{
	"read": true, "write": true, "edit": true,
	"glob": true, "grep": true, "todos": true, "plan": true, "question": true,
	"skill": true, "lsp": true, "diagnostics": true, "job_list": true,
	"memory": true,
}

// lspDirty records that something may have changed files without telling
// the language servers, so the next lspSync asks git.
func (h *Host) lspDirty() { h.lsp.dirty.Store(true) }

// lspSig is what a scan remembers about a file git listed.
type lspSig struct {
	mtime     time.Time
	size      int64
	gone      bool
	untracked bool
}

// lspSync tells every running server which files changed on disk since the
// last call, from the diff of two git status listings (a file that left the
// listing was restored, so it changed too). It runs only once something but
// zemi's file tools, which open the document they touch, ran (lspDirty).
// NOTE: outside a repository only zemi's own writes are seen, so a zemi
// tree's server (zemiLua) hears of a command's change when the file is next
// opened; a repository with a slow git status pays for it on every touch.
func (h *Host) lspSync() {
	root := h.Dirs.RepoRoot()
	if root == "" {
		return
	}
	h.lsp.scanMu.Lock()
	defer h.lsp.scanMu.Unlock()
	if !h.lsp.dirty.Swap(false) && h.lsp.scanned {
		return
	}
	h.lsp.mu.Lock()
	var clients []*lsp.Client
	for _, p := range h.lsp.running {
		select {
		case <-p.ready:
			if p.err == nil {
				clients = append(clients, p.client)
			}
		default:
		}
	}
	h.lsp.mu.Unlock()
	out, err := h.git(root, "-c", "core.quotePath=false", "status", "--porcelain", "-z", "-uall", "--no-renames")
	if err != nil {
		return // not a repository, or git missing: nothing to compare
	}
	now := map[string]lspSig{}
	for entry := range strings.SplitSeq(out, "\x00") {
		if len(entry) < 4 {
			continue
		}
		path := filepath.Join(root, entry[3:])
		sig := lspSig{gone: strings.ContainsRune(entry[:2], 'D'), untracked: entry[:2] == "??"}
		if fi, err := os.Stat(path); err == nil {
			sig.mtime, sig.size = fi.ModTime(), fi.Size()
		} else {
			sig.gone = true
		}
		now[path] = sig
	}
	prev, first := h.lsp.scan, !h.lsp.scanned
	h.lsp.scan, h.lsp.scanned = now, true
	if first || len(clients) == 0 {
		return
	}
	var changes []lsp.FileChange
	for path, sig := range now {
		old, seen := prev[path]
		switch {
		case sig == old:
		case sig.gone:
			if !old.gone {
				changes = append(changes, lsp.FileChange{Path: path, Type: 3})
			}
		case (!seen || old.gone) && sig.untracked:
			changes = append(changes, lsp.FileChange{Path: path, Type: 1})
		default:
			changes = append(changes, lsp.FileChange{Path: path, Type: 2})
		}
	}
	for path, old := range prev {
		if _, still := now[path]; still {
			continue
		}
		// back to the index's content, or created and deleted again
		if _, err := os.Stat(path); err == nil {
			changes = append(changes, lsp.FileChange{Path: path, Type: 2})
		} else if !old.gone {
			changes = append(changes, lsp.FileChange{Path: path, Type: 3})
		}
	}
	for _, c := range clients {
		c.NotifyChanged(changes)
	}
}

// lspClient returns the running server for def, starting it once; a failed
// start is remembered until /lsp restart.
func (h *Host) lspClient(ctx context.Context, def lspDef) (*lspProc, error) {
	h.lsp.mu.Lock()
	p, ok := h.lsp.running[def.Name]
	if !ok {
		if h.lsp.attempts[def.Name] >= lspMaxStarts {
			h.lsp.mu.Unlock()
			return nil, fmt.Errorf("%s failed %d times; /lsp restart %s to try again", def.Name, lspMaxStarts, def.Name)
		}
		h.lsp.attempts[def.Name]++
		p = &lspProc{h: h, def: def, ready: make(chan struct{})}
		h.lsp.running[def.Name] = p
	}
	h.lsp.mu.Unlock()
	if !ok {
		h.GoSafe("lsp start "+def.Name, func() {
			defer func() {
				close(p.ready)
				if ui := h.hooks(); ui != nil {
					ui.OpenOverlay(h.lspOverlay(true))
				}
			}()
			env, _ := h.Perm.ChildEnv(nil)
			for k, v := range def.Env {
				env = append(env, k+"="+v)
			}
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cfg := lsp.Config{
				Command: def.Command, Args: def.Args, Env: env, Root: cmp.Or(def.Root, h.Dirs.RepoRoot()),
				InitOptions: def.Init, Settings: def.Config,
				Stderr: serverLogWriter{h: h, source: "lsp", server: def.Name},
			}
			if h.sandboxLive() {
				policy := h.serverPolicy(def)
				cfg.Wrap = func(cmd *exec.Cmd) error { return sandbox.Wrap(cmd, policy) }
			}
			p.client, p.err = lsp.Start(sctx, cfg)
			if p.err != nil {
				h.Log.Warn("server failed to start", "origin", "lsp", "server", def.Name, "error", p.err)
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
		// The server died: forget it so the next file starts a fresh one,
		// and tell the user, who would otherwise only see diagnostics stop.
		h.lsp.mu.Lock()
		if h.lsp.running[def.Name] == p {
			delete(h.lsp.running, def.Name)
		}
		h.lsp.mu.Unlock()
		err := fmt.Errorf("%s exited", def.Name)
		h.RecordError("lsp", err.Error())
		return nil, err
	default:
	}
	return p, nil
}

// lspAnswered records whether a touch got fresh diagnostics. Answers clear
// the start counter, so three crashes over a long session do not disable
// the server for good. Silence alone proves nothing, a server loading its
// workspace publishes nothing either, so after lspMaxStalls silent touches
// the server is asked directly, and only one that does not reply before ctx
// ends is stopped, for the next file to start a fresh one.
func (h *Host) lspAnswered(ctx context.Context, p *lspProc, fresh bool) {
	h.lsp.mu.Lock()
	if p.stalls++; fresh {
		p.stalls = 0
		delete(h.lsp.attempts, p.def.Name)
	}
	quiet := p.stalls >= lspMaxStalls
	if quiet {
		p.stalls = 0
	}
	h.lsp.mu.Unlock()
	if !quiet || p.client.Alive(ctx) {
		return
	}
	h.lsp.mu.Lock()
	defer h.lsp.mu.Unlock()
	if h.lsp.running[p.def.Name] != p {
		return
	}
	delete(h.lsp.running, p.def.Name)
	h.Log.Warn("server stopped answering, restarting", "origin", "lsp", "server", p.def.Name)
	p.close()
}

// languageID is the LSP language id for a file: the known name, else the
// extension without its dot, so a filetype from zemi.lsp.add still sends
// something rather than an empty string.
func languageID(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if id, ok := languageIDs[ext]; ok {
		return id
	}
	return strings.TrimPrefix(ext, ".")
}

// lspCheck formats a file's diagnostics; ok is false when no server covers
// it or it did not answer. After a write it reports errors only (plus the
// errors the change caused in other files) on a short budget and syncs the
// tree in the background, since a turn must not wait on git for advice; the
// diagnostics tool waits, and reports warnings too.
func (h *Host) lspCheck(path string, afterWrite bool) (out string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	def, ok := h.lspFor(abs)
	if !ok {
		return "", false
	}
	// LSP severities: 1 error, 2 warning, 3 information, 4 hint
	maxSeverity, budget, none := 2, 10*time.Second, "problems"
	if afterWrite {
		maxSeverity, budget, none = 1, 3*time.Second, "errors"
	}
	// NOTE: a cold gopls or tsserver can take longer than this on a
	// big workspace; the edit then returns without diagnostics.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	p, err := h.lspClient(ctx, def)
	if err != nil {
		return "", false
	}
	if afterWrite {
		h.GoSafe("lsp sync", h.lspSync)
	} else {
		h.lspSync()
	}
	h.EmitAsync("lsp.request", map[string]any{"server": def.Name, "path": abs, "kind": "diagnostics"})
	diags, err := p.client.Touch(ctx, abs, languageID(abs))
	if errors.Is(err, lsp.ErrNoFresh) && filepath.Base(def.Command) == "lua-language-server" && p.client.Published(abs) {
		// lua-language-server publishes only a changed result, so its silence repeats the last one.
		// NOTE: a check slower than Touch's floor reads the same, reporting the
		// errors of the text before; a request per check (pull diagnostics) would not.
		diags, err = p.client.Diagnostics(abs), nil
	}
	h.lspAnswered(ctx, p, err == nil)
	if err != nil {
		return "", false
	}
	// servers answer with canonical paths (/tmp -> /private/tmp)
	root := h.Dirs.ProjectDir
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	rel := func(abs string) string {
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			abs = r
		}
		if r, ok := perm.Under(root, abs); ok {
			return r
		}
		return abs
	}
	var lines []string
	add := func(abs string, ds []lsp.Diagnostic, maxSeverity int) {
		for _, d := range ds {
			if d.Severity > maxSeverity || len(lines) > 30 {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s %s:%d:%d: %s", lsp.SeverityName(d.Severity), rel(abs), d.Line, d.Col, strings.TrimSpace(d.Message)))
		}
	}
	add(abs, diags, maxSeverity)
	if afterWrite {
		other := p.client.Others(abs)
		for _, o := range slices.Sorted(maps.Keys(other)) {
			if len(lines) == 0 {
				lines = append(lines, "errors in other files (a caller this change broke, or already broken):")
			}
			add(o, other[o], 1)
		}
	}
	if len(lines) > 30 {
		lines = append(lines[:30], "...")
	}
	if len(lines) == 0 {
		return fmt.Sprintf("%s reports no %s in %s", def.Name, none, rel(abs)), true
	}
	return fmt.Sprintf("<diagnostics source=%q>\n%s\n</diagnostics>", def.Name, strings.Join(lines, "\n")), true
}

// lspOpen backs the lsp tool: the server for path with the file synced.
func (h *Host) lspOpen(ctx context.Context, path string) (tools.LSPSession, error) {
	def, ok := h.lspFor(path)
	if !ok {
		return tools.LSPSession{}, h.noLSP(path)
	}
	p, err := h.lspClient(ctx, def)
	if err != nil {
		return tools.LSPSession{}, err
	}
	h.lspSync()
	h.EmitAsync("lsp.request", map[string]any{"server": def.Name, "path": path, "kind": "tool"})
	open := func(path string) error { return p.client.Open(path, languageID(path)) }
	if err := open(path); err != nil {
		return tools.LSPSession{}, err
	}
	return tools.LSPSession{Client: p.client, Server: def.Name, Open: open}, nil
}

// lspAfterWrite is the pipeline's AfterWrite hook: errors for the file just
// changed. The diagnostics tool is there for a slow server.
func (h *Host) lspAfterWrite(path string) string {
	if out, ok := h.lspCheck(path, true); ok {
		return "\n\n" + out
	}
	return ""
}

// lspDiagnostics backs the diagnostics tool.
func (h *Host) lspDiagnostics(path string) (string, error) {
	if out, ok := h.lspCheck(path, false); ok {
		return out, nil
	}
	if _, ok := h.lspFor(path); !ok {
		return "", h.noLSP(path)
	}
	return "", errors.New("the language server did not answer in time")
}

// noLSP says why path (or, when "", the project) has no language server.
func (h *Host) noLSP(path string) error {
	switch {
	case !h.ProjectTrusted():
		return errors.New("the project's language servers are off: it is not trusted (the user runs /trust)")
	case path == "":
		return fmt.Errorf("no language server for this project: none of %s is installed with its marker file here; zemi.lsp.add adds one", builtinCommands())
	}
	return fmt.Errorf("no language server for %s files (install one, or add it with zemi.lsp.add)", filepath.Ext(path))
}

func (h *Host) lspNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// add(name, { command, args, env, filetypes = { "go" }, settings, init_options }):
	// user config only. A name matching a builtin replaces it.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name, opts := L.CheckString(1), L.CheckTable(2)
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.lsp.add: only user config may add language servers")
		}
		def := lspDef{Name: name, Command: rawString(L, opts, "command"), Args: rawStrings(L, opts, "args"), Env: rawStringMap(L, opts, "env")}
		if def.Command == "" {
			L.RaiseError("zemi.lsp.add: command is required")
		}
		for _, ft := range rawStrings(L, opts, "filetypes") {
			def.Exts = append(def.Exts, "."+strings.TrimPrefix(ft, "."))
		}
		if len(def.Exts) == 0 {
			L.RaiseError("zemi.lsp.add: filetypes is required")
		}
		for key, dst := range map[string]*any{"settings": &def.Config, "init_options": &def.Init} {
			if v := opts.RawGetString(key); v != glua.LNil {
				g, err := lua.ToGo(v)
				if err != nil {
					L.RaiseError("zemi.lsp.add: %s: %v", key, err)
				}
				*dst = g
			}
		}
		h.lsp.mu.Lock()
		// the same name again, or a builtin's name, is a replacement
		h.lsp.user = append(slices.DeleteFunc(h.lsp.user, func(d lspDef) bool { return d.Name == name }), def)
		h.lsp.mu.Unlock()
		return 0
	}))
	// del(name): never start this server (a builtin, or one added above).
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.lsp.del: only user config may drop language servers")
		}
		h.lsp.mu.Lock()
		h.lsp.disabled[name] = true
		h.lsp.mu.Unlock()
		return 0
	}))
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, r := range h.lspRows() {
			out.Append(lua.FromGo(L, map[string]any{"name": r.Name, "command": r.Command, "filetypes": r.Filetypes, "status": string(r.Status), "error": r.Error}))
		}
		L.Push(out)
		return 1
	}))
	return tb
}

type lspRow struct {
	Name, Command, Filetypes, Error string
	Status                          ServiceStatus
}

// lspRows lists the servers this project can use with their state: running
// ones first, then those that would start on their first file (idle).
func (h *Host) lspRows() []lspRow {
	defs := h.lspDefs("")
	h.lsp.mu.Lock()
	defer h.lsp.mu.Unlock()
	var out []lspRow
	for _, d := range defs {
		if _, running := h.lsp.running[d.Name]; !running {
			out = append(out, lspRow{Name: d.Name, Command: d.Command, Filetypes: strings.Join(d.Exts, " "), Status: StatusIdle})
		}
	}
	for name, p := range h.lsp.running {
		row := lspRow{Name: name, Command: p.def.Command, Filetypes: strings.Join(p.def.Exts, " "), Status: StatusStarting}
		select {
		case <-p.ready:
			row.Status = StatusReady
			if p.err != nil {
				row.Status, row.Error = StatusFailed, p.err.Error()
			} else {
				select {
				case <-p.client.Done():
					row.Status = StatusStopped
				default:
				}
			}
		default:
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b lspRow) int {
		avail := func(r lspRow) int {
			if r.Status == StatusIdle {
				return 1
			}
			return 0
		}
		return cmp.Or(cmp.Compare(avail(a), avail(b)), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// lspCommands is /lsp.
func (h *Host) lspCommands() []*Command {
	return []*Command{{
		Name: "lsp", NArgs: NArgsAny, Desc: "manage language servers", Hidden: true,
		GoFn: func(h *Host, args string) error {
			if name, ok := strings.CutPrefix(strings.TrimSpace(args), "restart "); ok {
				name = strings.TrimSpace(name)
				h.lsp.mu.Lock()
				p, tries := h.lsp.running[name], h.lsp.attempts[name]
				delete(h.lsp.running, name)
				delete(h.lsp.attempts, name)
				h.lsp.mu.Unlock()
				if p == nil && tries == 0 {
					return Usagef("no language server %q", name)
				}
				if p != nil { // else it capped out earlier; the counter is clear now
					p.close()
				}
				h.Notify(slog.LevelInfo, "Language server stopped", name+"; it starts again on the next file it serves")
				return nil
			}
			ui, err := h.Screen()
			if err != nil {
				return err
			}
			ui.OpenOverlay(h.lspOverlay(false))
			return nil
		},
	}}
}

// lspOverlay is /lsp; with refresh it only replaces an open one.
func (h *Host) lspOverlay(refresh bool) Overlay {
	empty := "No language servers yet. Install " + builtinCommands() + ", or add one with zemi.lsp.add."
	if !h.ProjectTrusted() {
		empty = "The project's language servers are off: it is not trusted (/trust). zemi's own Lua, in your config and local plugins, still gets lua-language-server."
	}
	return Overlay{
		ID: "lsp", Title: "Language servers", Origin: lua.OriginBuiltin, Refresh: refresh, Items: h.lspItems(),
		Reload: func() ([]any, [][]Segment) { return h.lspItems(), nil }, Empty: empty,
	}
}

func (h *Host) lspItems() []any {
	var items []any
	for _, row := range h.lspRows() {
		files := row.Filetypes
		if row.Status == StatusIdle {
			files += " (starts on the first one)"
		}
		item := map[string]any{
			"text": row.Name, "desc": string(row.Status) + " · " + row.Filetypes,
			"fields": fieldList("command", row.Command, "status", string(row.Status), "files", files, "error", row.Error),
		}
		if row.Status != StatusIdle {
			item["actions"] = []any{map[string]any{"text": "Restart", "hotkey": "r", "run": "lsp restart " + row.Name}}
		}
		items = append(items, item)
	}
	return items
}
