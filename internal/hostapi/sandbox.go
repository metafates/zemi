// sandbox.go confines the commands a permission mode settles (DESIGN §9):
// the backend is probed once in NewHost, and shell and job_start runs a
// mode allowed start under sandboxPolicy.

package hostapi

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/tools"
)

type sandboxProbe struct {
	backend sandbox.Backend
	err     error
}

// sandboxLive reports a working backend. It never changes after NewHost
// (tests swap it), so a command settled as sandboxed always finds it.
func (h *Host) sandboxLive() bool {
	s := h.sandbox.Load()
	return s != nil && s.err == nil
}

// secretDirs are home paths a sandboxed command can neither read nor write:
// credentials, tokens and shell histories that code a build runs could print.
var secretDirs = []string{
	".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".config/gh", ".config/gcloud",
	".password-store", ".netrc", ".pypirc", ".cargo/credentials.toml", "Library/Keychains",
	".git-credentials", ".config/git/credentials", ".npmrc", ".yarnrc", ".yarnrc.yml", ".pgpass", ".my.cnf",
	".vault-token", ".terraform.d/credentials.tfrc.json", ".config/hub", ".config/op", ".config/rclone",
	".local/share/keyrings", ".m2/settings.xml", ".gradle/gradle.properties", ".gem/credentials", ".boto", ".oci",
	".config/doctl", ".config/fly", ".bash_history", ".zsh_history", ".local/share/fish/fish_history",
	".psql_history", ".mysql_history", ".python_history", ".node_repl_history",
}

// homes are where secretDirs sit: $HOME and, when HOME was moved (a wrapper,
// direnv, make first-run), the account's home from the user database.
func homes() []string {
	var hs []string
	home, _ := os.UserHomeDir()
	if home != "" {
		hs = append(hs, home)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" && u.HomeDir != home {
		hs = append(hs, u.HomeDir)
	}
	return hs
}

// sandboxPolicy is what a settled command may touch: the scratchpad pad,
// also its $TMPDIR, a Go build cache of its own and, when trusted and not
// for a read-only agent, the project; never .agents, zemi's own dirs or
// the parts of .git that run code (all of it but on seatbelt), and zemi's
// are unreadable too (sessions, logs, keys in init.lua).
// The project is writable only with a backend that keeps those read-only
// inside it, and never when it is $HOME or above, holds one of zemi's dirs
// or sits in one, holds a hidden path on a backend other than seatbelt, or
// its .git or .agents is a symlink: the backend protects the resolved
// target, so the link itself could be replaced.
// NOTE: GOCACHE points at a cache only sandboxed runs use, one per trust
// root: Go fails a build it cannot cache, and the user's cache written from
// here could poison a later unconfined build. Other tools' caches stay
// read-only, so uv (its lock in ~/.cache/uv) and npm (~/.npm) fail, and so
// does a module fetch under network: true; the fix is one Env entry each
// (UV_CACHE_DIR, npm_config_cache) at a dir in the pad, never the user's
// cache. The system temp dir is not writable for the same reason: macOS
// keeps xcrun_db there, which picks the binary /usr/bin/make or git runs.
// Seatbelt allows binding a unix socket under the pad alone, where tmux and a
// dev server put one; Landlock plus seccomp refuses AF_UNIX outright, since a
// pathname socket's connect cannot be told from its bind.
func (h *Host) sandboxPolicy(readOnly bool, pad string) sandbox.Policy {
	return h.policyWalk(readOnly, pad, true)
}

