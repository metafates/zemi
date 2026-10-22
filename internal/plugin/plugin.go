// Package plugin manages the plugin tree under <data>/plugins: symlinks in
// installed/ pointing into marketplace material (a marketplace clone, or the
// clone of an entry's own repo under marketplaces/.cache), directories the
// user develops in place in local/, and the names turned off in disabled.
// Plugins come from a marketplace, whose clones live in marketplaces/ as the
// material installs link into; updating a marketplace is what updates the
// plugins installed from it. It runs no Lua and needs no Host, so the
// /plugin command and the zemi plugin CLI share it.
package plugin

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/filelock"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/set"
)

// Plugin is one directory under installed/ or local/; an installed one is a
// symlink into marketplace material.
type Plugin struct {
	Name, Dir string
	Desc      string // the manifest's description, else line 1 of init.lua when it is a -- comment
	URL       string // the origin remote; "" for a local plugin or a link
	Version   string // the manifest's version, else the marketplace entry's, else the checked-out semver tag
	Local     bool
	Enabled   bool
	Uses      []string // the manifest's declared capabilities: what enabling it grants

	// Author and Homepage are the manifest's metadata, display-only: the
	// detail view's table. Author falls back to a Claude manifest beside it.
	Author, Homepage string

	// Marketplace is the registered marketplace the install came from, ""
	// for a local plugin or a clone from before marketplaces.
	Marketplace string

	// Provides is what a Claude-layout plugin contributes beyond that
	// vocabulary, display-only: CheckUses never sees it.
	Provides []string

	// revision is what the checkout is at, the tag else the short commit: what
	// update compares, since a manifest version may stand still across one
	revision string
}

// CodeDir is where a plugin keeps everything zemi runs or reads as code: its
// Lua, its tests, its workflows and its manifest. The resource dirs a plugin
// layers over the user's (skills, agents, commands) stay at its root.
const CodeDir = ".zemi"

// ManifestFile is the plugin's manifest, inside CodeDir: what marks the
// directory as a plugin and answers its version and description. A plugin
// without one reports its git tag and init.lua's first line, as before.
const ManifestFile = CodeDir + "/plugin.json"

// InitFile is the chunk loaded as the plugin origin, inside CodeDir.
const InitFile = CodeDir + "/init.lua"

// manifestBytes is the raw manifest of the plugin in dir, nil for the plugins
// that have none: the reader takes the first 4 KB of a regular file.
func manifestBytes(dir string) []byte { return readHead(filepath.Join(dir, ManifestFile)) }

// manifest is ManifestFile parsed; every field is optional.
type manifest struct {
	Version     string         `json:"version"`
	Description string         `json:"description"`
	Homepage    string         `json:"homepage"`
	Author      jsontext.Value `json:"author"`
	Uses        []string       `json:"uses"`
}

// authorName is a manifest's author as it displays: a string, or Claude's
// object with a name.
func authorName(raw jsontext.Value) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '{' {
		var j struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &j)
		return j.Name
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// Uses is the vocabulary a manifest's "uses" draws from, one word per host
// capability a plugin may call: what switching the plugin on grants. "perm"
// is the consent to steer the permission gate (a permission.request handler,
// zemi.perm.ask).
var Uses = []string{"agent", "fs.write", "http", "job", "model", "perm", "tool"}

// CheckUses refuses a uses list naming something outside the vocabulary: the
// plugin does not load, and the error carries the names that would.
func CheckUses(u []string) error {
	for _, name := range u {
		if !slices.Contains(Uses, name) {
			return fmt.Errorf("uses %q is not a capability; the valid ones are %s", name, strings.Join(Uses, ", "))
		}
	}
	return nil
}

// Readme is the plugin's README, "" when it has none: what the detail view
// shows instead of the manifest prose. A long one is cut; it is display text.
func Readme(dir string) string {
	for _, name := range []string{"README.md", "README.markdown", "README.txt", "README"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			if len(b) > 1<<16 {
				b = b[:1<<16]
			}
			return string(b)
		}
	}
	return ""
}

// manifestOf parses the manifest of the plugin in dir; a missing or malformed
// one is the zero manifest, which declares nothing. Description, author and
// homepage fall back to a Claude manifest beside it, which a Claude-layout
// plugin keeps.
func manifestOf(dir string) manifest {
	var m manifest
	_ = json.Unmarshal(manifestBytes(dir), &m)
	if m.Description == "" || len(m.Author) == 0 || m.Homepage == "" {
		if IsClaude(dir) {
			var j claudeManifestJSON
			if err := json.Unmarshal(readHead(filepath.Join(dir, ClaudeManifestFile)), &j); err == nil {
				if m.Description == "" {
					m.Description = j.Description
				}
				if len(m.Author) == 0 && j.Author.Name != "" {
					m.Author, _ = json.Marshal(struct {
						Name string `json:"name"`
					}{j.Author.Name})
				}
				if m.Homepage == "" {
					m.Homepage = j.Homepage
				}
			}
		}
	}
	return m
}

// a dot dir, such as a clone in flight, is never a plugin, and no name
// holds a dot, which require("<name>") would read as a directory
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// netTimeout bounds a clone or a fetch; other git calls get proc.Git's.
const netTimeout = 2 * time.Minute

// ErrUsage is Run's error for a verb or arguments it does not take.
var ErrUsage = errors.New("usage: plugin marketplace add <source> | marketplace list | marketplace remove <name> | marketplace update [name] | install <name>[@<marketplace>] | enable <name> | disable <name> | delete <name>")

// List reads every plugin, local ones first, each group by name, with the
// marketplace each install came from and the entry's metadata where the
// plugin's own manifest has none. It runs only local git reads.
func List(ctx context.Context, d config.Dirs) ([]Plugin, error) {
	ps, err := Scan(d)
	if err != nil {
		return nil, err
	}
	mf, err := readMarketplaces(d)
	if err != nil {
		return nil, err
	}
	manifests := map[string]Marketplace{}
	for i, p := range ps {
		provides := p.Provides
		ps[i] = load(ctx, p.Dir, p.Local, p.Enabled)
		ps[i].Provides = provides
		reg, ok := mf.Installs[p.Name]
		if !ok {
			continue
		}
		ps[i].Marketplace = reg.Marketplace
		if reg.Kind == kindCache {
			// the cache clone is the plugin's own repo: its tag is the
			// version an entry without one shows
			if target, err := filepath.EvalSymlinks(p.Dir); err == nil {
				if c := load(ctx, target, false, p.Enabled); c.URL != "" {
					ps[i].URL = c.URL
					ps[i].Version = cmp.Or(ps[i].Version, c.Version)
				}
			}
		}
		m, seen := manifests[reg.Marketplace]
		if !seen {
			m, _ = ReadMarketplace(filepath.Join(d.PluginsRoot(), "marketplaces", reg.Marketplace))
			manifests[reg.Marketplace] = m
		}
		e, found := m.find(reg.Entry)
		if !found {
			continue
		}
		ps[i].Version = cmp.Or(ps[i].Version, e.Version, reg.Version)
		ps[i].Desc = cmp.Or(ps[i].Desc, e.Description)
		ps[i].Author = cmp.Or(ps[i].Author, e.Author.Name)
		ps[i].Homepage = cmp.Or(ps[i].Homepage, e.Homepage)
	}
	return ps, nil
}

