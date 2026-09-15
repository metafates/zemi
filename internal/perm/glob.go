package perm

import (
	"regexp"
	"strings"
)

var globCache = map[string]*regexp.Regexp{}

// globRegexp compiles a glob (`*`, `?`) into an anchored regexp. Patterns
// are bounded in number (rules), so the cache cannot grow unboundedly.
func globRegexp(pattern string) *regexp.Regexp {
	if re, ok := globCache[pattern]; ok {
		return re
	}
	var b strings.Builder
	b.WriteString(`^`)
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`$`)
	re := regexp.MustCompile(b.String())
	if len(globCache) < 1024 {
		globCache[pattern] = re
	}
	return re
}

// specificity scores a pattern: fewer wildcards is more specific (lower
// score wins); ties break on the longer literal prefix.
func specificity(pattern string) int {
	score := strings.Count(pattern, "*") + strings.Count(pattern, "?")
	if score == 0 {
		return 0
	}
	return score
}

// literalPrefix returns the pattern's run of literal characters before the
// first wildcard (for tie-breaking).
func literalPrefix(pattern string) int {
	if i := strings.IndexAny(pattern, "*?"); i >= 0 {
		return i
	}
	return len(pattern)
}
