package perm

import (
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// CheckCommand decides a bash call (DESIGN §9): the whole call asks when
// the assignment gate or a heredoc fires, fails when normalization fails,
// and is allowed only when every segment and every redirect target
// matches an allow rule and no assignment prefix was stripped.
func (s *Store) CheckCommand(cmd, principal string) Outcome {
	return s.CheckCommandFor(SubjectBash, Normalize(cmd), principal)
}

// CheckCommandFor is CheckCommand on a parsed command, with the segments
// matched under subject (lua.job.start gates Lua-started programs the same
// way).
func (s *Store) CheckCommandFor(subject string, n Normalized, principal string) Outcome {
	if n.Fail != "" {
		return Outcome{Decision: Deny, Reason: n.Fail}
	}
	if n.ForceAsk != "" {
		return Outcome{Decision: Ask, Reason: n.ForceAsk}
	}
	eval := s.evaluator(subject, principal, true)
	// the rule an allow rests on: a network-marked one (its run stays
	// confined), else the first that is not the user's own (a model always
	// answer, a project file), else the last one
	var weak *Rule
	for i, seg := range n.Segments {
		o := eval(seg)
		// judged as written and with climbing words cleaned, the stricter
		// verdict holds: docs/* must not allow docs/../x, and a deny on the
		// cleaned path must still fire
		if c := cleanDotDot(n.Args[i]); c != seg {
			if oc := eval(c); oc.Decision == Deny || o.Decision == Allow {
				o = oc
			}
		}
		// a deny, ask or not_match written for the program also meets the
		// spellings that hide it, so a mode cannot settle them as unruled;
		// an allow stays judged as written
		if c := canonical(n.Args[i]); c != seg {
			if oc := eval(c); oc.Decision != Allow && oc.Ruled() && (oc.Decision == Deny || o.Decision == Allow || !o.Ruled()) {
				o = oc
			}
		}
		if o.Decision == Allow && slices.ContainsFunc(n.Args[i], expandsToDotDot) {
			o = Outcome{Decision: Ask, Reason: "the shell expands a path after the rules see it"}
		}
		if o.Decision != Allow {
			o.Reason = joinReason("segment "+seg, o.Reason)
			return o
		}
		if weak == nil || !weak.Network && (weak.Principal == "" && weak.Source == "" || o.Rule != nil && o.Rule.Network) {
			weak = o.Rule
		}
	}
	for _, p := range n.ReadPaths {
		o := s.Evaluate(SubjectRead, p, principal)
		if o.Decision != Allow {
			o.Reason = joinReason("read "+p, o.Reason)
			return o
		}
	}
	for _, p := range n.WritePaths {
		o := s.Evaluate(SubjectWrite, p, principal)
		if o.Decision != Allow {
			o.Reason = joinReason("write "+p, o.Reason)
			return o
		}
	}
	if n.Assign != "" {
		return Outcome{Decision: Ask, Reason: n.Assign}
	}
	return Outcome{Decision: Allow, Rule: weak}
}

// cleanDotDot is the segment with every word that climbs out of a directory
// it names cleaned as a path.
// NOTE: lexical, a command's working directory is unknown here, so a symlink
// under an allowed directory still leads out of it, with or without "..".
func cleanDotDot(argv []string) string {
	words := slices.Clone(argv)
	for i, w := range words {
		if strings.Contains(w, "/../") || strings.HasSuffix(w, "/..") {
			words[i] = path.Clean(w)
		}
	}
	return strings.Join(words, " ")
}

// runnerOpts are the runner prefixes canonical steps past, each with its
// options that take a value.
var runnerOpts = map[string][]string{
	"command": nil, "builtin": nil, "nohup": nil, "setsid": nil, "exec": {"-a"}, "time": {"-f", "-o"},
	"env": {"-u", "-C", "-S"}, "sudo": {"-u", "-g", "-C", "-D", "-h", "-p", "-r", "-t", "-U"}, "doas": {"-u", "-C"},
	"timeout": {"-s", "-k"}, "nice": {"-n"}, "ionice": {"-c", "-n", "-p"}, "stdbuf": {"-i", "-o", "-e"},
	"caffeinate": {"-t", "-w"}, "xargs": {"-n", "-I", "-P", "-L", "-d", "-E", "-s", "-a"},
}

// canonical is a segment as the program it runs: leading reserved words
// dropped, runner prefixes (env, sudo, timeout 5, ...) stepped past, the
// program by its base name and without the options before its first
// operand, so "git push" also names "/usr/bin/git -C . push".
// NOTE: best effort: a git alias (git -c alias.p=push p), a runner option
// missing from runnerOpts or a program picked at run time still slips past.
func canonical(argv []string) string {
	f := program(argv)
	for ok := true; ok && len(f) > 1; {
		f, ok = stepRunner(f)
	}
	if len(f) == 0 {
		return ""
	}
	i := Subcommand(f)
	if i < 0 {
		i = 1
	}
	return strings.Join(append([]string{filepath.Base(f[0])}, f[i:]...), " ")
}

// stepRunner steps past the runnerOpts runner f starts with and the options
// and values it takes (timeout 5, nice -n 5); ok is false when f starts with
// none.
func stepRunner(f []string) (_ []string, ok bool) {
	opts, ok := runnerOpts[filepath.Base(f[0])]
	if !ok {
		return f, false
	}
	f = f[1:]
	for len(f) > 1 {
		_, assign := assignmentName(f[0])
		switch a := f[0]; {
		case slices.Contains(opts, a):
			f = f[min(2, len(f)):]
		case strings.HasPrefix(a, "-") || assign || a != "" && a[0] >= '0' && a[0] <= '9':
			f = f[1:]
		default:
			return f, true
		}
	}
	return f, true
}

// Ruled reports an outcome a rule decided: a deny, ask or allow rule, or an
// allow a not_match excluded.
func (o Outcome) Ruled() bool {
	return o.Rule != nil || strings.Contains(o.Reason, "excluded by not_match")
}

// expandsToDotDot reports a path word the shell may still turn into a climb:
// a brace or parameter expansion, or a glob in a component with a leading
// dot, the only kind that matches "..".
// NOTE: quoting is gone by here, so a quoted literal asks too.
func expandsToDotDot(w string) bool {
	if !strings.Contains(w, "/") {
		return false
	}
	if strings.ContainsAny(w, "{$") {
		return true
	}
	for c := range strings.SplitSeq(w, "/") {
		if strings.HasPrefix(c, ".") && strings.ContainsAny(c, "*?[") {
			return true
		}
	}
	return false
}

func joinReason(what, why string) string {
	if why == "" {
		return what
	}
	return what + ": " + why
}
