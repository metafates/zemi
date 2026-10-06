package brain

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/goccy/go-yaml"

	"github.com/metafates/zemi/internal/skill"
)

// FieldCap caps one injected frontmatter field, in runes (§3): intake
// applies it before a write, and the render truncates a hand-edited longer
// field to the same cap, with a lint finding naming the file (D6).
const FieldCap = 200

// Frontmatter is a brain file's three fields: description and read_if are
// required non-empty free text, paths an optional glob or list of globs
// arming path-triggered recall (D10).
type Frontmatter struct {
	Description string
	ReadIf      string
	Paths       []string
}

// ParseFrontmatter reads a brain file's frontmatter block through
// skill.SplitFrontmatter and the yaml zemi already carries. Fields come
// back as written; Normalize makes them injectable. No block, or empty
// fields, is a zero field - lint names it (D6), the render still shows the
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

// BadGlob names the first malformed glob in Paths, for lint and intake: a
// file with one stays visible in the catalog but triggers nothing (D6).
func (fm Frontmatter) BadGlob() error {
	for _, glob := range fm.Paths {
		if _, err := filepath.Match(glob, ""); err != nil {
			return fmt.Errorf("bad paths glob %q: %w", glob, err)
		}
	}
	return nil
}

// Oversized reports a frontmatter field whose stripped length is over
// FieldCap: intake queues the write instead of truncating (D5).
func Oversized(s string) bool { return len([]rune(strip(s))) > FieldCap }

// Normalize makes one field safe to inject: control characters and
// newlines stripped, capped at FieldCap runes. Intake applies it and the
// render applies it again (D5).
func Normalize(s string) string {
	s = strip(s)
	if r := []rune(s); len(r) > FieldCap {
		return string(r[:FieldCap])
	}
	return s
}

// errLine is an error's first line, stripped: a yaml error's caret
// diagram is noise in an injected warning or note.
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

// pathList is paths: one glob or a list of them (§3).
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