// policyWalk is sandboxPolicy; walk false skips the project walk the
// namespace backend needs, which must not sit between submit and the
// provider request.
func (h *Host) policyWalk(readOnly bool, pad string, walk bool) sandbox.Policy {
	d := h.Dirs
	home, _ := os.UserHomeDir()
	p := sandbox.Policy{Loopback: true}
	var backend sandbox.Backend
	if s := h.sandbox.Load(); s != nil {
		backend = s.backend
	}
	zemiDirs := slices.DeleteFunc([]string{d.ConfigHome, d.DataHome, d.StateHome}, func(z string) bool { return z == "" })
	linked := false
	protect := func(pr string) {
		if slices.Contains(p.Protect, pr) {
			return
		}
		p.Protect = append(p.Protect, pr)
		if fi, err := os.Lstat(pr); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			linked = true
		}
		// a .git file points at the real git dir, which may sit in the project
		if dir := gitdirTarget(pr); dir != "" {
			p.Protect = append(p.Protect, dir)
		}
	}
	for _, root := range []string{d.ProjectDir, d.RepoRoot()} {
		if root != "" {
			protect(filepath.Join(root, ".git"))
			protect(filepath.Join(root, ".agents"))
		}
	}
	p.Protect = append(p.Protect, zemiDirs...)
	for _, z := range zemiDirs {
		// a project inside a zemi dir stays readable, as it is to the read tool
		if d.ProjectDir == "" || !h.inJail(d.ProjectDir, z) {
			p.Hide = append(p.Hide, z)
		}
	}
	for _, home := range homes() {
		for _, s := range secretDirs {
			p.Hide = append(p.Hide, filepath.Join(home, s))
		}
	}
	var hide []string
	p.Read, p.List, hide = localPluginCode(d)
	p.Hide = append(p.Hide, hide...)
	if d.DataHome != "" {
		p.Read = append(p.Read, h.shippedDirs()...)
	}
	nested := slices.ContainsFunc(zemiDirs, func(z string) bool { return h.inJail(z, d.ProjectDir) }) || h.inJail(d.ProjectDir, zemiDirs...) ||
		// Landlock rules only add access: a write root opens every hidden path inside it
		backend.Name != "seatbelt" && slices.ContainsFunc(p.Hide, func(z string) bool { return h.inJail(z, d.ProjectDir) })
	writable := h.ProjectTrusted() && !readOnly && backend.EnforcesProtect && d.ProjectDir != "" && home != "" && !h.inJail(home, d.ProjectDir) && !nested
	if writable && walk && backend.Name != "seatbelt" {
		// seatbelt protects every .git and .agents by name, the namespace
		// variant only the paths it binds
		found, ok := configEntries(d.ProjectDir)
		writable = ok
		for path, config := range found {
			if config {
				protect(path)
			}
		}
	}
	path := filepath.SplitList(os.Getenv("PATH"))
	// NOTE: a relative PATH entry (".", "bin", empty) makes every directory
	// zemi runs git or a server in a PATH dir, so the project stays read-only;
	// resolving zemi's helpers against a PATH without them would lift that.
	relative := slices.ContainsFunc(path, func(e string) bool { return !filepath.IsAbs(e) })
	if writable && !linked && !relative {
		p.Write = append(p.Write, d.ProjectDir)
		// seatbelt keeps what git runs or follows read-only by name, so the
		// rest of .git opens; a .git file's git dir (a linked worktree) stays shut
		if gd := filepath.Join(d.ProjectDir, ".git"); backend.Name == "seatbelt" {
			if fi, err := os.Lstat(gd); err == nil && fi.IsDir() {
				p.Git = gd
			}
		}
		// the one part of .agents a command may write; a real dir, since the
		// backend would open a link's target
		own, err := d.WorktreesIgnore()
		if err != nil {
			h.Log.Warn("cannot write .agents/worktrees/.gitignore", "origin", "sandbox", "error", err)
		}
		if wt := d.WorktreesDir(); own && h.inJail(wt, d.ProjectDir) {
			p.Write = append(p.Write, wt)
		}
	}
	// both made here: the sandbox cannot create them inside the protected
	// state dir. The pad is the temp dir too, so the file tools read what a
	// command left there without asking.
	// NOTE: a run outside any session (Lua at idle) has no pad and so no
	// TMPDIR: its temp writes fail and the hint names no fix
	p.Env = map[string]string{}
	if p.Git != "" {
		// git reports an upstream set though its config write failed, and auto
		// maintenance may prune what a confined command left unreachable
		// NOTE: a GIT_CONFIG_COUNT of the user's own is replaced in confined runs
		p.Env["GIT_CONFIG_COUNT"] = "2"
		p.Env["GIT_CONFIG_KEY_0"], p.Env["GIT_CONFIG_VALUE_0"] = "branch.autoSetupMerge", "false"
		p.Env["GIT_CONFIG_KEY_1"], p.Env["GIT_CONFIG_VALUE_1"] = "maintenance.auto", "false"
	}
	if pad != "" && os.MkdirAll(pad, 0o700) == nil {
		p.Write = append(p.Write, pad)
		p.Env["TMPDIR"], p.Env["TMP"], p.Env["TEMP"] = pad, pad, pad
	}
	if d.StateHome != "" {
		cache := filepath.Join(d.StateHome, "sandbox-cache", trustKey(h.trustRoot()), "go-build")
		if os.MkdirAll(cache, 0o700) == nil {
			p.Write = append(p.Write, cache)
			p.Env["GOCACHE"] = cache
		}
	}
	// zemi runs git, the shell (proc.Shell), language servers and clipboard
	// helpers unconfined by name, so a PATH dir inside a write root (.venv/bin)
	// must not take a binary
	for _, e := range path {
		if filepath.IsAbs(e) && h.inJail(e, p.Write...) {
			p.Protect = append(p.Protect, e)
		}
	}
	return p
}

