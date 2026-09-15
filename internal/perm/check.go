package perm

// CheckCommand decides a bash call (DESIGN §9): the whole call asks when
// the assignment gate or a heredoc fires, fails when normalization fails,
// and is allowed only when every segment and every redirect target
// matches an allow rule.
func (s *Store) CheckCommand(cmd string, principal string) Outcome {
	n := Normalize(cmd)
	if n.Fail != "" {
		return Outcome{Decision: Deny, Reason: n.Fail}
	}
	if n.ForceAsk != "" {
		return Outcome{Decision: Ask, Reason: n.ForceAsk}
	}
	for _, seg := range n.Segments {
		o := s.Evaluate("tool.bash", seg, principal)
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
	return Outcome{Decision: Allow}
}

func joinReason(what, why string) string {
	if why == "" {
		return what
	}
	return what + ": " + why
}
