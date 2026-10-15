package brain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/store"
)

// testPartitions gives both partitions of one throwaway state and data
// home, directories unwritten until a test writes into them.
func testPartitions(t *testing.T) (user, private Partition) {
	t.Helper()
	home := t.TempDir()
	state, data := filepath.Join(home, "state"), filepath.Join(home, "data")
	return UserPartition(data, state), PrivatePartition(state, filepath.Join(home, "repo"))
}

// testPartition gives one private partition whose directories a test
// creates by writing into it.
func testPartition(t *testing.T) Partition {
	t.Helper()
	_, private := testPartitions(t)
	return private
}

func TestPartitionRoots(t *testing.T) {
	home := t.TempDir()
	state, data := filepath.Join(home, "state"), filepath.Join(home, "data")
	root := filepath.Join(home, "repo")

	private := PrivatePartition(state, root)
	if want := filepath.Join(filepath.Dir(store.ProjectDBPath(state, root)), "brain"); private.Dir != want {
		t.Fatalf("private dir = %s, want the recipes-derived %s", private.Dir, want)
	}
	if private.DB != filepath.Join(filepath.Dir(private.Dir), "zemi.db") {
		t.Fatalf("private DB = %s, want the DB the dir is beside", private.DB)
	}
	if private.Kind.Scope() != "project" {
		t.Fatalf("private scope = %s", private.Kind.Scope())
	}

	user := UserPartition(data, state)
	if user.Dir != filepath.Join(data, "brain", "user") {
		t.Fatalf("user dir = %s", user.Dir)
	}
	if user.DB != filepath.Join(state, "zemi.db") {
		t.Fatalf("user DB = %s", user.DB)
	}
	if user.Kind.Scope() != "user" {
		t.Fatalf("user scope = %s", user.Kind.Scope())
	}

	// no home or no repository: nothing to load, lint, lock or write
	for _, p := range []Partition{
		PrivatePartition(state, ""),
		PrivatePartition("", root),
		UserPartition("", state),
		UserPartition(data, ""),
	} {
		if !p.Zero() {
			t.Fatalf("%+v: expected zero", p)
		}
	}
	// the fixed kind order the locks take, user then private
	if User >= Private {
		t.Fatal("User must sort before Private")
	}
}

func TestParseFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		fm      Frontmatter
		wantErr bool
	}{
		{
			name:    "both fields, one glob",
			content: "---\ndescription: Map of the packages.\nread_if: before any code change\npaths: internal/**/*.go\n---\nbody",
			fm:      Frontmatter{Description: "Map of the packages.", ReadIf: "before any code change", Paths: []string{"internal/**/*.go"}},
		},
		{
			name:    "paths as a list",
			content: "---\ndescription: d\nread_if: r\npaths:\n  - a*.go\n  - b/**\n---\n",
			fm:      Frontmatter{Description: "d", ReadIf: "r", Paths: []string{"a*.go", "b/**"}},
		},
		{
			name:    "no block",
			content: "just a body",
		},
		{
			name:    "empty fields are lint's, not parse's",
			content: "---\ndescription: \"\"\nread_if: r\n---\n",
			fm:      Frontmatter{ReadIf: "r"},
		},
		{
			name:    "unterminated block",
			content: "---\ndescription: d\n",
			wantErr: true,
		},
		{
			name:    "broken yaml",
			content: "---\ndescription: [unclosed\n---\n",
			wantErr: true,
		},
	} {
		fm, err := ParseFrontmatter(tc.content)
		if tc.wantErr != (err != nil) {
			t.Fatalf("%s: err = %v, wantErr %t", tc.name, err, tc.wantErr)
		}
		if fm.Description != tc.fm.Description || fm.ReadIf != tc.fm.ReadIf {
			t.Fatalf("%s: fields = %+v, want %+v", tc.name, fm, tc.fm)
		}
		if strings.Join(fm.Paths, "|") != strings.Join(tc.fm.Paths, "|") {
			t.Fatalf("%s: paths = %v, want %v", tc.name, fm.Paths, tc.fm.Paths)
		}
	}
}

func TestBadGlob(t *testing.T) {
	for _, tc := range []struct {
		paths   []string
		wantErr bool
	}{
		{nil, false},
		{[]string{"*.md", "internal/**"}, false},
		{[]string{"ok", "bad.["}, true},
	} {
		err := Frontmatter{Paths: tc.paths}.BadGlob()
		if tc.wantErr != (err != nil) {
			t.Fatalf("%v: err = %v, wantErr %t", tc.paths, err, tc.wantErr)
		}
	}
}