// sandboxPrompt is the system prompt's Sandbox block, "" for a run without
// shell: what a command runs under without the user's approval, each clause
// read off the live policy, so the text says what the backend enforces, and
// naming only the escapes and tools toolset has. It says auto mode, the one
// that settles commands confined: keyed on the mode, it would cost a cache
// miss per switch.
// NOTE: it reads the policy without the project walk, so on the namespace
// backend a project over sandboxWalkMax entries reads as writable here.
func (h *Host) sandboxPrompt(toolset []fantasy.AgentTool, readOnly bool, pad string) string {
	i := slices.IndexFunc(toolset, func(t fantasy.AgentTool) bool { return t.Info().Name == tools.ToolShell })
	if i < 0 {
		return ""
	}
	_, escapes := toolset[i].Info().Parameters["sandbox"]
	// without a UI an ask is denied, so no clause promises one
	ask, approved := "ask the user first", ", and run unconfined once approved"
	if h.hooks() == nil {
		ask, approved = "need the user's approval, which nobody can give in this run", ""
	}
	switch {
	case !h.sandboxLive():
		return "Sandbox: none on this machine, so commands that change anything " + ask + ".\n"
	case !h.ProjectTrusted():
		return "Sandbox: this project is not trusted, so only commands that just read run without asking (confined); other commands " + ask + approved + ".\n"
	}
	pol, backend, d := h.policyWalk(readOnly, pad, false), h.sandbox.Load().backend, h.Dirs
	var b strings.Builder
	b.WriteString("Sandbox (auto mode): commands you run without the user's approval are confined.\n")
	switch {
	case slices.Contains(pol.Write, d.ProjectDir):
		ro := ".git and .agents are"
		if pol.Git != "" {
			ro = ".agents and .git's config and hooks are"
		}
		fmt.Fprintf(&b, "- They write only %s.\n- %s read-only to them", pol.WriteNames(d.ProjectDir), ro)
		for _, e := range filepath.SplitList(os.Getenv("PATH")) {
			if rel, err := filepath.Rel(d.ProjectDir, e); err == nil && filepath.IsLocal(rel) && slices.Contains(pol.Protect, e) {
				fmt.Fprintf(&b, ", and so is %s, which is on PATH", rel)
			}
		}
		if hasTool(toolset, tools.ToolWrite) {
			b.WriteString(": write .agents files with the write tool")
		}
		if pol.Git != "" {
			b.WriteString(". git commit, checkout and stash run confined; git config, remote, rebase, push and fetch " + ask + ".\n")
		} else {
			b.WriteString("; git commands that change .git " + ask + ".\n")
		}
	case readOnly:
		fmt.Fprintf(&b, "- They write only %s: the project is read-only to this agent.\n", pol.WriteNames(d.ProjectDir))
	default:
		fmt.Fprintf(&b, "- They write only %s: the project is read-only to them.\n", pol.WriteNames(d.ProjectDir))
	}
	var zemi []string
	for _, z := range []string{d.ConfigHome, d.DataHome, d.StateHome} {
		if z != "" && slices.Contains(pol.Hide, z) {
			zemi = append(zemi, perm.TildePath(z))
		}
	}
	b.WriteString("- Hidden from them: ")
	if len(zemi) > 0 {
		fmt.Fprintf(&b, "zemi's own dirs (%s", strings.Join(zemi, ", "))
		if len(pol.Read) > 0 {
			b.WriteString("; its types, builtin skills and local plugins' code aside")
		}
		b.WriteString("), ")
	}
	b.WriteString("credential dirs (~/.ssh, ~/.aws and the like)")
	if backend.HidesSecretFiles {
		b.WriteString(" and secret-shaped files (.env, *.pem, *.key), in $TMPDIR too")
	}
	b.WriteString(".")
	if len(zemi) > 0 && hasTool(toolset, tools.ToolRead) {
		b.WriteString(" Read zemi's files with the read tool.")
	}
	if pol.Env["TMPDIR"] != "" {
		b.WriteString("\n- Other tools' caches are read-only: point one into $TMPDIR (npm_config_cache=$TMPDIR/npm).")
	}
	// seatbelt's localhost is the machine's, the namespace backend's its own
	switch {
	case backend.Name == "seatbelt":
		// its profile denies almost every mach service
		b.WriteString("\n- System services are off: a browser, GUI app, docker or the keychain fails or crashes (Abort trap: 6). ")
		if escapes && !readOnly {
			b.WriteString("Run one with sandbox: false.")
		} else {
			b.WriteString("Do not run one: say what you could not check.")
		}
		b.WriteString("\n- Network: localhost only.")
		if pol.Env["TMPDIR"] != "" {
			b.WriteString(" A unix socket you bind in $TMPDIR works (tmux); dialing one outside it, the ssh agent's or docker's, does not.")
		}
	case backend.EnforcesProtect:
		b.WriteString("\n- Network: the sandbox's own localhost only, so servers already running here are unreachable.")
	default:
		b.WriteString("\n- Network: none.")
	}
	switch {
	case !escapes:
		b.WriteString(" Nobody can approve more in this run.\n")
	case readOnly:
		b.WriteString(" network: true adds the network (needs approval).\n")
	default:
		b.WriteString(" network: true adds the network, sandbox: false drops every limit; both need approval, so try network: true first.\n")
	}
	return b.String()
}

