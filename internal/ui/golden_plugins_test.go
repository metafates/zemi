package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/proc"
)

// TestGoldenPlugins pins /plugins: the list sectioned by source with a failed
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
	todo := filepath.Join(host.Dirs.PluginsRoot(), "local", "todo-panel", "init.lua")
	write(todo, "-- a todo list in the statusline\nerror('no todos table')")
	write(filepath.Join(src, "init.lua"), "-- branch and changes in the statusline\nzemi.command.add('gs', function() end, {})\nzemi.keymap.set('ctrl+x g', 'gs')")
	gitIn := func(dir string, args ...string) {
		if out, err := (proc.Git{Dir: dir, Foreign: true}).Run(t.Context(), args...); err != nil {
			t.Fatal(err, out)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "x"}, {"tag", "v1.4.0"}, {"commit", "-qm", "y", "--allow-empty"}, {"tag", "v1.5.0"}} {
		gitIn(src, args...)
	}
	if _, err := plugin.Install(t.Context(), host.Dirs, src, "git-status"); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(host.Dirs.PluginsRoot(), "installed", "git-status")
	gitIn(clone, "checkout", "-q", "refs/tags/v1.4.0")
	gitIn(clone, "remote", "set-url", "origin", "https://github.com/someone/zemi-git-status")

	ctx := t.Context()
	// soft, as main.go runs it: a failed plugin fails no boot
	step := host.PluginsStep()
	run := step.Run
	step.Run = func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error { run(ctx, r, g); return nil }
	steps := []lua.BootStep{host.NamespacesStep(), host.BuiltinCommandsStep(), lua.RequireStep("zemi.tui"), step}
	if err := host.ForceReload(ctx, steps); err != nil {
		t.Fatal(err)
	}
	frame := func(name string) {
		t.Helper()
		for app.Pump() {
		}
		app.renderSync(t)
		t.Run(name, func(t *testing.T) { golden.RequireEqual(t, app.Frame()) })
	}
	if err := host.RunCommand(ctx, "plugins", ""); err != nil {
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
	if err := host.ForceReload(ctx, steps); err != nil {
		t.Fatal(err)
	}
	app.SignalBoot(nil)
	frame("reloaded")
}