// Scan is List without git: Name, Dir, Local, Enabled and the manifest's
// declared uses, for the boot step that loads plugins. A row's declared
// capabilities fill in for a source that carries no manifest of its own.
func Scan(d config.Dirs) ([]Plugin, error) {
	off, err := disabled(d)
	if err != nil {
		return nil, err
	}
	mf, err := readMarketplaces(d)
	if err != nil {
		return nil, err
	}
	var ps []Plugin
	for _, kind := range []string{"local", "installed"} {
		dir := filepath.Join(d.PluginsRoot(), kind)
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range entries {
			// a symlink is listed, so the loader can judge the link and say
			// why it refuses one it cannot follow
			if nameRe.MatchString(e.Name()) && (e.IsDir() || e.Type()&fs.ModeSymlink != 0) {
				root := filepath.Join(dir, e.Name())
				p := Plugin{Name: e.Name(), Dir: root, Local: kind == "local", Enabled: !off.Has(e.Name()), Uses: manifestOf(root).Uses}
				if reg, ok := mf.Installs[e.Name()]; ok && len(p.Uses) == 0 {
					p.Uses = reg.Uses
				}
				if raw := ClaudeRaw(d, root); raw != nil {
					p.Provides = claudeProvides(root, raw)
				}
				ps = append(ps, p)
			}
		}
	}
	return ps, nil
}

// Code reports whether an entry of a plugin's root holds what zemi loads or
// its tests read: CodeDir, and the dirs a plugin layers over the user's. The
// copier takes these entries whole.
func Code(name string) bool {
	return slices.Contains([]string{CodeDir, "skills", "agents", "commands"}, name)
}

// OldLayout is why a plugin in the layout before CodeDir cannot load, nil for
// one that keeps its Lua where zemi reads it: that root entry is what the
// loader no longer reaches. A Claude-layout plugin without zemi's code dir
// has no zemi layout; its root files are its own.
func OldLayout(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, CodeDir)); err != nil && IsClaude(dir) {
		return nil
	}
	for _, name := range []string{"init.lua", "lua", "tests", "workflows"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%s is at the plugin root, where zemi no longer loads it: move init.lua, lua/, tests/ and workflows/ under %s/", name, CodeDir)
		}
	}
	return nil
}

// ClaudeRaw is the Claude manifest bytes of the plugin in dir: the file
// beside it, else the entry-carried manifest its install row carries, nil
// for a plugin that is neither. What IsClaude reads, link-aware.
func ClaudeRaw(d config.Dirs, dir string) []byte {
	if raw := readHead(filepath.Join(dir, ClaudeManifestFile)); raw != nil {
		return raw
	}
	if reg, ok := MarketplaceOf(d, filepath.Base(dir)); ok && len(reg.Manifest) > 0 {
		return reg.Manifest
	}
	return nil
}

// Kept reports whether an entry below a plugin's code is code too: not a dot
// entry, since a nested .git may hold tokens, nor node_modules.
func Kept(name string) bool { return !strings.HasPrefix(name, ".") && name != "node_modules" }

// Name is the name a plugin installed from url gets by default: the URL's
// last segment without ".git".
func Name(url string) string {
	return strings.TrimSuffix(path.Base(strings.TrimRight(url, "/")), ".git")
}

// marketplaceSource resolves a `marketplace add` argument the way Claude and
// codex type one: a local directory, an ssh or https git URL, or GitHub
// shorthand owner/repo, with an optional #ref or @ref suffix.
func marketplaceSource(arg string) (url, ref string, err error) {
	s := strings.TrimSpace(arg)
	if s == "" {
		return "", "", errors.New("the marketplace source is empty")
	}
	// #ref pins any source; @ref only a non-local one, so ./dir@x and
	// git@host:path keep their meaning
	if base, r, ok := strings.Cut(s, "#"); ok {
		s, ref = base, strings.TrimSpace(r)
	} else if !looksLikeLocalPath(s) && !strings.Contains(s, "://") && !isSSHURL(s) {
		base, r, _ := strings.Cut(s, "@")
		s, ref = base, strings.TrimSpace(r)
	}
	switch {
	case looksLikeLocalPath(s):
		if ref != "" {
			return "", "", fmt.Errorf("%s: a ref pins a git source only", arg)
		}
		if rest, ok := strings.CutPrefix(s, "~/"); ok {
			if home, err := os.UserHomeDir(); err == nil {
				s = filepath.Join(home, rest)
			}
		}
		if url, err = filepath.Abs(s); err != nil {
			return "", "", err
		}
		if fi, err := os.Lstat(url); err == nil && !fi.IsDir() {
			return "", "", fmt.Errorf("%s is not a directory", url)
		}
		return url, "", nil
	case isSSHURL(s), strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "file://"):
		return normalizeGitURL(s), ref, nil
	case githubShorthand(s):
		return "https://github.com/" + s + ".git", ref, nil
	}
	return "", "", fmt.Errorf("%s is not a marketplace source: expected owner/repo, a git URL, or a local directory", arg)
}

// looksLikeLocalPath reports the shapes a marketplace add reads as a
// directory: absolute, ./ ../ . .. ~/ and the Windows drive and UNC forms.
func looksLikeLocalPath(s string) bool {
	return filepath.IsAbs(s) || s == "." || s == ".." ||
		strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "~/") || windowsAbs(s)
}

// windowsAbs reports the absolute shapes Windows users type, which a colon
// would otherwise read as a git URL.
func windowsAbs(s string) bool {
	if len(s) >= 3 && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') &&
		s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return true
	}
	return strings.HasPrefix(s, `\\`)
}

// isSSHURL reports ssh:// and the scp-like user@host:path, where the part
// before the colon holds no slash.
func isSSHURL(s string) bool {
	if strings.HasPrefix(s, "ssh://") {
		return true
	}
	at, colon := strings.IndexByte(s, '@'), strings.IndexByte(s, ':')
	return at >= 0 && colon > at && !strings.Contains(s[:colon], "/")
}

// normalizeGitURL strips trailing slashes and completes a github.com https
// URL to its clone form, so the shorthand and the URL are one source.
func normalizeGitURL(s string) string {
	s = strings.TrimRight(s, "/")
	if strings.HasPrefix(s, "https://github.com/") && !strings.HasSuffix(s, ".git") {
		return s + ".git"
	}
	return s
}

