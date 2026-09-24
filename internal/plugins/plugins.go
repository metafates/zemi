// Package plugins manages the plugin tree under <data>/plugins: git clones
// in installed/, directories the user develops in place in local/, and the
// names turned off in disabled. It runs no Lua and needs no Host, so the
// /plugins command and the zemi plugins CLI share it.
package plugins

import (
	"cmp"
	"context"
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
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/proc"
)

// Plugin is one directory under installed/ or local/.
type Plugin struct {
	Name, Dir string
	Desc      string // line 1 of init.lua when it is a -- comment
	URL       string // the origin remote; "" for a local plugin
	Version   string // the checked-out semver tag, else the short commit
	Latest    string // a newer tag in the same major, "v2.0.0 (major)" or "3 new commits"
	Local     bool
	Enabled   bool
}

var (
	// a dot dir, such as a clone in flight, is never a plugin
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)
)

// netTimeout bounds a clone or a fetch; other git calls get proc.Git's.
const netTimeout = 2 * time.Minute

const usage = "usage: plugins [list] | install <git url> [name] | update [name...] | check | enable <name> | disable <name> | delete <name>"

func root(d config.Dirs) string { return filepath.Join(d.DataHome, "plugins") }

// List reads every plugin, local ones first, each group by name. It runs
// only local git reads.
func List(ctx context.Context, d config.Dirs) ([]Plugin, error) {
	off, err := disabled(d)
	if err != nil {
		return nil, err
	}
	var ps []Plugin
	for _, kind := range []string{"local", "installed"} {
		dir := filepath.Join(root(d), kind)
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range entries {
			// a symlink is listed so the loader can say why it refuses it
			if nameRe.MatchString(e.Name()) && (e.IsDir() || e.Type()&fs.ModeSymlink != 0) {
				ps = append(ps, load(ctx, filepath.Join(dir, e.Name()), kind == "local", !off[e.Name()]))
			}
		}
	}
	return ps, nil
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
	name = cmp.Or(name, strings.TrimSuffix(path.Base(strings.TrimRight(url, "/")), ".git"))
	if err := free(d, name); err != nil {
		return Plugin{}, err
	}
	dir := filepath.Join(root(d), "installed")
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
	if top, _ := highest(tags, -1); top != "" {
		if _, err := git(ctx, tmp, "checkout", "--quiet", "refs/tags/"+top); err != nil {
			return Plugin{}, err
		}
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

// Update moves a plugin on a tag to the highest tag of the same major, and
// one on a branch forward with a fast-forward pull. A dirty checkout makes
// git refuse, and that error is returned as is.
func Update(ctx context.Context, d config.Dirs, name string) (Plugin, error) {
	dir, err := clone(d, name)
	if err != nil {
		return Plugin{}, err
	}
	nctx, cancel := context.WithTimeout(ctx, netTimeout)
	defer cancel()
	here, _ := git(ctx, dir, "tag", "--points-at", "HEAD")
	if cur, cv := highest(here, -1); cur != "" {
		if _, err := git(nctx, dir, "fetch", "--quiet", "--tags"); err != nil {
			return Plugin{}, err
		}
		tags, _ := git(ctx, dir, "tag", "--list")
		if top, v := highest(tags, cv[0]); slices.Compare(v[:], cv[:]) > 0 {
			if _, err := git(ctx, dir, "checkout", "--quiet", "refs/tags/"+top); err != nil {
				return Plugin{}, err
			}
		}
	} else if _, err := git(nctx, dir, "pull", "--quiet", "--ff-only"); err != nil {
		return Plugin{}, err
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

// Delete removes an installed plugin. A local one may exist nowhere else,
// so it is the user's to remove.
func Delete(d config.Dirs, name string) error {
	dir, err := clone(d, name)
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

// Run runs one verb of the zemi plugins CLI, which /plugins shares.
func Run(ctx context.Context, d config.Dirs, args []string, out io.Writer) error {
	verb, rest := "list", args
	if len(args) > 0 {
		verb, rest = args[0], args[1:]
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	defer w.Flush()
	switch {
	case verb == "list" && len(rest) == 0:
		ps, err := List(ctx, d)
		if len(ps) == 0 && err == nil {
			fmt.Fprintln(w, "No plugins yet. Install one with zemi plugins install <git url>.")
		}
		for _, p := range ps {
			version, source, on := p.Version, p.URL, "on"
			if p.Local {
				version, source = "local", p.Dir
			}
			if !p.Enabled {
				on = "off"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, version, on, source)
		}
		return err
	case verb == "install" && (len(rest) == 1 || len(rest) == 2):
		name := ""
		if len(rest) == 2 {
			name = rest[1]
		}
		p, err := Install(ctx, d, rest[0], name)
		if err == nil {
			fmt.Fprintf(w, "installed %s %s\n", p.Name, p.Version)
		}
		return err
	case verb == "update":
		if len(rest) == 0 {
			ps, err := List(ctx, d)
			if err != nil {
				return err
			}
			for _, p := range ps {
				if !p.Local {
					rest = append(rest, p.Name)
				}
			}
		}
		var errs []error
		for _, name := range rest {
			p, err := Update(ctx, d, name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			fmt.Fprintf(w, "%s\t%s\n", p.Name, p.Version)
		}
		return errors.Join(errs...)
	case verb == "check" && len(rest) == 0:
		ps, err := Check(ctx, d)
		n := 0
		for _, p := range ps {
			if p.Latest != "" {
				fmt.Fprintf(w, "%s\t%s -> %s\n", p.Name, p.Version, p.Latest)
				n++
			}
		}
		if n == 0 && err == nil {
			fmt.Fprintln(w, "Plugins up to date")
		}
		return err
	case (verb == "enable" || verb == "disable") && len(rest) == 1:
		return SetEnabled(d, rest[0], verb == "enable")
	case verb == "delete" && len(rest) == 1:
		return Delete(d, rest[0])
	}
	return errors.New(usage)
}

func load(ctx context.Context, dir string, local, enabled bool) Plugin {
	p := Plugin{Name: filepath.Base(dir), Dir: dir, Local: local, Enabled: enabled}
	data, _ := os.ReadFile(filepath.Join(dir, "init.lua"))
	if line, _, _ := strings.Cut(string(data), "\n"); strings.HasPrefix(line, "--") {
		p.Desc = strings.TrimSpace(strings.TrimLeft(line, "-"))
	}
	// git would read the config of whatever repository a symlink names
	if fi, err := os.Lstat(dir); local || err != nil || !fi.IsDir() {
		return p
	}
	p.URL, _ = git(ctx, dir, "config", "--get", "remote.origin.url")
	here, _ := git(ctx, dir, "tag", "--points-at", "HEAD")
	if cur, cv := highest(here, -1); cur != "" {
		p.Version = cur
		tags, _ := git(ctx, dir, "tag", "--list")
		if top, v := highest(tags, cv[0]); slices.Compare(v[:], cv[:]) > 0 {
			p.Latest = top
		} else if top, v := highest(tags, -1); v[0] > cv[0] {
			p.Latest = top + " (major)"
		}
		return p
	}
	p.Version, _ = git(ctx, dir, "rev-parse", "--short", "HEAD")
	if n, _ := git(ctx, dir, "rev-list", "--count", "HEAD..@{upstream}"); n == "1" {
		p.Latest = "1 new commit"
	} else if n != "" && n != "0" {
		p.Latest = n + " new commits"
	}
	return p
}

// highest returns the highest semver tag among the lines of tags whose major
// is major, or of any major when major is -1.
func highest(tags string, major int) (string, [3]int) {
	best, bv := "", [3]int{-1}
	for t := range strings.FieldsSeq(tags) {
		m := semverRe.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		var v [3]int
		for i := range v {
			v[i], _ = strconv.Atoi(m[i+1])
		}
		if (major < 0 || v[0] == major) && slices.Compare(v[:], bv[:]) > 0 {
			best, bv = t, v
		}
	}
	return best, bv
}

// free refuses a bad name and one a local or installed plugin already has.
func free(d config.Dirs, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%q is not a plugin name: lowercase letters, digits, '.', '_' and '-'", name)
	}
	for _, kind := range []string{"local", "installed"} {
		if dir := filepath.Join(root(d), kind, name); exists(dir) {
			return fmt.Errorf("a plugin is already named %s: %s", name, dir)
		}
	}
	return nil
}

// clone returns the directory of an installed plugin: a direct child of
// installed/ that is a directory and no symlink.
func clone(d config.Dirs, name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("no plugin is named %q", name)
	}
	dir := filepath.Join(root(d), "installed", name)
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		return dir, nil
	} else if err == nil {
		return "", fmt.Errorf("%s is not a directory zemi cloned", dir)
	}
	if local := filepath.Join(root(d), "local", name); exists(local) {
		return "", fmt.Errorf("%s is a local plugin, which zemi never changes: %s", name, local)
	}
	return "", fmt.Errorf("no installed plugin is named %s", name)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func disabled(d config.Dirs) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(root(d), "disabled"))
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
	if err := os.MkdirAll(root(d), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(root(d), ".disabled-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(b.String())
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(root(d), "disabled"))
}

// git runs git on a plugin repository, blanking the user's filter drivers too.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := proc.Git{Dir: dir, Foreign: true}.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
