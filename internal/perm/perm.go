// Package perm is the permission engine (DESIGN §9): rules
// (subject, pattern, decision) evaluated per operation, with a bash
// command normalizer that splits compound payloads and gates environment
// assignments. Decisions: deny beats ask beats allow; within one decision
// the most specific pattern wins.
package perm

import (
	"cmp"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
)

// Decision is what a rule instructs the caller to do.
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

// Scope is a rule's lifetime (DESIGN §6.1): session rules live in memory
// until the session ends; permanent rules survive restarts and are
// serialized into permissions.lua (Go-generated data, the store's dump).
type Scope string

const (
	ScopeSession   Scope = "session"
	ScopePermanent Scope = "permanent"
)

// Rule is one permission rule. Pattern is a glob over the operation
// argument (path, url, normalized command segment). NotMatch is an
// optional runtime exclusion: an argument matching Pattern but also
// NotMatch falls through to the next rule.
type Rule struct {
	Subject   string   `json:"subject"`   // tool.bash, fs.read, net.host, lua.fs.read
	Pattern   string   `json:"pattern"`   // glob
	Decision  Decision `json:"decision"`  // allow | ask | deny
	NotMatch  string   `json:"not_match"` // optional exclusion glob
	Principal string   `json:"principal"` // empty = any principal
	Scope     Scope    `json:"scope"`     // empty = session
}

// Store holds rules in evaluation order; lookups re-sort by specificity.
// Session rules added at runtime ("always" answers) live in a Store owned
// by the host; config-file rules are loaded into it at boot.
type Store struct {
	rules []Rule

	// mu guards rules: rules are added from the Lua actor goroutine, the
	// permission-ask hook and the review overlay goroutine, and read
	// concurrently by agent turns.
	mu sync.Mutex

	// Default applies when no rule matched a subject+arg (DESIGN §9
	// built-in defaults: fs.read inside the jail allows except secret
	// paths). It never overrides a matched rule, and an explicit
	// not_match exclusion still asks.
	Default func(subject, arg string) Decision
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{} }

// Add appends a rule unless an equal one is stored (config replays on every
// boot and reload) and reports whether it did; evaluation re-sorts.
func (s *Store) Add(r Rule) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.rules, r) {
		return false
	}
	s.rules = append(s.rules, r)
	return true
}

// Remove drops one rule equal to r, if there is one.
func (s *Store) Remove(r Rule) {
	s.mu.Lock()
	if i := slices.Index(s.rules, r); i >= 0 {
		s.rules = slices.Delete(s.rules, i, i+1)
	}
	s.mu.Unlock()
}

// Rules returns a copy of all rules.
func (s *Store) Rules() []Rule {
	s.mu.Lock()
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	s.mu.Unlock()
	return out
}

// globMatch matches a whole-string glob: `*` spans any characters
// (including separators), `?` matches one. Empty pattern matches only
// the empty string.
func globMatch(pattern, s string) bool { return globRegexp(pattern).MatchString(s) }

// MatchExample reports whether the rule pattern would match the example
// string. Used by zemi.permission.rule to validate that a rule does what
// its author claims before it is stored.
func MatchExample(pattern, s string) bool { return globMatch(pattern, s) }

// Outcome is the aggregated verdict for one operation.
type Outcome struct {
	Decision Decision
	// Rule is the winning rule; nil means the default applied.
	Rule   *Rule
	Reason string
}

// subjectMatches compares a rule's subject with the one being asked about.
// A rule may end in ".*" to cover a family: net.* answers net.example.com,
// the subject the fetch tool asks with.
func subjectMatches(rule, subject string) bool {
	if prefix, ok := strings.CutSuffix(rule, ".*"); ok {
		return subject == prefix || strings.HasPrefix(subject, prefix+".")
	}
	return rule == subject
}

// CanonPath is the form fs rules match against and file tools key by:
// absolute, clean, symlinks of the longest existing prefix resolved, and
// case folded where the filesystem folds.
// NOTE: a case-sensitive volume on darwin or windows folds two real files
// into one; a rule for one then covers both.
func CanonPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return foldCase(RealPath(p))
}

