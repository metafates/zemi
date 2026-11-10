package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/metafates/zemi/internal/config"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestLockInstance: one zemi opens a project - a second taker on the same
// dirs is refused naming the holder's pid, and a released lock is takeable.
func TestLockInstance(t *testing.T) {
	dirs := config.Dirs{StateHome: t.TempDir()}
	unlock, err := lockInstance(dirs)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockInstance(dirs)
	if err == nil || !strings.Contains(err.Error(), "already open") ||
		!strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
		t.Fatalf("second lock err = %v, want a refusal naming pid %d", err, os.Getpid())
	}
	unlock()
	if unlock, err = lockInstance(dirs); err != nil {
		t.Fatal(err)
	}
	unlock()
}

// TestConfigFingerprint: an edit to a config tree (here the project's
// .agents), a local plugin, a toggle and an install or update (a clone's git
// index) reload; git data, dependency trees, the .agents worktrees, a
// clone's files and one still in flight are not walked.
func TestConfigFingerprint(t *testing.T) {
	for _, c := range []struct {
		edit    string
		noticed bool
	}{
		{"agents/init.lua", true},
		{"agents/.git/index", false},
		{"agents/worktrees/w/init.lua", false},
		{"local/x/init.lua", true},
		{"disabled", true},
		{"installed/y/.git/index", true},
		{"installed/z/.git/index", true},
		{"installed/y/init.lua", false},
		{"installed/.z.tmp/.git/index", false},
		{"marketplaces.json", true},
		{"marketplaces/m/.zemi/marketplace.json", true},
		{"marketplaces/m/.claude-plugin/marketplace.json", true},
		{"marketplaces/m/plugins/sub/one.md", false},
		{"marketplaces/.add-tmp/.zemi/marketplace.json", false},
		{"local/x/node_modules/a.js", false},
		{"local/x/.luarc.json", false},
		{"local/x/.emmyrc.json", false},
		{"local/worktrees/init.lua", true}, // only the project's worktrees are skipped
	} {
		root := t.TempDir()
		for _, f := range []string{"agents/init.lua", "local/x/init.lua", "installed/y/init.lua", "installed/y/.git/index"} {
			writeFile(t, filepath.Join(root, f), "one")
		}
		// the roots are read again each time, as setReload does
		fingerprint := func() string {
			return configFingerprint(filepath.Join(root, "agents", "worktrees"), append(pluginRoots(root), filepath.Join(root, "agents"))...)
		}
		base := fingerprint()
		writeFile(t, filepath.Join(root, c.edit), "two!")
		if got := fingerprint() != base; got != c.noticed {
			t.Errorf("edit to %s noticed = %v, want %v", c.edit, got, c.noticed)
		}
	}
}
