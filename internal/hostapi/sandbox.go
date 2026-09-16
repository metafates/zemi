// sandbox.go confines the commands a permission mode settles (DESIGN §9):
// the backend is probed once at startup, and bash, job_start and window
// runs a mode allowed start under sandboxPolicy.
package hostapi

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/metafates/zemi/internal/sandbox"
)

type sandboxProbe struct {
	backend sandbox.Backend
	err     error
}

// probeSandbox checks the backend off the boot path. A value a test stored
// first wins.
func (h *Host) probeSandbox() {
	h.GoSafe("sandbox probe", func() {
		b, err := sandbox.Probe()
		h.sandbox.CompareAndSwap(nil, &sandboxProbe{b, err})
	})
}

// sandboxLive reports a working backend; there is none until the probe
// finishes. Once live it stays live, so a command settled as sandboxed
// always finds the backend when it starts.
func (h *Host) sandboxLive() bool {
	s := h.sandbox.Load()
	return s != nil && s.err == nil
}

// secretDirs are home dirs a sandboxed command can neither read nor write.
var secretDirs = []string{".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".config/gh", ".config/gcloud",
	".password-store", ".netrc", ".pypirc", ".cargo/credentials.toml", "Library/Keychains"}

// sandboxPolicy is what a settled command may touch: the project (trusted,
// not plan mode, and not $HOME or above it), the scratchpad and the temp
// dir; never .git, .zemi or zemi's own dirs.
// NOTE: caches are not writable, so Go loses new build and test cache
// entries; a writable cache could be poisoned for a later unconfined build.
func (h *Host) sandboxPolicy(plan bool) sandbox.Policy {
	d := h.Dirs
	home, _ := os.UserHomeDir()
	p := sandbox.Policy{Loopback: true}
	if h.ProjectTrusted() && !plan && d.ProjectDir != "" && home != "" && !h.inJail(home, d.ProjectDir) {
		p.Write = append(p.Write, d.ProjectDir)
	}
	if s := h.ScratchDir(); s != "" {
		p.Write = append(p.Write, s)
	}
	p.Write = append(p.Write, os.TempDir())
	for _, root := range []string{d.ProjectDir, d.RepoRoot()} {
		if root == "" {
			continue
		}
		git := filepath.Join(root, ".git")
		p.Protect = append(p.Protect, git, filepath.Join(root, ".zemi"))
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
	for _, dir := range []string{d.ConfigHome, d.DataHome, d.StateHome} {
		if dir != "" {
			p.Protect = append(p.Protect, dir)
		}
	}
	if home != "" {
		for _, s := range secretDirs {
			p.Hide = append(p.Hide, filepath.Join(home, s))
		}
	}
	return p
}
