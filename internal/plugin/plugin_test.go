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

// TestManifestMetadata: the detail table's author and homepage come from the
// manifest, a string author or Claude's object, and fall back to a Claude
// manifest beside it.
func TestManifestMetadata(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"str/.zemi/plugin.json":             `{"author": "Ada", "homepage": "https://a"}`,
		"obj/.zemi/plugin.json":             `{"author": {"name": "Grace"}}`,
		"claude/.claude-plugin/plugin.json": `{"name": "c", "description": "the claude one", "author": {"name": "Edsger"}, "homepage": "https://e"}`,
	})
	for _, c := range []struct{ name, author, home string }{
		{"str", "Ada", "https://a"}, {"obj", "Grace", ""}, {"claude", "Edsger", "https://e"},
	} {
		m, err := manifestOf(filepath.Join(dir, c.name))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := authorName(m.Author); got != c.author || m.Homepage != c.home {
			t.Errorf("%s: author %q, homepage %q; want %q, %q", c.name, got, m.Homepage, c.author, c.home)
		}
	}
	if m, err := manifestOf(filepath.Join(dir, "claude")); err != nil || m.Description != "the claude one" {
		t.Errorf("claude: description %q, %v; want the claude manifest's own", m.Description, err)
	}
	p := load(t.Context(), filepath.Join(dir, "claude"), true, true)
	if p.Author != "Edsger" || p.Homepage != "https://e" || p.Desc != "the claude one" {
		t.Errorf("load: author %q, homepage %q, desc %q", p.Author, p.Homepage, p.Desc)
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
	repo("y.git")
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

	// run invokes the command (nil args list the plugins) and checks its
	// output, or the error when it failed, against want.
	run := func(t *testing.T, args []string, want string, failed bool) {
		t.Helper()
		var out strings.Builder
		ps, _, _ := List(t.Context(), d)
		var err error
		if args == nil {
			for _, p := range ps {
				fmt.Fprintln(&out, p.Name, p.Version, p.Enabled, cmp.Or(p.URL, p.Dir), p.Marketplace)
			}
		} else {
			err = Run(t.Context(), d, args, &out)
		}
		got := "\n" + strings.Join(strings.Fields(out.String()), " ") + "\n"
		if err != nil {
			got = err.Error()
		}
		if (err != nil) != failed || !strings.Contains(got, want) {
			t.Errorf("plugins %q = %q (err %v), want %q", args, got, err, want)
		}
		if dots, _ := filepath.Glob(filepath.Join(d.PluginsRoot(), "installed", ".*")); len(dots) > 0 {
			t.Fatalf("left %v", dots)
		}
	}

	t.Run("local only", func(t *testing.T) {
		run(t, nil, "loc true "+local, false)
	})
	t.Run("the marketplace registers", func(t *testing.T) {
		run(t, []string{"marketplace", "add", mkt}, "marketplace demo-market added (3 plugins)", false)
	})
	t.Run("install takes the highest tag", func(t *testing.T) {
		run(t, []string{"install", "x@demo-market"}, "\nx v1.0.0\n", false)
	})
	t.Run("a taken name", func(t *testing.T) {
		run(t, []string{"install", "x@demo-market"}, "already named x", true)
	})
	t.Run("install a branch entry", func(t *testing.T) {
		run(t, []string{"install", "why@demo-market"}, "\nwhy ", false)
	})
	t.Run("a local plugin's name", func(t *testing.T) {
		run(t, []string{"install", "loc@demo-market"}, "already named loc", true)
	})
	t.Run("a URL is no longer a door", func(t *testing.T) {
		run(t, []string{"install", "file://" + tagged}, "marketplace add", true)
	})
	t.Run("a remote helper is never run", func(t *testing.T) {
		run(t, []string{"install", "ext::sh -c touch% " + marker}, "marketplace add", true)
	})
	t.Run("a plaintext transport is refused", func(t *testing.T) {
		run(t, []string{"marketplace", "add", "git://127.0.0.1/x"}, "not a marketplace source", true)
	})
	t.Run("a clone that fails leaves nothing", func(t *testing.T) {
		run(t, []string{"marketplace", "add", "file://" + tmp + "/nope"}, "clone", true)
	})
	t.Run("an install is a link into the entry's cache clone", func(t *testing.T) {
		run(t, nil, "", false)
		for _, name := range []string{"x", "why"} {
			dir := filepath.Join(installed, name)
			if target, err := os.Readlink(dir); err != nil || target != filepath.Join("..", "marketplaces", _cacheDirName, "demo-market", name) {
				t.Fatalf("%s: the install is not the cache link: %q, %v", name, target, err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
				t.Fatalf("%s: the cache clone is not behind the link: %v", name, err)
			}
		}
	})
	// the marketplace pull is what moves an install: the cache clone
	// follows the entry's highest tag, and the link reads it
	t.Run("a marketplace update moves the installs behind it", func(t *testing.T) {
		gitIn(tagged, "commit", "-q", "--allow-empty", "-m", "v1.1.0")
		gitIn(tagged, "tag", "v1.1.0")
		run(t, []string{"marketplace", "update", "demo-market"}, "marketplace demo-market updated", false)
	})
	t.Run("the moved install reads the new tag", func(t *testing.T) {
		run(t, nil, "x v1.1.0 true file://"+tagged+" demo-market", false)
	})
	t.Run("disable", func(t *testing.T) {
		run(t, []string{"disable", "x"}, "", false)
	})
	t.Run("list", func(t *testing.T) {
		run(t, nil, "x v1.1.0 false file://"+tagged+" demo-market", false)
	})
	t.Run("disable an unknown name", func(t *testing.T) {
		run(t, []string{"disable", "nope"}, "no plugin is named", true)
	})
	t.Run("delete a local plugin", func(t *testing.T) {
		os.MkdirAll(filepath.Join(filepath.Dir(local), "gone"), 0o755)
		run(t, []string{"delete", "gone"}, "", false)
	})
	t.Run("it is gone", func(t *testing.T) {
		run(t, []string{"enable", "gone"}, "no plugin is named", true)
	})
	// the reinstall below clones src/x, so following the link would fail it
	t.Run("delete a local symlink", func(t *testing.T) {
		os.Symlink(filepath.Join(tmp, "src", "x"), filepath.Join(filepath.Dir(local), "lnk"))
		run(t, []string{"delete", "lnk"}, "", false)
	})
	t.Run("delete a symlink to a local plugin", func(t *testing.T) {
		os.Symlink(local, filepath.Join(installed, "link"))
		run(t, []string{"delete", "link"}, "", false)
	})
	t.Run("the delete removed the cache clone too", func(t *testing.T) {
		run(t, nil, "", false)
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "marketplaces", _cacheDirName, "demo-market", "link")); err == nil {
			t.Error("the deleted install's cache clone is still there")
		}
	})
	t.Run("delete", func(t *testing.T) {
		run(t, []string{"delete", "x"}, "", false)
	})
	t.Run("reinstall after the delete", func(t *testing.T) {
		run(t, []string{"install", "x@demo-market"}, "\nx v1.1.0\n", false)
	})
	t.Run("an unknown verb", func(t *testing.T) {
		run(t, []string{"frob"}, "usage:", true)
	})
	t.Run("enable takes one name", func(t *testing.T) {
		run(t, []string{"enable"}, "usage:", true)
	})
	t.Run("marketplace takes a subcommand", func(t *testing.T) {
		run(t, []string{"marketplace", "frob"}, "usage:", true)
	})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a plugin clone ran the user's filter driver")
	}
	// a clone's .zemi/init.lua may be a symlink to /dev/zero: only a regular one is read
	leak := filepath.Join(tmp, "leak.lua")
	os.WriteFile(leak, []byte("-- leaked\n"), 0o644)
	os.Symlink(leak, filepath.Join(local, InitFile))
	if ps, _, _ := List(t.Context(), d); ps[0].Desc != "" {
		t.Errorf("%s: description %q read through a symlink", ps[0].Name, ps[0].Desc)
	}
	if off, _ := os.ReadFile(filepath.Join(d.PluginsRoot(), "disabled")); len(off) != 0 {
		t.Errorf("disabled = %q after the delete and the reinstall", off)
	}
}

