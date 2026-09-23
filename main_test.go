package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sandbox"
)

// On Linux the sandbox probe and Wrap re-exec this binary as the helper.
func TestMain(m *testing.M) {
	sandbox.DispatchHelper()
	os.Exit(m.Run())
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
			write := func(name, body string) {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(real, name)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(real, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("init.lua", "-- one")
			write(c.edit, "-- one")
			if err := os.MkdirAll(filepath.Dir(filepath.Join(cfg, c.link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(real, c.link), filepath.Join(cfg, c.link)); err != nil {
				t.Fatal(err)
			}
			roots := append([]string{cfg}, config.Dirs{ConfigHome: cfg}.UserTrees()...)
			base := configFingerprint(roots...)
			write(c.edit, "-- two!")
			if got := configFingerprint(roots...) != base; got != c.noticed {
				t.Fatalf("edit to %s noticed = %v, want %v", c.edit, got, c.noticed)
			}
		})
	}
}

// TestMainCommandArgs: each command declares the arguments it reads, so a
// stray one errors instead of being dropped (the host has no session store).
func TestMainCommandArgs(t *testing.T) {
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := hostapi.NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(mainCommands(func() {}, func() {})...)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, args, want string }{
		{"new", "idea: add caching", "/new takes no arguments"},
		{"resume", "some-id", "session store unavailable"},
	} {
		if got := fmt.Sprint(h.RunCommand(context.Background(), c.name, c.args)); got != c.want {
			t.Errorf("/%s %s = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}
