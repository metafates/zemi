package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/perm"
)

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
	for _, dir := range []string{"home/.git", "home/.zemi", "home/work/sub", "home/repo/.git", "home/repo/sub"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	for _, c := range []struct {
		name, cwd, root, config string
		folds                   bool // needs a case-folding volume
	}{
		{"dotfiles repo is not the root below home", "home/work/sub", "home/work/sub", "home/work/sub/.zemi", false},
		{"repo below home", "home/repo/sub", "home/repo", "home/repo/sub/.zemi", false},
		{"home itself", "home", "home", "home/.zemi", false},
		{"case variant of home", "HOME/work/sub", "HOME/work/sub", "HOME/work/sub/.zemi", true},
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
	for _, dir := range []string{"repo/.git", "repo/.zemi", "repo/sub/deep", "stray/.zemi", "stray/norepo"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ cwd, want string }{
		{"repo", "repo/.zemi"},
		{"repo/sub/deep", "repo/.zemi"},
		{"stray/norepo", "stray/norepo/.zemi"}, // no repo: a parent's .zemi is not used
	} {
		got := Dirs{ProjectDir: filepath.Join(tmp, c.cwd)}.ProjectConfigDir()
		if got != filepath.Join(tmp, c.want) {
			t.Errorf("%s: got %s", c.cwd, got)
		}
	}
}

func TestIgnoreLocal(t *testing.T) {
	for _, c := range []struct{ name, have, want string }{
		{"new file", "", "worktrees/\n.gitignore\n"},
		{"an older zemi's file gets the self line", "worktrees/\n", "worktrees/\n.gitignore\n"},
		{"a team's complete file is left alone", "cache\nworktrees/\n", "cache\nworktrees/\n"},
	} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".zemi", ".gitignore")
		for _, d := range []string{filepath.Join(tmp, ".git"), filepath.Dir(path)} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if c.have != "" {
			os.WriteFile(path, []byte(c.have), 0o644)
		}
		if err := (Dirs{ProjectDir: tmp}).IgnoreLocal(); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(path); string(b) != c.want {
			t.Errorf("%s: %q, want %q", c.name, b, c.want)
		}
	}
}
