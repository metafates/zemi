// Package config resolves filesystem locations and holds runtime settings.
package config

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/user"
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
	AgentsHome string // ~/.agents, whose skills/ other agents share; never from an override
}

// xdg resolves one root: zemi's own override first, then the XDG variable
// the default mirrors, then the default under home, "" when home is unknown.
// No root depends on the cwd: a relative XDG value is ignored, as the spec
// says, and a relative override is an error, since ignoring it would put a
// throwaway tree's writes in the real dirs.
func xdg(home, env, xdgEnv, underHome string) (string, error) {
	if v := os.Getenv(env); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s=%q is a relative path; set an absolute one", env, v)
		}
		return v, nil
	}
	if v := os.Getenv(xdgEnv); filepath.IsAbs(v) {
		return filepath.Join(v, "zemi"), nil
	}
	if home == "" {
		return "", nil
	}
	return filepath.Join(home, underHome), nil
}

// ResolveDirs computes the standard locations from the environment. With no
// $HOME (cron, a systemd unit) home is the passwd entry's, never a shared temp
// dir another user could plant an init.lua in; with neither it fails.
func ResolveDirs() (Dirs, error) {
	home, _ := os.UserHomeDir()
	if !filepath.IsAbs(home) {
		home = ""
		if u, err := user.Current(); err == nil && filepath.IsAbs(u.HomeDir) {
			home = u.HomeDir
		}
	}
	var errs []error
	root := func(env, xdgEnv, underHome string) string {
		v, err := xdg(home, env, xdgEnv, underHome)
		errs = append(errs, err)
		return v
	}
	d := Dirs{
		ConfigHome: root("ZEMI_CONFIG_HOME", "XDG_CONFIG_HOME", ".config/zemi"),
		DataHome:   root("ZEMI_DATA_HOME", "XDG_DATA_HOME", ".local/share/zemi"),
		StateHome:  root("ZEMI_STATE_HOME", "XDG_STATE_HOME", ".local/state/zemi"),
		CacheHome:  root("ZEMI_CACHE_HOME", "XDG_CACHE_HOME", ".cache/zemi"),
	}
	d.ProjectDir, _ = os.Getwd()
	if home != "" {
		d.AgentsHome = filepath.Join(home, ".agents")
	}
	if err := errors.Join(errs...); err != nil {
		return d, err
	}
	if slices.Contains([]string{d.ConfigHome, d.DataHome, d.StateHome, d.CacheHome}, "") {
		return d, errors.New("no home directory: set HOME or the ZEMI_*_HOME variables")
	}
	return d, nil
}

