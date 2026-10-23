package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/proc"
)

// TestGoldenPlugins pins /plugin: the list sectioned by source with a failed
// local plugin and an installed one behind its latest tag, an installed
// plugin's detail, and the list rebuilt by a reload that fixed the failure.
func TestGoldenPlugins(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	app, host := bootAgentApp(t, 80, 24)
	write := func(path, text string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// the paths the detail shows start with ~ on every machine
	t.Setenv("HOME", filepath.Dir(host.Dirs.DataHome))
	src, gitconfig := t.TempDir(), filepath.Join(t.TempDir(), "gitconfig")
	write(gitconfig, "[user]\nname = t\nemail = t@t\n")
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	todo := filepath.Join(host.Dirs.PluginsRoot(), "local", "todo-panel", plugin.InitFile)
	write(todo, "-- a todo list in the statusline\nerror('no todos table')")
	write(filepath.Join(src, plugin.InitFile), "-- branch and changes in the statusline\nzemi.command.add('gs', function() end, {})\nzemi.keymap.set('ctrl+x g', 'gs')\nzemi.keymap.set('ctrl+x j', function() zemi.job.start({ cmd = 'git', args = { 'status' } }) end)")
	write(filepath.Join(src, plugin.ManifestFile), `{"uses": ["job"]}`)
	gitIn := func(dir string, args ...string) {
		if out, err := (proc.Git{Dir: dir, Foreign: true}).Run(t.Context(), args...); err != nil {
			t.Fatal(err, out)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "x"}, {"tag", "v1.4.0"}, {"commit", "-qm", "y", "--allow-empty"}, {"tag", "v1.5.0"}} {
		gitIn(src, args...)
	}
	gitIn(src, "clone", "-q", "--bare", ".", "../x.git")
	// under the fake HOME, so the row's source reads ~/mkt on every machine
	mkt := filepath.Join(filepath.Dir(host.Dirs.DataHome), "mkt")
	write(filepath.Join(mkt, plugin.ZemiMarketplaceFile), fmt.Sprintf(`{"name": "examples", "plugins": [
		{"name": "git-status", "source": {"source": "git", "url": "file://%s"}, "description": "the git branch in the statusline"}]}`,
		filepath.Join(filepath.Dir(src), "x.git")))
	gitIn(mkt, "init", "-q")
	gitIn(mkt, "add", ".")
	gitIn(mkt, "commit", "-qm", "m")
	runPlugin := func(args ...string) {
		if err := plugin.Run(t.Context(), host.Dirs, args, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	runPlugin("marketplace", "add", mkt)
	runPlugin("install", "git-status@examples")
	clone := filepath.Join(host.Dirs.PluginsRoot(), "installed", "git-status")
	gitIn(clone, "checkout", "-q", "refs/tags/v1.4.0")
	gitIn(clone, "remote", "set-url", "origin", "https://github.com/someone/zemi-git-status")

	// the row's added date is the wall clock at install: pin it, or the
	// golden fails the day after it was made
	reg := filepath.Join(host.Dirs.PluginsRoot(), "marketplaces.json")
	b, err := os.ReadFile(reg)
	if err != nil {
		t.Fatal(err)
	}
	b = regexp.MustCompile(`"added":"[^"]*"`).ReplaceAll(b, []byte(`"added":"2026-01-02T00:00:00Z"`))
	if err := os.WriteFile(reg, b, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	// soft, as main.go runs it: a failed plugin fails no boot
	step := host.PluginsStep()
	run := step.Run
	step.Run = func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error { run(ctx, r, g); return nil }
	reloadUI(t, host, step)
	frame := func(name string) {
		t.Helper()
		for app.Pump() {
		}
		app.renderSync(t)
		t.Run(name, func(t *testing.T) { golden.RequireEqual(t, app.Frame()) })
	}
	if err := host.RunCommand(ctx, "plugin", ""); err != nil {
		t.Fatal(err)
	}
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 })
	frame("list")

	for range 4 {
		app.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // -> git-status
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	frame("detail")

	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	write(todo, "-- a todo list in the statusline\n")
	reloadUI(t, host, step)
	app.SignalBoot(nil)
	frame("reloaded")
}
