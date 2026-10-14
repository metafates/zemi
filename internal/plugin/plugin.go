// Package plugin manages the plugin tree under <data>/plugins: git clones
// in installed/, directories the user develops in place in local/, and the
// names turned off in disabled. Plugins come from a marketplace, whose
// clones live in marketplaces/ as the cache installs and copies update
// from. It runs no Lua and needs no Host, so the /plugins command and the
// zemi plugins CLI share it.
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
	"github.com/metafates/zemi/internal/proc"
)

// Plugin is one directory under installed/ or local/.
type Plugin struct {
	Name, Dir string
	Desc      string // the manifest's description, else line 1 of init.lua when it is a -- comment
	URL       string // the origin remote; "" for a local plugin
	Version   string // the manifest's version, else the checked-out semver tag, else the short commit
	Latest    string // a newer tag in the same major, "v2.0.0 (major)" or "3 new commits"
	Local     bool
	Enabled   bool
	Uses      []string // the manifest's declared capabilities: what enabling it grants

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
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Uses        []string `json:"uses"`
}

// Uses is the vocabulary a manifest's "uses" draws from, one word per host
// capability a plugin may call: what switching the plugin on grants.
var Uses = []string{"agent", "fs.write", "http", "job", "model", "tool"}

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

// manifestOf parses the manifest of the plugin in dir; a missing or malformed
// one is the zero manifest, which declares nothing.
func manifestOf(dir string) manifest {
	var m manifest
	_ = json.Unmarshal(manifestBytes(dir), &m)
	return m
}

// a dot dir, such as a clone in flight, is never a plugin, and no name
// holds a dot, which require("<name>") would read as a directory
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// netTimeout bounds a clone or a fetch; other git calls get proc.Git's.
const netTimeout = 2 * time.Minute

// ErrUsage is Run's error for a verb or arguments it does not take.
var ErrUsage = errors.New("usage: plugins marketplace add <url> | marketplace list | marketplace remove <name> | marketplace update [name] | install <name>[@<marketplace>] | update [name...] | check | enable <name> | disable <name> | delete <name>")

// List reads every plugin, local ones first, each group by name, with the
// marketplace each install came from. It runs only local git reads.
func List(ctx context.Context, d config.Dirs) ([]Plugin, error) {
	ps, err := Scan(d)
	if err != nil {
		return nil, err
	}
	mf, err := readMarketplaces(d)
	if err != nil {
		return nil, err
	}
	for i, p := range ps {
		provides := p.Provides
		ps[i] = load(ctx, p.Dir, p.Local, p.Enabled)
		ps[i].Provides = provides
		if reg, ok := mf.Installs[p.Name]; ok {
			ps[i].Marketplace = reg.Marketplace
		}
	}
	return ps, nil
}

// Scan is List without git: Name, Dir, Local, Enabled and the manifest's
// declared uses, for the boot step that loads plugins.
func Scan(d config.Dirs) ([]Plugin, error) {
	off, err := disabled(d)
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
			// a symlink is listed so the loader can say why it refuses it
			if nameRe.MatchString(e.Name()) && (e.IsDir() || e.Type()&fs.ModeSymlink != 0) {
				root := filepath.Join(dir, e.Name())
				p := Plugin{Name: e.Name(), Dir: root, Local: kind == "local", Enabled: !off[e.Name()], Uses: manifestOf(root).Uses}
				if IsClaude(root) {
					p.Provides = claudeProvides(root)
				}
				ps = append(ps, p)
			}
		}
	}
	return ps, nil
}

// Code reports whether an entry of a plugin's root holds what zemi loads or
// its tests read: CodeDir, and the dirs a plugin layers over the user's. The
// copier and the read jail take these entries whole.
func Code(name string) bool {
	return slices.Contains([]string{CodeDir, "skills", "agents", "commands"}, name)
}

