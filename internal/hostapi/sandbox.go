// sandbox.go confines the commands a permission mode settles (DESIGN §9):
// the backend is probed once in NewHost, and bash, job_start and window
// runs a mode allowed start under sandboxPolicy.
package hostapi

import (
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

// sandboxPolicy is what a settled command may touch: the scratchpad, the
// temp dir, a Go build cache of its own and, when trusted and not in plan
// mode, the project; never .git, .zemi or zemi's own dirs, and zemi's dirs
// are unreadable too (sessions, logs, keys in init.lua). The project is
// writable only with a backend that keeps those read-only inside it, and
// never when it is $HOME or above, holds one of zemi's dirs or sits in one,
// or its .git or .zemi is a symlink: the backend protects the resolved
// target, so the link itself could be replaced.
// NOTE: GOCACHE points at a cache only sandboxed runs use: Go fails a build
// it cannot cache, and the user's cache written from here could poison a
// later unconfined build. Other tools' caches stay read-only.
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
	p.Write = append(p.Write, os.TempDir())
	if d.StateHome != "" {
		cache := filepath.Join(d.StateHome, "sandbox-cache", "go-build")
		// made here: the sandbox cannot create it inside the protected state dir
		if os.MkdirAll(cache, 0o700) == nil {
			p.Write = append(p.Write, cache)
			p.Env = map[string]string{"GOCACHE": cache}
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

// TempDir is where zemi keeps its own temp files (the ctrl+g draft, a
// pasted image): the system temp dir is a sandbox write root.
func (h *Host) TempDir() (string, error) {
	if h.Dirs.StateHome == "" {
		return "", errors.New("no state dir for temp files")
	}
	dir := filepath.Join(h.Dirs.StateHome, "tmp")
	return dir, os.MkdirAll(dir, 0o700)
}
