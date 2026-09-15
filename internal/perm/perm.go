// Package perm is the permission engine (DESIGN §9): rules
// (subject, pattern, decision) evaluated per operation, with a bash
// command normalizer that splits compound payloads and gates environment
// assignments. Decisions: deny beats ask beats allow; within one decision
// the most specific pattern wins.
package perm

import (
	"sort"
	"strings"
)

// Decision is what a rule instructs the caller to do.
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
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
}

// Store holds rules in evaluation order; lookups re-sort by specificity.
// Session rules added at runtime ("always" answers) live in a Store owned
// by the host; config-file rules are loaded into it at boot.
type Store struct {
	rules []Rule
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{} }

// Add appends a rule (least specific first is fine; evaluation re-sorts).
func (s *Store) Add(r Rule) { s.rules = append(s.rules, r) }

// Rules returns a copy of all rules.
func (s *Store) Rules() []Rule {
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	return out
}

// globMatch matches a whole-string glob: `*` spans any characters
// (including separators), `?` matches one. Empty pattern matches only
// the empty string.
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	return globRegexp(pattern).MatchString(s)
}

// Outcome is the aggregated verdict for one operation.
type Outcome struct {
	Decision Decision
	// Rule is the winning rule; nil means the default applied.
	Rule   *Rule
	Reason string
}

// Evaluate decides one operation argument against one subject's rules.
// Ordering: deny > ask > allow; within a decision, most specific pattern
// (fewest wildcards, then longest literal prefix) wins. An argument that
// matches a rule's pattern but also its NotMatch is skipped (and the
// fall-through is reported for the audit log).
func (s *Store) Evaluate(subject, arg string, principal string) Outcome {
	type cand struct {
		rule  Rule
		score int
	}
	var (
		byDecision = map[Decision][]cand{}
		fellThru   []string
	)
	for _, r := range s.rules {
		if r.Subject != subject {
			continue
		}
		if r.Principal != "" && principal != "" && r.Principal != principal {
			continue
		}
		if !globMatch(r.Pattern, arg) {
			continue
		}
		if r.NotMatch != "" && globMatch(r.NotMatch, arg) {
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
		sort.Slice(list, func(i, j int) bool {
			if list[i].score != list[j].score {
				return list[i].score < list[j].score // fewer wildcards first
			}
			return literalPrefix(list[i].rule.Pattern) > literalPrefix(list[j].rule.Pattern)
		})
		r := list[0].rule
		return Outcome{Decision: d, Rule: &r}
	}
	if len(fellThru) > 0 {
		return Outcome{Decision: Ask, Reason: "excluded by not_match: " + strings.Join(fellThru, ", ")}
	}
	return Outcome{Decision: Ask, Reason: "no rule matched"}
}