// TestMarketplaceSource walks the grammar a `marketplace add` argument
// takes, the one Claude and codex type.
func TestMarketplaceSource(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	os.MkdirAll(home, 0o755)
	t.Setenv("HOME", home)
	dir := filepath.Join(tmp, "mkt")
	os.MkdirAll(dir, 0o755)
	wd, _ := os.Getwd()
	os.WriteFile(filepath.Join(tmp, "one.json"), []byte("{}"), 0o644)
	for _, c := range []struct {
		name, arg, url, ref, err string
	}{
		{name: "shorthand", arg: "DietrichGebert/ponytail", url: "https://github.com/DietrichGebert/ponytail.git"},
		{name: "shorthand with a #ref", arg: "o/r#v1.2", url: "https://github.com/o/r.git", ref: "v1.2"},
		{name: "shorthand with an @ref", arg: "o/r@main", url: "https://github.com/o/r.git", ref: "main"},
		{name: "a github https url completes", arg: "https://github.com/o/r", url: "https://github.com/o/r.git"},
		{name: "a github url with a trailing slash", arg: "https://github.com/o/r/", url: "https://github.com/o/r.git"},
		{name: "another host stays", arg: "https://gitlab.com/o/r", url: "https://gitlab.com/o/r"},
		{name: "an scp-like ssh url", arg: "git@github.com:o/r", url: "git@github.com:o/r"},
		{name: "an ssh url with a ref", arg: "git@github.com:o/r#v2", url: "git@github.com:o/r", ref: "v2"},
		{name: "another user's ssh url", arg: "deploy@gitlab.com:g/p.git", url: "deploy@gitlab.com:g/p.git"},
		{name: "ssh scheme", arg: "ssh://git@host/o/r", url: "ssh://git@host/o/r"},
		{name: "a file url is a git url", arg: "file://" + dir, url: "file://" + dir},
		{name: "an absolute dir", arg: dir, url: dir},
		{name: "a relative dir", arg: "./mkt", url: filepath.Join(wd, "mkt")},
		{name: "a tilde dir", arg: "~/mkt", url: filepath.Join(home, "mkt")},
		{name: "a url ref", arg: "https://x.io/m.git#v1", url: "https://x.io/m.git", ref: "v1"},
		{name: "a ref refuses a local path", arg: dir + "#v1", err: "a ref pins a git source only"},
		{name: "a file source is refused", arg: filepath.Join(tmp, "one.json"), err: "not a directory"},
		{name: "a bare word is no source", arg: "ponytail", err: "not a marketplace source"},
		{name: "three segments", arg: "o/r/sub", err: "not a marketplace source"},
		{name: "a bad shorthand segment", arg: "o/r x", err: "not a marketplace source"},
		{name: "a git scheme is no source", arg: "git://127.0.0.1/x", err: "not a marketplace source"},
		{name: "empty", arg: "  ", err: "empty"},
	} {
		url, ref, err := marketplaceSource(c.arg)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || url != c.url || ref != c.ref {
			t.Errorf("%s: = %q, %q, %v, want %q, %q", c.name, url, ref, err, c.url, c.ref)
		}
	}
}