// githubShorthand reports whether s is owner/repo, both segments of
// [A-Za-z0-9._-]: what expands to https://github.com/<s>.git.
func githubShorthand(s string) bool {
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || strings.Contains(repo, "/") {
		return false
	}
	return segment(owner) && segment(repo)
}

func segment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// sourceDisplay renders a marketplace's source with its ref pin, the shape
// the add argument names it by.
func sourceDisplay(source, ref string) string {
	if ref == "" {
		return source
	}
	return source + "#" + ref
}

// cloneRepo clones url into the dot dir tmp; a failed clone removes it.
func cloneRepo(ctx context.Context, url, tmp string) (err error) {
	parent := filepath.Dir(tmp)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	os.RemoveAll(tmp)
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	nctx, cancel := context.WithTimeout(ctx, netTimeout)
	defer cancel()
	_, err = git(nctx, parent, "clone", "--quiet", "--no-recurse-submodules", "--", url, tmp)
	return err
}

// checkoutRepo checks a fresh clone out to sha, else to ref, else to the
// highest semver tag it has.
func checkoutRepo(ctx context.Context, tmp, ref, sha string) error {
	to := cmp.Or(sha, ref)
	if to == "" {
		tags, _ := git(ctx, tmp, "tag", "--list")
		if top := highest(tags, ""); top != "" {
			to = "refs/tags/" + top
		}
	}
	if to != "" {
		if _, err := git(ctx, tmp, "checkout", "--quiet", to); err != nil {
			return err
		}
	}
	return nil
}

// CheckMarketplaces fetches every marketplace clone without moving it and
// names the ones whose remote moved, what the daily check with auto-update
// off reports; the pull stays the user's.
func CheckMarketplaces(ctx context.Context, d config.Dirs) ([]string, error) {
	f, err := readMarketplaces(d)
	if err != nil {
		return nil, err
	}
	var moved []string
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(f.Marketplaces)) {
		dir := filepath.Join(d.PluginsRoot(), "marketplaces", name)
		if !exists(dir) {
			continue // its clone is gone; there is nothing to compare against
		}
		ref := f.Marketplaces[name].Ref
		nctx, cancel := context.WithTimeout(ctx, netTimeout)
		fetch := []string{"fetch", "--quiet", "--tags"}
		if ref != "" {
			fetch = append(fetch, "origin", ref)
		}
		_, err := git(nctx, dir, fetch...)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("marketplace %s: %w", name, err))
			continue
		}
		remote := "origin/HEAD"
		if ref != "" {
			remote = "FETCH_HEAD"
		}
		head, _ := git(ctx, dir, "rev-parse", "HEAD")
		origin, _ := git(ctx, dir, "rev-parse", remote)
		if head != "" && origin != "" && head != origin {
			moved = append(moved, name)
		}
	}
	return moved, errors.Join(errs...)
}

// remove deletes the installed plugin: the symlink and, for a cache kind,
// the cache clone it pointed into; else the local plugin. A symlink in
// local/ loses the link only.
func remove(d config.Dirs, name string) error {
	dir := filepath.Join(d.PluginsRoot(), "local", name)
	if !exists(dir) {
		dir = filepath.Join(d.PluginsRoot(), "installed", name)
		fi, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("no plugin is named %s", name)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if err := os.Remove(dir); err != nil {
				return err
			}
			if reg, ok := MarketplaceOf(d, name); ok && reg.Kind == kindCache {
				os.RemoveAll(cacheDir(d, reg.Marketplace, name))
			}
			dir = "" // the link is gone; nothing else to delete
		}
	}
	// a real tree moves aside first, so the config watcher never loads half
	// of it
	if dir != "" {
		del := filepath.Join(filepath.Dir(dir), "."+name+".del")
		os.RemoveAll(del)
		if err := os.Rename(dir, del); err != nil {
			return err
		}
		if err := os.RemoveAll(del); err != nil {
			return err
		}
	}
	if err := setDisabled(d, name, false); err != nil {
		return err
	}
	return forgetInstall(d, name)
}

// SetEnabled turns a plugin on or off from the next load.
func SetEnabled(d config.Dirs, name string, on bool) error {
	if !nameRe.MatchString(name) || free(d, name) == nil {
		return fmt.Errorf("no plugin is named %q", name)
	}
	return setDisabled(d, name, !on)
}

// MarketplacesUpToDate is what marketplace update prints when no clone moved.
const MarketplacesUpToDate = "marketplaces up to date"

// Run runs one verb of the zemi plugin CLI, which /plugin shares; each
// prints plain lines a toast can show. The installs of the copy model
// re-link into their marketplace material first, once.
func Run(ctx context.Context, d config.Dirs, args []string, out io.Writer) error {
	if len(args) == 0 {
		return ErrUsage
	}
	if err := EnsureLinks(d); err != nil {
		return err
	}
	verb, rest := args[0], args[1:]
	switch {
	case verb == "marketplace" && len(rest) > 0:
		switch {
		case rest[0] == "add" && len(rest) == 2:
			return addMarketplace(ctx, d, rest[1], out)
		case rest[0] == "list" && len(rest) == 1:
			return listMarketplaces(d, out)
		case rest[0] == "remove" && len(rest) == 2:
			return removeMarketplace(d, rest[1], out)
		case rest[0] == "update" && len(rest) <= 2:
			return updateMarketplaces(ctx, d, rest[1:], out, false)
		}
		return ErrUsage
	case (verb == "install" || verb == "add") && len(rest) == 1:
		arg := rest[0]
		// <entry>@<marketplace> installs an entry of a registered
		// marketplace; a git URL with a user part, git@host:path, never
		// parses as one
		if entry, mkt, ok := strings.Cut(arg, "@"); ok && nameRe.MatchString(mkt) &&
			entry != "" && !strings.ContainsAny(entry, `<>:"|?*@`) {
			p, err := InstallFromMarketplace(ctx, d, mkt, entry)
			if err == nil {
				fmt.Fprintln(out, p.Name, p.Version)
			}
			return err
		}
		if !nameRe.MatchString(arg) {
			return fmt.Errorf("%s is not a plugin name: zemi installs plugins from marketplaces; add one with plugin marketplace add <source>", arg)
		}
		return installBare(ctx, d, arg, out)
	case (verb == "enable" || verb == "disable") && len(rest) == 1:
		return SetEnabled(d, rest[0], verb == "enable")
	case verb == "delete" && len(rest) == 1:
		return remove(d, rest[0])
	}
	return ErrUsage
}

// InstallKind says what an install links into: a link reaches into the
// marketplace clone itself, a cache into a clone of the entry's own repo
// under marketplaces/.cache. The zero value and "copy"/"git" are rows from
// before the field, which EnsureLinks migrates.
type InstallKind string

