package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