// Ensure creates the directories zemi writes into, and the starter
// init.lua when there is none, so the first thing a user edits shows
// what there is to set.
func (d Dirs) Ensure() error {
	for _, dir := range []string{
		d.ConfigHome,
		filepath.Join(d.ConfigHome, "lua"),
		filepath.Join(d.PluginsRoot(), "installed"),
		filepath.Join(d.PluginsRoot(), "local"),
		d.LogDir(),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	init := filepath.Join(d.ConfigHome, "init.lua")
	if _, err := os.Stat(init); errors.Is(err, os.ErrNotExist) {
		// atomic, as every other config write is: a half-written init.lua is a
		// boot error nobody asked for
		if err := WriteFileAtomic(init, []byte(StarterInit)); err != nil {
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
-- OPENAI_API_KEY, DEEPSEEK_API_KEY, ...; /model lists them), then start zemi.
-- A provider zemi does not know, a local server, or a gateway in front of
-- a known one (reuse its id: yours replaces it whole):
-- zemi.provider.add({
-- 	id = "local",
-- 	kind = zemi.provider.kind.OPENAI_COMPAT,   -- or ANTHROPIC, OPENAI, GOOGLE, OPENROUTER, VERCEL, SYSTEM_ONE
-- 	base_url = "http://localhost:11434/v1",   -- required for OPENAI_COMPAT, refused for OPENROUTER, an endpoint override for the rest
-- 	api_key = zemi.env.get("LOCAL_API_KEY"),   -- leave out for a server that needs none
-- 	headers = { ["X-Team"] = "core" },   -- extra headers a gateway needs; as secret as the key
-- 	body = { service_tier = "flex" },   -- extra request-body fields; not sent by GOOGLE
-- 	models = { { id = "qwen3", context_window = 32000 } },   -- cost_per_1m_in/out optional; vision = false for a text-only model
-- })
-- zemi.provider.del("openai")   -- drop one builtin provider
-- zemi.config.set("providers.builtin", false)   -- or all of them

-- Model roles: chat runs the conversation and every role left unset; agents
-- (subagents), titles, compact (summaries) and memory (the reflector) each
-- take their own. Unset, chat is the default of the first provider you add
-- here, else of the first builtin one with a key. /model roles sets them too.
-- zemi.config.set("models.chat", "anthropic/claude-sonnet-4-6")
-- zemi.config.set("models.titles", "anthropic/claude-haiku-4-5-20251001")

-- Settings: zemi.config.schema() lists every one with its default.
-- /config lists every setting; a pick there lasts across restarts unless a
-- line here sets that key.
-- zemi.config.set("ui.theme", "catppuccin") -- /theme lists them; a theme
-- is one line of colors: zemi.ui.theme.add(name, { bg, fg, ansi = { 16 } })

-- Keys: <leader> is keymap.leader (ctrl+x). The right-hand side is a
-- command name ("review", "/review"), a host action (zemi.HostAction in
-- the type annotations lists them) or a function.
-- zemi.keymap.set("<leader>r", "review", { desc = "review the diff" })
-- zemi.keymap.set("<leader>m", "zemi.model.pick", { desc = "switch model" })

-- Your own slash command, completed in the palette like the builtin ones.
-- zemi.command.add("tests", function(a)
-- 	zemi.agent.send("run the tests" .. (a.args ~= "" and " for " .. a.args or ""))
-- end, { desc = "ask the agent to run the tests", nargs = "?" })

-- Everything on screen is a widget: an id, a region, and a render that
-- returns lines of { text, hl } segments. Reuse a builtin id to replace it.
-- zemi.ui.widget.add({
-- 	id = "msgs", region = "statusline.right", priority = 5,
-- 	render = function(ctx) return { { { text = ctx.transcript_count .. " msgs", hl = "Dim" } } } end,
-- })

-- zemi writes type annotations for the whole zemi.* API and the editor
-- configs (.luarc.json, .emmyrc.json) beside this file, so your editor
-- completes and checks it.

-- /help lists the keys and commands, /health says what is missing.
`

// ProjectConfigDir is <repo root>/.agents, the one place a trusted project
// keeps zemi config.
func (d Dirs) ProjectConfigDir() string {
	if d.ProjectDir == "" {
		return ""
	}
	return filepath.Join(d.RepoRoot(), ".agents")
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

// WorktreesDir is <repo root>/.agents/worktrees, where the agent keeps git
// worktrees: the one part of .agents that commands and settled writes may
// change, and that nothing is loaded from.
func (d Dirs) WorktreesDir() string {
	if d.ProjectDir == "" {
		return ""
	}
	return filepath.Join(d.ProjectConfigDir(), "worktrees")
}

// WorktreesIgnore writes <WorktreesDir>/.gitignore ignoring everything,
// itself included, when there is none. It reports whether WorktreesDir is
// the repository's own directory, no link on the way, and writes only then:
// a symlinked .agents must not steer it elsewhere.
func (d Dirs) WorktreesIgnore() (bool, error) {
	wt := d.WorktreesDir()
	root, err := filepath.EvalSymlinks(d.RepoRoot())
	if err != nil {
		return false, nil
	}
	if got, err := filepath.EvalSymlinks(wt); err != nil || got != filepath.Join(root, ".agents", "worktrees") {
		return false, nil
	}
	if fi, err := os.Stat(wt); err != nil || !fi.IsDir() {
		return false, nil
	}
	f, err := os.OpenFile(filepath.Join(wt, ".gitignore"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	_, err = f.WriteString("/*\n")
	return true, errors.Join(err, f.Close())
}

// UserTrees are the directories zemi loads user files from: the config
// subdirectories and ~/.agents/skills. They are named apart from ConfigHome
// because a symlinked one resolves outside it, and both origin
// classification and the read jail work on resolved paths.
func (d Dirs) UserTrees() []string {
	if d.ConfigHome == "" {
		return nil
	}
	var out []string
	for _, name := range []string{"lua", "skills", "agents", "commands", "workflows"} {
		out = append(out, filepath.Join(d.ConfigHome, name))
	}
	if d.AgentsHome != "" {
		out = append(out, filepath.Join(d.AgentsHome, "skills"))
	}
	return out
}

// PluginsRoot is ~/.local/share/zemi/plugins: git clones in installed/,
// plugins developed in place in local/, and the disabled list.
func (d Dirs) PluginsRoot() string {
	return filepath.Join(d.DataHome, "plugins")
}

// WriteFileAtomic replaces path with data through a temp file in its
// directory, made owner-only when missing, so a reader or a crash never sees
// half a file and two zemi windows writing at once never share a temp file.
func WriteFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // names nothing once renamed
	_, err = f.Write(data)
	if err := errors.Join(err, f.Chmod(0o600), f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// LogDir is ~/.local/state/zemi/logs.
func (d Dirs) LogDir() string {
	return filepath.Join(d.StateHome, "logs")
}

// The saved trust decision values.
const (
	Trusted   = "trusted"
	Untrusted = "untrusted"
)

// ReadTrust is the saved trust decisions: trust roots to "trusted" or
// "untrusted".
func (d Dirs) ReadTrust() map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(filepath.Join(d.StateHome, "trust.json"))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

// WriteTrust replaces the saved trust decisions with m.
// NOTE: callers read, change and write, so two windows writing at once can
// lose one decision, which the next start asks again.
func (d Dirs) WriteTrust(m map[string]string) error {
	data, err := json.Marshal(m, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(d.StateHome, "trust.json"), data)
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
	return cmp.Or(mainCheckout(root), root)
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