// localPluginCode is what of the hidden data dir a sandboxed command may
// read, so zemi plugins test runs confined: each local plugin's code
// (plugin.Code), and the names in the plugin dirs and the disabled list
// the runner resolves a name with; hide is what below the code the runner
// does not copy (plugin.Kept). A plugin's .git and the rest stay hidden.
// NOTE: on Landlock, which hides no secret file by name, a key.pem in the
// code is readable, and a List dir lists the names of every dir below it.
func localPluginCode(d config.Dirs) (read, list, hide []string) {
	root := d.PluginsRoot()
	local := filepath.Join(root, "local")
	entries, err := os.ReadDir(local)
	if err != nil {
		return nil, nil, nil
	}
	read = []string{filepath.Join(root, "disabled")}
	list = []string{local, filepath.Join(root, "installed")}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(local, e.Name())
		list = append(list, dir)
		// a link reads as its target, which a rule on its own path never opens
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if !plugin.Code(f.Name(), f.IsDir()) {
				continue
			}
			code := filepath.Join(dir, f.Name())
			read = append(read, code)
			_ = filepath.WalkDir(code, func(p string, e fs.DirEntry, err error) error {
				if err != nil || plugin.Kept(e.Name()) {
					return nil
				}
				hide = append(hide, p)
				if e.IsDir() {
					return filepath.SkipDir
				}
				return nil
			})
		}
	}
	return read, list, hide
}

// trustKey names the trust root in cache paths. Caches are kept one per
// trust root: Go trusts its cache entries, so a shared one lets one
// project's build plant what another's links.
func trustKey(root string) string {
	key := sha256.Sum256([]byte(root))
	return hex.EncodeToString(key[:8])
}

