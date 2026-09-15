// Package skills implements M3 skill discovery and validation
// (DESIGN §10.5): SKILL.md files with yaml frontmatter (name,
// description), authored under the config repo's skills/ and learned
// under the data dir's skills/. Learned skills are quarantined by the
// enabled-state store; discovery only reports what is on disk.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// Source distinguishes authored skills (user's config repo, enabled by
// default) from learned ones (data dir, start disabled, §10.5).
type Source string

const (
	Authored Source = "authored"
	Learned  Source = "learned"
)

// Caps: SKILL.md is bounded so a huge file cannot stall boot or the
// skill tool; the description is what the model sees in the index.
const (
	MaxDescriptionRunes = 512
	MaxFileBytes        = 1 << 20
)

// Skill is one validated skill directory.
type Skill struct {
	Name        string
	Dir         string
	Source      Source
	Description string
	Hook        string // path to zemi-skill.lua, "" when absent
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidName reports whether name is usable as a skill directory name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Parse validates one skill directory: SKILL.md must exist (not a
// symlink), carry `---`-fenced yaml frontmatter with a name matching the
// directory and a non-empty description within the cap. An optional
// zemi-skill.lua enable hook is reported.
func Parse(dir string, source Source) (Skill, error) {
	sk := Skill{Name: filepath.Base(dir), Dir: dir, Source: source}
	if !nameRe.MatchString(sk.Name) {
		return sk, fmt.Errorf("skills: %q: directory name must match [a-z0-9][a-z0-9-]{0,63}", sk.Name)
	}
	path := filepath.Join(dir, "SKILL.md")
	fi, err := os.Lstat(path)
	if err != nil {
		return sk, fmt.Errorf("skills: %s: no SKILL.md", sk.Name)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return sk, fmt.Errorf("skills: %s: SKILL.md must not be a symlink", sk.Name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sk, err
	}
	if len(data) > MaxFileBytes {
		return sk, fmt.Errorf("skills: %s: SKILL.md exceeds %d bytes", sk.Name, MaxFileBytes)
	}
	name, desc, err := frontmatter(data)
	if err != nil {
		return sk, fmt.Errorf("skills: %s: %w", sk.Name, err)
	}
	if name != sk.Name {
		return sk, fmt.Errorf("skills: %s: frontmatter name %q must match the directory", sk.Name, name)
	}
	if runes := len([]rune(desc)); runes > MaxDescriptionRunes {
		return sk, fmt.Errorf("skills: %s: description exceeds %d runes", sk.Name, MaxDescriptionRunes)
	}
	sk.Description = desc
	if hook := filepath.Join(dir, "zemi-skill.lua"); fileExists(hook) {
		sk.Hook = hook
	}
	return sk, nil
}

// frontmatter splits the leading `---` block and decodes name and
// description. The body is the remainder and is not validated here.
func frontmatter(data []byte) (name, description string, err error) {
	s := string(data)
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return "", "", fmt.Errorf("SKILL.md must start with a --- frontmatter block")
	}
	block, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", "", fmt.Errorf("unterminated frontmatter block")
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return "", "", fmt.Errorf("frontmatter: %w", err)
	}
	if fm.Name == "" {
		return "", "", fmt.Errorf("frontmatter: name is required")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return "", "", fmt.Errorf("frontmatter: description is required")
	}
	return fm.Name, fm.Description, nil
}

// Discover merges authored and learned skill directories; on a name
// clash the higher-trust source wins (authored over learned, §10.5).
// Unreadable or invalid directories are skipped with a returned warning
// per failure, never fatal: a broken skill must not hide the others.
func Discover(authoredDir, learnedDir string) (skills []Skill, warnings []string) {
	seen := map[string]bool{}
	scan := func(dir string, source Source) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			sk, err := Parse(filepath.Join(dir, e.Name()), source)
			if err != nil {
				warnings = append(warnings, err.Error())
				continue
			}
			seen[sk.Name] = true
			skills = append(skills, sk)
		}
	}
	scan(authoredDir, Authored)
	scan(learnedDir, Learned)
	return skills, warnings
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