const (
	kindLink  InstallKind = "link"
	kindCache InstallKind = "cache"
	kindCopy  InstallKind = "copy" // legacy: a copied subtree, re-linked on boot
	kindGit   InstallKind = "git"  // legacy: a clone in installed/, moved into the cache
)

// marketplaceFile is marketplaces.json under the plugins root: the
// registered marketplaces and, per installed plugin, where it came from.
type marketplaceFile struct {
	Marketplaces map[string]marketplaceReg `json:"marketplaces"`
	Installs     map[string]installReg     `json:"installs"`
	Checked      time.Time                 `json:"checked,omitzero"` // the last daily sweep that ran
}

type marketplaceReg struct {
	Source string    `json:"source"`
	Ref    string    `json:"ref,omitempty"` // a ref pin the clone updates along
	Added  time.Time `json:"added"`
}

// installReg is the provenance of a plugin installed from a marketplace, and
// the sidecar for what a symlinked install cannot carry: the entry's declared
// capabilities and, when the entry's source holds no manifest of its own, the
// entry itself as one.
type installReg struct {
	Marketplace string         `json:"marketplace"`
	Entry       string         `json:"entry"`
	Version     string         `json:"version"`
	Kind        InstallKind    `json:"kind"`
	ID          string         `json:"id"` // the install's identity, shared with .git/zemi-install for a legacy clone
	Uses        []string       `json:"uses,omitempty"`
	Manifest    jsontext.Value `json:"manifest,omitempty"`
}

func readMarketplaces(d config.Dirs) (marketplaceFile, error) {
	var f marketplaceFile
	data, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "marketplaces.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(data, &f)
}

func (f marketplaceFile) save(d config.Dirs) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(d.PluginsRoot(), "marketplaces.json"), data)
}

// Provenance is the marketplace origin an installed plugin came from.
type Provenance struct {
	Marketplace, Entry, Version string
	Kind                        InstallKind
	ID                          string
	Uses                        []string
	Manifest                    jsontext.Value
}

// MarketplaceOf reads the provenance of the installed plugin name, false for
// a local plugin or a clone installed before marketplaces.
func MarketplaceOf(d config.Dirs, name string) (Provenance, bool) {
	f, err := readMarketplaces(d)
	if err != nil {
		return Provenance{}, false
	}
	reg, ok := f.Installs[name]
	return Provenance(reg), ok
}

// recordInstall writes the provenance of an installed entry. The caller owns
// the install id: a fresh install supplies a new one, so a stale row cannot
// hand its state to a reinstall, an update re-supplies the row's.
func recordInstall(d config.Dirs, name, mkt string, e MarketplacePlugin, kind InstallKind, id string, uses []string, manifest jsontext.Value) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if f.Installs == nil {
		f.Installs = map[string]installReg{}
	}
	f.Installs[name] = installReg{
		Marketplace: mkt, Entry: e.Name, Version: e.Version, Kind: kind, ID: id,
		Uses: uses, Manifest: manifest,
	}
	return f.save(d)
}

// forgetInstall drops the provenance of a deleted plugin.
func forgetInstall(d config.Dirs, name string) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if _, ok := f.Installs[name]; !ok {
		return nil
	}
	delete(f.Installs, name)
	return f.save(d)
}

// MarketplaceInfo is one registered marketplace and the entries its clone's
// manifest lists, what list, completion and the overlay display. Err names
// why the manifest could not be read, so a failure never reads as "lists no
// plugins".
type MarketplaceInfo struct {
	Name, Source string
	Added        time.Time
	Entries      []MarketplaceEntry
	Err          string
}

// MarketplaceEntry is one entry of a registered marketplace, display-only.
type MarketplaceEntry struct {
	Name, Version, Description string
	Category, Author, Homepage string
	Uses                       []string
}