// TestMarketplacesUnreadable: a manifest that cannot be parsed is named on
// the marketplace row, never read as "no plugins"; a gone clone stays the
// documented empty state.
func TestMarketplacesUnreadable(t *testing.T) {
	d := config.Dirs{DataHome: t.TempDir()}
	write(t, filepath.Join(d.PluginsRoot(), "marketplaces", "broken"), map[string]string{
		ZemiMarketplaceFile: `{"name": "broken", "plugins": [`,
	})
	os.MkdirAll(filepath.Join(d.PluginsRoot(), "marketplaces", "gone"), 0o755)
	added := time.Now().UTC()
	f := marketplaceFile{Marketplaces: map[string]marketplaceReg{
		"broken": {Source: "owner/repo", Added: added},
		"gone":   {Source: "owner/repo", Added: added},
	}}
	if err := f.save(d); err != nil {
		t.Fatal(err)
	}
	ms, err := Marketplaces(d)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]MarketplaceInfo{}
	for _, m := range ms {
		got[m.Name] = m
	}
	if m := got["broken"]; m.Err == "" || len(m.Entries) != 0 {
		t.Errorf("broken: err %q, %d entries, want the read error and none", m.Err, len(m.Entries))
	}
	if m := got["gone"]; m.Err != "" || len(m.Entries) != 0 {
		t.Errorf("gone: err %q, %d entries, want the empty state", m.Err, len(m.Entries))
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
				{"name": "Rel One", "source": "./plugins/rel-one", "version": %q, "description": "the rel one",
				 "author": {"name": "Ada", "email": "ada@example.com"}},
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

	// a marketplace repo whose stable branch a pin holds the clone to; main
	// moves ahead of it, so only the pin explains what the clone shows
	pinned := filepath.Join(tmp, "src", "pinned")
	write(t, pinned, map[string]string{
		".claude-plugin/marketplace.json": `{"name": "pinned-mkt", "plugins": [{"name": "p-one", "source": "./p", "version": "1.0.0"}]}`,
		"p/commands/one.md":               "one",
	})
	gitIn(pinned, "init", "-q", "-b", "main")
	gitIn(pinned, "add", ".")
	gitIn(pinned, "commit", "-qm", "init")
	gitIn(pinned, "branch", "stable")
	write(t, pinned, map[string]string{"p/commands/main.md": "main"})
	gitIn(pinned, "add", ".")
	gitIn(pinned, "commit", "-qam", "main moves")
	gitIn(pinned, "checkout", "-q", "stable")
	write(t, pinned, map[string]string{
		".claude-plugin/marketplace.json": `{"name": "pinned-mkt", "plugins": [{"name": "p-one", "source": "./p", "version": "1.1.0"}]}`,
		"p/commands/two.md":               "two",
	})
	gitIn(pinned, "add", ".")
	gitIn(pinned, "commit", "-qam", "stable two")
	gitIn(pinned, "checkout", "-q", "main")

	// a marketplace whose one entry is the repo itself
	rootsrc := filepath.Join(tmp, "src", "rootsrc")
	write(t, rootsrc, map[string]string{
		".claude-plugin/marketplace.json": `{"name": "rootsrc-mkt", "plugins": [{"name": "root-one", "source": "./", "version": "1.0.0"}]}`,
		".zemi/init.lua":                  "-- the root plugin\n",
	})
	gitIn(rootsrc, "init", "-q")
	gitIn(rootsrc, "add", ".")
	gitIn(rootsrc, "commit", "-qm", "init")

	bump := func(relOne, entryManifest, subOne, gitOne string) {
		write(t, mkt, map[string]string{".claude-plugin/marketplace.json": manifest(relOne, entryManifest, subOne, gitOne)})
		gitIn(mkt, "commit", "-qam", "bump")
	}
	provides := func(t *testing.T, name string) []string {
		t.Helper()
		ps, _, err := Scan(d)
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

	// run invokes the command and checks its output, or the error when it
	// failed, against want.
	run := func(t *testing.T, args []string, want string, failed bool) {
		t.Helper()
		var out strings.Builder
		err := Run(t.Context(), d, args, &out)
		got := "\n" + strings.Join(strings.Fields(out.String()), " ") + "\n"
		if err != nil {
			got = err.Error()
		}
		if (err != nil) != failed || !strings.Contains(got, want) {
			t.Errorf("plugins %q = %q (err %v), want %q", args, got, err, want)
		}
		if dots, _ := filepath.Glob(filepath.Join(d.PluginsRoot(), "installed", ".*")); len(dots) > 0 {
			t.Fatalf("left %v", dots)
		}
	}

	t.Run("a marketplace repo registers by its manifest", func(t *testing.T) {
		run(t, []string{"marketplace", "add", mkt}, "marketplace demo-market added (7 plugins)", false)
		if _, err := os.Stat(filepath.Join(d.PluginsRoot(), "marketplaces", "demo-market", ".claude-plugin", "marketplace.json")); err != nil {
			t.Fatal(err)
		}
		ms, err := Marketplaces(d)
		if err != nil || len(ms) != 1 || ms[0].Name != "demo-market" || ms[0].Source != mkt || len(ms[0].Entries) != 7 {
			t.Fatalf("marketplaces: %+v, %v", ms, err)
		}
		added = ms[0].Added
		// the drill-down's provides read each entry's material on disk
		provs := EntryProvides(d, "demo-market")
		if got := provs["Rel One"]; !slices.Equal(got, []string{"2 skills"}) {
			t.Errorf("rel-one provides %v, want 2 skills", got)
		}
		if got := provs["entry-manifest"]; !slices.Equal(got, []string{"2 commands", "mcp: srv", "hooks: PostToolUse"}) {
			t.Errorf("entry-manifest provides %v", got)
		}
		if got, ok := provs["Own Manifest"]; !ok || len(got) != 0 {
			t.Errorf("own-manifest provides %v, %v, want present and empty", got, ok)
		}
		if got := provs["loose"]; !slices.Equal(got, []string{"1 command"}) {
			t.Errorf("loose provides %v, want 1 command", got)
		}
		if _, ok := provs["git-one"]; ok {
			t.Error("git-one's clone was never fetched, yet provides read it")
		}
	})
	t.Run("a same-source re-add refreshes the clone and keeps its Added", func(t *testing.T) {
		run(t, []string{"marketplace", "add", mkt}, "marketplace demo-market added (7 plugins)", false)
		ms, _ := Marketplaces(d)
		if !ms[0].Added.Equal(added) {
			t.Errorf("the re-add reset Added: %v, want %v", ms[0].Added, added)
		}
	})
	t.Run("a same name from another source is refused", func(t *testing.T) {
		run(t, []string{"marketplace", "add", mkt2}, "demo-market is already registered from "+mkt+", not "+mkt2, true)
	})
	t.Run("a marketplace with a climbing source is refused whole", func(t *testing.T) {
		run(t, []string{"marketplace", "add", bad}, "stay inside the marketplace", true)
		ms, err := os.ReadDir(filepath.Join(d.PluginsRoot(), "marketplaces"))
		if err != nil || len(ms) != 1 || ms[0].Name() != "demo-market" {
			t.Errorf("the refused marketplace registered anyway: %v, %v", ms, err)
		}
	})
	t.Run("a relative entry links into the marketplace clone", func(t *testing.T) {
		run(t, []string{"install", "rel-one@demo-market"}, "\nrel-one 1.0.0\n", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "rel-one")
		if target, err := os.Readlink(dir); err != nil || target != filepath.Join("..", "marketplaces", "demo-market", "plugins", "rel-one") {
			t.Fatalf("the install is not a link into the clone: %q, %v", target, err)
		}
		// the entry becomes the manifest only in the row: the clone's
		// tree stays what the marketplace committed
		if _, err := os.Lstat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil {
			t.Error("the entry manifest was written into the clone")
		}
		prov, ok := MarketplaceOf(d, "rel-one")
		if !ok || prov.Marketplace != "demo-market" || prov.Entry != "Rel One" || prov.Version != "1.0.0" ||
			prov.Kind != _kindLink || prov.ID == "" || len(prov.Manifest) == 0 {
			t.Errorf("provenance: %+v, %v", prov, ok)
		}
		if got := provides(t, "rel-one"); !slices.Equal(got, []string{"2 skills"}) {
			t.Errorf("provides %v", got)
		}
		m, err := ReadClaudeRaw(dir, "/data", prov.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		if m.Author.Name != "Ada" || m.Author.Email != "ada@example.com" {
			t.Errorf("the entry's author did not survive the manifest: %+v", m.Author)
		}
	})
	t.Run("component fields ride along an entry manifest", func(t *testing.T) {
		run(t, []string{"install", "entry-manifest@demo-market"}, "\nentry-manifest 0.2.0\n", false)
		prov, _ := MarketplaceOf(d, "entry-manifest")
		dir := filepath.Join(d.PluginsRoot(), "installed", "entry-manifest")
		m, err := ReadClaudeRaw(dir, "/data", prov.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.MCP) != 1 || m.MCP[0].Name != "srv" || len(m.Hooks) != 1 || m.Hooks[0].Event != "PostToolUse" {
			t.Errorf("the entry's components did not pass through: %+v", m)
		}
		if got := provides(t, "entry-manifest"); !slices.Equal(got, []string{"2 commands", "mcp: srv", "hooks: PostToolUse"}) {
			t.Errorf("provides %v", got)
		}
	})
	t.Run("an own manifest is kept", func(t *testing.T) {
		run(t, []string{"install", "own-manifest@demo-market"}, "\nown-manifest 5.0.0\n", false)
		b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "own-manifest", ".claude-plugin", "plugin.json"))
		if err != nil || !strings.Contains(string(b), `"version": "1.2.3"`) {
			t.Errorf("the on-disk manifest was overwritten: %q, %v", b, err)
		}
		if prov, _ := MarketplaceOf(d, "own-manifest"); len(prov.Manifest) != 0 {
			t.Errorf("the row carries a manifest the source has: %q", prov.Manifest)
		}
	})
	t.Run("a git entry clones into the cache and records the entry", func(t *testing.T) {
		run(t, []string{"install", "git-one@demo-market"}, "\ngit-one v0.1.0\n", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "git-one")
		if target, err := os.Readlink(dir); err != nil || !strings.Contains(target, _cacheDirName) {
			t.Fatalf("the git entry is not a cache link: %q, %v", target, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
		prov, ok := MarketplaceOf(d, "git-one")
		if !ok || prov.Kind != _kindCache || prov.ID == "" {
			t.Errorf("provenance: %+v, %v", prov, ok)
		}
	})
	t.Run("a git-subdir entry links its path in the cache clone", func(t *testing.T) {
		run(t, []string{"install", "sub-one@demo-market"}, "\nsub-one 0.1.0\n", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "sub-one")
		if _, err := os.Lstat(filepath.Join(dir, "commands", "one.md")); err != nil {
			t.Fatal(err)
		}
		if prov, ok := MarketplaceOf(d, "sub-one"); !ok || prov.Entry != "sub-one" || prov.Kind != _kindCache {
			t.Errorf("provenance: %+v, %v", prov, ok)
		}
	})
	t.Run("an npm entry is refused", func(t *testing.T) {
		run(t, []string{"install", "npm-one@demo-market"}, "not supported", true)
	})
	t.Run("an unknown marketplace", func(t *testing.T) {
		run(t, []string{"install", "x@nope"}, "no marketplace is named nope", true)
	})
	t.Run("an unknown entry", func(t *testing.T) {
		run(t, []string{"install", "nope@demo-market"}, "lists no plugin named nope", true)
	})
	t.Run("a zemi marketplace registers by its own manifest", func(t *testing.T) {
		run(t, []string{"marketplace", "add", both}, "marketplace bothz added (3 plugins)", false)
	})
	t.Run("a zemi entry installs under its exact name", func(t *testing.T) {
		run(t, []string{"install", "strict_one@bothz"}, "\nstrict_one 1.0.0\n", false)
		// the entry's uses ride in the row: the source carries no manifest
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "strict_one", ManifestFile)); err == nil {
			t.Error("the entry manifest was written into the clone")
		}
		ps, _, err := Scan(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.Name == "strict_one" && !slices.Equal(p.Uses, []string{"http"}) {
				t.Errorf("uses: %v", p.Uses)
			}
		}
	})
	t.Run("a zemi entry matches exactly, not sanitized", func(t *testing.T) {
		run(t, []string{"install", "strict-one@bothz"}, "lists no plugin named strict-one", true)
	})
	t.Run("a zemi entry's own manifest wins", func(t *testing.T) {
		run(t, []string{"install", "own-z@bothz"}, "\nown-z 2.0.0\n", false)
		b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "own-z", ManifestFile))
		if err != nil || !strings.Contains(string(b), "2.0.0") || strings.Contains(string(b), "9.9.9") {
			t.Errorf("the dir's manifest was overwritten: %q, %v", b, err)
		}
	})
	t.Run("a reinstall over an orphan row takes a fresh id", func(t *testing.T) {
		if prov, ok := MarketplaceOf(d, "strict_one"); ok {
			orphanID = prov.ID
		}
		// the dir went away without the registry learning it
		os.RemoveAll(filepath.Join(d.PluginsRoot(), "installed", "strict_one"))
		run(t, []string{"install", "strict_one@bothz"}, "\nstrict_one 1.0.0\n", false)
		prov, ok := MarketplaceOf(d, "strict_one")
		if !ok || prov.ID == "" || prov.ID == orphanID {
			t.Errorf("the reinstall did not reset the identity: %+v, stale %q", prov, orphanID)
		}
	})
	t.Run("a bare name found in two marketplaces lists the candidates", func(t *testing.T) {
		run(t, []string{"install", "rel-one"}, "rel-one@bothz, rel-one@demo-market", true)
	})
	t.Run("a bare name in no marketplace names the ones searched", func(t *testing.T) {
		run(t, []string{"install", "nope"}, `no marketplace lists "nope" (searched bothz, demo-market)`, true)
	})
	t.Run("a ghost marketplace registers", func(t *testing.T) {
		run(t, []string{"marketplace", "add", ghost}, "marketplace ghost added (1 plugin)", false)
	})
	t.Run("a ghost git entry installs from its own clone", func(t *testing.T) {
		run(t, []string{"install", "ghost-one@ghost"}, "\nghost-one v0.1.0\n", false)
	})
	t.Run("a bare name skips a marketplace whose clone is gone", func(t *testing.T) {
		os.RemoveAll(filepath.Join(d.PluginsRoot(), "marketplaces", "ghost"))
		run(t, []string{"install", "loose"}, "skipped ghost: its clone is missing", false)
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "loose")); err != nil {
			t.Error("loose did not install:", err)
		}
	})
	t.Run("a bare name with a skip reports it in the error too", func(t *testing.T) {
		run(t, []string{"install", "nope"}, "skipped ghost: its clone is missing", true)
	})
	t.Run("a marketplace update re-records the version its pull brought", func(t *testing.T) {
		if prov, ok := MarketplaceOf(d, "rel-one"); ok {
			relID = prov.ID
		}
		bump("1.1.0", "0.2.0", "0.1.0", "0.5.0")
		run(t, []string{"marketplace", "update", "demo-market"}, "marketplace demo-market updated rel-one 1.0.0 -> 1.1.0", false)
		// the link needs no re-copy: the pull moved what it points into
		ps, _, err := List(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.Name == "rel-one" {
				if p.Version != "1.1.0" {
					t.Errorf("the install shows %q, want 1.1.0", p.Version)
				}
				if desc := p.Desc; desc != "the rel one" {
					t.Errorf("description %q", desc)
				}
			}
		}
		prov, _ := MarketplaceOf(d, "rel-one")
		if prov.Version != "1.1.0" || prov.ID != relID {
			t.Errorf("provenance version %q id %q, want 1.1.0 and %q", prov.Version, prov.ID, relID)
		}
	})
	t.Run("disable a copy", func(t *testing.T) {
		run(t, []string{"disable", "entry-manifest"}, "", false)
	})
	t.Run("a marketplace update keeps a disabled install off", func(t *testing.T) {
		bump("1.1.0", "0.3.0", "0.1.0", "0.5.0")
		run(t, []string{"marketplace", "update", "demo-market"}, "entry-manifest 0.2.0 -> 0.3.0", false)
		ps, _, err := Scan(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.Name == "entry-manifest" && p.Enabled {
				t.Error("the update re-enabled the install")
			}
		}
	})
	t.Run("an update with nothing behind it stays quiet", func(t *testing.T) {
		run(t, []string{"marketplace", "update", "demo-market"}, "\nmarketplaces up to date\n", false)
	})
	t.Run("a moved clone with still versions prints updated alone", func(t *testing.T) {
		write(t, mkt, map[string]string{"plugins/rel-one/CHANGELOG.md": "note"})
		gitIn(mkt, "add", ".")
		gitIn(mkt, "commit", "-qm", "note")
		run(t, []string{"marketplace", "update", "demo-market"}, "\nmarketplace demo-market updated\n", false)
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "marketplaces", "demo-market", "plugins", "rel-one", "MARKER")); err == nil {
			t.Error("MARKER leaked into the clone, which a reset should have dropped")
		}
	})
	// one bare update pulls each of the two marketplaces once and
	// re-records every row: the installs behind the links moved already
	t.Run("one update pulls both marketplaces", func(t *testing.T) {
		write(t, both, map[string]string{".zemi/marketplace.json": bothManifest("1.1.0")})
		gitIn(both, "commit", "-qam", "bump")
		bump("1.3.0", "0.3.0", "0.1.0", "0.5.0")
		run(t, []string{"marketplace", "update"}, "marketplace bothz updated strict_one 1.0.0 -> 1.1.0 marketplace demo-market updated rel-one 1.1.0 -> 1.3.0 skipped marketplace ghost: its clone is missing", false)
	})
	t.Run("a git-subdir install moves with its own repo", func(t *testing.T) {
		write(t, zedsub, map[string]string{"plugins/sub/commands/two.md": "do two"})
		gitIn(zedsub, "add", ".")
		gitIn(zedsub, "commit", "-qm", "two")
		bump("1.3.0", "0.3.0", "0.2.0", "0.5.0")
		run(t, []string{"marketplace", "update", "demo-market"}, "marketplace demo-market updated sub-one 0.1.0 -> 0.2.0", false)
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "sub-one", "commands", "two.md")); err != nil {
			t.Errorf("the cache clone did not bring the new file: %v", err)
		}
		if prov, _ := MarketplaceOf(d, "sub-one"); prov.Version != "0.2.0" {
			t.Errorf("provenance version %q", prov.Version)
		}
	})
	// the entry's pin is what the cache clone sits at: a new tag alone
	// moves nothing, a repin in the manifest does
	t.Run("a cache install holds its pin until the manifest repins", func(t *testing.T) {
		gitIn(zed, "commit", "-q", "--allow-empty", "-m", "v2")
		gitIn(zed, "tag", "v0.2.0")
		run(t, []string{"marketplace", "update", "demo-market"}, "\nmarketplaces up to date\n", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "git-one")
		if out, err := (proc.Git{Dir: dir, Foreign: true}).Run(t.Context(), "tag", "--points-at", "HEAD"); err != nil || strings.TrimSpace(out) != "v0.1.0" {
			t.Errorf("the cache clone left its pin: %q, %v", out, err)
		}
	})
	t.Run("a repin moves the cache install", func(t *testing.T) {
		renamed := strings.Replace(manifest("1.3.0", "0.3.0", "0.2.0", "0.5.0"), `"ref": "v0.1.0"`, `"ref": "v0.2.0"`, 1)
		write(t, mkt, map[string]string{".claude-plugin/marketplace.json": renamed})
		gitIn(mkt, "commit", "-qam", "repin")
		run(t, []string{"marketplace", "update", "demo-market"}, "\nmarketplace demo-market updated\n", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "git-one")
		if out, err := (proc.Git{Dir: dir, Foreign: true}).Run(t.Context(), "tag", "--points-at", "HEAD"); err != nil || strings.TrimSpace(out) != "v0.2.0" {
			t.Errorf("the cache clone did not take the repin: %q, %v", out, err)
		}
	})
	t.Run("an entry the marketplace dropped is named, its install kept", func(t *testing.T) {
		renamed := strings.Replace(manifest("1.3.0", "0.3.0", "0.2.0", "0.5.0"),
			`{"name": "loose", "source": "./plugins/loose"}`, `{"name": "loose", "source": "./plugins/loose", "renames": {"gone": "loose"}}`, 1)
		renamed = strings.Replace(renamed, `{"name": "loose", "source": "./plugins/loose", "renames": {"gone": "loose"}}`,
			`{"name": "loose-x", "source": "./plugins/loose"}`, 1)
		write(t, mkt, map[string]string{".claude-plugin/marketplace.json": renamed})
		gitIn(mkt, "commit", "-qam", "drop loose")
		run(t, []string{"marketplace", "update", "demo-market"}, "loose is no longer in marketplace demo-market", false)
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "loose")); err != nil {
			t.Error("the dropped entry's install went away:", err)
		}
	})
	t.Run("a delete drops the provenance and the link", func(t *testing.T) {
		run(t, []string{"delete", "rel-one"}, "", false)
		if _, ok := MarketplaceOf(d, "rel-one"); ok {
			t.Error("the provenance outlived the plugin")
		}
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "installed", "rel-one")); err == nil {
			t.Error("the link is still there")
		}
	})
	t.Run("enable an unknown name", func(t *testing.T) {
		run(t, []string{"enable", "rel-one"}, "no plugin is named", true)
	})
	t.Run("a marketplace with installs is not removable", func(t *testing.T) {
		run(t, []string{"marketplace", "remove", "bothz"}, "still has installs updating from it: own-z, strict_one", true)
	})
	t.Run("a marketplace with a cache install is not removable either", func(t *testing.T) {
		run(t, []string{"marketplace", "remove", "ghost"}, "still has installs updating from it: ghost-one", true)
	})
	t.Run("a ref pins the clone to its branch", func(t *testing.T) {
		run(t, []string{"marketplace", "add", "file://" + pinned + "#stable"}, "marketplace pinned-mkt added (1 plugin)", false)
		dir := filepath.Join(d.PluginsRoot(), "marketplaces", "pinned-mkt")
		if _, err := os.Lstat(filepath.Join(dir, "p", "commands", "two.md")); err != nil {
			t.Fatal("the pin did not check stable out:", err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "p", "commands", "main.md")); err == nil {
			t.Error("the checkout took the default branch")
		}
		ms, _ := Marketplaces(d)
		if !slices.ContainsFunc(ms, func(m MarketplaceInfo) bool { return m.Name == "pinned-mkt" && m.Source == "file://"+pinned+"#stable" }) {
			t.Errorf("the row lost its pin: %+v", ms)
		}
	})
	t.Run("another ref of a taken name is refused", func(t *testing.T) {
		run(t, []string{"marketplace", "add", "file://" + pinned + "#main"}, "already registered from file://"+pinned+"#stable", true)
	})
	t.Run("an install reads the pinned clone", func(t *testing.T) {
		run(t, []string{"install", "p-one@pinned-mkt"}, "\np-one 1.1.0\n", false)
	})
	t.Run("a marketplace update follows the pin", func(t *testing.T) {
		gitIn(pinned, "checkout", "-q", "stable")
		write(t, pinned, map[string]string{"p/commands/three.md": "three"})
		gitIn(pinned, "add", ".")
		gitIn(pinned, "commit", "-qam", "three")
		gitIn(pinned, "checkout", "-q", "main")
		run(t, []string{"marketplace", "update", "pinned-mkt"}, "marketplace pinned-mkt updated", false)
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "marketplaces", "pinned-mkt", "p", "commands", "three.md")); err != nil {
			t.Error("the update did not take the pinned ref:", err)
		}
	})
	t.Run("a marketplace of one root entry registers", func(t *testing.T) {
		run(t, []string{"marketplace", "add", rootsrc}, "marketplace rootsrc-mkt added (1 plugin)", false)
	})
	t.Run("an entry that is the marketplace's own repo links at the clone root", func(t *testing.T) {
		run(t, []string{"install", "root-one@rootsrc-mkt"}, "", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "root-one")
		if target, err := os.Readlink(dir); err != nil || target != filepath.Join("..", "marketplaces", "rootsrc-mkt") {
			t.Fatalf("the root entry is not a link at the clone root: %q, %v", target, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, InitFile)); err != nil {
			t.Fatal(err)
		}
		prov, ok := MarketplaceOf(d, "root-one")
		if !ok || prov.Kind != _kindLink || prov.Version != "1.0.0" {
			t.Errorf("provenance: %+v, %v", prov, ok)
		}
	})
	// the installs of the copy model migrate on the next command: a
	// relative copy re-links into the clone, a git clone moves whole
	t.Run("a legacy copy install re-links", func(t *testing.T) {
		legacy := filepath.Join(d.PluginsRoot(), "installed", "legacy-rel")
		if err := os.CopyFS(legacy, os.DirFS(filepath.Join(d.PluginsRoot(), "marketplaces", "demo-market", "plugins", "own-manifest"))); err != nil {
			t.Fatal(err)
		}
		f, err := readMarketplaces(d)
		if err != nil {
			t.Fatal(err)
		}
		f.Installs["legacy-rel"] = installReg{Marketplace: "demo-market", Entry: "Own Manifest", Version: "1.2.3", Kind: "copy", ID: "legacy"}
		if err := f.save(d); err != nil {
			t.Fatal(err)
		}
		run(t, []string{"marketplace", "list"}, "", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "legacy-rel")
		target, err := os.Readlink(dir)
		if err != nil || !strings.HasSuffix(target, filepath.Join("demo-market", "plugins", "own-manifest")) {
			t.Fatalf("the legacy copy did not re-link: %q, %v", target, err)
		}
		prov, _ := MarketplaceOf(d, "legacy-rel")
		if prov.Kind != _kindLink || prov.ID != "legacy" {
			t.Errorf("provenance: %+v", prov)
		}
	})
	t.Run("a legacy git-kind install moves into the cache", func(t *testing.T) {
		legacy := filepath.Join(d.PluginsRoot(), "installed", "legacy-git")
		if _, err := (proc.Git{Dir: d.PluginsRoot(), Foreign: true}).Run(t.Context(), "clone", "-q", "file://"+zed, legacy); err != nil {
			t.Fatal(err)
		}
		f, err := readMarketplaces(d)
		if err != nil {
			t.Fatal(err)
		}
		f.Installs["legacy-git"] = installReg{Marketplace: "demo-market", Entry: "git-one", Kind: "git", ID: "legacygit"}
		if err := f.save(d); err != nil {
			t.Fatal(err)
		}
		run(t, []string{"marketplace", "list"}, "", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "legacy-git")
		target, err := os.Readlink(dir)
		if err != nil || !strings.Contains(target, _cacheDirName) {
			t.Fatalf("the legacy clone did not move into the cache: %q, %v", target, err)
		}
		if _, err := os.Lstat(filepath.Join(d.PluginsRoot(), "marketplaces", _cacheDirName, "demo-market", "legacy-git", ".git")); err != nil {
			t.Error("the clone did not survive the move:", err)
		}
		if prov, _ := MarketplaceOf(d, "legacy-git"); prov.Kind != _kindCache || prov.ID != "legacygit" {
			t.Errorf("provenance: %+v", prov)
		}
	})
	t.Run("a legacy git-subdir copy re-links at its marketplace update", func(t *testing.T) {
		legacy := filepath.Join(d.PluginsRoot(), "installed", "legacy-sub")
		if err := os.CopyFS(legacy, os.DirFS(filepath.Join(d.PluginsRoot(), "marketplaces", "demo-market", "plugins", "own-manifest"))); err != nil {
			t.Fatal(err)
		}
		f, err := readMarketplaces(d)
		if err != nil {
			t.Fatal(err)
		}
		f.Installs["legacy-sub"] = installReg{Marketplace: "demo-market", Entry: "sub-one", Version: "0.2.0", Kind: _kindCopy, ID: "legacysub"}
		if err := f.save(d); err != nil {
			t.Fatal(err)
		}
		run(t, []string{"marketplace", "update", "demo-market"}, "", false)
		dir := filepath.Join(d.PluginsRoot(), "installed", "legacy-sub")
		target, err := os.Readlink(dir)
		if err != nil || !strings.Contains(target, _cacheDirName) {
			t.Fatalf("the legacy copy did not re-link: %q, %v", target, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "commands", "one.md")); err != nil {
			t.Error("the cache clone's subdir is not behind the link:", err)
		}
		if prov, _ := MarketplaceOf(d, "legacy-sub"); prov.Kind != _kindCache || prov.ID != "legacysub" {
			t.Errorf("provenance: %+v", prov)
		}
	})
	if _, err := os.Stat(filepath.Join(d.PluginsRoot(), "marketplaces", "demo-market", "plugins", "own-manifest")); err != nil {
		t.Errorf("the marketplace clone does not serve the other entries: %v", err)
	}
}