// OldLayout is why a plugin in the layout before CodeDir cannot load, nil for
// one that keeps its Lua where zemi reads it: that root entry is what the
// loader no longer reaches.
func OldLayout(dir string) error {
	for _, name := range []string{"init.lua", "lua", "tests", "workflows"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%s is at the plugin root, where zemi no longer loads it: move init.lua, lua/, tests/ and workflows/ under %s/", name, CodeDir)
		}
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

// installURL resolves an install source: a local path (as git tells it from
// host:path) is made absolute, since git would resolve a relative one
// against the plugins tree; a URL-like flag is refused early.
func installURL(url string) (string, error) {
	if strings.HasPrefix(url, "-") {
		return "", fmt.Errorf("%q is not a git URL", url)
	}
	if i := strings.IndexByte(url, ':'); i < 0 || strings.Contains(url[:i], "/") {
		return filepath.Abs(url)
	}
	return url, nil
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

// place moves a fresh clone into installed/name and returns it with the
// install id written into .git: the id separates its state from a previous
// install of the same name, and the rename keeps the config watcher off a
// half-moved tree.
func place(ctx context.Context, d config.Dirs, name, tmp string) (Plugin, string, error) {
	id := rand.Text()
	if err := os.WriteFile(filepath.Join(tmp, ".git", idFile), []byte(id+"\n"), 0o644); err != nil {
		return Plugin{}, "", err
	}
	if err := free(d, name); err != nil {
		return Plugin{}, "", err
	}
	dest := filepath.Join(d.PluginsRoot(), "installed", name)
	if err := os.Rename(tmp, dest); err != nil {
		return Plugin{}, "", err
	}
	// installing is a deliberate act, so a name disabled before is on again
	if err := setDisabled(d, name, false); err != nil {
		return Plugin{}, "", err
	}
	return load(ctx, dest, false, true), id, nil
}

// update fetches a plugin and checks out its highest semver tag of the same
// major when it is on a tag already; a branch moves to the highest tag ahead
// of it, or else fast-forward. A dirty checkout makes git refuse, and that
// error is returned as is.
func update(ctx context.Context, d config.Dirs, name string) (Plugin, error) {
	dir, err := clone(d, name)
	if err != nil {
		return Plugin{}, err
	}
	nctx, cancel := context.WithTimeout(ctx, netTimeout)
	defer cancel()
	if _, err := git(nctx, dir, "fetch", "--quiet", "--tags"); err != nil {
		return Plugin{}, err
	}
	// a cancel stops the fetch only: a killed merge or checkout would leave
	// .git/index.lock and a half-moved worktree the next boot loads
	ctx = context.WithoutCancel(ctx)
	to := ""
	here, _ := git(ctx, dir, "tag", "--points-at", "HEAD")
	if cur := highest(here, ""); cur != "" {
		tags, _ := git(ctx, dir, "tag", "--list")
		if top := highest(tags, semver.Major(cur)); semver.Compare(top, cur) > 0 {
			to = top
		}
	} else if to = tagAhead(ctx, dir); to == "" {
		if _, err := git(ctx, dir, "merge", "--quiet", "--ff-only", "@{upstream}"); err != nil {
			return Plugin{}, err
		}
	}
	if to != "" {
		if _, err := git(ctx, dir, "checkout", "--quiet", "refs/tags/"+to); err != nil {
			return Plugin{}, err
		}
	}
	off, err := disabled(d)
	if err != nil {
		return Plugin{}, err
	}
	return load(ctx, dir, false, !off[name]), nil
}

// Check fetches every installed plugin and every marketplace a copy came
// from, so Latest reads the remotes' state from local refs from then on. A
// marketplace clone is fetched but never reset: the manifest is read off the
// fetched ref, so a concurrent update's worktree is left alone.
func Check(ctx context.Context, d config.Dirs) ([]Plugin, error) {
	ps, err := List(ctx, d)
	if err != nil {
		return nil, err
	}
	mf, err := readMarketplaces(d)
	if err != nil {
		return nil, err
	}
	var errs []error
	copies := map[string][]int{}
	for i, p := range ps {
		if p.Local {
			continue
		}
		if reg, ok := mf.Installs[p.Name]; ok && reg.Kind != kindGit {
			copies[reg.Marketplace] = append(copies[reg.Marketplace], i)
			continue
		}
		dir, err := clone(d, p.Name)
		if err != nil {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
			continue // not a clone zemi can fetch
		}
		nctx, cancel := context.WithTimeout(ctx, netTimeout)
		_, err = git(nctx, p.Dir, "fetch", "--quiet", "--tags")
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
			continue
		}
		ps[i] = load(ctx, p.Dir, false, p.Enabled)
	}
	for _, mkt := range slices.Sorted(maps.Keys(copies)) {
		dir := filepath.Join(d.PluginsRoot(), "marketplaces", mkt)
		if !exists(dir) {
			continue // its clone is gone; there is nothing to compare against
		}
		nctx, cancel := context.WithTimeout(ctx, netTimeout)
		_, err := git(nctx, dir, "fetch", "--quiet", "--tags")
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("marketplace %s: %w", mkt, err))
			continue
		}
		m, err := readMarketplaceRef(ctx, dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("marketplace %s: %w", mkt, err))
			continue
		}
		head, _ := git(ctx, dir, "rev-parse", "HEAD")
		origin, _ := git(ctx, dir, "rev-parse", "origin/HEAD")
		moved := head != "" && origin != "" && head != origin
		for _, i := range copies[mkt] {
			reg := mf.Installs[ps[i].Name]
			e, ok := m.find(reg.Entry)
			if !ok {
				continue
			}
			switch {
			case e.Version != "" && e.Version != reg.Version:
				ps[i].Latest = e.Version
				ps[i].Version = cmp.Or(ps[i].Version, reg.Version)
			case moved:
				ps[i].Latest = "updated in the marketplace (" + mkt + ")"
			}
		}
	}
	return ps, errors.Join(errs...)
}