// Marketplaces lists the registered marketplaces and the entries their
// clones' manifests declare; a marketplace whose clone is gone lists none.
func Marketplaces(d config.Dirs) ([]MarketplaceInfo, error) {
	f, err := readMarketplaces(d)
	if err != nil {
		return nil, err
	}
	out := make([]MarketplaceInfo, 0, len(f.Marketplaces))
	for _, name := range slices.Sorted(maps.Keys(f.Marketplaces)) {
		reg := f.Marketplaces[name]
		info := MarketplaceInfo{Name: name, Source: sourceDisplay(reg.Source, reg.Ref), Added: reg.Added}
		if m, err := ReadMarketplace(filepath.Join(d.PluginsRoot(), "marketplaces", name)); err != nil {
			// a gone clone is the documented empty state; anything else must
			// not read as "lists no plugins"
			if !errors.Is(err, fs.ErrNotExist) {
				info.Err = err.Error()
			}
		} else {
			for _, e := range m.Plugins {
				info.Entries = append(info.Entries, MarketplaceEntry{
					Name: e.Name, Version: e.Version, Description: e.Description,
					Category: e.Category, Author: e.Author.Name, Homepage: e.Homepage, Uses: e.Uses,
				})
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// InstalledEntries maps a marketplace's entry names to the plugin each one
// installed as, for the drill-down's installed markers.
func InstalledEntries(d config.Dirs, marketplace string) map[string]string {
	f, err := readMarketplaces(d)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for name, reg := range f.Installs {
		if reg.Marketplace == marketplace {
			out[reg.Entry] = name
		}
	}
	return out
}

// listMarketplaces prints one line per marketplace and one per entry.
func listMarketplaces(d config.Dirs, out io.Writer) error {
	ms, err := Marketplaces(d)
	if err != nil {
		return err
	}
	for _, m := range ms {
		fmt.Fprintf(out, "%s %s (%d plugins)\n", m.Name, m.Source, len(m.Entries))
		for _, e := range m.Entries {
			line := []string{e.Name}
			if e.Version != "" {
				line = append(line, e.Version)
			}
			if e.Description != "" {
				line = append(line, e.Description)
			}
			fmt.Fprintf(out, "  %s\n", strings.Join(line, " "))
		}
	}
	return nil
}

// addMarketplace clones a marketplace repo into the cache and registers it
// under the name its manifest declares: the zemi one, else Claude's
// sanitized. A re-add of the same source and ref just refreshes the clone.
func addMarketplace(ctx context.Context, d config.Dirs, arg string, out io.Writer) error {
	source, ref, err := marketplaceSource(arg)
	if err != nil {
		return err
	}
	dir := filepath.Join(d.PluginsRoot(), "marketplaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// the manifest decides the destination, so no name can be derived from
	// the URL: a temp dir keeps concurrent adds apart
	tmp, err := os.MkdirTemp(dir, ".add-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := cloneRepo(ctx, source, tmp); err != nil {
		return err
	}
	if ref != "" {
		// the pin holds for the marketplace's life: update fetches this ref
		if _, err := git(ctx, tmp, "checkout", "--quiet", ref); err != nil {
			return err
		}
	}
	m, err := ReadMarketplace(tmp)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s holds neither %s nor %s: zemi installs plugins only from a marketplace", source, ZemiMarketplaceFile, ClaudeMarketplaceFile)
	}
	if err != nil {
		return err
	}
	name := m.Name
	if m.Kind == KindClaude {
		if name = SanitizeName(m.Name); name == "" {
			return fmt.Errorf("%s names no marketplace", ClaudeMarketplaceFile)
		}
	}
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if old, ok := f.Marketplaces[name]; ok && (old.Source != source || old.Ref != ref) {
		return fmt.Errorf("marketplace %s is already registered from %s, not %s",
			name, sourceDisplay(old.Source, old.Ref), sourceDisplay(source, ref))
	}
	if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	// a same-source re-add keeps its row and Added
	if _, ok := f.Marketplaces[name]; !ok {
		if f.Marketplaces == nil {
			f.Marketplaces = map[string]marketplaceReg{}
		}
		f.Marketplaces[name] = marketplaceReg{Source: source, Ref: ref, Added: time.Now().UTC()}
		if err := f.save(d); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "marketplace %s added (%d plugins)\n", name, len(m.Plugins))
	return nil
}

// removeMarketplace drops a marketplace: its clone, its cache clones and its
// registry row go. Refused while any install still links into its material.
func removeMarketplace(d config.Dirs, name string, out io.Writer) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if _, ok := f.Marketplaces[name]; !ok {
		return fmt.Errorf("no marketplace is named %s", name)
	}
	var users []string
	for _, pname := range slices.Sorted(maps.Keys(f.Installs)) {
		if f.Installs[pname].Marketplace == name {
			users = append(users, pname)
		}
	}
	if len(users) > 0 {
		return fmt.Errorf("marketplace %s still has installs updating from it: %s; delete them first", name, strings.Join(users, ", "))
	}
	delete(f.Marketplaces, name)
	if err := f.save(d); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(d.PluginsRoot(), "marketplaces", name)); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(d.PluginsRoot(), "marketplaces", cacheDirName, name)); err != nil {
		return err
	}
	fmt.Fprintf(out, "marketplace %s removed\n", name)
	return nil
}

// updateMarketplaces pulls one or all marketplace clones; the installs
// linking into them move with the pull, and cache-kind installs move with the
// cache refresh that follows. all says the run covered every marketplace, so
// it dates the last daily sweep.
func updateMarketplaces(ctx context.Context, d config.Dirs, names []string, out io.Writer, all bool) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		names, all = slices.Sorted(maps.Keys(f.Marketplaces)), true
	}
	var errs []error
	moved := false
	for _, name := range names {
		if _, ok := f.Marketplaces[name]; !ok {
			errs = append(errs, fmt.Errorf("no marketplace is named %s", name))
			continue
		}
		dir := filepath.Join(d.PluginsRoot(), "marketplaces", name)
		if !exists(dir) {
			// a sweep names a gone clone and moves on; a named update errors,
			// or its caller toasts a false "updated"
			if !all {
				errs = append(errs, fmt.Errorf("marketplace %s: its clone is missing", name))
				continue
			}
			fmt.Fprintf(out, "skipped marketplace %s: its clone is missing\n", name)
			continue
		}
		changed, err := pullMarketplace(ctx, dir, f.Marketplaces[name].Ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("marketplace %s: %w", name, err))
			continue
		}
		var installs []string
		caches := false
		for pname, reg := range f.Installs {
			if reg.Marketplace != name {
				continue
			}
			installs = append(installs, pname)
			caches = caches || reg.Kind == kindCache
		}
		slices.Sort(installs)
		var lines []string
		cacheMoved := false
		if changed || caches {
			m, err := ReadMarketplace(dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("marketplace %s: %w", name, err))
				continue
			}
			for _, pname := range installs {
				reg := f.Installs[pname]
				e, ok := m.find(reg.Entry)
				if !ok {
					lines = append(lines, fmt.Sprintf("%s is no longer in marketplace %s", pname, name))
					continue
				}
				kind := reg.Kind
				if fi, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", pname)); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
					// a legacy copy the boot could not re-link, for it needed
					// a clone: this update makes one
					kind, err = relinkInstall(ctx, d, name, pname, e, cmp.Or(reg.ID, rand.Text()))
					if err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", pname, err))
						continue
					}
				}
				src := filepath.Join(dir, e.Source.Dir)
				if kind == kindCache {
					mv, err := refreshCache(ctx, d, name, pname, e)
					if err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", pname, err))
						continue
					}
					cacheMoved = cacheMoved || mv
					src = cacheDir(d, name, pname)
				}
				if e.Version != reg.Version {
					lines = append(lines, fmt.Sprintf("%s %s -> %s", pname, cmp.Or(reg.Version, "?"), cmp.Or(e.Version, "?")))
					uses, manifest := sidecar(src, e)
					if err := recordInstall(d, pname, name, e, kind, cmp.Or(reg.ID, rand.Text()), uses, manifest); err != nil {
						errs = append(errs, err)
					}
				}
			}
		}
		if changed || cacheMoved {
			moved = true
			fmt.Fprintf(out, "marketplace %s updated\n", name)
			for _, line := range lines {
				fmt.Fprintln(out, line)
			}
		}
	}
	// a clean full run dates the sweep; a failed marketplace retries at the
	// next open
	if all && len(errs) == 0 {
		f, err := readMarketplaces(d)
		if err != nil {
			return err
		}
		f.Checked = time.Now().UTC()
		if err := f.save(d); err != nil {
			return err
		}
	}
	if !moved && len(errs) == 0 {
		fmt.Fprintln(out, MarketplacesUpToDate)
	}
	return errors.Join(errs...)
}

// sweepEvery is how long a marketplace set goes unchecked: the daily sweep
// runs when marketplaces.json's last one is older than this.
const sweepEvery = 24 * time.Hour

