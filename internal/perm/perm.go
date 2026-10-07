// Package perm is the permission engine (DESIGN §9): rules
// (subject, pattern, decision) evaluated per operation, with a shell
// command normalizer that splits compound payloads and gates environment
// assignments. Decisions: deny beats ask beats allow; within one decision
// the most specific pattern wins.
package perm

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Decision is what a rule instructs the caller to do.
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

// The subjects zemi's own tools ask about; the set is open (net.<host>,
// lua.<ns.call>, mcp.<server>).
const (
	SubjectShell    = "tool.shell"
	SubjectLua      = "tool.lua"
	SubjectWorkflow = "tool.workflow"
	SubjectRead     = "fs.read"
	SubjectWrite    = "fs.write"
)

// PrincipalModel scopes a rule to the model's calls, like an always answer.
const PrincipalModel = "model"

const pluginPrefix = "plugin:"

// PluginPrincipal is the principal a plugin's calls run as.
func PluginPrincipal(name string) string { return pluginPrefix + name }

// CutPluginPrincipal returns the plugin a principal names.
func CutPluginPrincipal(p string) (string, bool) { return strings.CutPrefix(p, pluginPrefix) }

// SubjectPrefixes are the namespaces a subject lives in. A rule for
// anything else can never fire, so zemi refuses it (DESIGN §9).
var SubjectPrefixes = []string{"tool.", "fs.", "net.", "lua.", "mcp.", "memory."}

// Rule is one permission rule. Pattern is a glob over the operation
// argument (path, url, normalized command segment). NotMatch is an
// optional runtime exclusion: an argument matching Pattern but also
// NotMatch falls through to the next rule.
type Rule struct {
	Subject   string   // tool.shell, fs.read, net.host, lua.fs.read
	Pattern   string   // glob
	Decision  Decision // allow | ask | deny
	NotMatch  string   // optional exclusion glob
	Principal string   // empty = any principal, else only that one
	// Source is where a principal-less rule came from, so /permissions can
	// say what to edit: "" for user config, "permissions" for a project's
	// permissions file, else the Lua origin that added it
	Source string
	// Network marks a session rule written from a network: true ask: a
	// command it allows keeps the sandbox and gets sockets, as the ask said,
	// instead of running unconfined like other rule-allowed commands
	Network bool
}

// Store holds rules in evaluation order; lookups re-sort by specificity.
// Session rules added at runtime ("always" answers) live in a Store owned
// by the host; config-file rules are loaded into it at boot.
type Store struct {
	rules []Rule

	// mu guards rules: rules are added from the Lua actor goroutine and the
	// permission-ask hook, and read concurrently by agent turns.
	mu sync.Mutex

	// Default applies when no rule matched a subject+arg (DESIGN §9
	// built-in defaults: fs.read inside the jail allows except secret
	// paths). It never overrides a matched rule, and an explicit
	// not_match exclusion still asks.
	Default func(subject, arg string) Decision

	// EnvKeep returns the comma-separated names ChildEnv passes through
	// despite their secret shape; the host reads permissions.env_keep from
	// user config, so a build that needs its own NPM_TOKEN has a way back.
	EnvKeep func() string
}

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
	defer s.mu.Unlock()
	return slices.Clone(s.rules)
}

// Match reports whether a rule pattern matches the whole of s: `*` spans
// any characters (separators included), `?` matches one.
func Match(pattern, s string) bool { return globRegexp(pattern).MatchString(s) }

