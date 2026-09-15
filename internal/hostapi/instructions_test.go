package hostapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// TestInstructionFiles: the user's AGENTS.md comes first, then each
// directory from the repository root down to the working directory; a
// directory without ZEMI.md or AGENTS.md falls back to CLAUDE.md, and
// nothing above the repository root is read.
func TestInstructionFiles(t *testing.T) {
	cfg, top := t.TempDir(), t.TempDir()
	repo := filepath.Join(top, "repo")
	sub := filepath.Join(repo, "svc", "api")
	for path, body := range map[string]string{
		filepath.Join(cfg, "AGENTS.md"):         "global rules",
		filepath.Join(top, "AGENTS.md"):         "outside the repo",
		filepath.Join(repo, "AGENTS.md"):        "root agents",
		filepath.Join(repo, "CLAUDE.md"):        "root claude, shadowed",
		filepath.Join(repo, "svc", "CLAUDE.md"): "svc claude fallback",
		filepath.Join(sub, "ZEMI.md"):           "api zemi",
		filepath.Join(sub, "AGENTS.md"):         "api agents",
		filepath.Join(repo, ".git", "HEAD"):     "ref: main",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := logging.New(t.TempDir())
	h := NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{ConfigHome: cfg, ProjectDir: sub})

	sys := h.systemWith("base", nil)
	for _, want := range []string{"global rules", "root agents", "svc claude fallback", "api zemi", "api agents"} {
		i := strings.Index(sys, want)
		if i < 0 {
			t.Fatalf("system prompt misses %q:\n%s", want, sys)
		}
		sys = sys[i:]
	}
	full := h.systemWith("base", nil)
	for _, absent := range []string{"outside the repo", "root claude, shadowed"} {
		if strings.Contains(full, absent) {
			t.Fatalf("system prompt must not contain %q", absent)
		}
	}
	if !strings.Contains(full, "## Instructions: svc/CLAUDE.md") {
		t.Fatalf("sections should name files relative to the repo root:\n%s", full)
	}
}
