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

	"github.com/metafates/zemi/internal/learn"
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
	// Prefix joins every discovered name, Root and Data ride the parsed
	// skills' substitutions: a Claude-layout plugin's, "" for the rest.
	Prefix     string
	Root, Data string
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
	// File is the body file when it is not Dir/SKILL.md: a Claude plugin's
	// command, which loads as a model-invocable skill row. Root and Data are
	// what ${CLAUDE_PLUGIN_ROOT} and ${CLAUDE_PLUGIN_DATA} stand for in the
	// body, set for a Claude-layout plugin's entries, "" for everything else.
	File       string
	Root, Data string
	// UserInvocable is the frontmatter's user-invocable key, kept as parsed
	// metadata: a skill is invoked with +name, never a command of its own.
	UserInvocable bool
}

// maxNameLen caps a name: a skill's directory, a workflow file, a
// plugin-provided layer file.
const maxNameLen = 64

// NameRule is what ValidName accepts, in the words every failure naming the
// rule uses, the cap included: a name a save takes is one a loader keeps.
var NameRule = fmt.Sprintf("a lowercase letter or digit, then up to %d lowercase letters, digits or dashes", maxNameLen-1)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidName reports whether name is usable as a skill directory name.
func ValidName(name string) bool { return len(name) <= maxNameLen && nameRe.MatchString(name) }

// Parse validates one skill directory: SKILL.md must exist (not a
// symlink), carry `---`-fenced yaml frontmatter whose name, when present,
// matches the directory, with a description that falls back to the body's
// first line. The description is cut to the cap with a warning rather than
// refused. An optional zemi-skill.lua enable hook is reported.
func Parse(dir string, source Source, prefix string) (Skill, error) {
	base := filepath.Base(dir)
	sk := Skill{Name: prefix + base, Dir: dir, Source: source}
	if !ValidName(base) {
		return sk, fmt.Errorf("skills: %q: directory name must be %s", base, NameRule)
	}
	if !ValidName(sk.Name) {
		return sk, fmt.Errorf("skills: %s: name must be %s", sk.Name, NameRule)
	}
	data, err := readSkillFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return sk, fmt.Errorf("skills: %s: %w", base, err)
	}
	fm, err := frontmatter(data)
	if err != nil {
		return sk, fmt.Errorf("skills: %s: %w", base, err)
	}
	if fm.Name != "" && fm.Name != base {
		return sk, fmt.Errorf("skills: %s: frontmatter name %q must match the directory", base, fm.Name)
	}
	if source != Recipe && fm.UserInvocable != nil { // a recipe is the model's own procedure
		sk.UserInvocable = *fm.UserInvocable
	}
	desc := fm.Description
	if strings.TrimSpace(desc) == "" { // Claude's fallback: the body speaks first
		_, body, _ := SplitFrontmatter(string(data))
		desc = FirstBodyLine(body)
	}
	var cut error
	if short := learn.Truncate(desc, MaxDescriptionRunes); short != desc {
		desc = short
		cut = fmt.Errorf("skills: %s: %w to %d runes", base, ErrDescriptionCut, MaxDescriptionRunes)
	}
	sk.Description = desc
	if hook := filepath.Join(dir, "zemi-skill.lua"); fileExists(hook) {
		sk.Hook = hook
	}
	return sk, cut
}

// FirstBodyLine is the body's first non-empty line, Claude's description
// fallback.
func FirstBodyLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
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
	Name          string `yaml:"name"`
	Description   string `yaml:"description"`
	UserInvocable *bool  `yaml:"user-invocable"`
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
	return fm, nil
}

// readSkillFile reads a skill's SKILL.md, which must be a regular file: a
// symlink reads outside the trees skills come from, and reading a FIFO blocks
// the boot step or the turn that got here. Its size is bounded here, before
// the read.
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
	if fi.Size() > MaxFileBytes {
		return nil, fmt.Errorf("SKILL.md exceeds %d bytes", MaxFileBytes)
	}
	return os.ReadFile(path)
}

// Discover merges skill directories in the order given, higher trust first:
// on a name clash the earlier one wins (§10.5), with a warning only when two
// plugins ship one name, since any other clash is an override by design.
// A skill that does not parse is skipped with a warning, never fatal: a
// broken skill must not hide the others. A missing or unreadable directory
// is skipped.
func Discover(dirs ...Dir) (skills []Skill, warnings []error) {
	seen := map[string]Skill{}
	for _, d := range dirs {
		entries, err := os.ReadDir(d.Path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type()&fs.ModeSymlink != 0 {
				warnings = append(warnings, fmt.Errorf("skills: %s: skill dir is a symlink, skipped", filepath.Join(d.Path, e.Name())))
				continue
			}
			if !e.IsDir() {
				continue
			}
			name := d.Prefix + e.Name()
			if kept, ok := seen[name]; ok {
				if kept.Source == Plugin && d.Source == Plugin {
					warnings = append(warnings, fmt.Errorf("skills: %s: skill %s is already in %s", filepath.Join(d.Path, e.Name()), name, kept.Dir))
				}
				continue
			}
			sk, err := Parse(filepath.Join(d.Path, e.Name()), d.Source, d.Prefix)
			if err != nil {
				warnings = append(warnings, err)
				if !errors.Is(err, ErrDescriptionCut) {
					// a broken skill still holds its name: taking it away
					// would let a lower-trust same-named one in unnoticed
					seen[sk.Name] = sk
					continue
				}
			}
			sk.Root, sk.Data = d.Root, d.Data
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

// Body reads a skill's body without its frontmatter: File when set, else
// Dir/SKILL.md. The index already showed name and description; the body is
// what the model follows.
func (s Skill) Body() (string, error) {
	path := filepath.Join(s.Dir, "SKILL.md")
	if s.File != "" {
		path = s.File
	}
	return BodyFile(path)
}

// BodyFile reads the body of the skill file at path, frontmatter split off.
func BodyFile(path string) (string, error) {
	data, err := readSkillFile(path)
	if err != nil {
		return "", err
	}
	_, body, err := SplitFrontmatter(string(data))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(body), nil
}