// remove deletes the installed clone of that name, else the local plugin. A
// symlink in local/ loses the link only.
func remove(d config.Dirs, name string) error {
	dir, err := clone(d, name)
	if errors.Is(err, errLocal) {
		dir, err = filepath.Join(d.PluginsRoot(), "local", name), nil
	}
	if err != nil {
		return err
	}
	// moved aside first, so the config watcher never loads half of it
	del := filepath.Join(filepath.Dir(dir), "."+name+".del")
	os.RemoveAll(del)
	if err := os.Rename(dir, del); err != nil {
		return err
	}
	if err := os.RemoveAll(del); err != nil {
		return err
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

// UpToDate is what check and update print when no plugin has news.
const UpToDate = "Plugins up to date"

// Run runs one verb of the zemi plugins CLI, which /plugins shares; each
// prints plain lines a toast can show.
func Run(ctx context.Context, d config.Dirs, args []string, out io.Writer) error {
	if len(args) == 0 {
		return ErrUsage
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
			return updateMarketplaces(ctx, d, rest[1:], out)
		}
		return ErrUsage
	case verb == "install" && len(rest) == 1:
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
			return fmt.Errorf("%s is not a plugin name: zemi installs plugins from marketplaces; add one with plugins marketplace add <url>", arg)
		}
		return installBare(ctx, d, arg, out)
	case verb == "update":
		ps, err := List(ctx, d)
		if err != nil {
			return err
		}
		mf, err := readMarketplaces(d)
		if err != nil {
			return err
		}
		was, all := map[string]string{}, len(rest) == 0
		for _, p := range ps {
			if !p.Local {
				was[p.Name] = p.revision
				if all {
					rest = append(rest, p.Name)
				}
			}
		}
		// copies group by marketplace, so one update pulls each clone once;
		// everything else updates through its own git
		groups := map[string][]string{}
		var clones []string
		for _, name := range rest {
			if reg, ok := mf.Installs[name]; ok && reg.Kind != kindGit {
				groups[reg.Marketplace] = append(groups[reg.Marketplace], name)
			} else {
				clones = append(clones, name)
			}
		}
		var errs []error
		moved := false
		for _, mkt := range slices.Sorted(maps.Keys(groups)) {
			m, err := updateCopies(ctx, d, mkt, groups[mkt], mf, out)
			moved = moved || m
			if err != nil {
				errs = append(errs, err)
			}
		}
		for _, name := range clones {
			p, err := update(ctx, d, name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			if p.revision != was[name] {
				fmt.Fprintf(out, "%s %s -> %s\n", p.Name, was[name], p.revision)
				moved = true
			}
			if reg, ok := mf.Installs[name]; ok {
				if err := refreshInstall(d, name, reg); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", name, err))
				}
			}
		}
		if !moved && len(errs) == 0 {
			fmt.Fprintln(out, UpToDate)
		}
		return errors.Join(errs...)
	case verb == "check" && len(rest) == 0:
		ps, err := Check(ctx, d)
		n := 0
		for _, p := range ps {
			if p.Latest == "" {
				continue
			}
			if p.Version == "" {
				fmt.Fprintf(out, "%s %s\n", p.Name, p.Latest)
			} else {
				fmt.Fprintf(out, "%s %s -> %s\n", p.Name, p.Version, p.Latest)
			}
			n++
		}
		if n == 0 && err == nil {
			fmt.Fprintln(out, UpToDate)
		}
		return err
	case (verb == "enable" || verb == "disable") && len(rest) == 1:
		return SetEnabled(d, rest[0], verb == "enable")
	case verb == "delete" && len(rest) == 1:
		return remove(d, rest[0])
	}
	return ErrUsage
}

