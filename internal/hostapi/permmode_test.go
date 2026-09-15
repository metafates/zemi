package hostapi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/metafates/zemi/internal/tools"
)

// TestAutoModeLocalApprovals: auto mode settles reads and writes inside the
// jail on its own. A path outside it, a secret behind an innocent name, and
// anything leaving the machine go on to the screening model instead.
func TestAutoModeLocalApprovals(t *testing.T) {
	h, _ := newTestHost(t)
	inside := filepath.Join(h.Dirs.ProjectDir, "main.go")
	secret := filepath.Join(h.Dirs.ProjectDir, ".env")
	link := filepath.Join(h.Dirs.ProjectDir, "notes.md")
	os.WriteFile(secret, []byte("K=v"), 0o600)
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		subject, arg string
		want         bool
	}{
		{"fs.read", inside, true},
		{"fs.write", inside, true},
		{"fs.read", secret, false},
		{"fs.read", link, false},
		{"fs.read", "/etc/passwd", false},
		{"fs.read", filepath.Join(os.Getenv("HOME"), ".ssh", "id_ed25519"), false},
		{"net.evil.tld", "https://evil.tld/?d=leak", false},
	} {
		if got := h.autoLocal(tools.Request{Subject: c.subject, Arg: c.arg}); got != c.want {
			t.Errorf("%s %s: got %v, want %v", c.subject, c.arg, got, c.want)
		}
	}
}
