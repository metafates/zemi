// Package glob matches paths against rg-style globs: ** crosses directories,
// {a,b} alternates, [...] is a class and a pattern without / matches base
// names. The file tools and the LSP client's watched-file filters share it.
package glob

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Match compiles pattern into a matcher of relative paths.
func Match(pattern string) (func(rel string) bool, error) {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '{':
			depth++
			b.WriteString("(?:")
		case c == '}' && depth > 0:
			depth--
			b.WriteString(")")
		case c == ',' && depth > 0:
			b.WriteString("|")
		case c == '[' && strings.IndexByte(pattern[i:], ']') > 1:
			j := i + strings.IndexByte(pattern[i:], ']')
			class := pattern[i+1 : j]
			if rest, ok := strings.CutPrefix(class, "!"); ok {
				class = "^" + rest
			}
			b.WriteString("[" + class + "]")
			i = j
		case c == '[':
			// rg refuses this too, and a matcher for a literal "[" would
			// read as "no files match" instead of "your pattern is wrong"
			return nil, fmt.Errorf("bad glob %q: [ without a closing ]", pattern)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re, err := regexp.Compile("^" + b.String() + "$")
	if err != nil {
		return nil, fmt.Errorf("bad glob %q", pattern)
	}
	base := !strings.Contains(pattern, "/")
	return func(rel string) bool {
		rel = filepath.ToSlash(rel)
		if base {
			rel = filepath.Base(rel)
		}
		return re.MatchString(rel)
	}, nil
}
