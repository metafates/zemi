package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/exp/golden"
)

// TestGoldenSkillsList pins /skills: a toggle list sectioned by source, the
// user's own skills first.
func TestGoldenSkillsList(t *testing.T) {
	app, host := bootAgentAppPinned(t, 80, 24)
	dir := filepath.Join(host.Dirs.ConfigHome, "skills", "deploy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: deploy\ndescription: ship a release to staging\n---\n\nSteps.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	reloadUI(t, host, host.SkillsUserHooksStep())
	if err := host.RunCommand(ctx, "skills", "", false); err != nil {
		t.Fatal(err)
	}
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 })
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
}