// The install kinds of a registry row.
const (
	kindCopy = "copy"
	kindGit  = "git"
)

// marketplaceFile is marketplaces.json under the plugins root: the
// registered marketplaces and, per installed plugin, where it came from and
// how it updates.
type marketplaceFile struct {
	Marketplaces map[string]marketplaceReg `json:"marketplaces"`
	Installs     map[string]installReg     `json:"installs"`
}

type marketplaceReg struct {
	Source string    `json:"source"`
	Added  time.Time `json:"added"`
}

// installReg is the provenance of a plugin installed from a marketplace: a
// copy re-copies from the clone, a git one updates from its own repo.
type installReg struct {
	Marketplace string `json:"marketplace"`
	Entry       string `json:"entry"`
	Version     string `json:"version"`
	Kind        string `json:"kind"` // copy or git; a row without one is an older copy
	ID          string `json:"id"`   // the install's identity, shared with .git/zemi-install for a git install
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
	Kind, ID                    string
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

// recordInstall writes the provenance of an installed entry; an existing row
// keeps its id, so an update never resets the identity its state hangs on.
func recordInstall(d config.Dirs, name, mkt string, e MarketplacePlugin, kind, id string) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if old := f.Installs[name]; old.ID != "" {
		id = old.ID
	}
	if f.Installs == nil {
		f.Installs = map[string]installReg{}
	}
	f.Installs[name] = installReg{Marketplace: mkt, Entry: e.Name, Version: e.Version, Kind: kind, ID: id}
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
// manifest lists, what list, completion and the overlay display.
type MarketplaceInfo struct {
	Name, Source string
	Added        time.Time
	Entries      []MarketplaceEntry
}

// MarketplaceEntry is one entry of a registered marketplace, display-only.
type MarketplaceEntry struct {
	Name, Version, Description string
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
		info := MarketplaceInfo{Name: name, Source: reg.Source, Added: reg.Added}
		if m, err := ReadMarketplace(filepath.Join(d.PluginsRoot(), "marketplaces", name)); err == nil {
			for _, e := range m.Plugins {
				info.Entries = append(info.Entries, MarketplaceEntry{Name: e.Name, Version: e.Version, Description: e.Description})
			}
		}
		out = append(out, info)
	}
	return out, nil
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
// sanitized. A re-add of the same source just refreshes the clone.
func addMarketplace(ctx context.Context, d config.Dirs, url string, out io.Writer) error {
	source, err := installURL(url)
	if err != nil {
		return err
	}
	dir := filepath.Join(d.PluginsRoot(), "marketplaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// one fixed tmp, since the manifest decides the destination and no name
	// can be derived from the URL; concurrent adds serialize on it
	tmp := filepath.Join(dir, ".add.tmp")
	if err := cloneRepo(ctx, source, tmp); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
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
	if old, ok := f.Marketplaces[name]; ok && old.Source != source {
		return fmt.Errorf("marketplace %s is already registered from %s, not %s", name, old.Source, source)
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
		f.Marketplaces[name] = marketplaceReg{Source: source, Added: time.Now().UTC()}
		if err := f.save(d); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "marketplace %s added (%d plugins)\n", name, len(m.Plugins))
	return nil
}

// removeMarketplace drops a marketplace: its clone and registry row go,
// unless a copy install still needs the clone to update from.
func removeMarketplace(d config.Dirs, name string, out io.Writer) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if _, ok := f.Marketplaces[name]; !ok {
		return fmt.Errorf("no marketplace is named %s", name)
	}
	var copies, gits []string
	for _, pname := range slices.Sorted(maps.Keys(f.Installs)) {
		reg := f.Installs[pname]
		if reg.Marketplace != name {
			continue
		}
		if reg.Kind == kindGit {
			gits = append(gits, pname)
		} else {
			copies = append(copies, pname)
		}
	}
	if len(copies) > 0 {
		return fmt.Errorf("marketplace %s still has copy installs that update from it: %s; delete them first", name, strings.Join(copies, ", "))
	}
	for _, pname := range gits {
		// a git install is its own clone and keeps working without the cache
		delete(f.Installs, pname)
		fmt.Fprintf(out, "%s keeps its own clone, no longer updated via %s\n", pname, name)
	}
	delete(f.Marketplaces, name)
	if err := f.save(d); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(d.PluginsRoot(), "marketplaces", name)); err != nil {
		return err
	}
	fmt.Fprintf(out, "marketplace %s removed\n", name)
	return nil
}

