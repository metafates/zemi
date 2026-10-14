// Package plugin manages the plugin tree under <data>/plugins: git clones
// in installed/, directories the user develops in place in local/, and the
// names turned off in disabled. It runs no Lua and needs no Host, so the
// /plugins command and the zemi plugins CLI share it.
package plugin

import (
	"cmp"
	"context"
	"crypto/rand"
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
var ErrUsage = errors.New("usage: plugins install <git url> [name] | update [name...] | check | enable <name> | disable <name> | delete <name>")

// List reads every plugin, local ones first, each group by name. It runs
// only local git reads.
func List(ctx context.Context, d config.Dirs) ([]Plugin, error) {
	ps, err := Scan(d)
	for i, p := range ps {
		ps[i] = load(ctx, p.Dir, p.Local, p.Enabled)
	}
	return ps, err
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
				ps = append(ps, Plugin{Name: e.Name(), Dir: root, Local: kind == "local", Enabled: !off[e.Name()], Uses: manifestOf(root).Uses})
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

// Install clones url as name (default: the URL's last segment) and checks
// out its highest semver tag, if it has one. The clone lands in a dot dir
// and is renamed into place, so the config watcher never loads half of it.
func Install(ctx context.Context, d config.Dirs, url, name string) (Plugin, error) {
	if strings.HasPrefix(url, "-") {
		return Plugin{}, fmt.Errorf("%q is not a git URL", url)
	}
	// a local path, as git tells it from host:path; git would resolve a
	// relative one against installed/
	if i := strings.IndexByte(url, ':'); i < 0 || strings.Contains(url[:i], "/") {
		abs, err := filepath.Abs(url)
		if err != nil {
			return Plugin{}, err
		}
		url = abs
	}
	name = cmp.Or(name, Name(url))
	if err := free(d, name); err != nil {
		return Plugin{}, err
	}
	dir := filepath.Join(d.PluginsRoot(), "installed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Plugin{}, err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	nctx, cancel := context.WithTimeout(ctx, netTimeout)
	defer cancel()
	if _, err := git(nctx, dir, "clone", "--quiet", "--no-recurse-submodules", "--", url, tmp); err != nil {
		return Plugin{}, err
	}
	tags, _ := git(ctx, tmp, "tag", "--list")
	if top := highest(tags, ""); top != "" {
		if _, err := git(ctx, tmp, "checkout", "--quiet", "refs/tags/"+top); err != nil {
			return Plugin{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, ".git", idFile), []byte(rand.Text()+"\n"), 0o644); err != nil {
		return Plugin{}, err
	}
	if err := free(d, name); err != nil {
		return Plugin{}, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return Plugin{}, err
	}
	// installing is a deliberate act, so a name disabled before is on again
	if err := setDisabled(d, name, false); err != nil {
		return Plugin{}, err
	}
	return load(ctx, filepath.Join(dir, name), false, true), nil
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

// Check fetches every installed plugin, so Latest reads the remote's state
// from local refs from then on, and returns the list.
func Check(ctx context.Context, d config.Dirs) ([]Plugin, error) {
	ps, err := List(ctx, d)
	if err != nil {
		return nil, err
	}
	var errs []error
	for i, p := range ps {
		if _, err := clone(d, p.Name); p.Local || err != nil {
			continue
		}
		nctx, cancel := context.WithTimeout(ctx, netTimeout)
		_, err := git(nctx, p.Dir, "fetch", "--quiet", "--tags")
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
			continue
		}
		ps[i] = load(ctx, p.Dir, false, p.Enabled)
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
	return setDisabled(d, name, false)
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
	case verb == "install" && (len(rest) == 1 || len(rest) == 2):
		name := ""
		if len(rest) == 2 {
			name = rest[1]
		}
		p, err := Install(ctx, d, rest[0], name)
		if err == nil {
			fmt.Fprintln(out, p.Name, p.Version)
		}
		return err
	case verb == "update":
		ps, err := List(ctx, d)
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
		var errs []error
		moved := false
		for _, name := range rest {
			p, err := update(ctx, d, name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			} else if p.revision != was[name] {
				fmt.Fprintf(out, "%s %s -> %s\n", p.Name, was[name], p.revision)
				moved = true
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
			if p.Latest != "" {
				fmt.Fprintf(out, "%s %s -> %s\n", p.Name, p.Version, p.Latest)
				n++
			}
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

func load(ctx context.Context, dir string, local, enabled bool) Plugin {
	p := Plugin{Name: filepath.Base(dir), Dir: dir, Local: local, Enabled: enabled}
	// the manifest is the plugin's own answer; init.lua's line fills in only
	// what it leaves out
	m := manifestOf(dir)
	p.Version, p.Desc, p.Uses = m.Version, m.Description, m.Uses
	if line := firstLine(filepath.Join(dir, InitFile)); p.Desc == "" && strings.HasPrefix(line, "--") {
		p.Desc = strings.TrimSpace(strings.TrimLeft(line, "-"))
	}
	// git would read the config of whatever repository a symlink names
	if fi, err := os.Lstat(dir); local || err != nil || !fi.IsDir() {
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

// idFile, in a clone's .git, holds the random id Install gave it.
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
