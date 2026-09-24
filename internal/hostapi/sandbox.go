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
var secretDirs = []string{".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".config/gh", ".config/gcloud",
	".password-store", ".netrc", ".pypirc", ".cargo/credentials.toml", "Library/Keychains",
	".git-credentials", ".config/git/credentials", ".npmrc", ".yarnrc", ".yarnrc.yml", ".pgpass", ".my.cnf",
	".vault-token", ".terraform.d/credentials.tfrc.json", ".config/hub", ".config/op", ".config/rclone",
	".local/share/keyrings", ".m2/settings.xml", ".gradle/gradle.properties", ".gem/credentials", ".boto", ".oci",
	".config/doctl", ".config/fly", ".bash_history", ".zsh_history", ".local/share/fish/fish_history",
	".psql_history", ".mysql_history", ".python_history", ".node_repl_history"}

// sandboxPolicy is what a settled command may touch: the scratchpad, a
// temp dir and a Go build cache of its own and, when trusted and not in plan
// mode, the project; never .git, .agents or zemi's own dirs, and zemi's dirs
// are unreadable too (sessions, logs, keys in init.lua). The project is
// writable only with a backend that keeps those read-only inside it, and
// never when it is $HOME or above, holds one of zemi's dirs or sits in one,
// holds a hidden path on a backend other than seatbelt, or its .git or .agents
// is a symlink: the backend protects the resolved target, so the link itself
// could be replaced.
// NOTE: GOCACHE points at a cache only sandboxed runs use: Go fails a build
// it cannot cache, and the user's cache written from here could poison a
// later unconfined build. Other tools' caches stay read-only. The system
// temp dir is not writable for the same reason: macOS keeps xcrun_db there,
// which picks the binary /usr/bin/make or git runs.
func (h *Host) sandboxPolicy(readOnly bool) sandbox.Policy {
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
	nested := slices.ContainsFunc(zemiDirs, func(z string) bool { return h.inJail(z, d.ProjectDir) }) || h.inJail(d.ProjectDir, zemiDirs...) ||
		// Landlock rules only add access: a write root opens every hidden path inside it
		backend.Name != "seatbelt" && slices.ContainsFunc(p.Hide, func(z string) bool { return h.inJail(z, d.ProjectDir) })
	writable := h.ProjectTrusted() && !readOnly && backend.EnforcesProtect && d.ProjectDir != "" && home != "" && !h.inJail(home, d.ProjectDir) && !nested
	if writable && backend.Name != "seatbelt" {
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
	if s := h.ScratchDir(); s != "" {
		p.Write = append(p.Write, s)
	}
	if d.StateHome != "" {
		p.Env = map[string]string{}
		cache := filepath.Join(d.StateHome, "sandbox-cache", h.trustKey(), "go-build")
		// made here: the sandbox cannot create them inside the protected state dir
		if os.MkdirAll(cache, 0o700) == nil {
			p.Write = append(p.Write, cache)
			p.Env["GOCACHE"] = cache
		}
		// per session, swept with the scratchpads
		tmp := filepath.Join(d.StateHome, "sandbox-tmp", cmp.Or(h.AgentSessionID(), "no-session"))
		if os.MkdirAll(tmp, 0o700) == nil {
			p.Write = append(p.Write, tmp)
			p.Env["TMPDIR"], p.Env["TMP"], p.Env["TEMP"] = tmp, tmp, tmp
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
	p := h.sandboxPolicy(true)
	p.Write, p.Env = nil, map[string]string{}
	if h.Dirs.CacheHome == "" {
		return p
	}
	if !filepath.IsLocal(name) {
		name = hex.EncodeToString([]byte(name))
	}
	dir := filepath.Join(h.Dirs.CacheHome, "lsp", h.trustKey(), name)
	for k, sub := range map[string]string{"XDG_CACHE_HOME": "cache", "GOCACHE": "go-build", "GOPLSCACHE": "gopls",
		"CARGO_TARGET_DIR": "target", "TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp"} {
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
	h.Notify("warn", "Trust is off", why)
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
