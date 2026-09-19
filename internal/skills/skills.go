// Package skills implements skill discovery and validation
// (DESIGN §10.5): SKILL.md files with yaml frontmatter (name,
// description), authored under the config dir's skills/, learned under the
// data dir's skills/ and, in a trusted project, its own skills dirs. Learned skills are quarantined by the
// enabled-state store; discovery only reports what is on disk.
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// Source distinguishes authored skills (user's config dir, enabled by
// default) from learned ones (data dir, start disabled, §10.5).
type Source string

const (
	Authored Source = "authored"
	Builtin  Source = "builtin" // shipped with zemi, enabled by default
	Learned  Source = "learned"
	Project  Source = "project" // a trusted project's, body only, never stored
	Recipe   Source = "recipe"  // a trusted project's, kept in zemi's state dir, body only, never stored
)

// Stored reports a source with a row in the skills table, so an enabled flag.
func (s Source) Stored() bool { return s != Project && s != Recipe }

// Dir is a directory of skill directories from one source.
type Dir struct {
	Path   string
	Source Source
}

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
// directory and a non-empty description, which is cut to the cap with a
// warning rather than refused. An optional zemi-skill.lua enable hook is
// reported.
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
	var cut error
	if runes := []rune(desc); len(runes) > MaxDescriptionRunes {
		desc = string(runes[:MaxDescriptionRunes])
		cut = fmt.Errorf("skills: %s: %w to %d runes", sk.Name, ErrDescriptionCut, MaxDescriptionRunes)
	}
	sk.Description = desc
	if hook := filepath.Join(dir, "zemi-skill.lua"); fileExists(hook) {
		sk.Hook = hook
	}
	return sk, cut
}

// ErrDescriptionCut rides along with a usable skill whose description was
// over the cap: published skills are written for other agents' longer
// limits, and dropping the body over a long trigger line helps nobody.
var ErrDescriptionCut = errors.New("description cut")

// SplitFrontmatter separates a leading `---` yaml block from the body. No
// block means the whole text is the body.
func SplitFrontmatter(s string) (block, body string, err error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return "", s, nil
	}
	block, body, ok = strings.Cut(rest, "\n---")
	if !ok {
		return "", "", errors.New("unterminated frontmatter block")
	}
	_, body, _ = strings.Cut(body, "\n") // the rest of the closing line
	return block, body, nil
}

// frontmatter decodes the name and description of a SKILL.md.
func frontmatter(data []byte) (name, description string, err error) {
	block, _, err := SplitFrontmatter(string(data))
	if err != nil {
		return "", "", err
	}
	if block == "" {
		return "", "", errors.New("SKILL.md must start with a --- frontmatter block")
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return "", "", fmt.Errorf("frontmatter: %w", err)
	}
	if fm.Name == "" {
		return "", "", errors.New("frontmatter: name is required")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return "", "", errors.New("frontmatter: description is required")
	}
	return fm.Name, fm.Description, nil
}

// Discover merges skill directories in the order given, higher trust
// first: on a name clash the earlier one wins (authored over builtin over
// learned, §10.5). Unreadable or invalid directories are skipped with a
// returned warning per failure, never fatal: a broken skill must not hide
// the others.
func Discover(dirs ...Dir) (skills []Skill, warnings []string) {
	seen := map[string]bool{}
	for _, d := range dirs {
		entries, err := os.ReadDir(d.Path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			sk, err := Parse(filepath.Join(d.Path, e.Name()), d.Source)
			if err != nil {
				warnings = append(warnings, err.Error())
				if !errors.Is(err, ErrDescriptionCut) {
					continue
				}
			}
			seen[sk.Name] = true
			skills = append(skills, sk)
		}
	}
	return skills, warnings
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// Body reads a skill's SKILL.md without its frontmatter: the index already
// showed name and description, the body is what the model follows.
func Body(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", err
	}
	_, body, err := SplitFrontmatter(string(data))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(body), nil
}
