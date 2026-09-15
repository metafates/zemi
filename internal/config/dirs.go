// Package config resolves filesystem locations and holds runtime settings.
package config

import (
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

func xdg(env, defaultPath string) string {
	if v := os.Getenv(env); v != "" {
		return v
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
		ConfigHome: xdg("ZEMI_CONFIG_HOME", "~/.config/zemi"),
		DataHome:   xdg("ZEMI_DATA_HOME", "~/.local/share/zemi"),
		StateHome:  xdg("ZEMI_STATE_HOME", "~/.local/state/zemi"),
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

// Ensure creates the directories zemi writes into.
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
	return nil
}

// ProjectConfigDir is <project>/.zemi.
func (d Dirs) ProjectConfigDir() string {
	if d.ProjectDir == "" {
		return ""
	}
	return filepath.Join(d.ProjectDir, ".zemi")
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