// Outcome is the aggregated verdict for one operation.
type Outcome struct {
	Decision Decision
	// Rule is the winning rule; nil means the default applied.
	Rule   *Rule
	Reason string
	// notMatched: a rule's match was excluded by its not_match, so the ask is
	// the rule's own and not the default's.
	notMatched bool
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
// absolute, symlinks and .. resolved as the kernel resolves them, and case
// folded where the filesystem folds.
// NOTE: a case-sensitive volume on darwin or windows folds two real files
// into one; a rule for one then covers both.
func CanonPath(p string) string { return FoldCase(RealPath(p)) }

// TildePath shortens the home directory, or a path under it, to ~.
func TildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// Under reports whether path is root or below it, and gives its path
// relative to root. Both must already be resolved; run RealPath first when
// either may hold a symlink. A real entry whose own name starts with ".."
// is inside, which a plain HasPrefix(rel, "..") test gets wrong.
func Under(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// FreeZone reports a path nothing of the user's depends on: inside the pad
// (a request's scratchpad, "" when it has none) or a native temp dir. A
// command may write there, and the file tools read, write and delete there
// with no ask.
func FreeZone(pad, path string) bool {
	if InPad(pad, path) {
		return true
	}
	real := CanonPath(path)
	for _, root := range TempRoots() {
		if _, ok := Under(root, real); ok {
			return true
		}
	}
	return false
}

// InPad reports path inside pad, both resolved, so a symlink in the pad
// cannot lead out. The pad is zemi's own zone, where no path is a protected
// one, though it sits in zemi's state dir.
func InPad(pad, path string) bool {
	real := CanonPath(path)
	_, ok := Under(CanonPath(pad), real)
	return ok && real != ""
}

// TempRoots are the machine's own temp dirs, this process's temp dir and
// /tmp, deduped and in the form rules match (/tmp is /private/tmp on macOS).
func TempRoots() []string {
	var roots []string
	for _, r := range []string{os.TempDir(), "/tmp"} {
		if r = CanonPath(r); r != "" && !slices.Contains(roots, r) {
			roots = append(roots, r)
		}
	}
	return roots
}

// RealPath makes p absolute and resolves it one component at a time, so
// L/../x with L a symlink lands where open lands, not beside L. A missing
// component is joined lexically, so a file about to be created is judged by
// its real parent directory, and a later .. climbs back to resolving
// components that exist (new/../L/x). A symlink loop gives
// "", which is inside no root, and so does an empty path: it names nothing,
// where resolving it to the working directory would judge the project.
func RealPath(p string) string {
	if p == "" {
		return ""
	}
	sep := string(filepath.Separator)
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		// not filepath.Abs: it cleans .. before any symlink is seen
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		p = wd + sep + p
	}
	vol := filepath.VolumeName(p)
	real, rest := vol+sep, p[len(vol):]
	hops := 0
	for rest != "" {
		var c string
		c, rest, _ = strings.Cut(rest, sep)
		switch c {
		case "", ".":
			continue
		case "..":
			real = filepath.Dir(real)
			continue
		}
		next := filepath.Join(real, c)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			real = next
			continue
		}
		link, err := os.Readlink(next)
		if hops++; err != nil || hops > 255 {
			return ""
		}
		link = filepath.FromSlash(link)
		if filepath.IsAbs(link) {
			v := filepath.VolumeName(link)
			real, link = v+sep, link[len(v):]
		}
		rest = link + sep + rest
	}
	return real
}

// Path is an absolute path resolved as the kernel resolves it, the proof
// Open acts on: every directory component is resolved, and the last one too
// unless Lresolve made it, when Open refuses a link there. Only Resolve and
// Lresolve make one, and the zero value opens nothing.
type Path struct{ s string }

// Resolve resolves p with RealPath.
func Resolve(p string) (Path, error) {
	r := RealPath(p)
	if r == "" {
		return Path{}, fmt.Errorf("%s cannot be resolved", p)
	}
	return Path{r}, nil
}

// Lresolve is Resolve leaving the last component as named, like lstat(2):
// a symlink there is the link itself, which Open refuses and Remove deletes.
func Lresolve(p string) (Path, error) {
	sep := string(filepath.Separator)
	dir, name, ok := strings.CutLast(p, sep)
	if !ok {
		dir, name = ".", p
	}
	if name == "" || name == "." || name == ".." {
		return Resolve(p)
	}
	r, err := Resolve(dir + sep)
	if err != nil {
		return Path{}, err
	}
	return Path{filepath.Join(r.s, name)}, nil
}

func (p Path) String() string { return p.s }

// FoldCase folds p on darwin and windows, where the filesystem ignores case.
func FoldCase(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return fold(p)
	}
	return p
}