func TestNormalizeStripsAndCaps(t *testing.T) {
	if got := Normalize("the\tdploy gate\n \x1b[31mred\r"); got != "thedploy gate [31mred" {
		t.Fatalf("Normalize left control characters: %q", got)
	}
	long := strings.Repeat("w", FieldCap+50)
	if got := Normalize(long); len([]rune(got)) != FieldCap || got != long[:FieldCap] {
		t.Fatalf("Normalize did not cap at %d runes", FieldCap)
	}
	if got := Normalize("short"); got != "short" {
		t.Fatalf("Normalize changed plain text: %q", got)
	}
}

func TestLoad(t *testing.T) {
	t.Run("absent partition is empty, not an error", func(t *testing.T) {
		p := testPartition(t)
		l := Load(p)
		if len(l.Entries) != 0 || l.Mem != nil || l.MemErr != nil || l.Over {
			t.Fatalf("absent partition loaded %+v", l)
		}
	})

	t.Run("per-file errors are preserved", func(t *testing.T) {
		p := testPartition(t)
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		good := "---\ndescription: fine\nread_if: always fine\n---\nbody\n"
		for name, content := range map[string]string{
			"good.md":   good,
			"broken.md": "---\ndescription: [unclosed\n",
		} {
			if err := os.WriteFile(filepath.Join(p.Dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		l := Load(p)
		if len(l.Entries) != 2 {
			t.Fatalf("entries = %+v", l.Entries)
		}
		if l.Entries[0].Name != "broken.md" || l.Entries[0].Err == nil {
			t.Fatalf("broken file not named: %+v", l.Entries[0])
		}
		if l.Entries[1].Name != "good.md" || l.Entries[1].Err != nil || l.Entries[1].FM.Description != "fine" {
			t.Fatalf("good file damaged: %+v", l.Entries[1])
		}
	})

	t.Run("file ceiling", func(t *testing.T) {
		p := testPartition(t)
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := []byte("---\ndescription: d\nread_if: r\n---\n")
		for i := range MaxCatalogFiles + 1 {
			if err := os.WriteFile(filepath.Join(p.Dir, fmt.Sprintf("f%02d.md", i)), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		l := Load(p)
		if len(l.Entries) != MaxCatalogFiles || !l.Over {
			t.Fatalf("entries = %d, over = %t", len(l.Entries), l.Over)
		}
	})

	t.Run("file-safety keeps an out-of-partition file out", func(t *testing.T) {
		p := testPartition(t)
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := []byte("---\ndescription: stolen\nread_if: r\n---\n")
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(p.Dir, "link.md")); err != nil {
			t.Fatal(err)
		}
		l := Load(p)
		if len(l.Entries) != 0 {
			t.Fatalf("unsafe files cataloged: %+v", l.Entries)
		}
	})

	t.Run("frontmatter parse rides the peek", func(t *testing.T) {
		p := testPartition(t)
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		block := "---\ndescription: " + strings.Repeat("w", FrontPeek) + "\n---\n"
		if err := os.WriteFile(filepath.Join(p.Dir, "long.md"), []byte(block), 0o600); err != nil {
			t.Fatal(err)
		}
		l := Load(p)
		if len(l.Entries) != 1 || l.Entries[0].Err == nil {
			t.Fatalf("unterminated block not named: %+v", l.Entries)
		}
	})
}

func TestLoadMemFile(t *testing.T) {
	p := testPartition(t)
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.memPath(), []byte("- [abcd2345 learned 2026-10-01] hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Load(p)
	if string(l.Mem) != "- [abcd2345 learned 2026-10-01] hi\n" || l.MemErr != nil {
		t.Fatalf("mem = %q, err = %v", l.Mem, l.MemErr)
	}
	// the reserved name is not a catalog entry
	if len(l.Entries) != 0 {
		t.Fatalf("memories.md cataloged: %+v", l.Entries)
	}

	// an unreadable reserved file is a named error, never dropped silently
	if err := os.Chmod(p.memPath(), 0o000); err != nil {
		t.Skip("cannot make a file unreadable here")
	}
	_, err := os.ReadFile(p.memPath())
	if !errors.Is(err, fs.ErrPermission) {
		t.Skip("running as root ignores the mode")
	}
	if l := Load(p); l.MemErr == nil {
		t.Fatal("unreadable memories.md read without error")
	}
}