// updateMarketplaces pulls one or all marketplace clones and names what
// moved and the copy installs now behind it.
func updateMarketplaces(ctx context.Context, d config.Dirs, names []string, out io.Writer) error {
	f, err := readMarketplaces(d)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		names = slices.Sorted(maps.Keys(f.Marketplaces))
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
			errs = append(errs, fmt.Errorf("marketplace %s has no clone", name))
			continue
		}
		changed, err := pullMarketplace(ctx, dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("marketplace %s: %w", name, err))
			continue
		}
		if !changed {
			continue
		}
		moved = true
		fmt.Fprintf(out, "marketplace %s updated\n", name)
		var behind []string
		for _, pname := range slices.Sorted(maps.Keys(f.Installs)) {
			if reg := f.Installs[pname]; reg.Marketplace == name && reg.Kind != kindGit {
				behind = append(behind, pname)
			}
		}
		if len(behind) > 0 {
			fmt.Fprintf(out, "  behind it: %s\n", strings.Join(behind, ", "))
		}
	}
	if !moved && len(errs) == 0 {
		fmt.Fprintln(out, "marketplaces up to date")
	}
	return errors.Join(errs...)
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
		return errors.New("no marketplace is registered: zemi installs plugins from marketplaces; add one with plugins marketplace add <url>")
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
		return fmt.Errorf("several marketplaces list %s: %s; name one with plugins install <name>@<marketplace>", name, strings.Join(found, ", "))
	default:
		msg := fmt.Sprintf("no marketplace lists %q (searched %s)", name, strings.Join(slices.Sorted(maps.Keys(f.Marketplaces)), ", "))
		if len(skipped) > 0 {
			msg += "; skipped " + strings.Join(skipped, ", ")
		}
		return fmt.Errorf("%s; zemi installs plugins from marketplaces: add one with plugins marketplace add <url>", msg)
	}
}