// AutoUpdate is the daily sweep zemi runs on open: with auto on it pulls
// every marketplace and moves the plugins installed from them, with auto off
// it only fetches and names the marketplaces whose remote moved. The news
// lines are the caller's toast, and moved says the marketplaces' material
// changed, which the config watcher should see. Checked dates only a run
// that finished clean, so a failed one retries at the next open; the lock
// keeps two windows from pulling one clone at once.
func AutoUpdate(ctx context.Context, d config.Dirs, auto bool) (news []string, moved bool, err error) {
	if err := os.MkdirAll(d.PluginsRoot(), 0o755); err != nil {
		return nil, false, err
	}
	unlock, err := filelock.Lock(filepath.Join(d.PluginsRoot(), "sweep.lock"), 30*time.Second)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	f, err := readMarketplaces(d)
	if err != nil {
		return nil, false, err
	}
	if len(f.Marketplaces) == 0 {
		return nil, false, nil
	}
	if !f.Checked.IsZero() && time.Since(f.Checked) < sweepEvery {
		return nil, false, nil
	}
	if !auto {
		mkts, err := CheckMarketplaces(ctx, d)
		if err == nil {
			if f, ferr := readMarketplaces(d); ferr == nil {
				f.Checked = time.Now().UTC()
				if ferr = f.save(d); ferr != nil {
					return news, len(mkts) > 0, ferr
				}
			}
		}
		for _, name := range mkts {
			news = append(news, "marketplace "+name+" has updates")
		}
		return news, len(mkts) > 0, err
	}
	var out strings.Builder
	err = updateMarketplaces(ctx, d, nil, &out, true)
	for line := range strings.Lines(strings.TrimSpace(out.String())) {
		if line = strings.TrimSpace(line); line != "" && line != MarketplacesUpToDate {
			news = append(news, line)
		}
	}
	return news, len(news) > 0, err
}

// installBare installs the one marketplace entry the name picks out: every
// registered marketplace is searched by its own kind's rule, and several
// matches list their candidates instead of guessing.
func installBare(ctx context.Context, d config.Dirs, name string, out io.Writer) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if len(f.Marketplaces) == 0 {
		return errors.New("no marketplace is registered: zemi installs plugins from marketplaces; add one with plugin marketplace add <source>")
	}
	var found, skipped []string
	for _, mkt := range slices.Sorted(maps.Keys(f.Marketplaces)) {
		m, err := ReadMarketplace(filepath.Join(d.PluginsRoot(), "marketplaces", mkt))
		if err != nil {
			why := "its clone is missing"
			if !errors.Is(err, fs.ErrNotExist) {
				why = err.Error()
			}
			skipped = append(skipped, mkt+": "+why)
			continue
		}
		if e, ok := m.find(name); ok {
			found = append(found, e.installName()+"@"+mkt)
		}
	}
	for _, s := range skipped {
		fmt.Fprintf(out, "skipped %s\n", s)
	}
	switch {
	case len(found) == 1:
		entry, mkt, _ := strings.Cut(found[0], "@")
		p, err := InstallFromMarketplace(ctx, d, mkt, entry)
		if err == nil {
			fmt.Fprintf(out, "%s %s (from %s)\n", p.Name, p.Version, mkt)
		}
		return err
	case len(found) > 1:
		return fmt.Errorf("several marketplaces list %s: %s; name one with plugin install <name>@<marketplace>", name, strings.Join(found, ", "))
	default:
		msg := fmt.Sprintf("no marketplace lists %q (searched %s)", name, strings.Join(slices.Sorted(maps.Keys(f.Marketplaces)), ", "))
		if len(skipped) > 0 {
			msg += "; skipped " + strings.Join(skipped, ", ")
		}
		return fmt.Errorf("%s; zemi installs plugins from marketplaces: add one with plugin marketplace add <source>", msg)
	}
}

// InstallFromMarketplace installs the entry of a registered marketplace: a
// symlink in installed/ points into the marketplace clone (a relative
// source, the root one at the clone itself), else into the entry's cache
// clone; npm and pip are refused.
func InstallFromMarketplace(ctx context.Context, d config.Dirs, mkt, entry string) (Plugin, error) {
	f, err := readMarketplaces(d)
	if err != nil {
		return Plugin{}, err
	}
	if _, ok := f.Marketplaces[mkt]; !ok {
		return Plugin{}, fmt.Errorf("no marketplace is named %s", mkt)
	}
	clone := filepath.Join(d.PluginsRoot(), "marketplaces", mkt)
	m, err := ReadMarketplace(clone)
	if err != nil {
		return Plugin{}, fmt.Errorf("marketplace %s: %w", mkt, err)
	}
	e, ok := m.find(entry)
	if !ok {
		return Plugin{}, fmt.Errorf("marketplace %s lists no plugin named %s", mkt, entry)
	}
	name := e.installName()
	if name == "" {
		return Plugin{}, fmt.Errorf("plugin %q has no usable name", e.Name)
	}
	if err := free(d, name); err != nil {
		return Plugin{}, err
	}
	kind := e.Source.Kind
	var target string
	switch kind {
	case SourceRelative, SourceGitSubdir:
		if kind == SourceRelative {
			target = clone
			if filepath.Clean(e.Source.Dir) != "." {
				target = filepath.Join(clone, e.Source.Dir)
			}
		} else {
			if target, err = ensureCache(ctx, d, mkt, name, e.Source.URL, e.Source.Ref, e.Source.SHA); err != nil {
				return Plugin{}, err
			}
			target = filepath.Join(target, e.Source.Dir)
		}
	case SourceGit, SourceURL, SourceGithub:
		if target, err = ensureCache(ctx, d, mkt, name, e.Source.URL, e.Source.Ref, e.Source.SHA); err != nil {
			return Plugin{}, err
		}
	default:
		return Plugin{}, fmt.Errorf("plugin %s: %s sources are not supported", e.Name, e.Source.Kind)
	}
	if err := linkPlugin(d, name, target, true /* on: a fresh install turns the name on */); err != nil {
		return Plugin{}, err
	}
	uses, manifest := sidecar(target, e)
	if err := recordInstall(d, name, mkt, e, kindLinkKind(kind), rand.Text(), uses, manifest); err != nil {
		return Plugin{}, err
	}
	p := load(ctx, target, false, true)
	p.Name, p.Dir = name, filepath.Join(d.PluginsRoot(), "installed", name)
	// the entry's scalars are the fallback for a source with no manifest of
	// its own; the row carries them, load read the tree
	p.Version = cmp.Or(p.Version, e.Version)
	p.Desc = cmp.Or(p.Desc, e.Description)
	return p, nil
}

// kindLinkKind is the row kind a source kind installs as: the marketplace
// clone's own material links, another repo's clones into the cache.
func kindLinkKind(kind SourceKind) InstallKind {
	if kind == SourceRelative {
		return kindLink
	}
	return kindCache
}

// cacheDirName is the dot directory under marketplaces/ holding the clones
// of entries whose source is another repo. A dot name keeps it out of every
// glob that walks marketplace clones.
const cacheDirName = ".cache"

// CacheDirName is cacheDirName for callers outside the package.
const CacheDirName = cacheDirName

// cacheDir is where the entry an install came from keeps its own clone, for
// the cache kinds whose material is not the marketplace clone itself.
func cacheDir(d config.Dirs, mkt, name string) string {
	return filepath.Join(d.PluginsRoot(), "marketplaces", cacheDirName, mkt, name)
}