// fold compares names the way APFS does: full Unicode case folding over the
// canonical decomposition, so U+017F (long s) matches s and U+00E9 matches e+U+0301.
func fold(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool { return r >= utf8.RuneSelf }) {
		// lowercase last: Apache/Cherokee case pairs fold to each other
		// (U+AB83 <-> U+13B3), so folding alone would oscillate and leave no
		// stable canonical; ToLower lands both spellings on one representative
		return strings.ToLower(norm.NFC.String(cases.Fold().String(norm.NFD.String(s))))
	}
	return strings.ToLower(s)
}

// canonPattern puts an fs rule pattern in CanonPath form: the literal
// directory before the first wildcard is canonicalized, the rest only case
// folded. A pattern that starts with a wildcard ("*/.env") stays as is.
func canonPattern(pat string) string {
	// a loop resolves to "", which would turn /loop/* into /*
	canon := func(p string) string { return cmp.Or(CanonPath(p), FoldCase(p)) }
	i := strings.IndexAny(pat, "*?")
	if i < 0 {
		return canon(pat)
	}
	sep := string(filepath.Separator)
	cut := strings.LastIndex(pat[:i], sep)
	switch {
	case i == 0 || cut == 0:
		return FoldCase(pat)
	case cut < 0:
		return canon(".") + sep + FoldCase(pat)
	}
	return canon(pat[:cut]) + FoldCase(pat[cut:])
}

// Evaluate decides one operation argument against one subject's rules.
// Ordering: deny > ask > allow; within a decision, most specific pattern
// (fewest wildcards, then longest literal prefix) wins. An argument that
// matches a rule's pattern but also its NotMatch is skipped, and when no
// other rule matches it forces ask, ahead of Default.
func (s *Store) Evaluate(subject, arg, principal string) Outcome {
	return s.Evaluator(subject, principal)(arg)
}

// Evaluator is Evaluate for many arguments: the rules are read and their
// patterns canonicalized once, which resolves symlinks, instead of per call.
func (s *Store) Evaluator(subject, principal string) func(arg string) Outcome {
	return s.evaluator(subject, principal, false)
}

// evaluator is Evaluator; segment says arg is one command segment, which
// Normalize already split at every unquoted newline (a quoted one is one
// argument, and the pattern the user approved covers it).
func (s *Store) evaluator(subject, principal string, segment bool) func(arg string) Outcome {
	type prepared struct {
		Rule
		pattern, notMatch string
	}
	// DESIGN §9: .. resolves before matching (CanonPath), so dir/* cannot
	// cover dir/../x
	isPath := strings.HasPrefix(subject, "fs.") || strings.HasPrefix(subject, "lua.fs.")
	s.mu.Lock()
	var rules []prepared
	for _, r := range s.rules {
		if subjectMatches(r.Subject, subject) && (r.Principal == "" || r.Principal == principal) {
			rules = append(rules, prepared{Rule: r, pattern: r.Pattern, notMatch: r.NotMatch})
		}
	}
	s.mu.Unlock()
	// every spelling of one file (relative, ./, //, a symlink, another
	// case) must meet the same rules, so both sides match canonical
	for i, r := range rules {
		if isPath {
			rules[i].pattern = canonPattern(r.Pattern)
			if r.NotMatch != "" {
				rules[i].notMatch = canonPattern(r.NotMatch)
			}
		}
	}
	return func(arg string) Outcome {
		type cand struct {
			rule  Rule
			score int
		}
		var (
			byDecision = map[Decision][]cand{}
			fellThru   []string
		)
		match := arg
		if isPath {
			match = CanonPath(arg)
		}
		for _, pr := range rules {
			r := pr.Rule
			if !Match(pr.pattern, match) {
				continue
			}
			// globs span newlines so deny and ask rules hold, but an allow for
			// one line must not cover a second line the user never saw
			if r.Decision == Allow && !segment && strings.Contains(arg, "\n") && !strings.Contains(r.Pattern, "\n") {
				continue
			}
			if r.NotMatch != "" && Match(pr.notMatch, match) {
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
			return Outcome{Decision: Ask, notMatched: true, Reason: "excluded by not_match: " + strings.Join(fellThru, ", ")}
		}
		if s.Default != nil {
			if d := s.Default(subject, arg); d != Ask {
				return Outcome{Decision: d, Reason: "default"}
			}
		}
		return Outcome{Decision: Ask, Reason: "no rule matched"}
	}
}