// serverPolicy confines a language server, which runs project code
// (build.rs, proc macros) the model may have written: it reads what a
// sandboxed command reads and writes only <cache>/lsp/<trust key>/<name>,
// which the cache, build, temp and lua-language-server's log and meta
// variables point at. A zemi tree's server (Root) also reads the tree's
// editor configs.
// NOTE: nothing sweeps it; a rust target dir there can be large.
func (h *Host) serverPolicy(def lspDef) sandbox.Policy {
	p := h.sandboxPolicy(true, "")
	p.Write, p.Env = nil, map[string]string{}
	switch def.Root {
	case "":
	case h.Dirs.ConfigHome:
		// the read tool reads it too; its .git may hold tokens
		p.Read = append(p.Read, def.Root)
		p.Hide = append(p.Hide, filepath.Join(def.Root, ".git"))
	default: // a local plugin, whose code sandboxPolicy already reads
		p.Read = append(p.Read, filepath.Join(def.Root, ".luarc.json"), filepath.Join(def.Root, ".emmyrc.json"))
	}
	if h.Dirs.CacheHome == "" {
		return p
	}
	name := def.Name
	if !filepath.IsLocal(name) {
		name = hex.EncodeToString([]byte(name))
	}
	dir := filepath.Join(h.Dirs.CacheHome, "lsp", trustKey(h.trustRoot()), name)
	for k, sub := range map[string]string{
		"XDG_CACHE_HOME": "cache", "GOCACHE": "go-build", "GOPLSCACHE": "gopls",
		"CARGO_TARGET_DIR": "target", "TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp",
		"LLS_LOG_PATH": "log", "LLS_META_PATH": "meta",
	} {
		if os.MkdirAll(filepath.Join(dir, sub), 0o700) == nil {
			p.Env[k] = filepath.Join(dir, sub)
		}
	}
	p.Write = []string{dir}
	return p
}

// gitdirTarget is the git dir a .git file at dotgit names, or "".
func gitdirTarget(dotgit string) string {
	b, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	if dir = strings.TrimSpace(dir); !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(dotgit), dir)
	}
	return dir
}

// sandboxGuard is one running sandboxed command's protected paths: missing
// when it started, and created since. walk is the writable project on a
// backend that protects only the paths it binds, and before its
// configEntries when the command started.
type sandboxGuard struct {
	missing, created []string
	walk             string
	before           map[string]bool
}

// guardSandbox is called before a command starts under p. The namespace
// backend makes a protected path read-only only if it exists, so a command
// could create .agents/init.lua or a .git with hooks, at the project root or
// in any directory below it. The returned func runs once the command exited:
// if a missing path or a new .git or .agents appeared, trust is held off
// (holdTrust) and the text for the tool result says so.
// NOTE: anything else creating the path while the command runs (the user's
// git init) counts too; seatbelt's rules cover missing paths, so it is skipped.
// Nested entries are checked at exit only: this process loads no .agents below
// the project, and Host.git runs no program a nested .git names.
func (h *Host) guardSandbox(p sandbox.Policy) func() string {
	g := &sandboxGuard{}
	for _, pr := range p.Protect {
		writable := slices.ContainsFunc(p.Write, func(w string) bool {
			return pr == w || strings.HasPrefix(pr, strings.TrimSuffix(w, string(filepath.Separator))+string(filepath.Separator))
		})
		if _, err := os.Lstat(pr); err != nil && writable && !slices.Contains(g.missing, pr) {
			g.missing = append(g.missing, pr)
		}
	}
	s := h.sandbox.Load()
	if s == nil || s.backend.Name == "seatbelt" {
		return func() string { return "" }
	}
	if h.Dirs.ProjectDir != "" && slices.Contains(p.Write, h.Dirs.ProjectDir) {
		g.walk = h.Dirs.ProjectDir
		g.before, _ = configEntries(g.walk)
	}
	if len(g.missing) == 0 && g.walk == "" {
		return func() string { return "" }
	}
	h.guardMu.Lock()
	if h.guards == nil {
		h.guards = map[*sandboxGuard]bool{}
	}
	h.guards[g] = true
	h.guardMu.Unlock()
	return func() string {
		var nested []string
		complete := true
		if g.walk != "" {
			var after map[string]bool
			after, complete = configEntries(g.walk)
			for path := range after {
				if !g.before[path] {
					nested = append(nested, path)
				}
			}
			slices.Sort(nested)
		}
		h.guardMu.Lock()
		delete(h.guards, g)
		created := append(g.appeared(), nested...)
		g.created = append(g.created, nested...)
		all := slices.Clone(g.created)
		h.guardMu.Unlock()
		msg := ""
		switch {
		case !complete:
			msg = fmt.Sprintf("a sandboxed command ran in a project of over %d entries, too many to check for a created .git or .agents; project trust is off until /trust", sandboxWalkMax)
		case len(created) > 0:
			msg = createdMessage(created)
		}
		h.holdTrust(msg)
		if len(all) == 0 && complete {
			return ""
		}
		return "\nzemi: " + cmp.Or(msg, createdMessage(all))
	}
}