// InstallFromMarketplace installs the entry of a registered marketplace: a
// relative source copies its subtree into installed/ as a plain dir, a git
// one clones it there, npm and pip are refused.
func InstallFromMarketplace(ctx context.Context, d config.Dirs, mkt, entry string) (Plugin, error) {
	f, err := readMarketplaces(d)
	if err != nil {
		return Plugin{}, err
	}
	if _, ok := f.Marketplaces[mkt]; !ok {
		return Plugin{}, fmt.Errorf("no marketplace is named %s", mkt)
	}
	m, err := ReadMarketplace(filepath.Join(d.PluginsRoot(), "marketplaces", mkt))
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
	switch e.Source.Kind {
	case "git", "url", "github":
		tmp := filepath.Join(d.PluginsRoot(), "installed", "."+name+".tmp")
		if err := cloneRepo(ctx, e.Source.URL, tmp); err != nil {
			return Plugin{}, err
		}
		defer os.RemoveAll(tmp)
		if err := checkoutRepo(ctx, tmp, e.Source.Ref, e.Source.SHA); err != nil {
			return Plugin{}, err
		}
		synthed, err := e.synthManifest(tmp)
		if err != nil {
			return Plugin{}, err
		}
		p, id, err := place(ctx, d, name, tmp)
		if err != nil {
			return Plugin{}, err
		}
		if err := recordInstall(d, name, mkt, e, kindGit, id); err != nil {
			return Plugin{}, err
		}
		if synthed {
			// the entry's scalars sit in a manifest load does not read yet
			p.Version = cmp.Or(p.Version, e.Version)
			p.Desc = cmp.Or(p.Desc, e.Description)
		}
		return p, nil
	case "relative", "git-subdir":
		return placeCopy(ctx, d, mkt, e, name, true)
	default:
		return Plugin{}, fmt.Errorf("plugin %s: %s sources are not supported", e.Name, e.Source.Kind)
	}
}

// copySource resolves where an entry's copy comes from: its dir in the
// marketplace clone, or a fresh clone of the repo a git-subdir entry names.
func copySource(ctx context.Context, d config.Dirs, mkt string, e MarketplacePlugin) (src string, cleanup func(), err error) {
	if e.Source.Kind != "git-subdir" {
		return filepath.Join(d.PluginsRoot(), "marketplaces", mkt, e.Source.Dir), func() {}, nil
	}
	tmp := filepath.Join(d.PluginsRoot(), "installed", "."+e.installName()+".tmp")
	if err := cloneRepo(ctx, e.Source.URL, tmp); err != nil {
		return "", nil, err
	}
	if err := checkoutRepo(ctx, tmp, e.Source.Ref, e.Source.SHA); err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	return filepath.Join(tmp, e.Source.Dir), func() { os.RemoveAll(tmp) }, nil
}

// placeCopy (re)copies the entry's subtree into installed/<name>: a plain
// dir, no .git, so scan, sweep and uninstall treat it like any other
// install. The entry becomes the manifest when the dir has none of its own.
// A copy is staged and renamed, so a reload never reads half of it.
func placeCopy(ctx context.Context, d config.Dirs, mkt string, e MarketplacePlugin, name string, on bool) (Plugin, error) {
	src, done, err := copySource(ctx, d, mkt, e)
	if err != nil {
		return Plugin{}, err
	}
	defer done()
	dest := filepath.Join(d.PluginsRoot(), "installed", name)
	// .copy.tmp: copySource may be reading a clone staged at .<name>.tmp
	staging := filepath.Join(d.PluginsRoot(), "installed", "."+name+".copy.tmp")
	// NOTE: os.CopyFS refuses a symlink in the subtree; a plugin carrying
	// one fails the copy until CopyFS learns to copy them
	os.RemoveAll(staging)
	if err := os.CopyFS(staging, os.DirFS(src)); err != nil {
		os.RemoveAll(staging)
		return Plugin{}, err
	}
	synth, err := e.synthManifest(staging)
	if err != nil {
		os.RemoveAll(staging)
		return Plugin{}, err
	}
	if err := os.RemoveAll(dest); err != nil {
		os.RemoveAll(staging)
		return Plugin{}, err
	}
	if err := os.Rename(staging, dest); err != nil {
		os.RemoveAll(staging)
		return Plugin{}, err
	}
	off, err := disabled(d)
	if err != nil {
		return Plugin{}, err
	}
	if on {
		// a fresh install is a deliberate act, so a name disabled before is
		// on again; an update keeps the switch as it is
		if err := setDisabled(d, name, false); err != nil {
			return Plugin{}, err
		}
		off = nil
	}
	if err := recordInstall(d, name, mkt, e, kindCopy, rand.Text()); err != nil {
		return Plugin{}, err
	}
	p := load(ctx, dest, false, !off[name])
	if synth {
		// the entry's scalars sit in a manifest load does not read yet
		p.Version = cmp.Or(p.Version, e.Version)
		p.Desc = cmp.Or(p.Desc, e.Description)
	}
	return p, nil
}

