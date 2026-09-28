// sandbox.go confines the commands a permission mode settles (DESIGN §9):
// the backend is probed once in NewHost, and bash and job_start runs a
// mode allowed start under sandboxPolicy.
package hostapi

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/plugins"
	"github.com/metafates/zemi/internal/sandbox"
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

// sandboxPolicy is what a settled command may touch: the scratchpad pad,
// also its $TMPDIR, a Go build cache of its own and, when trusted and not
// in plan mode, the project; never .git, .agents or zemi's own dirs, and zemi's dirs
// are unreadable too (sessions, logs, keys in init.lua). The project is
// writable only with a backend that keeps those read-only inside it, and
// never when it is $HOME or above, holds one of zemi's dirs or sits in one,
// holds a hidden path on a backend other than seatbelt, or its .git or .agents
// is a symlink: the backend protects the resolved target, so the link itself
// could be replaced.
// NOTE: GOCACHE points at a cache only sandboxed runs use, one per trust
// root: Go fails a build it cannot cache, and the user's cache written from
// here could poison a later unconfined build. Other tools' caches stay
// read-only, so uv (its lock in ~/.cache/uv) and npm (~/.npm) fail, and so
// does a module fetch under network: true; the fix is one Env entry each
// (UV_CACHE_DIR, npm_config_cache) at a dir in the pad, never the user's
// cache. The system temp dir is not writable for the same reason: macOS
// keeps xcrun_db there, which picks the binary /usr/bin/make or git runs.
// Seatbelt denies binding a unix socket; the fix is a seatbelt rule allowing
// bind under the pad.
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
	if home != "" {
		for _, s := range secretDirs {
			p.Hide = append(p.Hide, filepath.Join(home, s))
		}
	}
	var hide []string
	p.Read, p.List, hide = localPluginCode(d)
	p.Hide = append(p.Hide, hide...)
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
		// the one part of .agents a command may write; a real dir, since the
		// backend would open a link's target
		own, err := d.WorktreesIgnore()
		if err != nil {
			h.Log.Warn("sandbox", "cannot write .agents/worktrees/.gitignore", map[string]any{"error": err.Error()})
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
	if pad != "" && os.MkdirAll(pad, 0o700) == nil {
		p.Write, p.Pad = append(p.Write, pad), pad
		p.Env["TMPDIR"], p.Env["TMP"], p.Env["TEMP"] = pad, pad, pad
	}
	if d.StateHome != "" {
		cache := filepath.Join(d.StateHome, "sandbox-cache", h.trustKey(), "go-build")
		if os.MkdirAll(cache, 0o700) == nil {
			p.Write = append(p.Write, cache)
			p.Env["GOCACHE"] = cache
		}
	}
	// zemi runs git, sh, language servers and clipboard helpers unconfined by
	// name, so a PATH dir inside a write root (.venv/bin) must not take a binary
	for _, e := range path {
		if filepath.IsAbs(e) && h.inJail(e, p.Write...) {
			p.Protect = append(p.Protect, e)
		}
	}
	return p
}

