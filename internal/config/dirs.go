// Package config resolves filesystem locations and holds runtime settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dirs holds the resolved XDG locations zemi uses. Every directory is
// absolute and may be overridden for tests.
type Dirs struct {
	ConfigHome string // ~/.config/zemi
	DataHome   string // ~/.local/share/zemi
	StateHome  string // ~/.local/state/zemi
	ProjectDir string // current project root (cwd)
}

// xdg resolves one root: zemi's own override first, then the XDG variable
// the default mirrors, then the default under the home directory.
func xdg(env, xdgEnv, defaultPath string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	if v := os.Getenv(xdgEnv); v != "" {
		return filepath.Join(v, "zemi")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return defaultPath
	}
	return strings.Replace(defaultPath, "~", home, 1)
}

// ResolveDirs computes the standard locations from the environment.
func ResolveDirs() Dirs {
	return Dirs{
		ConfigHome: xdg("ZEMI_CONFIG_HOME", "XDG_CONFIG_HOME", "~/.config/zemi"),
		DataHome:   xdg("ZEMI_DATA_HOME", "XDG_DATA_HOME", "~/.local/share/zemi"),
		StateHome:  xdg("ZEMI_STATE_HOME", "XDG_STATE_HOME", "~/.local/state/zemi"),
		ProjectDir: projectRoot(),
	}
}

func projectRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// Ensure creates the directories zemi writes into, and the starter
// init.lua when there is none: zemi ships no providers, so an empty
// config cannot answer a single prompt.
func (d Dirs) Ensure() error {
	for _, dir := range []string{
		d.ConfigHome,
		filepath.Join(d.ConfigHome, "lua"),
		filepath.Join(d.DataHome, "site", "pack"),
		filepath.Join(d.StateHome, "logs"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	init := filepath.Join(d.ConfigHome, "init.lua")
	if _, err := os.Stat(init); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(init, []byte(starterInit), 0o600); err != nil {
			return fmt.Errorf("create %s: %w", init, err)
		}
	}
	return nil
}

// starterInit is the file a first run leaves behind: everything is
// commented, so zemi registers nothing the user did not ask for, and the
// provider block only needs uncommenting.
const starterInit = `-- zemi config. Lua, reloaded whenever you save this file.

-- A provider and a model: zemi ships none, so uncomment this and zemi
-- works. The key is read from your environment; paste it here instead if
-- you keep it elsewhere.
-- zemi.provider.register({
--   id = "anthropic",
--   kind = zemi.provider.kind.ANTHROPIC,   -- or OPENAI, OPENAI_COMPAT
--   api_key = zemi.env.get("ANTHROPIC_API_KEY"),
--   models = {
--     { id = "claude-sonnet-4-5", display = "Sonnet 4.5", context_window = 200000,
--       cost_per_1m_in = 3, cost_per_1m_out = 15 },   -- cost is optional
--   },
-- })
-- zemi.config.set("agent.model", "anthropic/claude-sonnet-4-5")

-- Settings: /config list shows every one with its default.
-- zemi.config.set("ui.theme", "default")

-- Keys: <leader> is keymap.leader, ctrl+g unless you change it.
-- zemi.keymap.set("<leader>r", "review", { desc = "review the diff" })

-- Editing this file: zemi ships type annotations for the whole zemi.* API
-- and a .luarc.json beside this file, so a lua-language-server gives
-- completion, signatures and warnings here the way it does for vim.*.

-- /help lists the keys and commands, /health says what is missing.
`

// ProjectConfigDir is <project>/.zemi.
func (d Dirs) ProjectConfigDir() string {
	if d.ProjectDir == "" {
		return ""
	}
	// Started in a subdirectory, zemi still finds the repository's .zemi:
	// search up to the directory holding .git. Outside a repository only
	// ProjectDir counts, so a stray .zemi in a parent is never picked up.
	root := d.RepoRoot()
	for dir := d.ProjectDir; ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(filepath.Join(dir, ".zemi")); err == nil && fi.IsDir() {
			return filepath.Join(dir, ".zemi")
		}
		if dir == root || filepath.Dir(dir) == dir {
			break
		}
	}
	return filepath.Join(d.ProjectDir, ".zemi")
}

// RepoRoot is the nearest directory at or above ProjectDir holding .git,
// or ProjectDir itself outside a repository.
func (d Dirs) RepoRoot() string {
	for dir := d.ProjectDir; dir != ""; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return d.ProjectDir
}

// PluginsDir is ~/.local/share/zemi/site/pack.
func (d Dirs) PluginsDir() string {
	return filepath.Join(d.DataHome, "site", "pack")
}

// LogDir is ~/.local/state/zemi/logs.
func (d Dirs) LogDir() string {
	return filepath.Join(d.StateHome, "logs")
}

// TrustFile stores the project-config trust hashes.
func (d Dirs) TrustFile() string {
	return filepath.Join(d.StateHome, "project_trust.json")
}