// ensureCache clones url into the entry's cache clone at the pin, or fetches
// the clone that is there and moves it to the pin; an unpinned one follows
// the highest release, else the remote's default branch. A cache clone that
// will not move is re-cloned.
func ensureCache(ctx context.Context, d config.Dirs, mkt, name, url, ref, sha string) (string, error) {
	dir := cacheDir(d, mkt, name)
	if !exists(filepath.Join(dir, ".git")) {
		if err := cloneRepo(ctx, url, dir); err != nil {
			return "", err
		}
		if err := checkoutRepo(ctx, dir, ref, sha); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
		return dir, nil
	}
	if _, err := git(ctx, dir, "fetch", "--quiet", "--tags", "origin"); err != nil {
		os.RemoveAll(dir)
		return ensureCache(ctx, d, mkt, name, url, ref, sha)
	}
	to := cmp.Or(sha, ref)
	if to == "" {
		// an unpinned source follows the highest release, else the branch
		tags, _ := git(ctx, dir, "tag", "--list")
		if top := highest(tags, ""); top != "" {
			to = "refs/tags/" + top
		} else {
			to = "origin/HEAD"
		}
	}
	if _, err := git(ctx, dir, "checkout", "--quiet", to); err != nil {
		os.RemoveAll(dir)
		return ensureCache(ctx, d, mkt, name, url, ref, sha)
	}
	if _, err := git(ctx, dir, "reset", "--quiet", "--hard", to); err != nil {
		os.RemoveAll(dir)
		return ensureCache(ctx, d, mkt, name, url, ref, sha)
	}
	return dir, nil
}

// refreshCache moves an installed cache-kind install to what the entry now
// pins, reporting whether the clone's HEAD moved; the clone is a cache, so a
// checkout that will not move re-clones.
func refreshCache(ctx context.Context, d config.Dirs, mkt, name string, e MarketplacePlugin) (bool, error) {
	dir := cacheDir(d, mkt, name)
	was, _ := git(ctx, dir, "rev-parse", "HEAD")
	if _, err := ensureCache(ctx, d, mkt, name, e.Source.URL, e.Source.Ref, e.Source.SHA); err != nil {
		return false, err
	}
	now, _ := git(ctx, dir, "rev-parse", "HEAD")
	return was != now, nil
}

// relinkInstall re-links a legacy real-dir install into its marketplace
// material, making the cache clone a git-sourced entry needs, and records
// the row the link's kind. It is the EnsureLinks case that wanted a network,
// run where one is allowed.
func relinkInstall(ctx context.Context, d config.Dirs, mkt, name string, e MarketplacePlugin, id string) (InstallKind, error) {
	kind := kindLinkKind(e.Source.Kind)
	target := filepath.Join(d.PluginsRoot(), "marketplaces", mkt)
	if filepath.Clean(e.Source.Dir) != "." && e.Source.Dir != "" {
		target = filepath.Join(target, e.Source.Dir)
	}
	if kind == kindCache {
		t, err := ensureCache(ctx, d, mkt, name, e.Source.URL, e.Source.Ref, e.Source.SHA)
		if err != nil {
			return "", err
		}
		target = t
		if e.Source.Kind == SourceGitSubdir && e.Source.Dir != "" {
			target = filepath.Join(t, e.Source.Dir)
		}
	}
	if err := linkPlugin(d, name, target, false); err != nil {
		return "", err
	}
	uses, manifest := sidecar(target, e)
	return kind, recordInstall(d, name, mkt, e, kind, id, uses, manifest)
}

// sidecar is what the row carries for an entry whose source holds no manifest
// of its own: the entry's declared capabilities and the entry itself as a
// manifest, so a symlinked install loads and grants without one. A source
// with a manifest carries nothing.
func sidecar(src string, e MarketplacePlugin) (uses []string, manifest jsontext.Value) {
	if manifestBytes(src) != nil || readHead(filepath.Join(src, ClaudeManifestFile)) != nil {
		return nil, nil
	}
	return e.Uses, e.entryManifest()
}

// entryManifest renders the entry as the manifest an install without one of
// its own loads, zemi's shape for a zemi entry, Claude's otherwise.
func (e MarketplacePlugin) entryManifest() jsontext.Value {
	if e.Kind == KindZemi {
		var author jsontext.Value
		if e.Author.Name != "" {
			author, _ = json.Marshal(e.Author)
		}
		b, _ := json.Marshal(struct {
			Version     string         `json:"version,omitempty"`
			Description string         `json:"description,omitempty"`
			Homepage    string         `json:"homepage,omitempty"`
			Author      jsontext.Value `json:"author,omitempty"`
			Uses        []string       `json:"uses,omitempty"`
		}{e.Version, e.Description, e.Homepage, author, e.Uses})
		return b
	}
	b, _ := e.manifestJSON()
	return b
}

// EnsureLinks makes installed/ match the registry: every row's plugin is a
// symlink into its marketplace material. It migrates the installs of the
// copy model - a relative one re-links into the clone, a git one moves its
// clone into the cache - and needs no network; the git-subdir copies of the
// copy model wait for the next marketplace update, which re-links them with
// their cache clone.
func EnsureLinks(d config.Dirs) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(f.Installs)) {
		reg := f.Installs[name]
		dest := filepath.Join(d.PluginsRoot(), "installed", name)
		fi, err := os.Lstat(dest)
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			continue // gone, or already a link
		}
		clone := filepath.Join(d.PluginsRoot(), "marketplaces", reg.Marketplace)
		if !exists(clone) {
			continue // its clone is gone; there is nothing to link into
		}
		m, err := ReadMarketplace(clone)
		if err != nil {
			continue
		}
		e, ok := m.find(reg.Entry)
		if !ok {
			continue
		}
		kind := e.Source.Kind
		var target string
		var uses []string
		var manifest jsontext.Value
		rowKind := kindLink
		switch {
		case kind == SourceRelative && filepath.Clean(e.Source.Dir) == ".":
			// the marketplace's own repo is the plugin
			target = clone
			uses, manifest = sidecar(target, e)
		case kind == SourceRelative:
			target = filepath.Join(clone, e.Source.Dir)
			uses, manifest = sidecar(target, e)
		case reg.Kind == kindGit:
			// a legacy git-kind row: its clone is the entry's repo, and it
			// moves into the cache whole, where its updates find it
			cache := cacheDir(d, reg.Marketplace, name)
			if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
				return err
			}
			os.RemoveAll(cache)
			if err := os.Rename(dest, cache); err != nil {
				return err
			}
			target, uses, manifest, rowKind = cache, e.Uses, e.entryManifest(), kindCache
		default:
			continue // a subtree copy of another repo needs a clone; an update re-links it
		}
		if err := linkPlugin(d, name, target, false); err != nil {
			return err
		}
		if err := recordInstall(d, name, reg.Marketplace, e, rowKind, reg.ID, uses, manifest); err != nil {
			return err
		}
	}
	return nil
}

