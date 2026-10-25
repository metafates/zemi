package cli

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	zemiruntime "github.com/metafates/zemi/runtime"
)

func TestMain(m *testing.M) {
	m.Run()
}

// TestConfigFingerprintFollowsSymlinks: a dotfiles setup links the config
// home, a user tree or a file; an edit behind the link still counts. A dir
// linked deeper is not walked, since it could be any size.
func TestConfigFingerprintFollowsSymlinks(t *testing.T) {
	for _, c := range []struct {
		name, link, edit string
		noticed          bool
	}{
		{"symlinked root", "", "init.lua", true},
		{"symlinked user tree", "lua", "lua/x.lua", true},
		{"symlinked file", "init.lua", "init.lua", true},
		{"dir linked below a tree", "lua/vendor", "lua/vendor/x.lua", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			real, cfg := t.TempDir(), filepath.Join(t.TempDir(), "zemi")
			writeFile(t, filepath.Join(real, "init.lua"), "-- one")
			writeFile(t, filepath.Join(real, c.edit), "-- one")
			if err := os.MkdirAll(filepath.Dir(filepath.Join(cfg, c.link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(real, c.link), filepath.Join(cfg, c.link)); err != nil {
				t.Fatal(err)
			}
			roots := append([]string{cfg}, config.Dirs{ConfigHome: cfg}.UserTrees()...)
			base := configFingerprint("", roots...)
			writeFile(t, filepath.Join(real, c.edit), "-- two!")
			if got := configFingerprint("", roots...) != base; got != c.noticed {
				t.Fatalf("edit to %s noticed = %v, want %v", c.edit, got, c.noticed)
			}
		})
	}
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

// commandHost is a host with the builtin and main commands and no session
// store.
func commandHost(t *testing.T) *hostapi.Host {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := hostapi.NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(mainCommands(func() {}, func() {})...)}); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestMainCommandArgs: each command declares the arguments it reads, so a
// stray one errors instead of being dropped.
func TestMainCommandArgs(t *testing.T) {
	h := commandHost(t)
	for _, c := range []struct{ name, args, want string }{
		{"new", "idea: add caching", "/new takes no arguments"},
		{"resume", "some-id", "session store unavailable"},
	} {
		if got := fmt.Sprint(h.RunCommand(t.Context(), c.name, c.args, false)); got != c.want {
			t.Errorf("/%s %s = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

// TestSkillsCoverBuiltinCommands: every Go-owned slash command appears in
// the builtin skill (internal/hostapi covers the rest of the surface).
func TestSkillsCoverBuiltinCommands(t *testing.T) {
	var text strings.Builder
	_ = fs.WalkDir(zemiruntime.Skills, "skills", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := fs.ReadFile(zemiruntime.Skills, path)
			text.Write(data)
		}
		return err
	})
	for _, c := range commandHost(t).Commands.List() {
		if !strings.Contains(text.String(), "/"+c.Name) {
			t.Errorf("no builtin skill mentions /%s", c.Name)
		}
	}
}
