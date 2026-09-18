// Package config resolves filesystem locations and holds runtime settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/metafates/zemi/internal/perm"
)

// Dirs holds the resolved XDG locations zemi uses. Every directory is
// absolute and may be overridden for tests.
type Dirs struct {
	ConfigHome string // ~/.config/zemi
	DataHome   string // ~/.local/share/zemi
	StateHome  string // ~/.local/state/zemi
	CacheHome  string // ~/.cache/zemi
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
		CacheHome:  xdg("ZEMI_CACHE_HOME", "XDG_CACHE_HOME", "~/.cache/zemi"),
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
		d.PluginsDir(),
		d.LogDir(),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	init := filepath.Join(d.ConfigHome, "init.lua")
	if _, err := os.Stat(init); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(init, []byte(StarterInit), 0o600); err != nil {
			return fmt.Errorf("create %s: %w", init, err)
		}
	}
	return nil
}

// StarterInit is the file a first run leaves behind: everything is
// commented, so zemi registers nothing the user did not ask for, and the
// provider block only needs uncommenting.
const StarterInit = `-- zemi config. Lua, reloaded whenever you save this file.
-- Nothing here is on: uncomment what you want. What you register takes
-- add/del/list, what holds a value takes set/get.

-- A provider and a model: zemi ships none, so uncomment this and zemi
-- works. The key is read from your environment; paste it here instead if
-- you keep it elsewhere.
-- zemi.provider.add({
--   id = "anthropic",
--   kind = zemi.provider.kind.ANTHROPIC,   -- or OPENAI, OPENAI_COMPAT
--   api_key = zemi.env.get("ANTHROPIC_API_KEY"),
--   base_url = "http://localhost:11434/v1",   -- required for OPENAI_COMPAT, an endpoint override for the rest
--   headers = { ["HTTP-Referer"] = "zemi" },   -- extra headers a gateway needs; as secret as the key
--   models = {
--     { id = "claude-sonnet-4-5", display = "Sonnet 4.5", context_window = 200000,
--       cost_per_1m_in = 3, cost_per_1m_out = 15 },   -- cost is optional; images = false for a text-only model
--   },
-- })
-- zemi.config.set("agent.model", "anthropic/claude-sonnet-4-5")
-- zemi.config.set("agent.subagent_model", "anthropic/claude-haiku-4-5") -- subagents default to a cheaper model

-- Settings: zemi.config.schema() lists every one with its default.
-- zemi.config.set("ui.theme", "default")

-- Keys: <leader> is keymap.leader, ctrl+g unless you change it. The
-- right-hand side is a command name ("review", "/review"), a host action
-- (zemi.HostAction in the type annotations lists them) or a function.
-- zemi.keymap.set("<leader>r", "review", { desc = "review the diff" })
-- zemi.keymap.set("<leader>m", "zemi.model.pick", { desc = "switch model" })

-- Your own slash command, completed in the palette like the builtin ones.
-- zemi.command.add("tests", function(a)
--   zemi.agent.send("run the tests" .. (a.args ~= "" and " for " .. a.args or ""))
-- end, { desc = "ask the agent to run the tests", nargs = "?" })

-- Everything on screen is a widget: an id, a region, and a render that
-- returns lines of { text, hl } segments. Reuse a builtin id to replace it.
-- zemi.ui.widget.add({
--   id = "msgs", region = "statusline.right", priority = 5,
--   render = function(ctx) return { { { text = ctx.transcript_count .. " msgs", hl = "Comment" } } } end,
-- })

-- Permissions: deny and ask rules come from any config, allow rules only
-- from this file. Every rule carries an argument it must catch, checked
-- here, so a pattern that matches nothing you meant fails at startup.
-- zemi.permission.rule.add({
--   subject = "tool.bash", pattern = "git push *", decision = "ask",
--   match = "git push origin main",
-- })

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
// or ProjectDir itself outside a repository. The walk stops below $HOME: a
// dotfiles repository there is not the project of every directory in home.
func (d Dirs) RepoRoot() string {
	home, _ := os.UserHomeDir()
	// canonical: a case variant of home is home on a folding volume
	canonHome := perm.CanonPath(home)
	for dir := d.ProjectDir; dir != ""; dir = filepath.Dir(dir) {
		if dir != d.ProjectDir && home != "" && perm.CanonPath(dir) == canonHome {
			break
		}
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

// TrustFile maps trust roots to "trusted" or "untrusted".
func (d Dirs) TrustFile() string {
	return filepath.Join(d.StateHome, "trust.json")
}

// TrustRoot is the directory a trust decision belongs to: the repository
// root with symlinks resolved, and for a linked worktree its main checkout,
// so every worktree of a repository shares one decision.
// NOTE: another case spelling of the path on a case-insensitive volume is
// another key, which asks again.
func (d Dirs) TrustRoot() string {
	root := d.RepoRoot()
	if root == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if main := mainCheckout(root); main != "" {
		return main
	}
	return root
}

// mainCheckout is the main checkout of the linked worktree at root, or "".
// It reads the files git keeps and never runs git, which would honor the
// repository's config: .git names <main>/.git/worktrees/<id>, whose gitdir
// file must point back at root. That back-pointer lives in the other
// repository, so a forged .git file cannot claim it.
func mainCheckout(root string) string {
	// small reads one short path file, "" when it is not a small regular file
	small := func(p string) string {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<10 {
			return ""
		}
		b, _ := os.ReadFile(p)
		return strings.TrimSpace(string(b))
	}
	// resolve reads a path relative to dir, symlinks resolved
	resolve := func(dir, p string) string {
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		p, _ = filepath.EvalSymlinks(p)
		return p
	}
	dotgit := filepath.Join(root, ".git")
	if fi, err := os.Lstat(dotgit); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	text, ok := strings.CutPrefix(small(dotgit), "gitdir:")
	gitdir := resolve(root, strings.TrimSpace(text))
	if !ok || gitdir == "" || filepath.Base(filepath.Dir(gitdir)) != "worktrees" {
		return ""
	}
	common := filepath.Dir(filepath.Dir(gitdir))
	if filepath.Base(common) != ".git" ||
		resolve(gitdir, small(filepath.Join(gitdir, "gitdir"))) != dotgit ||
		resolve(gitdir, small(filepath.Join(gitdir, "commondir"))) != common {
		return ""
	}
	return filepath.Dir(common)
}
