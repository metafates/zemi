package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

func writePermissions(t *testing.T, h *Host, content string) {
	t.Helper()
	dir := filepath.Join(h.Dirs.ProjectDir, ".zemi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "permissions"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectPermissions(t *testing.T) {
	h, _ := newTestHost(t)
	writePermissions(t, h, `
git push
make all

# a wrapper program is refused: no rule from a command the user did not see
python x.py
`)
	h.SetProjectTrusted(true)
	if err := h.ProjectPermissionsStep().Run(context.Background(), h.RT, &lua.Generation{ID: 1}); err != nil {
		t.Fatalf("step: %v", err)
	}
	want := []struct {
		cmd  string
		allow bool
	}{
		{"git push origin main", true},
		{"make all -j4", true},
		{"git status", false},
		{"python x.py", false},
	}
	for _, c := range want {
		if got := h.Perm.CheckCommand(c.cmd, "").Decision == perm.Allow; got != c.allow {
			t.Errorf("CheckCommand(%q) allow=%v, want %v", c.cmd, got, c.allow)
		}
	}
	ignore, err := os.ReadFile(filepath.Join(filepath.Dir(h.Dirs.WorktreesDir()), ".gitignore"))
	if err != nil {
		t.Fatalf("reading .zemi/.gitignore: %v", err)
	}
	if !strings.Contains(string(ignore), "permissions") {
		t.Errorf(".zemi/.gitignore missing permissions entry:\n%s", ignore)
	}
}

func TestProjectPermissionsUntrusted(t *testing.T) {
	h, _ := newTestHost(t)
	writePermissions(t, h, "git push\n")
	if err := h.ProjectPermissionsStep().Run(context.Background(), h.RT, &lua.Generation{ID: 1}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if h.Perm.CheckCommand("git push", "").Decision == perm.Allow {
		t.Error("untrusted project must not load .zemi/permissions")
	}
}
