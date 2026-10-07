// Package skill discovers and validates skills (DESIGN §10.5): SKILL.md
// files with yaml frontmatter (name, description, the invocation fields)
// under the config dir's skills/, a plugin's, or a trusted project's.
// Discovery only reports what is on disk.
package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// Source is where a skill was found (§10.5).
type Source string

const (
	User    Source = "user"    // the user's config dir
	Builtin Source = "builtin" // shipped with zemi
	Plugin  Source = "plugin"  // an enabled plugin's, body only
	Project Source = "project" // a trusted project's, body only, never stored
	Recipe  Source = "recipe"  // a trusted project's, kept in zemi's state dir, body only, never stored
)

var Sources = []Source{User, Builtin, Plugin, Project, Recipe}

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
	// UserInvocable: the skill is also /<name>. A skill the model may not
	// load is the user's, so it is one unless its frontmatter says otherwise.
	UserInvocable          bool
	DisableModelInvocation bool
	ArgumentHint           string
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
	data, err := readSkillFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return sk, fmt.Errorf("skills: %s: %w", sk.Name, err)
	}
	if len(data) > MaxFileBytes {
		return sk, fmt.Errorf("skills: %s: SKILL.md exceeds %d bytes", sk.Name, MaxFileBytes)
	}
	fm, err := frontmatter(data)
	if err != nil {
		return sk, fmt.Errorf("skills: %s: %w", sk.Name, err)
	}
	if fm.Name != sk.Name {
		return sk, fmt.Errorf("skills: %s: frontmatter name %q must match the directory", sk.Name, fm.Name)
	}
	if source != Recipe { // a recipe is the model's own procedure
		sk.DisableModelInvocation, sk.ArgumentHint = fm.DisableModelInvocation, string(fm.ArgumentHint)
		sk.UserInvocable = fm.DisableModelInvocation
		if fm.UserInvocable != nil {
			sk.UserInvocable = *fm.UserInvocable
		}
	}
	desc := fm.Description
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

type front struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	UserInvocable          *bool  `yaml:"user-invocable"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
	ArgumentHint           Hint   `yaml:"argument-hint"`
}

// Hint is argument-hint, which Claude Code's docs write as a flow list
// (`[pr-number]`): any shape is its text, so a cosmetic key never drops a file.
type Hint string

func (h *Hint) UnmarshalYAML(unmarshal func(any) error) error {
	var v any
	if err := unmarshal(&v); err != nil {
		return err
	}
	if v != nil {
		*h = Hint(fmt.Sprint(v))
	}
	return nil
}

func frontmatter(data []byte) (front, error) {
	var fm front
	block, _, err := SplitFrontmatter(string(data))
	if err != nil {
		return fm, err
	}
	if block == "" {
		return fm, errors.New("SKILL.md must start with a --- frontmatter block")
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return fm, fmt.Errorf("frontmatter: %w", err)
	}
	if fm.Name == "" {
		return fm, errors.New("frontmatter: name is required")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return fm, errors.New("frontmatter: description is required")
	}
	return fm, nil
}

// readSkillFile reads a skill's SKILL.md, which must be a regular file: a
// symlink reads outside the trees skills come from, and reading a FIFO blocks
// the boot step or the turn that got here.
func readSkillFile(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("no SKILL.md")
		}
		return nil, fmt.Errorf("SKILL.md: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("SKILL.md must not be a symlink")
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("SKILL.md is not a regular file")
	}
	return os.ReadFile(path)
}

// Discover merges skill directories in the order given, higher trust first:
// on a name clash the earlier one wins (§10.5), with a warning only when two
// plugins ship one name, since any other clash is an override by design.
// A skill that does not parse is skipped with a warning, never fatal: a
// broken skill must not hide the others. A missing or unreadable directory
// is skipped.
func Discover(dirs ...Dir) (skills []Skill, warnings []string) {
	seen := map[string]Skill{}
	for _, d := range dirs {
		entries, err := os.ReadDir(d.Path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if kept, ok := seen[e.Name()]; ok {
				if kept.Source == Plugin && d.Source == Plugin {
					warnings = append(warnings, fmt.Sprintf("skills: %s: skill %s is already in %s", filepath.Join(d.Path, e.Name()), e.Name(), kept.Dir))
				}
				continue
			}
			sk, err := Parse(filepath.Join(d.Path, e.Name()), d.Source)
			if err != nil {
				warnings = append(warnings, err.Error())
				if !errors.Is(err, ErrDescriptionCut) {
					// a broken skill still holds its name: taking it away
					// would let a lower-trust same-named one in unnoticed
					seen[sk.Name] = sk
					continue
				}
			}
			seen[sk.Name] = sk
			skills = append(skills, sk)
		}
	}
	return skills, warnings
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// Body reads a skill's SKILL.md without its frontmatter: the index already
// showed name and description, the body is what the model follows.
func Body(dir string) (string, error) {
	data, err := readSkillFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", err
	}
	_, body, err := SplitFrontmatter(string(data))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(body), nil
}
