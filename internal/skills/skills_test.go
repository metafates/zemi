package skills

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

func TestParseValid(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local-integration")
	writeFile(t, filepath.Join(dir, "SKILL.md"), `---
name: local-integration
description: run the local integration suite
---

Body instructions here.
`)
	writeFile(t, filepath.Join(dir, "zemi-skill.lua"), "-- hook\n")
	sk, err := Parse(dir, Authored)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sk.Name != "local-integration" || sk.Description != "run the local integration suite" {
		t.Fatalf("skill: %+v", sk)
	}
	if sk.Source != Authored {
		t.Fatalf("source: %s", sk.Source)
	}
	if sk.Hook == "" {
		t.Fatal("hook missing")
	}

	crlf := filepath.Join(t.TempDir(), "crlf")
	writeFile(t, filepath.Join(crlf, "SKILL.md"), "---\r\nname: crlf\r\ndescription: saved on windows\r\n---\r\n\r\nBody.\r\n")
	if sk, err := Parse(crlf, Authored); err != nil || sk.Description != "saved on windows" {
		t.Fatalf("crlf: %+v %v", sk, err)
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
		{"missing name", "x", "---\ndescription: d\n---\n", "name is required"},
		{"missing description", "x", "---\nname: x\n---\n", "description is required"},
		{"name mismatch", "x", "---\nname: y\ndescription: d\n---\n", "must match the directory"},
		{"bad dir name", "Bad_Name", "---\nname: Bad_Name\ndescription: d\n---\n", "directory name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), c.dirName)
			if c.content != "" {
				writeFile(t, filepath.Join(dir, "SKILL.md"), c.content)
			}
			if _, err := Parse(dir, Authored); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%v, want %q", err, c.want)
			}
		})
	}
}

func TestParseDescriptionCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	long := strings.Repeat("a", MaxDescriptionRunes+1)
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: x\ndescription: "+long+"\n---\n")
	if _, err := Parse(dir, Authored); err == nil || !strings.Contains(err.Error(), "runes") {
		t.Fatalf("err=%v", err)
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
	if _, err := Parse(dir, Authored); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
}

func TestDiscoverMergesAuthoredOverLearned(t *testing.T) {
	root := t.TempDir()
	authored, learned := filepath.Join(root, "authored"), filepath.Join(root, "learned")
	writeFile(t, filepath.Join(authored, "alpha", "SKILL.md"), "---\nname: alpha\ndescription: authored alpha\n---\n")
	writeFile(t, filepath.Join(authored, "broken", "SKILL.md"), "not frontmatter")
	writeFile(t, filepath.Join(learned, "alpha", "SKILL.md"), "---\nname: alpha\ndescription: learned alpha\n---\n")
	writeFile(t, filepath.Join(learned, "beta", "SKILL.md"), "---\nname: beta\ndescription: learned beta\n---\n")

	got, warns := Discover(authored, "", learned)
	if len(warns) != 1 || !strings.Contains(warns[0], "broken") {
		t.Fatalf("warnings=%v", warns)
	}
	byName := map[string]Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if len(got) != 2 {
		t.Fatalf("skills=%d", len(got))
	}
	if byName["alpha"].Source != Authored || byName["alpha"].Description != "authored alpha" {
		t.Fatalf("alpha: %+v", byName["alpha"])
	}
	if byName["beta"].Source != Learned {
		t.Fatalf("beta: %+v", byName["beta"])
	}
}