// sandboxWalkMax bounds configEntries; a var so tests can lower it.
// NOTE: a writable project with more entries (node_modules) stays read-only
// to sandboxed commands on backends other than seatbelt; skipping what git
// ignores would raise the ceiling.
var sandboxWalkMax = 100_000

// configEntries lists the .git and .agents entries under root (true), which
// it does not descend into (.agents/worktrees aside), and the directories it cannot read (false),
// where a command could hide one. ok is false past sandboxWalkMax entries.
func configEntries(root string) (found map[string]bool, ok bool) {
	found, n := map[string]bool{}, 0
	tooMany := errors.New("too many entries")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if n++; n > sandboxWalkMax {
			return tooMany
		}
		switch {
		case err != nil:
			if path == root {
				return err
			}
			found[path] = false
		case path != root && (strings.EqualFold(d.Name(), ".git") || strings.EqualFold(d.Name(), ".agents")):
			found[path] = true
			// .agents/worktrees is writable, so the walk goes on below it only
			if d.IsDir() && !strings.EqualFold(d.Name(), ".agents") {
				return filepath.SkipDir
			}
		case d.IsDir() && strings.EqualFold(filepath.Base(filepath.Dir(path)), ".agents") && d.Name() != "worktrees":
			return filepath.SkipDir
		}
		return nil
	})
	return found, err == nil
}

// CheckSandboxed holds trust off when a sandboxed command still running
// created a protected path; call it before reading project config.
func (h *Host) CheckSandboxed() {
	h.guardMu.Lock()
	var created []string
	for g := range h.guards {
		created = append(created, g.appeared()...)
	}
	h.guardMu.Unlock()
	if len(created) > 0 {
		h.holdTrust(createdMessage(created))
	}
}

// appeared moves the missing paths that exist now to created and returns them.
func (g *sandboxGuard) appeared() []string {
	var now []string
	g.missing = slices.DeleteFunc(g.missing, func(p string) bool {
		_, err := os.Lstat(p)
		if err == nil {
			now = append(now, p)
		}
		return err == nil
	})
	g.created = append(g.created, now...)
	return now
}

func createdMessage(created []string) string {
	return "a sandboxed command created " + strings.Join(created, ", ") + "; project trust is off until /trust"
}

// holdTrust turns project trust off for this process, without saving, until
// /trust (ReleaseTrust), so config a sandboxed command created never loads.
// An empty why (the warning) holds nothing.
func (h *Host) holdTrust(why string) {
	if why == "" {
		return
	}
	h.trustHeld.Store(true)
	h.SetProjectTrusted(false)
	h.Log.Warn(why, "origin", "sandbox")
	h.Notify(slog.LevelWarn, "Trust is off", why)
}

// TrustHeld reports trust held off by holdTrust.
func (h *Host) TrustHeld() bool { return h.trustHeld.Load() }

// ReleaseTrust ends holdTrust: the user answered /trust.
func (h *Host) ReleaseTrust() { h.trustHeld.Store(false) }

// TempDir is where zemi keeps its own temp files (the ctrl+g draft, a
// pasted image), out of the temp dir every process of the user shares.
func (h *Host) TempDir() (string, error) {
	if h.Dirs.StateHome == "" {
		return "", errors.New("no state dir for temp files")
	}
	dir := filepath.Join(h.Dirs.StateHome, "tmp")
	return dir, os.MkdirAll(dir, 0o700)
}