// updateCopies re-copies the named copies from one marketplace clone,
// pulled once for the group; a pull that moved nothing and an entry whose
// version is unchanged re-copy and record nothing.
func updateCopies(ctx context.Context, d config.Dirs, mkt string, names []string, mf marketplaceFile, out io.Writer) (bool, error) {
	dir := filepath.Join(d.PluginsRoot(), "marketplaces", mkt)
	if !exists(dir) {
		return false, fmt.Errorf("marketplace %s is not installed", mkt)
	}
	changed, err := pullMarketplace(ctx, dir)
	if err != nil {
		return false, fmt.Errorf("marketplace %s: %w", mkt, err)
	}
	m, err := ReadMarketplace(dir)
	if err != nil {
		return false, fmt.Errorf("marketplace %s: %w", mkt, err)
	}
	var errs []error
	moved := false
	for _, name := range names {
		reg := mf.Installs[name]
		e, ok := m.find(reg.Entry)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: marketplace %s no longer lists %s", name, mkt, reg.Entry))
			continue
		}
		if in := e.installName(); in != name {
			errs = append(errs, fmt.Errorf("%s is now named %s: delete and install it again", reg.Entry, in))
			continue
		}
		if !changed && reg.Version == e.Version {
			continue
		}
		if _, err := placeCopy(ctx, d, mkt, e, name, false); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if reg.Version != e.Version {
			fmt.Fprintf(out, "%s %s -> %s\n", name, reg.Version, e.Version)
		} else {
			fmt.Fprintf(out, "%s updated\n", name)
		}
		moved = true
	}
	return moved, errors.Join(errs...)
}

// refreshInstall points a git-kind row's recorded version at what its
// marketplace clone currently lists, so the next check compares against the
// marketplace's state as of this update. The clone is a cache: no pull.
func refreshInstall(d config.Dirs, name string, reg installReg) error {
	m, err := ReadMarketplace(filepath.Join(d.PluginsRoot(), "marketplaces", reg.Marketplace))
	if err != nil {
		return err
	}
	e, ok := m.find(reg.Entry)
	if !ok {
		return fmt.Errorf("marketplace %s no longer lists %s", reg.Marketplace, reg.Entry)
	}
	if e.Version == reg.Version {
		return nil
	}
	return recordInstall(d, name, reg.Marketplace, e, reg.Kind, "")
}

// pullMarketplace fetches the marketplace clone and hard-resets it to the
// remote's default branch; the clone is a cache, so local state is dropped.
// It reports whether the reset moved HEAD, the movement a versionless entry
// has to show for an update.
func pullMarketplace(ctx context.Context, dir string) (bool, error) {
	nctx, cancel := context.WithTimeout(ctx, netTimeout)
	defer cancel()
	if _, err := git(nctx, dir, "fetch", "--quiet", "--tags"); err != nil {
		return false, err
	}
	// the reset is local and finishes even when the update is cancelled
	ctx = context.WithoutCancel(ctx)
	was, _ := git(ctx, dir, "rev-parse", "HEAD")
	if _, err := git(ctx, dir, "reset", "--quiet", "--hard", "origin/HEAD"); err != nil {
		return false, err
	}
	now, _ := git(ctx, dir, "rev-parse", "HEAD")
	return was != now, nil
}

