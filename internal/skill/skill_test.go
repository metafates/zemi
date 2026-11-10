package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		dirName string
		content string
		want    string
	}{
		{"no skill file", "x", "", "no SKILL.md"},
		{"no frontmatter", "x", "just text", "frontmatter block"},
		{"unterminated", "x", "---\nname: x\n", "unterminated"},
		{"name mismatch", "x", "---\nname: y\ndescription: d\n---\n", "must match the directory"},
		{"bad dir name", "Bad_Name", "---\nname: Bad_Name\ndescription: d\n---\n", "directory name"},
		{"a SKILL.md over the cap", "x", strings.Repeat("a", MaxFileBytes+1), "SKILL.md: exceeds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), c.dirName)
			if c.content != "" {
				writeFile(t, filepath.Join(dir, "SKILL.md"), c.content)
			}
			if _, err := Parse(dir, User, ""); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%v, want %q", err, c.want)
			}
		})
	}
}

func TestParseSymlinkedSkillFile(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "x")
	target := filepath.Join(tmp, "elsewhere.md")
	writeFile(t, target, "---\nname: x\ndescription: d\n---\n")
	writeFile(t, filepath.Join(dir, "placeholder"), "")
	if err := os.Symlink(target, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Parse(dir, User, ""); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
}

// A SKILL.md that is not a regular file must be refused, not read: reading a
// FIFO there blocks the boot step that discovers skills, and Body blocks a turn.
func TestParseNonRegularSkillFile(t *testing.T) {
	cases := []struct {
		name string
		make func(t *testing.T, path string)
	}{
		{"directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", makeFifo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "x")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			c.make(t, filepath.Join(dir, "SKILL.md"))
			if _, err := Parse(dir, User, ""); err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("parse err=%v", err)
			}
			if _, err := (Skill{Dir: dir}).Body(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("body err=%v", err)
			}
		})
	}
}

// A skill that fails to parse still holds its name: a lower-trust directory's
// same-named skill must not slip in behind the warning about the broken one.
// A symlinked skill directory is refused like a symlinked SKILL.md, with a
// warning naming it: a stowed skill must not vanish silently.
func TestDiscoverSymlinkedSkillDir(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "real", "SKILL.md"), "---\nname: real\ndescription: d\n---\nbody")
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	got, warns := Discover(Dir{Path: base, Source: User})
	if len(got) != 1 || got[0].Name != "real" || len(warns) != 1 || !strings.Contains(warns[0].Error(), "symlink") {
		t.Fatalf("discover: %+v %v", got, warns)
	}
}
