package plugins

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

// TestPlugins walks one plugin tree through the CLI verbs, in order.
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
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "init.lua"), []byte("-- the "+name+" plugin\n"), 0o644)
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
	os.MkdirAll(local, 0o755)
	installed := filepath.Join(d.PluginsRoot(), "installed")

	for _, c := range []struct {
		name   string
		before func()
		args   []string // nil lists with Table
		want   string   // in the output (words joined by one space, a newline on each end), or in the error when err
		err    bool
	}{
		{name: "local only", args: nil, want: "loc local on " + local},
		{name: "install takes the highest tag", args: []string{"install", "file://" + tagged}, want: "\nx v1.0.0\n"},
		{name: "install a branch under a name", args: []string{"install", filepath.Join(tmp, "src", "y.git"), "why"}, want: "\nwhy "},
		{name: "a taken name", args: []string{"install", "file://" + tagged}, want: "already named x", err: true},
		{name: "a local plugin's name", args: []string{"install", "file://" + tagged, "loc"}, want: "already named loc", err: true},
		{name: "a name that climbs", args: []string{"install", "file://" + tagged, "../x"}, want: "not a plugin name", err: true},
		{name: "a dotted name, which require cannot reach", args: []string{"install", "file://" + tagged, "x.zemi"}, want: "not a plugin name", err: true},
		{name: "a name like a flag", args: []string{"install", "file://" + tagged, "-x"}, want: "not a plugin name", err: true},
		{name: "a URL like a flag", args: []string{"install", "--upload-pack=x"}, want: "not a git URL", err: true},
		{name: "a remote helper", args: []string{"install", "ext::sh -c touch% " + marker, "e"}, want: "not allowed", err: true},
		{name: "a plaintext transport", args: []string{"install", "git://127.0.0.1/x", "g"}, want: "not allowed", err: true},
		{name: "a clone that fails leaves nothing", args: []string{"install", "file://" + tmp + "/nope", "nope"}, want: "clone", err: true},
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
		{name: "list", args: nil, want: "x v0.10.0 off file://" + tagged},
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
				if _, err := Install(t.Context(), d, older, ""); err != nil {
					t.Fatal(err)
				}
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
		{name: "reinstall after the delete", args: []string{"install", "file://" + tagged}, want: "\nx v1.0.0\n"},
		{name: "an unknown verb", args: []string{"frob"}, want: "usage:", err: true},
		{name: "enable takes one name", args: []string{"enable"}, want: "usage:", err: true},
	} {
		if c.before != nil {
			c.before()
		}
		var out strings.Builder
		ps, err := List(t.Context(), d)
		if c.args == nil {
			err = errors.Join(err, Table(&out, ps))
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
	// a clone's init.lua may be a symlink to /dev/zero: only a regular one is read
	leak := filepath.Join(tmp, "leak.lua")
	os.WriteFile(leak, []byte("-- leaked\n"), 0o644)
	os.Symlink(leak, filepath.Join(local, "init.lua"))
	if ps, _ := List(t.Context(), d); ps[0].Desc != "" {
		t.Errorf("%s: description %q read through a symlink", ps[0].Name, ps[0].Desc)
	}
	if off, _ := os.ReadFile(filepath.Join(d.PluginsRoot(), "disabled")); len(off) != 0 {
		t.Errorf("disabled = %q after the delete and the reinstall", off)
	}
}