// readMarketplaceRef reads the marketplace manifest off the fetched
// origin/HEAD instead of the clone's working tree, which a reset would move.
func readMarketplaceRef(ctx context.Context, dir string) (Marketplace, error) {
	for _, c := range []struct {
		file string
		kind string
	}{
		{ZemiMarketplaceFile, KindZemi},
		{ClaudeMarketplaceFile, KindClaude},
	} {
		raw, err := git(ctx, dir, "show", "origin/HEAD:"+c.file)
		if err != nil {
			continue
		}
		return readMarketplace(jsontext.Value(raw), c.kind)
	}
	return Marketplace{}, fmt.Errorf("neither %s nor %s on origin/HEAD", ZemiMarketplaceFile, ClaudeMarketplaceFile)
}

func load(ctx context.Context, dir string, local, enabled bool) Plugin {
	p := Plugin{Name: filepath.Base(dir), Dir: dir, Local: local, Enabled: enabled}
	// the manifest is the plugin's own answer; init.lua's line fills in only
	// what it leaves out
	m := manifestOf(dir)
	p.Version, p.Desc, p.Uses = m.Version, m.Description, m.Uses
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
		tags, _ := git(ctx, dir, "tag", "--list")
		if top := highest(tags, semver.Major(tag)); semver.Compare(top, tag) > 0 {
			p.Latest = top
		} else if top := highest(tags, ""); semver.Compare(top, tag) > 0 {
			// nothing newer in tag's major, so a newer top is a new major
			p.Latest = top + " (major)"
		}
		return p
	}
	p.revision, _ = git(ctx, dir, "rev-parse", "--short", "HEAD")
	if p.Version == "" {
		p.Version = p.revision
	}
	if p.Latest = tagAhead(ctx, dir); p.Latest != "" {
		return p
	}
	if n, _ := git(ctx, dir, "rev-list", "--count", "HEAD..@{upstream}"); n == "1" {
		p.Latest = "1 new commit"
	} else if n != "" && n != "0" {
		p.Latest = n + " new commits"
	}
	return p
}

// tagAhead returns the highest semver tag when HEAD is its ancestor, where a
// branch checkout moves: a release tagged behind the branch is a downgrade.
func tagAhead(ctx context.Context, dir string) string {
	tags, _ := git(ctx, dir, "tag", "--list")
	top := highest(tags, "")
	if top == "" {
		return ""
	}
	if _, err := git(ctx, dir, "merge-base", "--is-ancestor", "HEAD", "refs/tags/"+top); err != nil {
		return ""
	}
	return top
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

var errLocal = errors.New("a local plugin, which zemi never changes")

// clone returns the directory of an installed plugin: a direct child of
// installed/ that is a directory and no symlink.
func clone(d config.Dirs, name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("no plugin is named %q", name)
	}
	dir := filepath.Join(d.PluginsRoot(), "installed", name)
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		return dir, nil
	} else if err == nil {
		return "", fmt.Errorf("%s is not a directory zemi cloned", dir)
	}
	if local := filepath.Join(d.PluginsRoot(), "local", name); exists(local) {
		return "", fmt.Errorf("%s is %w: %s", name, errLocal, local)
	}
	return "", fmt.Errorf("no installed plugin is named %s", name)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func disabled(d config.Dirs) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "disabled"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	off := map[string]bool{}
	for line := range strings.FieldsSeq(string(data)) {
		off[line] = true
	}
	return off, nil
}

// setDisabled rewrites disabled through a temp file and a rename, so a CLI
// and a TUI writing at once never leave a torn file.
// NOTE: two concurrent writes can keep only one change; an flock on the root
// would serialize them.
func setDisabled(d config.Dirs, name string, off bool) error {
	set, err := disabled(d)
	if err != nil || set[name] == off {
		return err
	}
	if off {
		set[name] = true
	} else {
		delete(set, name)
	}
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(set)) {
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
