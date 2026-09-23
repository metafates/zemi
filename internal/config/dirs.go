// Package config resolves filesystem locations and holds runtime settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
// init.lua when there is none, so the first thing a user edits shows
// what there is to set.
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
// commented, so zemi registers nothing the user did not ask for, and a
// block only needs uncommenting.
const StarterInit = `-- zemi config. Lua, reloaded whenever you save this file. Nothing here is on:
-- uncomment what you want, or ask zemi to change this file for you.

-- Models: export a provider's key in your shell (ANTHROPIC_API_KEY,
-- OPENAI_API_KEY, DEEPSEEK_API_KEY, ...; /model lists them) and it is on.
-- A provider zemi does not know, a local server, or a gateway in front of
-- a known one (reuse its id: yours replaces it whole):
-- zemi.provider.add({
--   id = "local",
--   kind = zemi.provider.kind.OPENAI_COMPAT,   -- or ANTHROPIC, OPENAI, GOOGLE
--   base_url = "http://localhost:11434/v1",   -- required for OPENAI_COMPAT, an endpoint override for the rest
--   api_key = zemi.env.get("LOCAL_API_KEY"),   -- leave out for a server that needs none
--   headers = { ["X-Team"] = "core" },   -- extra headers a gateway needs; as secret as the key
--   body = { service_tier = "flex" },   -- extra request-body fields; ANTHROPIC and OPENAI_COMPAT only
--   models = { { id = "qwen3", context_window = 32000 } },   -- cost_per_1m_in/out optional; images = false for a text-only model
-- })
-- zemi.provider.del("openai")   -- drop one builtin provider
-- zemi.config.set("providers.builtin", false)   -- or all of them

-- Model tiers: main runs everything, small the short jobs (session titles,
-- compaction summaries, the reflector, the web subagent), smart the main
-- conversation. Unset, main is the default of the first provider you add
-- here, else of the first builtin one with a key; small is main when you set
-- main, else that provider's small default; smart is main.
-- zemi.config.set("models.main", "anthropic/claude-sonnet-4-6")
-- zemi.config.set("models.small", "anthropic/claude-haiku-4-5-20251001")

-- Settings: zemi.config.schema() lists every one with its default.
-- zemi.config.set("ui.theme", "default-light")

-- Keys: <leader> is keymap.leader (ctrl+x). The right-hand side is a
-- command name ("review", "/review"), a host action (zemi.HostAction in
-- the type annotations lists them) or a function.
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
--   render = function(ctx) return { { { text = ctx.transcript_count .. " msgs", hl = "Dim" } } } end,
-- })

-- Permissions: allow rules come only from your own config. Each rule needs
-- match, an argument its pattern must match, so a typo fails at startup.
-- zemi.permission.rule.add({
--   subject = "tool.bash", pattern = "git push *", decision = "ask",
--   match = "git push origin main",
-- })

-- zemi writes type annotations for the whole zemi.* API and a .luarc.json
-- beside this file, so lua-language-server completes and checks it.

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

// WorktreesDir is <repo root>/.zemi/worktrees, where the agent keeps git
// worktrees: the one part of .zemi that commands and settled writes may
// change, and that nothing is loaded from.
func (d Dirs) WorktreesDir() string {
	if d.ProjectDir == "" {
		return ""
	}
	return filepath.Join(d.RepoRoot(), ".zemi", "worktrees")
}

// IgnoreLocal makes <repo root>/.zemi/.gitignore list the entries local to
// this checkout, appending to an existing file: .zemi also holds checked-in
// config, so nothing else is ignored.
func (d Dirs) IgnoreLocal() error {
	entries := []string{"worktrees/"}
	path := filepath.Join(filepath.Dir(d.WorktreesDir()), ".gitignore")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	have := map[string]bool{}
	own := true // every line is one zemi writes, so the file is zemi's
	for line := range strings.Lines(string(b)) {
		l := strings.TrimSpace(line)
		have[l] = true
		own = own && (l == "" || l == ".gitignore" || slices.Contains(entries, l))
	}
	var add []string
	for _, e := range entries {
		if !have[e] {
			add = append(add, e)
		}
	}
	// the file ignores itself, or git status shows .zemi/ as untracked; a
	// team's copy gets the line only with an entry it lacked anyway
	if !have[".gitignore"] && (own || len(add) > 0) {
		add = append(add, ".gitignore")
	}
	if len(add) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	sep := ""
	if len(b) > 0 && b[len(b)-1] != '\n' {
		sep = "\n"
	}
	_, err = f.WriteString(sep + strings.Join(add, "\n") + "\n")
	return err
}

// UserTrees are the config subdirectories zemi loads user files from.
// They are named apart from ConfigHome because a symlinked one (skills ->
// ~/.agents/skills) resolves outside it, and both origin classification and
// the read jail work on resolved paths.
func (d Dirs) UserTrees() []string {
	if d.ConfigHome == "" {
		return nil
	}
	var out []string
	for _, name := range []string{"lua", "skills", "agents", "commands", "workflows"} {
		out = append(out, filepath.Join(d.ConfigHome, name))
	}
	return out
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
