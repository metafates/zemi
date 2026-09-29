package perm

import (
	"strings"
	"testing"
)

// The stuff a fuzzer can panic on is exactly the untrusted input the
// sandbox gates judge (DESIGN §9): commands from the model, paths, and
// rule patterns. A panic or hang here is a security bug, so each parser
// gets a fuzz target fed real table-case seeds. `go test -fuzz=Name -fuzztime=30s`
// runs the campaign; plain `make check` runs the seeds as normal cases.

func FuzzNormalize(f *testing.F) {
	for _, s := range []string{
		"echo one && echo two; echo three | grep e",
		"D=/a/b; ls $D; cat ${D}/x.go",
		"r\\\nm -rf x",
		"FOO=1 BAR=2 make test",
		"echo a#b '#c' x\\#y",
		"PATH=/evil; ls $PATH",
		"grep $(rm -rf x)",
		"git log --format=\"`id`\"",
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		n := Normalize(cmd)
		// the recover that fails such a call closed is a net, not a way out
		if strings.Contains(n.Fail, panicked) {
			t.Fatalf("%q: %s", cmd, n.Fail)
		}
		n.Programs("") // the permission prompt lists them for every command asked about
		// a command Normalize gates (fail or a forced ask) must never be
		// allowed through by the full decision path, whatever the rules say
		s := new(Store)
		s.Add(Rule{Subject: "tool.shell", Pattern: "*", Decision: Allow})
		if (n.Fail != "" || n.ForceAsk != "") && s.CheckCommandFor(SubjectShell, n, "").Decision == Allow {
			t.Fatalf("gated command allowed: %q (%s %s)", cmd, n.Fail, n.ForceAsk)
		}
	})
}

func FuzzReadCommand(f *testing.F) {
	for _, s := range []string{
		"git log --oneline",
		"git log --format=\"`id`\"",
		"git log # it's\nrm -rf x\n'",
		"cat docs/*",
		"cat docs/../x",
		"ls -la /tmp",
		"grep -rn KEY .",
		"GIT_EXTERNAL_DIFF=./x git diff",
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		Normalize(cmd).ReadCommand(func(_ string, _ ArgUse) bool { return true })
	})
}

func FuzzPathIsSecret(f *testing.F) {
	for _, s := range []string{
		"/tmp/x", ".env", "/etc/passwd", "~/.ssh/id_rsa",
		"some/dir/.git/config", "a/b/../../c", "", ".",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		PathIsSecret(p)
	})
}

func FuzzCanonPath(f *testing.F) {
	for _, s := range []string{
		"private/x", "./private/x", "/tmp/../etc/passwd",
		"a//b", ".", "..", "loop/../x", "/tmp/*/x", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		// canonicalizing twice must be the identity: once is fully resolved
		if c := CanonPath(p); CanonPath(c) != c {
			t.Fatalf("CanonPath not idempotent: %q -> %q -> %q", p, c, CanonPath(c))
		}
	})
}

// FuzzEvaluate pins the core security ordering on a non-fs subject (no path
// canonicalization, so globRegexp is authoritative): a deny rule whose
// pattern matches the argument must always beat a global allow.
func FuzzEvaluate(f *testing.F) {
	for _, seed := range [][2]string{
		{"*", "git log"},
		{"git*", "git log"},
		{"*/rm *", "rm -rf /"},
		{"a?b", "abc"},
		{"x/*/y", "x/z/y"},
		{"*\n*", "a"},
		{"", "anything"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, pat, arg string) {
		s := new(Store)
		s.Add(Rule{Subject: "tool.shell", Pattern: "*", Decision: Allow})
		s.Add(Rule{Subject: "tool.shell", Pattern: pat, Decision: Deny})
		if Match(pat, arg) && s.Evaluate("tool.shell", arg, "").Decision != Deny {
			t.Fatalf("deny %q matched %q but verdict is not Deny", pat, arg)
		}
	})
}

// FuzzNotMatch pins the other half of the ordering (DESIGN §9): an argument a
// rule's NotMatch excludes must never be allowed, even when its pattern
// matches - it falls through and forces Ask.
func FuzzNotMatch(f *testing.F) {
	for _, seed := range [][2]string{
		{"/x/*", "/x/secret*"},
		{"a*", "ab*"},
		{"*", "x*"},
		{"git*", "git secret*"},
		{"", ""},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, pat, not string) {
		if not == "" {
			return // an empty NotMatch means "no exclusion", not a matcher
		}
		s := new(Store)
		s.Add(Rule{Subject: "tool.shell", Pattern: pat, Decision: Allow, NotMatch: not})
		// an argument matching both pattern and NotMatch must be forced to Ask
		arg := not
		if o := s.Evaluate("tool.shell", arg, ""); o.Decision == Allow {
			t.Fatalf("not_match %q excluded arg %q of rule %q but verdict is Allow", not, arg, pat)
		}
	})
}
