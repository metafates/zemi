// sandbox.go confines the commands a permission mode settles (DESIGN §9):
// the backend is probed once in NewHost, and bash, job_start and window
// runs a mode allowed start under sandboxPolicy.
package hostapi

import (
	"cmp"
	"errors"
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
// mode, the project; never .git, .zemi or zemi's own dirs, and zemi's dirs
// are unreadable too (sessions, logs, keys in init.lua). The project is
// writable only with a backend that keeps those read-only inside it, and
// never when it is $HOME or above, holds one of zemi's dirs or sits in one,
// or its .git or .zemi is a symlink: the backend protects the resolved
// target, so the link itself could be replaced.
// NOTE: GOCACHE points at a cache only sandboxed runs use: Go fails a build
// it cannot cache, and the user's cache written from here could poison a
// later unconfined build. Other tools' caches stay read-only. The system
// temp dir is not writable for the same reason: macOS keeps xcrun_db there,
// which picks the binary /usr/bin/make or git runs.
func (h *Host) sandboxPolicy(plan bool) sandbox.Policy {
	d := h.Dirs
	home, _ := os.UserHomeDir()
	p := sandbox.Policy{Loopback: true}
	var backend sandbox.Backend
	if s := h.sandbox.Load(); s != nil {
		backend = s.backend
	}
	zemiDirs := slices.DeleteFunc([]string{d.ConfigHome, d.DataHome, d.StateHome}, func(z string) bool { return z == "" })
	linked := false
	for _, root := range []string{d.ProjectDir, d.RepoRoot()} {
		if root == "" {
			continue
		}
		git := filepath.Join(root, ".git")
		for _, pr := range []string{git, filepath.Join(root, ".zemi")} {
			p.Protect = append(p.Protect, pr)
			if fi, err := os.Lstat(pr); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				linked = true
			}
		}
		// a .git file points at the real git dir, which may sit in the project
		if b, err := os.ReadFile(git); err == nil {
			if dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:"); ok {
				dir = strings.TrimSpace(dir)
				if !filepath.IsAbs(dir) {
					dir = filepath.Join(root, dir)
				}
				p.Protect = append(p.Protect, dir)
			}
		}
	}
	nested := slices.ContainsFunc(zemiDirs, func(z string) bool { return h.inJail(z, d.ProjectDir) }) || h.inJail(d.ProjectDir, zemiDirs...)
	if h.ProjectTrusted() && !plan && backend.EnforcesProtect && d.ProjectDir != "" && home != "" && !h.inJail(home, d.ProjectDir) && !nested && !linked {
		p.Write = append(p.Write, d.ProjectDir)
	}
	if s := h.ScratchDir(); s != "" {
		p.Write = append(p.Write, s)
	}
	if d.StateHome != "" {
		p.Env = map[string]string{}
		// made here: the sandbox cannot create them inside the protected state dir
		cache := filepath.Join(d.StateHome, "sandbox-cache", "go-build")
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
	return p
}

// sandboxGuard is one running sandboxed command's protected paths: missing
// when it started, and created since.
type sandboxGuard struct{ missing, created []string }

// guardSandbox is called before a command starts under p. The namespace
// backend makes a protected path read-only only if it exists, so a command
// could create .zemi/init.lua or a .git with hooks. The returned func runs
// once the command exited: if a missing path appeared, trust is held off
// (holdTrust) and the text for the tool result says so.
// NOTE: anything else creating the path while the command runs (the user's
// git init) counts too; seatbelt's rules cover missing paths, so it is skipped.
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
	if s := h.sandbox.Load(); s == nil || s.backend.Name == "seatbelt" || len(g.missing) == 0 {
		return func() string { return "" }
	}
	h.guardMu.Lock()
	if h.guards == nil {
		h.guards = map[*sandboxGuard]bool{}
	}
	h.guards[g] = true
	h.guardMu.Unlock()
	return func() string {
		h.guardMu.Lock()
		delete(h.guards, g)
		created := g.appeared()
		all := slices.Clone(g.created)
		h.guardMu.Unlock()
		h.holdTrust(created)
		if len(all) == 0 {
			return ""
		}
		return "\nzemi: " + createdMessage(all)
	}
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
	h.holdTrust(created)
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
func (h *Host) holdTrust(created []string) {
	if len(created) == 0 {
		return
	}
	h.trustHeld.Store(true)
	h.SetProjectTrusted(false)
	h.Log.Warn("sandbox", createdMessage(created), nil)
	h.Notify("warn", "trust", createdMessage(created))
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
