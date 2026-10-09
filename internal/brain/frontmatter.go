package brain

import (
	"errors"
	"strings"
	"unicode"

	"github.com/goccy/go-yaml"

	"github.com/metafates/zemi/internal/glob"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/skill"
)

// FieldCap caps one injected frontmatter field, in runes (DESIGN §10.7): intake
// applies it before a write, and the render truncates a hand-edited longer
// field to the same cap, with a lint finding naming the file (DESIGN §10.7).
const FieldCap = 200

// Frontmatter is a brain file's three fields: description and read_if are
// required non-empty free text, paths an optional glob or list of globs
// arming path-triggered recall (DESIGN §10.7).
type Frontmatter struct {
	Description string
	ReadIf      string
	Paths       []string
}

// ParseFrontmatter reads a brain file's frontmatter block through
// skill.SplitFrontmatter and the yaml zemi already carries. Fields come
// back as written; Normalize makes them injectable. No block, or empty
// fields, is a zero field - lint names it (DESIGN §10.7), the render still shows the
// file. A yaml error returns what could not be parsed and no fields.
func ParseFrontmatter(content string) (fm Frontmatter, err error) {
	block, _, err := skill.SplitFrontmatter(content)
	if err != nil {
		return fm, err
	}
	if block == "" {
		return fm, nil
	}
	var raw struct {
		Description string   `yaml:"description"`
		ReadIf      string   `yaml:"read_if"`
		Paths       pathList `yaml:"paths"`
	}
	if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
		return fm, err
	}
	return Frontmatter{Description: raw.Description, ReadIf: raw.ReadIf, Paths: raw.Paths}, nil
}

// Matchers compiles Paths into rg-style path matchers, one per glob, for the
// code that arms them: a glob that will not compile fails the whole file,
// which is the check lint and intake run too (DESIGN §10.7).
func (fm Frontmatter) Matchers() ([]func(rel string) bool, error) {
	out := make([]func(rel string) bool, 0, len(fm.Paths))
	for _, p := range fm.Paths {
		match, err := glob.Match(p)
		if err != nil {
			return nil, err
		}
		out = append(out, match)
	}
	return out, nil
}

// BadGlob names the first malformed glob in Paths, for lint and intake: a
// file with one stays visible in the catalog but triggers nothing (DESIGN §10.7).
func (fm Frontmatter) BadGlob() error {
	_, err := fm.Matchers()
	return err
}

// Oversized reports a frontmatter field whose stripped length is over
// FieldCap: intake queues the write instead of truncating (DESIGN §10.4).
func Oversized(s string) bool { return len([]rune(strip(s))) > FieldCap }

// Normalize makes one field safe to inject: control characters and
// newlines stripped, capped at FieldCap runes. Intake applies it and the
// render applies it again (DESIGN §10.4).
func Normalize(s string) string {
	return learn.Truncate(strip(s), FieldCap)
}

// errLine is an error's first line, stripped: a yaml error's caret
// diagram is noise in a one-line finding or note.
func errLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return strip(s)
}

// strip removes control characters, newlines included, so text renders as
// one line (the topLevel filter's precedent).
func strip(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// pathList is paths: one glob or a list of them (DESIGN §7).
type pathList []string

func (p *pathList) UnmarshalYAML(unmarshal func(any) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		*p = list
		return nil
	}
	var one string
	if err := unmarshal(&one); err == nil {
		*p = pathList{one}
		return nil
	}
	return errors.New("paths: a glob or a list of globs")
}
