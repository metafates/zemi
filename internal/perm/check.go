package perm

import (
	"path"
	"slices"
	"strings"
)

// CheckCommand decides a bash call (DESIGN §9): the whole call asks when
// the assignment gate or a heredoc fires, fails when normalization fails,
// and is allowed only when every segment and every redirect target
// matches an allow rule and no assignment prefix was stripped.
func (s *Store) CheckCommand(cmd string, principal string) Outcome {
	return s.CheckCommandFor("tool.bash", cmd, principal)
}

// CheckCommandFor is CheckCommand with the segments matched under subject
// (lua.job.start gates Lua-started programs the same way).
func (s *Store) CheckCommandFor(subject, cmd, principal string) Outcome {
	n := Normalize(cmd)
	if n.Fail != "" {
		return Outcome{Decision: Deny, Reason: n.Fail}
	}
	if n.ForceAsk != "" {
		return Outcome{Decision: Ask, Reason: n.ForceAsk}
	}
	eval := s.evaluator(subject, principal, true)
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
		if o.Decision == Allow && slices.ContainsFunc(n.Args[i], expandsToDotDot) {
			o = Outcome{Decision: Ask, Reason: "the shell expands a path after the rules see it"}
		}
		if o.Decision != Allow {
			o.Reason = joinReason("segment "+seg, o.Reason)
			return o
		}
	}
	for _, p := range n.ReadPaths {
		o := s.Evaluate("fs.read", p, principal)
		if o.Decision != Allow {
			o.Reason = joinReason("read "+p, o.Reason)
			return o
		}
	}
	for _, p := range n.WritePaths {
		o := s.Evaluate("fs.write", p, principal)
		if o.Decision != Allow {
			o.Reason = joinReason("write "+p, o.Reason)
			return o
		}
	}
	if n.Assign != "" {
		return Outcome{Decision: Ask, Reason: n.Assign}
	}
	return Outcome{Decision: Allow}
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
