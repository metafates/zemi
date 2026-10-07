package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	sk, err := Parse(dir, User)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sk.Name != "local-integration" || sk.Description != "run the local integration suite" {
		t.Fatalf("skill: %+v", sk)
	}
	if sk.Source != User {
		t.Fatalf("source: %s", sk.Source)
	}
	if sk.Hook == "" {
		t.Fatal("hook missing")
	}

	crlf := filepath.Join(t.TempDir(), "crlf")
	writeFile(t, filepath.Join(crlf, "SKILL.md"), "---\r\nname: crlf\r\ndescription: saved on windows\r\n---\r\n\r\nBody.\r\n")
	if sk, err := Parse(crlf, User); err != nil || sk.Description != "saved on windows" {
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
			if _, err := Parse(dir, User); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%v, want %q", err, c.want)
			}
		})
	}
}

func TestParseInvocation(t *testing.T) {
	cases := []struct {
		name, extra  string
		source       Source
		user, hidden bool
		hint         string
		wantErr      bool
	}{
		{name: "plain", source: User},
		{name: "user-invocable", extra: "user-invocable: true", source: User, user: true},
		{name: "hidden from the model is a command", extra: "disable-model-invocation: true", source: User, user: true, hidden: true},
		{name: "hidden and no command", extra: "disable-model-invocation: true\nuser-invocable: false", source: User, hidden: true},
		{name: "hint", extra: "argument-hint: <pr>", source: Plugin, hint: "<pr>"},
		{name: "hint as a flow list", extra: "argument-hint: [pr-number]", source: User, hint: "[pr-number]"},
		{name: "a recipe takes neither", extra: "user-invocable: true\ndisable-model-invocation: true\nargument-hint: <x>", source: Recipe},
		{name: "quoted bool", extra: `user-invocable: "true"`, source: User, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "x")
			writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: x\ndescription: d\n"+c.extra+"\n---\nBody $ARGUMENTS.\n")
			sk, err := Parse(dir, c.source)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v", err)
			}
			if err == nil && (sk.UserInvocable != c.user || sk.DisableModelInvocation != c.hidden || sk.ArgumentHint != c.hint) {
				t.Fatalf("got %+v", sk)
			}
		})
	}
}

// A description over the cap is cut, not a reason to drop the skill:
// published skills are written for longer limits than zemi's index.
func TestParseDescriptionCap(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "x")
	long := strings.Repeat("a", MaxDescriptionRunes+1)
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: x\ndescription: "+long+"\n---\n\nBody.\n")
	sk, err := Parse(dir, User)
	if !errors.Is(err, ErrDescriptionCut) {
		t.Fatalf("err=%v", err)
	}
	if len([]rune(sk.Description)) != MaxDescriptionRunes {
		t.Fatalf("description kept %d runes", len([]rune(sk.Description)))
	}
	found, warnings := Discover(Dir{Path: base, Source: User})
	if len(found) != 1 || len(warnings) != 1 {
		t.Fatalf("discover: %+v %q", found, warnings)
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
	if _, err := Parse(dir, User); err == nil || !strings.Contains(err.Error(), "symlink") {
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
		{"fifo", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("mkfifo unavailable: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "x")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			c.make(t, filepath.Join(dir, "SKILL.md"))
			if _, err := Parse(dir, User); err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("parse err=%v", err)
			}
			if _, err := Body(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("body err=%v", err)
			}
		})
	}
}

// A skill that fails to parse still holds its name: a lower-trust directory's
// same-named skill must not slip in behind the warning about the broken one.
func TestDiscoverBrokenSkillHoldsItsName(t *testing.T) {
	root := t.TempDir()
	user, project := filepath.Join(root, "user"), filepath.Join(root, "project")
	writeFile(t, filepath.Join(user, "x", "SKILL.md"), "not frontmatter")
	writeFile(t, filepath.Join(project, "x", "SKILL.md"), "---\nname: x\ndescription: project x\n---\n")

	got, warns := Discover(Dir{user, User}, Dir{project, Project})
	if len(got) != 0 {
		t.Fatalf("a lower-trust skill took the name: %+v", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "frontmatter block") {
		t.Fatalf("warnings=%v", warns)
	}
}

func TestDiscoverMergesUserOverBuiltin(t *testing.T) {
	root := t.TempDir()
	user, builtin := filepath.Join(root, "user"), filepath.Join(root, "builtin")
	writeFile(t, filepath.Join(user, "alpha", "SKILL.md"), "---\nname: alpha\ndescription: user alpha\n---\n")
	writeFile(t, filepath.Join(user, "broken", "SKILL.md"), "not frontmatter")
	writeFile(t, filepath.Join(builtin, "alpha", "SKILL.md"), "---\nname: alpha\ndescription: builtin alpha\n---\n")
	writeFile(t, filepath.Join(builtin, "beta", "SKILL.md"), "---\nname: beta\ndescription: builtin beta\n---\n")

	got, warns := Discover(Dir{user, User}, Dir{builtin, Builtin})
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
	if byName["alpha"].Source != User || byName["alpha"].Description != "user alpha" {
		t.Fatalf("alpha: %+v", byName["alpha"])
	}
	if byName["beta"].Source != Builtin {
		t.Fatalf("beta: %+v", byName["beta"])
	}
}
