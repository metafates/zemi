package plugin

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/proc"
)

func TestHighest(t *testing.T) {
	tags := "v1.2.3 1.9.0 v1.10.0-rc.1 v1.3 v01.4.0 v1.2.4+meta v2.0.0 junk"
	for major, want := range map[string]string{"": "v2.0.0", "v1": "v1.2.3", "v3": ""} {
		if got := highest(tags, major); got != want {
			t.Errorf("highest(%q) = %q, want %q", major, got, want)
		}
	}
}

// TestManifest pins what a plugin's manifest answers: its version and
// description when it has one, and .zemi/init.lua's line when it has none.
func TestManifest(t *testing.T) {
	d := config.Dirs{DataHome: t.TempDir()}
	local := filepath.Join(d.PluginsRoot(), "local")
	if err := os.CopyFS(filepath.Join(local, "demo"), os.DirFS("../../testdata/plugins/demo")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(local, "bare", CodeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "bare", InitFile), []byte("-- the bare plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := List(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Plugin{}
	for _, p := range ps {
		got[p.Name] = p
	}
	for _, c := range []struct {
		name, version, desc string
		uses                []string
	}{
		{name: "bare", desc: "the bare plugin"},
		{name: "demo", version: "1.2.0", desc: "snippets, the git branch and the weather", uses: []string{"agent", "http", "job", "model"}},
	} {
		p, ok := got[c.name]
		if !ok {
			t.Fatalf("%s is not listed", c.name)
		}
		if p.Version != c.version || p.Desc != c.desc || !slices.Equal(p.Uses, c.uses) {
			t.Errorf("%s: version %q, desc %q, uses %v; want %q, %q, %v", c.name, p.Version, p.Desc, p.Uses, c.version, c.desc, c.uses)
		}
	}
}

// TestCheckUses pins the vocabulary a manifest's uses draws from.
func TestCheckUses(t *testing.T) {
	want := []string{"agent", "fs.write", "http", "job", "model", "perm", "tool"}
	if !slices.Equal(Uses, want) {
		t.Errorf("the vocabulary is %v, want %v", Uses, want)
	}
	for _, c := range []struct {
		name string
		uses []string
		want string
	}{
		{name: "the whole list", uses: want},
		{name: "none", uses: nil},
		{name: "an unknown word", uses: []string{"http", "fs.read"}, want: `uses "fs.read" is not a capability; the valid ones are ` + strings.Join(Uses, ", ")},
	} {
		err := CheckUses(c.uses)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || err.Error() != c.want):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// TestPlugins walks one plugin tree through the CLI verbs, in order: the
// marketplace registers, its entries install and update through their own
// git, a clone from before marketplaces keeps updating, and enable, disable
// and delete behave.
func TestPlugins(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	global := filepath.Join(tmp, "gitconfig")
	os.WriteFile(global, []byte("[user]\nname = t\nemail = t@t\n"), 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	d := config.Dirs{DataHome: filepath.Join(tmp, "data")}
	gitIn := func(dir string, args ...string) {
		t.Helper()
		if out, err := (proc.Git{Dir: dir, Foreign: true}).Run(t.Context(), args...); err != nil {
			t.Fatal(err, out)
		}
	}
	repo := func(name string, tags ...string) string {
		dir := filepath.Join(tmp, "src", name)
		os.MkdirAll(filepath.Join(dir, CodeDir), 0o755)
		os.WriteFile(filepath.Join(dir, InitFile), []byte("-- the "+name+" plugin\n"), 0o644)
		os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=x\n"), 0o644)
		gitIn(dir, "init", "-q")
		gitIn(dir, "add", ".")
		gitIn(dir, "commit", "-qm", "init")
		for _, tag := range tags {
			gitIn(dir, "commit", "-q", "--allow-empty", "-m", tag)
			gitIn(dir, "tag", tag)
		}
		return dir
	}
	tagged := repo("x", "v0.1.0", "v0.2.0", "v0.10.0", "v1.0.0")
	branch := repo("y.git")
	// a clone must not run the user's own filter driver
	marker := filepath.Join(tmp, "ran")
	f, _ := os.OpenFile(global, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[filter \"x\"]\nsmudge = touch " + marker + "; cat\nclean = touch " + marker + "; cat\nrequired = true\n")
	f.Close()
	local := filepath.Join(d.PluginsRoot(), "local", "loc")
	os.MkdirAll(filepath.Join(local, CodeDir), 0o755)
	installed := filepath.Join(d.PluginsRoot(), "installed")

	mkt := filepath.Join(tmp, "src", "mkt")
	write(t, mkt, map[string]string{
		".claude-plugin/marketplace.json": fmt.Sprintf(`{
			"name": "Demo Market", "owner": {"name": "t"},
			"plugins": [
				{"name": "x", "source": {"source": "git", "url": "file://%s"}},
				{"name": "why", "source": {"source": "git", "url": "%s"}},
				{"name": "loc", "source": "./plugins/loc"}
			]
		}`, tagged, filepath.Join(tmp, "src", "y.git")),
		"plugins/loc/.zemi/plugin.json": `{"description": "the local twin"}`,
	})
	gitIn(mkt, "init", "-q")
	gitIn(mkt, "add", ".")
	gitIn(mkt, "commit", "-qm", "init")

	for _, c := range []struct {
		name   string
		before func()
		args   []string // nil lists: name, version, enabled, source, marketplace
		want   string   // in the output (words joined by one space, a newline on each end), or in the error when err
		err    bool
	}{
		{name: "local only", args: nil, want: "loc true " + local},
		{name: "the marketplace registers", args: []string{"marketplace", "add", mkt}, want: "marketplace demo-market added (3 plugins)"},
		{name: "install takes the highest tag", args: []string{"install", "x@demo-market"}, want: "\nx v1.0.0\n"},
		{name: "a taken name", args: []string{"install", "x@demo-market"}, want: "already named x", err: true},
		{name: "install a branch entry", args: []string{"install", "why@demo-market"}, want: "\nwhy "},
		{name: "a local plugin's name", args: []string{"install", "loc@demo-market"}, want: "already named loc", err: true},
		{name: "a URL is no longer a door", args: []string{"install", "file://" + tagged}, want: "marketplace add", err: true},
		{name: "a remote helper is never run", args: []string{"install", "ext::sh -c touch% " + marker}, want: "marketplace add", err: true},
		{name: "a plaintext transport is refused", args: []string{"marketplace", "add", "git://127.0.0.1/x"}, want: "not allowed", err: true},
		{name: "a clone that fails leaves nothing", args: []string{"marketplace", "add", "file://" + tmp + "/nope"}, want: "clone", err: true},
		{
			name:   "check sees the same major first",
			before: func() { gitIn(filepath.Join(installed, "x"), "checkout", "-q", "refs/tags/v0.1.0") },
			args:   []string{"check"}, want: "x v0.1.0 -> v0.10.0",
		},
		{
			name:   "check counts new commits on a branch",
			before: func() { gitIn(branch, "commit", "-q", "--allow-empty", "-m", "more") },
			args:   []string{"check"}, want: "-> 1 new commit",
		},
		{name: "update stays in the major", args: []string{"update", "x"}, want: "\nx v0.1.0 -> v0.10.0\n"},
		{name: "then the major shows", args: []string{"check"}, want: "x v0.10.0 -> v1.0.0 (major)"},
		{name: "update pulls a branch", args: []string{"update"}, want: "\nwhy "},
		{name: "check after the pull", args: []string{"check"}, want: "\nx v0.10.0 -> v1.0.0 (major)\n"},
		{name: "nothing new", args: []string{"update"}, want: "\nPlugins up to date\n"},
		{
			name: "check names the tag a branch moves to",
			before: func() {
				gitIn(branch, "commit", "-q", "--allow-empty", "-m", "tagged")
				gitIn(branch, "tag", "v0.1.0")
				gitIn(branch, "commit", "-q", "--allow-empty", "-m", "untagged")
			},
			args: []string{"check"}, want: " -> v0.1.0 x ",
		},
		{name: "a branch moves to the first tag", args: []string{"update", "why"}, want: " -> v0.1.0\n"},
		{name: "disable", args: []string{"disable", "x"}},
		{name: "list", args: nil, want: "x v0.10.0 false file://" + tagged + " demo-market"},
		{name: "disable an unknown name", args: []string{"disable", "nope"}, want: "no plugin is named", err: true},
		{
			name:   "delete a local plugin",
			before: func() { os.MkdirAll(filepath.Join(filepath.Dir(local), "gone"), 0o755) },
			args:   []string{"delete", "gone"},
		},
		{name: "it is gone", args: []string{"enable", "gone"}, want: "no plugin is named", err: true},
		{
			// the reinstall below clones src/x, so following the link would fail it
			name:   "delete a local symlink",
			before: func() { os.Symlink(filepath.Join(tmp, "src", "x"), filepath.Join(filepath.Dir(local), "lnk")) },
			args:   []string{"delete", "lnk"},
		},
		{
			name:   "delete a symlink",
			before: func() { os.Symlink(local, filepath.Join(installed, "link")) },
			args:   []string{"delete", "link"}, want: "not a directory zemi cloned", err: true,
		},
		{name: "check skips a symlink", args: []string{"check"}, want: "\nx v0.10.0 -> v1.0.0 (major)\n"},
		{
			name: "a tag behind a branch is no update",
			before: func() {
				older := repo("w")
				gitIn(older, "commit", "-q", "--allow-empty", "-m", "c2")
				// a clone from before marketplaces: no registry row, its own git
				gitIn(tmp, "clone", "-q", "file://"+older, filepath.Join(installed, "w"))
				gitIn(older, "tag", "v0.1.0", "HEAD~1")
				gitIn(older, "commit", "-q", "--allow-empty", "-m", "c3")
			},
			args: []string{"update", "w"}, want: "\nw ",
		},
		{
			name:   "it stays on the branch",
			before: func() { gitIn(filepath.Join(tmp, "src", "w"), "commit", "-q", "--allow-empty", "-m", "c4") },
			args:   []string{"check"}, want: " -> 1 new commit x ",
		},
		{name: "delete", args: []string{"delete", "x"}},
		{name: "reinstall after the delete", args: []string{"install", "x@demo-market"}, want: "\nx v1.0.0\n"},
		{
			name: "a manifest outranks the tag",
			before: func() {
				manifest := filepath.Join(installed, "x", ".zemi", "plugin.json")
				os.MkdirAll(filepath.Dir(manifest), 0o755)
				os.WriteFile(manifest, []byte(`{"version": "3.0.0", "description": "from the manifest"}`), 0o644)
			},
			args: nil, want: "x 3.0.0 true file://",
		},
		{
			name:   "a manifest version does not hide a move",
			before: func() { gitIn(filepath.Join(installed, "x"), "checkout", "-q", "refs/tags/v0.1.0") },
			args:   []string{"update", "x"}, want: "\nx v0.1.0 -> v0.10.0\n",
		},
		{name: "an unknown verb", args: []string{"frob"}, want: "usage:", err: true},
		{name: "enable takes one name", args: []string{"enable"}, want: "usage:", err: true},
		{name: "marketplace takes a subcommand", args: []string{"marketplace", "frob"}, want: "usage:", err: true},
	} {
		if c.before != nil {
			c.before()
		}
		var out strings.Builder
		ps, err := List(t.Context(), d)
		if c.args == nil {
			for _, p := range ps {
				fmt.Fprintln(&out, p.Name, p.Version, p.Enabled, cmp.Or(p.URL, p.Dir), p.Marketplace)
			}
		} else {
			err = Run(t.Context(), d, c.args, &out)
		}
		got := "\n" + strings.Join(strings.Fields(out.String()), " ") + "\n"
		if err != nil {
			got = err.Error()
		}
		if (err != nil) != c.err || !strings.Contains(got, c.want) {
			t.Errorf("%s: plugins %q = %q (err %v), want %q", c.name, c.args, got, err, c.want)
		}
		if dots, _ := filepath.Glob(filepath.Join(d.PluginsRoot(), "*", ".*")); len(dots) > 0 {
			t.Fatalf("%s: left %v", c.name, dots)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a plugin clone ran the user's filter driver")
	}
	if InstallID(filepath.Join(installed, "x")) == "" {
		t.Error("the reinstall has no install id, so it would get the deleted one's state")
	}
	// a clone's .zemi/init.lua may be a symlink to /dev/zero: only a regular one is read
	leak := filepath.Join(tmp, "leak.lua")
	os.WriteFile(leak, []byte("-- leaked\n"), 0o644)
	os.Symlink(leak, filepath.Join(local, InitFile))
	if ps, _ := List(t.Context(), d); ps[0].Desc != "" {
		t.Errorf("%s: description %q read through a symlink", ps[0].Name, ps[0].Desc)
	}
	if off, _ := os.ReadFile(filepath.Join(d.PluginsRoot(), "disabled")); len(off) != 0 {
		t.Errorf("disabled = %q after the delete and the reinstall", off)
	}
}

// TestMarketplace walks one marketplace tree through the CLI verbs, in
// order: the repos register, their entries install by their own kind's
// matching, copies update from pulled clones, a check reads the fetched ref
// without moving a worktree, and removals respect what still needs the cache.
func TestMarketplace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	global := filepath.Join(tmp, "gitconfig")
	os.WriteFile(global, []byte("[user]\nname = t\nemail = t@t\n"), 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	d := config.Dirs{DataHome: filepath.Join(tmp, "data")}
	gitIn := func(dir string, args ...string) {
		t.Helper()
		if out, err := (proc.Git{Dir: dir, Foreign: true}).Run(t.Context(), args...); err != nil {
			t.Fatal(err, out)
		}
	}
	zed := filepath.Join(tmp, "src", "zed")
	os.MkdirAll(zed, 0o755)
	gitIn(zed, "init", "-q")
	gitIn(zed, "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(zed, "tag", "v0.1.0")

	zedsub := filepath.Join(tmp, "src", "zedsub")
	write(t, zedsub, map[string]string{
		"plugins/sub/commands/one.md": "do one",
	})
	gitIn(zedsub, "init", "-q")
	gitIn(zedsub, "add", ".")
	gitIn(zedsub, "commit", "-qm", "init")

	mkt := filepath.Join(tmp, "src", "mkt")
	manifest := func(relOne, entryManifest, subOne, gitOne string) string {
		return fmt.Sprintf(`{
			"name": "Demo Market",
			"owner": {"name": "t"},
			"$schema": "https://example.com/schema.json",
			"plugins": [
				{"name": "Rel One", "source": "./plugins/rel-one", "version": %q, "description": "the rel one"},
				{"name": "entry-manifest", "source": "./plugins/entry-manifest", "version": %q,
				 "mcpServers": {"srv": {"command": "srv"}}},
				{"name": "Own Manifest", "source": "./plugins/own-manifest", "version": "5.0.0"},
				{"name": "git-one", "source": {"source": "git", "url": "file://%s", "ref": "v0.1.0"}, "version": %q},
				{"name": "npm-one", "source": {"source": "npm", "package": "x"}},
				{"name": "sub-one", "source": {"source": "git-subdir", "url": "file://%s", "path": "plugins/sub"}, "version": %q},
				{"name": "loose", "source": "./plugins/loose"}
			]
		}`, relOne, entryManifest, zed, gitOne, zedsub, subOne)
	}
	write(t, mkt, map[string]string{
		".claude-plugin/marketplace.json":                 manifest("1.0.0", "0.2.0", "0.1.0", "0.5.0"),
		"plugins/rel-one/skills/a/SKILL.md":               "---\ndescription: a\n---\nbody",
		"plugins/rel-one/skills/b/SKILL.md":               "---\ndescription: b\n---\nbody",
		"plugins/entry-manifest/commands/x.md":            "do x",
		"plugins/entry-manifest/commands/y.md":            "do y",
		"plugins/entry-manifest/hooks/hooks.json":         `{"hooks": {"PostToolUse": [{"hooks": [{"type": "command", "command": "fmt.sh"}]}]}}`,
		"plugins/own-manifest/.claude-plugin/plugin.json": `{"version": "1.2.3", "description": "its own"}`,
		"plugins/loose/commands/loose.md":                 "loose",
	})
	gitIn(mkt, "init", "-q")
	gitIn(mkt, "add", ".")
	gitIn(mkt, "commit", "-qm", "init")

	// a second repo claiming the same marketplace name, from another source
	mkt2 := filepath.Join(tmp, "src", "mkt2")
	write(t, mkt2, map[string]string{".claude-plugin/marketplace.json": `{"name": "Demo Market", "plugins": []}`})
	gitIn(mkt2, "init", "-q")
	gitIn(mkt2, "add", ".")
	gitIn(mkt2, "commit", "-qm", "init")

	// a zemi-native marketplace; its claude manifest would name and list
	// different things, so the add shows which one was read
	both := filepath.Join(tmp, "src", "both")
	bothManifest := func(strictOne string) string {
		return fmt.Sprintf(`{
			"name": "bothz", "owner": {"name": "t"},
			"plugins": [
				{"name": "strict_one", "source": "./plugins/strict-one", "version": %q, "uses": ["http"]},
				{"name": "own-z", "source": "./plugins/own-z", "version": "9.9.9", "description": "entry says"},
				{"name": "rel-one", "source": "./plugins/rel-one"}
			]
		}`, strictOne)
	}
	write(t, both, map[string]string{
		".zemi/marketplace.json":            bothManifest("1.0.0"),
		".claude-plugin/marketplace.json":   `{"name": "Other Name", "plugins": [{"name": "claude-only", "source": "./plugins/strict-one"}]}`,
		"plugins/strict-one/.zemi/init.lua": "-- strict one\n",
		"plugins/own-z/.zemi/plugin.json":   `{"version": "2.0.0", "description": "its own", "uses": ["agent"]}`,
	})
	gitIn(both, "init", "-q")
	gitIn(both, "add", ".")
	gitIn(both, "commit", "-qm", "init")

	ghost := filepath.Join(tmp, "src", "ghost")
	write(t, ghost, map[string]string{
		".claude-plugin/marketplace.json": `{"name": "Ghost", "plugins": [
			{"name": "ghost-one", "source": {"source": "git", "url": "file://` + zed + `"}}
		]}`,
	})
	gitIn(ghost, "init", "-q")
	gitIn(ghost, "add", ".")
	gitIn(ghost, "commit", "-qm", "init")

	bad := filepath.Join(tmp, "src", "bad")
	write(t, bad, map[string]string{
		".claude-plugin/marketplace.json": `{
			"name": "Bad Market",
			"plugins": [{"name": "climber", "source": "./../../secret"}]
		}`,
	})
	gitIn(bad, "init", "-q")
	gitIn(bad, "add", ".")
	gitIn(bad, "commit", "-qm", "init")
	bump := func(relOne, entryManifest, subOne, gitOne string) {
		write(t, mkt, map[string]string{".claude-plugin/marketplace.json": manifest(relOne, entryManifest, subOne, gitOne)})
		gitIn(mkt, "commit", "-qam", "bump")
	}
	provides := func(t *testing.T, name string) []string {
		t.Helper()
		ps, err := Scan(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.Name == name {
				return p.Provides
			}
		}
		t.Fatalf("%s is not scanned", name)
		return nil
	}
	var added time.Time
	var orphanID, relID string

	for _, c := range []struct {
		name   string
		before func()
		args   []string
		want   string
		err    bool
		check  func(t *testing.T)
	}{
		{
			name: "a marketplace repo registers by its manifest",
			args: []string{"marketplace", "add", mkt}, want: "marketplace demo-market added (7 plugins)",
			check: func(t *testing.T) {
				if _, err := os.Stat(filepath.Join(d.PluginsRoot(), "marketplaces", "demo-market", ".claude-plugin", "marketplace.json")); err != nil {
					t.Fatal(err)
				}
				ms, err := Marketplaces(d)
				if err != nil || len(ms) != 1 || ms[0].Name != "demo-market" || ms[0].Source != mkt || len(ms[0].Entries) != 7 {
					t.Fatalf("marketplaces: %+v, %v", ms, err)
				}
				added = ms[0].Added
			},
		},
		{
			name: "a same-source re-add refreshes the clone and keeps its Added",
			args: []string{"marketplace", "add", mkt}, want: "marketplace demo-market added (7 plugins)",
			check: func(t *testing.T) {
				ms, _ := Marketplaces(d)
				if !ms[0].Added.Equal(added) {
					t.Errorf("the re-add reset Added: %v, want %v", ms[0].Added, added)
				}
			},
		},
		{
			name: "a same name from another source is refused",
			args: []string{"marketplace", "add", mkt2},
			want: "demo-market is already registered from " + mkt + ", not " + mkt2, err: true,
		},
		{
			name: "a marketplace with a climbing source is refused whole",
			args: []string{"marketplace", "add", bad}, want: "stay inside the marketplace", err: true,
			check: func(t *testing.T) {
				ms, err := os.ReadDir(filepath.Join(d.PluginsRoot(), "marketplaces"))
				if err != nil || len(ms) != 1 || ms[0].Name() != "demo-market" {
					t.Errorf("the refused marketplace registered anyway: %v, %v", ms, err)
				}
			},
		},
		{
			name: "a relative entry copies in as a plain dir",
			args: []string{"install", "rel-one@demo-market"}, want: "\nrel-one 1.0.0\n",
			check: func(t *testing.T) {
				dir := filepath.Join(d.PluginsRoot(), "installed", "rel-one")
				if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
					t.Error("the copy has a .git, where sweep and uninstall expect a plain dir")
				}
				b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
				if err != nil || !strings.Contains(string(b), `"version":"1.0.0"`) || !strings.Contains(string(b), `"name":"Rel One"`) {
					t.Errorf("the entry did not become the manifest: %q, %v", b, err)
				}
				prov, ok := MarketplaceOf(d, "rel-one")
				if !ok || prov.Marketplace != "demo-market" || prov.Entry != "Rel One" || prov.Version != "1.0.0" ||
					prov.Kind != kindCopy || prov.ID == "" {
					t.Errorf("provenance: %+v, %v", prov, ok)
				}
				if got := provides(t, "rel-one"); !slices.Equal(got, []string{"2 skills"}) {
					t.Errorf("provides %v", got)
				}
			},
		},
		{
			name: "component fields ride along an entry manifest",
			args: []string{"install", "entry-manifest@demo-market"}, want: "\nentry-manifest 0.2.0\n",
			check: func(t *testing.T) {
				dir := filepath.Join(d.PluginsRoot(), "installed", "entry-manifest")
				m, err := ReadClaude(dir, "/data")
				if err != nil {
					t.Fatal(err)
				}
				if len(m.MCP) != 1 || m.MCP[0].Name != "srv" || len(m.Hooks) != 1 || m.Hooks[0].Event != "PostToolUse" {
					t.Errorf("the entry's components did not pass through: %+v", m)
				}
				if got := provides(t, "entry-manifest"); !slices.Equal(got, []string{"2 commands", "mcp: srv", "hooks: PostToolUse"}) {
					t.Errorf("provides %v", got)
				}
			},
		},
		{
			name: "an own manifest is kept",
			args: []string{"install", "own-manifest@demo-market"}, want: "\nown-manifest\n",
			check: func(t *testing.T) {
				b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "own-manifest", ".claude-plugin", "plugin.json"))
				if err != nil || !strings.Contains(string(b), `"version": "1.2.3"`) {
					t.Errorf("the on-disk manifest was overwritten: %q, %v", b, err)
				}
			},
		},
		{
			name: "a git entry clones, synthesizes its manifest and records git provenance",
			args: []string{"install", "git-one@demo-market"}, want: "\ngit-one v0.1.0\n",
			check: func(t *testing.T) {
				dir := filepath.Join(d.PluginsRoot(), "installed", "git-one")
				if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(filepath.Join(dir, ClaudeManifestFile))
				if err != nil || !strings.Contains(string(b), `"version":"0.5.0"`) {
					t.Errorf("the entry did not become the manifest: %q, %v", b, err)
				}
				prov, ok := MarketplaceOf(d, "git-one")
				if !ok || prov.Kind != kindGit || prov.ID == "" || prov.ID != InstallID(dir) {
					t.Errorf("provenance: %+v, %v, install id %q", prov, ok, InstallID(dir))
				}
			},
		},
		{
			name: "a git-subdir entry copies its path",
			args: []string{"install", "sub-one@demo-market"}, want: "\nsub-one 0.1.0\n",
			check: func(t *testing.T) {
				dir := filepath.Join(d.PluginsRoot(), "installed", "sub-one")
				if _, err := os.Lstat(filepath.Join(dir, "commands", "one.md")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
					t.Error("the git-subdir copy carries a .git")
				}
				if prov, ok := MarketplaceOf(d, "sub-one"); !ok || prov.Entry != "sub-one" {
					t.Errorf("provenance: %+v, %v", prov, ok)
				}
			},
		},
		{name: "an npm entry is refused", args: []string{"install", "npm-one@demo-market"}, want: "not supported", err: true},
		{name: "an unknown marketplace", args: []string{"install", "x@nope"}, want: "no marketplace is named nope", err: true},
		{name: "an unknown entry", args: []string{"install", "nope@demo-market"}, want: "lists no plugin named nope", err: true},
		{
			name: "a zemi marketplace registers by its own manifest",
			args: []string{"marketplace", "add", both}, want: "marketplace bothz added (3 plugins)",
		},
		{
			name: "a zemi entry installs under its exact name",
			args: []string{"install", "strict_one@bothz"}, want: "\nstrict_one 1.0.0\n",
			check: func(t *testing.T) {
				b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "strict_one", ManifestFile))
				if err != nil || !strings.Contains(string(b), `"uses":["http"]`) {
					t.Errorf("the entry did not become a zemi manifest: %q, %v", b, err)
				}
				ps, err := Scan(d)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range ps {
					if p.Name == "strict_one" && !slices.Equal(p.Uses, []string{"http"}) {
						t.Errorf("uses: %v", p.Uses)
					}
				}
			},
		},
		{
			name: "a zemi entry matches exactly, not sanitized",
			args: []string{"install", "strict-one@bothz"}, want: "lists no plugin named strict-one", err: true,
		},
		{
			name: "a zemi entry's own manifest wins",
			args: []string{"install", "own-z@bothz"}, want: "\nown-z 2.0.0\n",
			check: func(t *testing.T) {
				b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "own-z", ManifestFile))
				if err != nil || !strings.Contains(string(b), "2.0.0") || strings.Contains(string(b), "9.9.9") {
					t.Errorf("the dir's manifest was overwritten: %q, %v", b, err)
				}
			},
		},
		{
			name: "a reinstall over an orphan row takes a fresh id",
			before: func() {
				if prov, ok := MarketplaceOf(d, "strict_one"); ok {
					orphanID = prov.ID
				}
				// the dir went away without the registry learning it
				os.RemoveAll(filepath.Join(d.PluginsRoot(), "installed", "strict_one"))
			},
			args: []string{"install", "strict_one@bothz"}, want: "\nstrict_one 1.0.0\n",
			check: func(t *testing.T) {
				prov, ok := MarketplaceOf(d, "strict_one")
				if !ok || prov.ID == "" || prov.ID == orphanID {
					t.Errorf("the reinstall did not reset the identity: %+v, stale %q", prov, orphanID)
				}
			},
		},
		{
			name: "a bare name found in two marketplaces lists the candidates",
			args: []string{"install", "rel-one"}, want: "rel-one@bothz, rel-one@demo-market", err: true,
		},
		{
			name: "a bare name in no marketplace names the ones searched",
			args: []string{"install", "nope"},
			want: `no marketplace lists "nope" (searched bothz, demo-market)`, err: true,
		},
		{name: "a ghost marketplace registers", args: []string{"marketplace", "add", ghost}, want: "marketplace ghost added (1 plugins)"},
		{
			name: "a ghost git entry installs from its own clone",
			args: []string{"install", "ghost-one@ghost"}, want: "\nghost-one v0.1.0\n",
		},
		{
			name:   "a bare name skips a marketplace whose clone is gone",
			before: func() { os.RemoveAll(filepath.Join(d.PluginsRoot(), "marketplaces", "ghost")) },
			args:   []string{"install", "loose"},
			want:   "skipped ghost: its clone is missing",
			check: func(t *testing.T) {
				if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "loose")); err != nil {
					t.Error("loose did not install:", err)
				}
			},
		},
		{
			name: "a bare name with a skip reports it in the error too",
			args: []string{"install", "nope"},
			want: "skipped ghost: its clone is missing", err: true,
		},
		{name: "check skips the copies", args: []string{"check"}, want: "\nPlugins up to date\n"},
		{
			name: "an update re-copies from a pulled clone",
			before: func() {
				if prov, ok := MarketplaceOf(d, "rel-one"); ok {
					relID = prov.ID
				}
				bump("1.1.0", "0.2.0", "0.1.0", "0.5.0")
			},
			args: []string{"update", "rel-one"}, want: "\nrel-one 1.0.0 -> 1.1.0\n",
			check: func(t *testing.T) {
				b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "rel-one", ".claude-plugin", "plugin.json"))
				if err != nil || !strings.Contains(string(b), `"version":"1.1.0"`) {
					t.Errorf("the copy did not move to 1.1.0: %q, %v", b, err)
				}
				prov, _ := MarketplaceOf(d, "rel-one")
				if prov.Version != "1.1.0" {
					t.Errorf("provenance version %q", prov.Version)
				}
				if prov.ID != relID {
					t.Errorf("the update reset the install id: %q, want %q", prov.ID, relID)
				}
			},
		},
		{name: "disable a copy", args: []string{"disable", "entry-manifest"}},
		{
			name:   "an update keeps a disabled copy off",
			before: func() { bump("1.1.0", "0.3.0", "0.1.0", "0.5.0") },
			args:   []string{"update", "entry-manifest"}, want: "0.2.0 -> 0.3.0",
			check: func(t *testing.T) {
				ps, err := Scan(d)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range ps {
					if p.Name == "entry-manifest" && p.Enabled {
						t.Error("the update re-enabled the copy")
					}
				}
			},
		},
		{
			name: "a no-op update re-copies nothing",
			before: func() {
				os.WriteFile(filepath.Join(d.PluginsRoot(), "installed", "rel-one", "MARKER"), []byte("x"), 0o644)
			},
			args: []string{"update", "rel-one"}, want: "\nPlugins up to date\n",
			check: func(t *testing.T) {
				if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "rel-one", "MARKER")); err != nil {
					t.Error("the no-op update re-copied the tree:", err)
				}
			},
		},
		{
			name: "a moved clone with a still version prints updated",
			before: func() {
				write(t, mkt, map[string]string{"plugins/rel-one/CHANGELOG.md": "note"})
				gitIn(mkt, "add", ".")
				gitIn(mkt, "commit", "-qm", "note")
			},
			args: []string{"update", "rel-one"}, want: "\nrel-one updated\n",
			check: func(t *testing.T) {
				if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "rel-one", "MARKER")); err == nil {
					t.Error("the re-copy left the old tree in place")
				}
			},
		},
		{
			name:   "check reads a newer version off the fetched ref",
			before: func() { bump("1.2.0", "0.3.0", "0.1.0", "0.5.0") },
			args:   []string{"check"},
			want: "entry-manifest updated in the marketplace (demo-market) loose updated in the marketplace (demo-market) " +
				"own-manifest updated in the marketplace (demo-market) rel-one 1.1.0 -> 1.2.0 sub-one updated in the marketplace (demo-market)",
			check: func(t *testing.T) {
				b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "rel-one", ".claude-plugin", "plugin.json"))
				if err != nil || strings.Contains(string(b), "1.2.0") {
					t.Errorf("check reset the copy: %q, %v", b, err)
				}
			},
		},
		{
			name: "an update takes the version check saw",
			args: []string{"update", "rel-one"}, want: "\nrel-one 1.1.0 -> 1.2.0\n",
		},
		{
			// one bare update pulls each of the two marketplaces once and
			// re-copies every group: a dropped group loses its line
			name: "one update re-copies both marketplaces' copies",
			before: func() {
				write(t, both, map[string]string{".zemi/marketplace.json": bothManifest("1.1.0")})
				gitIn(both, "commit", "-qam", "bump")
				bump("1.3.0", "0.3.0", "0.1.0", "0.5.0")
			},
			args: []string{"update"},
			want: "own-z updated strict_one 1.0.0 -> 1.1.0 entry-manifest updated loose updated own-manifest updated rel-one 1.2.0 -> 1.3.0 sub-one updated",
		},
		{
			name: "a git-subdir copy updates from its own repo",
			before: func() {
				write(t, zedsub, map[string]string{"plugins/sub/commands/two.md": "do two"})
				gitIn(zedsub, "add", ".")
				gitIn(zedsub, "commit", "-qm", "two")
				bump("1.3.0", "0.3.0", "0.2.0", "0.5.0")
			},
			args: []string{"update", "sub-one"}, want: "\nsub-one 0.1.0 -> 0.2.0\n",
			check: func(t *testing.T) {
				if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "sub-one", "commands", "two.md")); err != nil {
					t.Errorf("the re-copy did not bring the new file: %v", err)
				}
				if prov, _ := MarketplaceOf(d, "sub-one"); prov.Version != "0.2.0" {
					t.Errorf("provenance version %q", prov.Version)
				}
			},
		},
		{
			name: "a marketplace update pulls once and names the installs behind it",
			before: func() {
				// upstream grows the very path the install's synthesized
				// manifest occupies, so the checkout would refuse without
				// zemi dropping its own file first
				write(t, zed, map[string]string{".claude-plugin/plugin.json": `{"version": "2.0.0", "description": "upstream's own"}`})
				gitIn(zed, "add", ".")
				gitIn(zed, "commit", "-qm", "v2")
				gitIn(zed, "tag", "v0.2.0")
				bump("1.3.0", "0.3.0", "0.2.0", "0.6.0")
			},
			args: []string{"marketplace", "update", "demo-market"},
			want: "behind it: entry-manifest, loose, own-manifest, rel-one, sub-one",
			check: func(t *testing.T) {
				b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "rel-one", ".claude-plugin", "plugin.json"))
				if err != nil || strings.Contains(string(b), "0.6.0") {
					t.Errorf("the marketplace update touched an install: %q, %v", b, err)
				}
			},
		},
		{
			name: "a git-kind install updates through its own git",
			args: []string{"update", "git-one"}, want: "\ngit-one v0.1.0 -> v0.2.0\n",
			check: func(t *testing.T) {
				prov, ok := MarketplaceOf(d, "git-one")
				if !ok || prov.Version != "0.6.0" || prov.Kind != kindGit {
					t.Errorf("the row did not refresh to the marketplace's version: %+v, %v", prov, ok)
				}
				// upstream's own manifest replaced the synthesized one
				b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "git-one", ClaudeManifestFile))
				if err != nil || !strings.Contains(string(b), "2.0.0") {
					t.Errorf("the update did not take upstream's manifest: %q, %v", b, err)
				}
			},
		},
		{
			name: "an update refuses an entry the marketplace renamed",
			before: func() {
				renamed := strings.Replace(manifest("1.3.0", "0.3.0", "0.2.0", "0.6.0"), `"name": "Rel One"`, `"name": "Rel Two"`, 1)
				write(t, mkt, map[string]string{".claude-plugin/marketplace.json": renamed})
				gitIn(mkt, "commit", "-qam", "rename")
			},
			args: []string{"update", "rel-one"}, want: "no longer lists Rel One", err: true,
		},
		{
			name: "a delete drops the provenance",
			args: []string{"delete", "rel-one"},
			check: func(t *testing.T) {
				if _, ok := MarketplaceOf(d, "rel-one"); ok {
					t.Error("the provenance outlived the plugin")
				}
				if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "rel-one")); err == nil {
					t.Error("the copy is still there")
				}
			},
		},
		{name: "enable an unknown name", args: []string{"enable", "rel-one"}, want: "no plugin is named", err: true},
		{
			name: "a marketplace with copy installs is not removable",
			args: []string{"marketplace", "remove", "bothz"},
			want: "still has copy installs that update from it: own-z, strict_one", err: true,
		},
		{
			name: "a marketplace of git installs removes with a note",
			args: []string{"marketplace", "remove", "ghost"},
			want: "ghost-one keeps its own clone",
			check: func(t *testing.T) {
				if _, ok := MarketplaceOf(d, "ghost-one"); ok {
					t.Error("the git-kind row outlived the marketplace")
				}
				ps, err := Scan(d)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(ps, func(p Plugin) bool { return p.Name == "ghost-one" }) {
					t.Error("the git-kind install stopped being scanned")
				}
				if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "marketplaces", "ghost")); err == nil {
					t.Error("the clone is still there")
				}
			},
		},
	} {
		if c.before != nil {
			c.before()
		}
		var out strings.Builder
		err := Run(t.Context(), d, c.args, &out)
		got := "\n" + strings.Join(strings.Fields(out.String()), " ") + "\n"
		if err != nil {
			got = err.Error()
		}
		if (err != nil) != c.err || !strings.Contains(got, c.want) {
			t.Errorf("%s: plugins %q = %q (err %v), want %q", c.name, c.args, got, err, c.want)
		}
		if c.check != nil {
			c.check(t)
		}
		if dots, _ := filepath.Glob(filepath.Join(d.PluginsRoot(), "*", ".*")); len(dots) > 0 {
			t.Fatalf("%s: left %v", c.name, dots)
		}
	}
	if _, err := os.Stat(filepath.Join(d.PluginsRoot(), "marketplaces", "demo-market", "plugins", "own-manifest")); err != nil {
		t.Errorf("the marketplace clone does not serve the other entries: %v", err)
	}
}
