package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/perm"
)

// TestXDG: every root stays absolute; with no $HOME home is the passwd
// entry's.
func TestXDG(t *testing.T) {
	tmp := t.TempDir()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, zemi, xdgHome, home, want, err string }{
		{"zemi override", "/z", "/x", tmp, "/z", ""},
		{"xdg", "", "/x", tmp, "/x/zemi", ""},
		{"relative xdg ignored", "", "x", tmp, filepath.Join(tmp, ".config/zemi"), ""},
		{"relative override refused", "z", "/x", tmp, "", `ZEMI_CONFIG_HOME="z" is a relative path`},
		{"no home", "", "", "", filepath.Join(u.HomeDir, ".config/zemi"), ""},
	} {
		t.Setenv("ZEMI_CONFIG_HOME", c.zemi)
		t.Setenv("XDG_CONFIG_HOME", c.xdgHome)
		t.Setenv("HOME", c.home)
		d, err := ResolveDirs()
		if d.ConfigHome != c.want || (err == nil) != (c.err == "") || err != nil && !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: ConfigHome = %q, %v, want %q, %q", c.name, d.ConfigHome, err, c.want, c.err)
		}
	}
}

// TestEnsureStarterInit: a fresh config home gets the starter, an
// init.lua the user already has is never touched.
func TestEnsureStarterInit(t *testing.T) {
	tmp := t.TempDir()
	d := Dirs{ConfigHome: filepath.Join(tmp, "config"), DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state")}
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d.ConfigHome, "init.lua")
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), `zemi.provider.del("openai")`) {
		t.Fatalf("starter init.lua = %q, %v", body, err)
	}
	if err := os.WriteFile(path, []byte("-- mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path); string(body) != "-- mine" {
		t.Fatalf("Ensure overwrote init.lua: %q", body)
	}
}

func TestTrustRoot(t *testing.T) {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	for _, dir := range []string{"main/.git/worktrees/wt", "main/.git/modules/sub", "main/sub", "main/deep/er", "wt", "forged", "evil/.git/worktrees/x", "norepo"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(tmp, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a linked worktree as git writes it, with a relative commondir
	write("wt/.git", "gitdir: "+filepath.Join(tmp, "main/.git/worktrees/wt")+"\n")
	write("main/.git/worktrees/wt/gitdir", filepath.Join(tmp, "wt/.git")+"\n")
	write("main/.git/worktrees/wt/commondir", "../..\n")
	// a checkout claiming main's worktree entry, whose gitdir points at wt
	write("forged/.git", "gitdir: "+filepath.Join(tmp, "main/.git/worktrees/wt"))
	write("main/sub/.git", "gitdir: ../.git/modules/sub")
	// an entry the forger wrote itself cannot name another repository
	write("evil/.git/worktrees/x/gitdir", filepath.Join(tmp, "norepo/.git"))
	write("evil/.git/worktrees/x/commondir", filepath.Join(tmp, "main/.git"))
	write("norepo/.git", "gitdir: "+filepath.Join(tmp, "evil/.git/worktrees/x"))
	for _, c := range []struct{ name, cwd, want string }{
		{"repo root", "main", "main"},
		{"subdirectory", "main/deep/er", "main"},
		{"linked worktree", "wt", "main"},
		{"forged gitdir", "forged", "forged"},
		{"submodule", "main/sub", "main/sub"},
		{"commondir elsewhere", "norepo", "norepo"},
	} {
		if got := (Dirs{ProjectDir: filepath.Join(tmp, c.cwd)}).TrustRoot(); got != filepath.Join(tmp, c.want) {
			t.Errorf("%s: got %s", c.name, got)
		}
	}
	outside := t.TempDir()
	if got, _ := filepath.EvalSymlinks(outside); (Dirs{ProjectDir: outside}).TrustRoot() != got {
		t.Errorf("no repository: got %s", (Dirs{ProjectDir: outside}).TrustRoot())
	}
}

func TestRepoRootStopsAtHome(t *testing.T) {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	for _, dir := range []string{"home/.git", "home/.agents", "home/work/sub", "home/repo/.git", "home/repo/sub"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	for _, c := range []struct {
		name, cwd, root, config string
		folds                   bool // needs a case-folding volume
	}{
		{"dotfiles repo is not the root below home", "home/work/sub", "home/work/sub", "home/work/sub/.agents", false},
		{"repo below home", "home/repo/sub", "home/repo", "home/repo/.agents", false},
		{"home itself", "home", "home", "home/.agents", false},
		{"case variant of home", "HOME/work/sub", "HOME/work/sub", "HOME/work/sub/.agents", true},
	} {
		if c.folds && perm.FoldCase("A") == "A" {
			continue
		}
		d := Dirs{ProjectDir: filepath.Join(tmp, c.cwd)}
		if got := d.RepoRoot(); got != filepath.Join(tmp, c.root) {
			t.Errorf("%s: root %s", c.name, got)
		}
		if got := d.TrustRoot(); got != filepath.Join(tmp, c.root) {
			t.Errorf("%s: trust root %s", c.name, got)
		}
		if got := d.ProjectConfigDir(); got != filepath.Join(tmp, c.config) {
			t.Errorf("%s: config dir %s", c.name, got)
		}
	}
}

func TestProjectConfigDir(t *testing.T) {
	tmp := t.TempDir()
	for _, dir := range []string{"repo/.git", "repo/sub/.agents", "stray/.agents", "stray/norepo"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ cwd, want string }{
		{"repo/sub", "repo/.agents"},             // the repository root's, never a nearer one
		{"stray/norepo", "stray/norepo/.agents"}, // no repo: a parent's is not used
	} {
		got := Dirs{ProjectDir: filepath.Join(tmp, c.cwd)}.ProjectConfigDir()
		if got != filepath.Join(tmp, c.want) {
			t.Errorf("%s: got %s", c.cwd, got)
		}
	}
}

func TestWorktreesIgnore(t *testing.T) {
	for _, c := range []struct {
		name, have, want, setup string
		own                     bool
	}{
		{"new file ignores everything", "", "/*\n", "dir", true},
		{"an existing file is left alone", "keep\n", "keep\n", "dir", true},
		{"no directory, no file", "", "", "none", false},
		{"a symlinked .agents is not followed", "", "", "linked", false},
	} {
		d := Dirs{ProjectDir: t.TempDir()}
		os.Mkdir(filepath.Join(d.ProjectDir, ".git"), 0o755)
		wt := d.WorktreesDir()
		switch c.setup {
		case "dir":
			os.MkdirAll(wt, 0o755)
		case "linked":
			outside := t.TempDir()
			os.Mkdir(filepath.Join(outside, "worktrees"), 0o755)
			os.Symlink(outside, filepath.Dir(wt))
		}
		if c.have != "" {
			os.WriteFile(filepath.Join(wt, ".gitignore"), []byte(c.have), 0o644)
		}
		own, err := d.WorktreesIgnore()
		if err != nil {
			t.Fatal(err)
		}
		// through the link too: nothing may land in the target
		if b, _ := os.ReadFile(filepath.Join(wt, ".gitignore")); string(b) != c.want || own != c.own {
			t.Errorf("%s: %q own %v, want %q own %v", c.name, b, own, c.want, c.own)
		}
	}
}