// sandboxPrompt is the environment's Sandbox block: what a command runs
// under without the user's approval, each clause read off the live policy,
// so the text says what the backend enforces. escapes is whether bash
// carries the network and sandbox parameters. It says auto mode, the one
// that settles commands confined: keyed on the mode, it would cost a cache
// miss per switch.
// NOTE: it reads the policy without the project walk, so on the namespace
// backend a project over sandboxWalkMax entries reads as writable here.
func (h *Host) sandboxPrompt(readOnly, escapes bool, pad string) string {
	switch {
	case !h.sandboxLive():
		return "Sandbox: none on this machine, so commands that change anything ask the user first.\n"
	case !h.ProjectTrusted():
		return "Sandbox: this project is not trusted, so only commands that just read run without asking (confined); any other command asks the user and runs unconfined once approved.\n"
	}
	pol, backend, d := h.policyWalk(readOnly, pad, false), h.sandbox.Load().backend, h.Dirs
	var b strings.Builder
	b.WriteString("Sandbox (auto mode): commands you run without the user's approval are confined.\n")
	switch {
	case slices.Contains(pol.Write, d.ProjectDir):
		fmt.Fprintf(&b, "- They write only %s.\n- .git and .agents are read-only to them", pol.WriteNames(d.ProjectDir))
		for _, e := range filepath.SplitList(os.Getenv("PATH")) {
			if rel, err := filepath.Rel(d.ProjectDir, e); err == nil && filepath.IsLocal(rel) && slices.Contains(pol.Protect, e) {
				fmt.Fprintf(&b, ", and so is %s, which is on PATH", rel)
			}
		}
		b.WriteString(": write .agents files with the write tool; git commands that change .git ask first.\n")
	case readOnly:
		fmt.Fprintf(&b, "- They write only %s: the project is read-only while planning.\n", pol.WriteNames(d.ProjectDir))
	default:
		fmt.Fprintf(&b, "- They write only %s: the project is read-only to them.\n", pol.WriteNames(d.ProjectDir))
	}
	var zemi []string
	for _, z := range []string{d.ConfigHome, d.DataHome, d.StateHome} {
		if z != "" && slices.Contains(pol.Hide, z) {
			zemi = append(zemi, TildePath(z))
		}
	}
	b.WriteString("- Hidden from them: ")
	if len(zemi) > 0 {
		fmt.Fprintf(&b, "zemi's own dirs (%s", strings.Join(zemi, ", "))
		if len(pol.Read) > 0 {
			b.WriteString("; a local plugin's code aside")
		}
		b.WriteString("), ")
	}
	b.WriteString("credential dirs (~/.ssh, ~/.aws and the like)")
	if backend.HidesSecretFiles {
		b.WriteString(" and secret-shaped files (.env, *.pem, *.key) outside $TMPDIR")
	}
	b.WriteString(".")
	if len(zemi) > 0 {
		b.WriteString(" Read zemi's files with the read tool.")
	}
	if pol.Env["TMPDIR"] != "" {
		b.WriteString("\n- Other tools' caches are read-only: point one into $TMPDIR (npm_config_cache=$TMPDIR/npm).")
	}
	// seatbelt's localhost is the machine's, the namespace backend's its own
	switch {
	case backend.Name == "seatbelt":
		b.WriteString("\n- Network: localhost only.")
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
// (plugins.Code), and the names in the plugin dirs and the disabled list
// the runner resolves a name with; hide is what below the code the runner
// does not copy (plugins.Kept). A plugin's .git and the rest stay hidden.
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
			if !plugins.Code(f.Name(), f.IsDir()) {
				continue
			}
			code := filepath.Join(dir, f.Name())
			read = append(read, code)
			_ = filepath.WalkDir(code, func(p string, e fs.DirEntry, err error) error {
				if err != nil || plugins.Kept(e.Name()) {
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
func (h *Host) trustKey() string {
	key := sha256.Sum256([]byte(h.trustRoot()))
	return hex.EncodeToString(key[:8])
}

// serverPolicy confines a language server, which runs project code
// (build.rs, proc macros) the model may have written: it reads what a
// sandboxed command reads and writes only <cache>/lsp/<trust key>/<name>,
// which the cache, build and temp variables point at.
// NOTE: nothing sweeps it; a rust target dir there can be large.
func (h *Host) serverPolicy(name string) sandbox.Policy {
	p := h.sandboxPolicy(true, "")
	p.Write, p.Env = nil, map[string]string{}
	if h.Dirs.CacheHome == "" {
		return p
	}
	if !filepath.IsLocal(name) {
		name = hex.EncodeToString([]byte(name))
	}
	dir := filepath.Join(h.Dirs.CacheHome, "lsp", h.trustKey(), name)
	for k, sub := range map[string]string{
		"XDG_CACHE_HOME": "cache", "GOCACHE": "go-build", "GOPLSCACHE": "gopls",
		"CARGO_TARGET_DIR": "target", "TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp",
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
	h.Log.Warn("sandbox", why, nil)
	h.Notify(logging.Warn, "Trust is off", why)
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