// linkPlugin points installed/name at target with a relative symlink,
// placed by a rename so a reload never reads a half-placed install, and
// turns a name disabled before back on when on says the act was deliberate.
func linkPlugin(d config.Dirs, name, target string, on bool) error {
	dest := filepath.Join(d.PluginsRoot(), "installed", name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(dest), target)
	if err != nil {
		return err
	}
	staging := filepath.Join(d.PluginsRoot(), "installed", "."+name+".link.tmp")
	os.RemoveAll(staging)
	if err := os.Symlink(rel, staging); err != nil {
		return err
	}
	// a rename replaces a link in one step; a legacy real tree moves aside
	// first, so nothing reads it half-deleted
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		del := filepath.Join(filepath.Dir(dest), "."+name+".del")
		os.RemoveAll(del)
		if err := os.Rename(dest, del); err != nil {
			os.RemoveAll(staging)
			return err
		}
		defer os.RemoveAll(del)
	}
	if err := os.Rename(staging, dest); err != nil {
		os.RemoveAll(staging)
		return err
	}
	if on {
		// a fresh install is a deliberate act, so a name disabled before is
		// on again
		return setDisabled(d, name, false)
	}
	return nil
}

// pullMarketplace fetches the marketplace clone and hard-resets it to the
// remote's default branch, or to the ref the row pins, dropping local state:
// the clone is a cache. It reports whether the reset moved HEAD, the
// movement a versionless entry has to show for an update.
func pullMarketplace(ctx context.Context, dir, ref string) (bool, error) {
	nctx, cancel := context.WithTimeout(ctx, netTimeout)
	defer cancel()
	fetch := []string{"fetch", "--quiet", "--tags"}
	if ref != "" {
		fetch = append(fetch, "origin", ref)
	}
	if _, err := git(nctx, dir, fetch...); err != nil {
		return false, err
	}
	// the reset is local and finishes even when the update is cancelled
	ctx = context.WithoutCancel(ctx)
	was, _ := git(ctx, dir, "rev-parse", "HEAD")
	to := "origin/HEAD"
	if ref != "" {
		to = "FETCH_HEAD"
	}
	if _, err := git(ctx, dir, "reset", "--quiet", "--hard", to); err != nil {
		return false, err
	}
	now, _ := git(ctx, dir, "rev-parse", "HEAD")
	return was != now, nil
}

func load(ctx context.Context, dir string, local, enabled bool) Plugin {
	p := Plugin{Name: filepath.Base(dir), Dir: dir, Local: local, Enabled: enabled}
	// the manifest is the plugin's own answer; init.lua's line fills in only
	// what it leaves out
	m := manifestOf(dir)
	p.Version, p.Desc, p.Uses = m.Version, m.Description, m.Uses
	p.Author, p.Homepage = authorName(m.Author), m.Homepage
	if line := firstLine(filepath.Join(dir, InitFile)); p.Desc == "" && strings.HasPrefix(line, "--") {
		p.Desc = strings.TrimSpace(strings.TrimLeft(line, "-"))
	}
	// git would read the config of whatever repository a parent dir or a
	// symlink names: an installed plugin always has its own .git
	if fi, err := os.Lstat(dir); local || err != nil || !fi.IsDir() {
		return p
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		return p
	}
	p.URL, _ = git(ctx, dir, "config", "--get", "remote.origin.url")
	here, _ := git(ctx, dir, "tag", "--points-at", "HEAD")
	if tag := highest(here, ""); tag != "" {
		p.revision = tag
		p.Version = cmp.Or(p.Version, tag)
		return p
	}
	p.revision, _ = git(ctx, dir, "rev-parse", "--short", "HEAD")
	if p.Version == "" {
		p.Version = p.revision
	}
	return p
}

// firstLine reads line 1 of a regular file, or "" for anything else.
func firstLine(path string) string {
	line, _, _ := strings.Cut(string(readHead(path)), "\n")
	return line
}

// readHead reads the first 4 KB of a regular file: a clone can hold a symlink
// to /dev/zero or a FIFO, which would never end.
func readHead(path string) []byte {
	lfi, err := os.Lstat(path)
	if err != nil || !lfi.Mode().IsRegular() {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	// the file opened must be the one checked, not a link swapped in since
	if fi, err := f.Stat(); err != nil || !os.SameFile(lfi, fi) {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(f, 4<<10))
	return data
}

// idFile, in a clone's .git, holds the random id an install gave it.
const idFile = "zemi-install"

// InstallID tells one install of a plugin from another under the same name,
// so the host can drop the state a deleted one left; "" for a local plugin.
func InstallID(dir string) string {
	return strings.TrimSpace(firstLine(filepath.Join(dir, ".git", idFile)))
}

// highest returns the highest vX.Y.Z tag among the lines of tags in major
// ("v1"), or in any major when major is "". A prerelease, a build suffix or
// a shorthand like v1.2 is no release.
func highest(tags, major string) string {
	best := ""
	for t := range strings.FieldsSeq(tags) {
		if semver.Canonical(t) == t && semver.Prerelease(t) == "" &&
			(major == "" || semver.Major(t) == major) && semver.Compare(t, best) > 0 {
			best = t
		}
	}
	return best
}

// free refuses a bad name and one a local or installed plugin already has.
func free(d config.Dirs, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%q is not a plugin name: lowercase letters, digits, '_' and '-'", name)
	}
	for _, kind := range []string{"local", "installed"} {
		if dir := filepath.Join(d.PluginsRoot(), kind, name); exists(dir) {
			return fmt.Errorf("a plugin is already named %s: %s", name, dir)
		}
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func disabled(d config.Dirs) (set.Set[string], error) {
	data, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "disabled"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	off := set.Set[string]{}
	for line := range strings.FieldsSeq(string(data)) {
		off.Add(line)
	}
	return off, nil
}

// setDisabled rewrites disabled through a temp file and a rename, so a CLI
// and a TUI writing at once never leave a torn file.
// NOTE: two concurrent writes can keep only one change; an flock on the root
// would serialize them.
func setDisabled(d config.Dirs, name string, off bool) error {
	cur, err := disabled(d)
	if err != nil || cur.Has(name) == off {
		return err
	}
	if off {
		cur.Add(name)
	} else {
		cur.Remove(name)
	}
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(cur)) {
		b.WriteString(n + "\n")
	}
	return config.WriteFileAtomic(filepath.Join(d.PluginsRoot(), "disabled"), []byte(b.String()))
}

// git runs git on a plugin repository, blanking the user's filter drivers too.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := proc.Git{Dir: dir, Foreign: true}.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
