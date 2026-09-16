package perm

import (
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// globCache is shared by every goroutine that evaluates rules (turns,
// subagents, Lua gates), so it is locked.
var (
	globMu    sync.Mutex
	globCache = map[string]*regexp.Regexp{}
)

// globRegexp compiles a glob (`*`, `?`) into an anchored regexp. Patterns
// are bounded in number (rules), so the cache cannot grow unboundedly.
func globRegexp(pattern string) *regexp.Regexp {
	globMu.Lock()
	re, ok := globCache[pattern]
	globMu.Unlock()
	if ok {
		return re
	}
	var b strings.Builder
	// (?s): a quoted newline in an argument must not slip past a deny rule
	// (Evaluate keeps allow rules to one line)
	b.WriteString(`(?s)^`)
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
	re = regexp.MustCompile(b.String())
	globMu.Lock()
	if len(globCache) < 1024 {
		globCache[pattern] = re
	}
	globMu.Unlock()
	return re
}

// specificity scores a pattern: fewer wildcards is more specific (lower
// score wins); ties break on the longer literal prefix.
func specificity(pattern string) int {
	return strings.Count(pattern, "*") + strings.Count(pattern, "?")
}

// literalPrefix counts the pattern's literal characters before the first
// wildcard (for tie-breaking).
func literalPrefix(pattern string) int {
	if i := strings.IndexAny(pattern, "*?"); i >= 0 {
		pattern = pattern[:i]
	}
	return utf8.RuneCountInString(pattern)
}
