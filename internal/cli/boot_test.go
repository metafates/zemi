package cli

import (
	"fmt"
	"io"
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

// TestHeadlessModelFlag: a headless boot (zemi health's) applies the run's
// -m, so health checks the model the flag named; an id no provider names
// fails the run.
func TestHeadlessModelFlag(t *testing.T) {
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("PWD", base)
	writeFile(t, filepath.Join(home, "config", "init.lua"), `zemi.provider.add({ id = "gw", kind = "openai_compat", base_url = "http://x", models = { { id = "m1" } } })`)
	dirs, err := config.ResolveDirs()
	if err != nil {
		t.Fatal(err)
	}
	h, done, err := bootHeadless(options{model: "gw/m1", dirs: dirs}, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Settings.Str("models.default"); got != "gw/m1" {
		t.Fatalf("models.default = %q, want the flag's model", got)
	}
	done()
	// an id no provider names fails the run; an unlisted one only warns
	// (TestHeadlessNotices)
	if _, _, err := bootHeadless(options{model: "compact", dirs: dirs}, false, io.Discard); err == nil {
		t.Fatal("a headless boot took a model no provider names")
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
		{"quit", "now", "/quit takes no arguments"},
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

// TestHealthRowActionsRunCommands: a /health row's action runs a command by
// name, and the rows live apart from the commands they name, so a rename that
// misses them fails here rather than as a dead key in the overlay. The mcp
// row's action appears only with servers, which this harness starts none of.
func TestHealthRowActionsRunCommands(t *testing.T) {
	h := commandHost(t)
	h.RecordError("command", "probe", "planted so the errors row carries its action")
	for _, c := range h.HealthRows() {
		if c.Run == "" {
			continue
		}
		name, _ := hostapi.SplitCommand(c.Run)
		if h.Commands.Lookup(name) == nil {
			t.Errorf("health row %q runs %q, which is no command", c.Name, c.Run)
		}
	}
}