// RealPath resolves symlinks in the longest existing prefix of p, so a file
// about to be created is judged by its real parent directory.
func RealPath(p string) string {
	suffix := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, suffix)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, suffix)
		}
		suffix = filepath.Join(filepath.Base(p), suffix)
		p = parent
	}
}

func foldCase(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// canonPattern puts an fs rule pattern in CanonPath form: the literal
// directory before the first wildcard is canonicalized, the rest only case
// folded. A pattern that starts with a wildcard ("*/.env") stays as is.
func canonPattern(pat string) string {
	i := strings.IndexAny(pat, "*?")
	if i < 0 {
		return CanonPath(pat)
	}
	sep := string(filepath.Separator)
	cut := strings.LastIndex(pat[:i], sep)
	switch {
	case i == 0 || cut == 0:
		return foldCase(pat)
	case cut < 0:
		return CanonPath(".") + sep + foldCase(pat)
	}
	return CanonPath(pat[:cut]) + foldCase(pat[cut:])
}

// Evaluate decides one operation argument against one subject's rules.
// Ordering: deny > ask > allow; within a decision, most specific pattern
// (fewest wildcards, then longest literal prefix) wins. An argument that
// matches a rule's pattern but also its NotMatch is skipped, and when no
// other rule matches it forces ask, ahead of Default.
func (s *Store) Evaluate(subject, arg string, principal string) Outcome {
	type cand struct {
		rule  Rule
		score int
	}
	var (
		byDecision = map[Decision][]cand{}
		fellThru   []string
	)
	// DESIGN §9: .. resolves before matching, so dir/* cannot cover dir/../x
	isPath := strings.HasPrefix(subject, "fs.") || strings.HasPrefix(subject, "lua.fs.")
	if isPath && strings.Contains(arg, "..") {
		arg = filepath.Clean(arg)
	}
	match, canon := arg, func(p string) string { return p }
	if isPath {
		// every spelling of one file (relative, ./, //, a symlink, another
		// case) must meet the same rules, so both sides match canonical
		match, canon = CanonPath(arg), canonPattern
	}
	s.mu.Lock()
	rules := make([]Rule, len(s.rules))
	copy(rules, s.rules)
	s.mu.Unlock()
	for _, r := range rules {
		if !subjectMatches(r.Subject, subject) {
			continue
		}
		if r.Principal != "" && principal != "" && r.Principal != principal {
			continue
		}
		if !globMatch(canon(r.Pattern), match) {
			continue
		}
		// globs span newlines so deny and ask rules hold, but an allow for
		// one line must not cover a second line the user never saw
		if r.Decision == Allow && strings.Contains(arg, "\n") && !strings.Contains(r.Pattern, "\n") {
			continue
		}
		if r.NotMatch != "" && globMatch(canon(r.NotMatch), match) {
			fellThru = append(fellThru, r.Pattern)
			continue
		}
		byDecision[r.Decision] = append(byDecision[r.Decision], cand{r, specificity(r.Pattern)})
	}
	for _, d := range []Decision{Deny, Ask, Allow} {
		list := byDecision[d]
		if len(list) == 0 {
			continue
		}
		slices.SortFunc(list, func(a, b cand) int {
			return cmp.Or(
				cmp.Compare(a.score, b.score), // fewer wildcards first
				cmp.Compare(literalPrefix(b.rule.Pattern), literalPrefix(a.rule.Pattern)),
			)
		})
		r := list[0].rule
		return Outcome{Decision: d, Rule: &r}
	}
	if len(fellThru) > 0 {
		return Outcome{Decision: Ask, Reason: "excluded by not_match: " + strings.Join(fellThru, ", ")}
	}
	if s.Default != nil {
		if d := s.Default(subject, arg); d != Ask {
			return Outcome{Decision: d, Reason: "default"}
		}
	}
	return Outcome{Decision: Ask, Reason: "no rule matched"}
}
