package perm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Normalized is the result of normalizing a bash command (DESIGN §9).
type Normalized struct {
	// Segments are the collapsed command segments: separator-split,
	// quote-aware argv, assignment prefixes unwrapped, sh -c recursion
	// applied. Each segment is matched against rules on its own.
	Segments []string
	// Args are the segments' argv, each word as the program receives it.
	Args [][]string
	// ReadPaths and WritePaths are redirect targets gating as fs.read /
	// fs.write (device exemptions already applied).
	ReadPaths  []string
	WritePaths []string
	// ForceAsk is a non-empty reason when the whole call must ask
	// regardless of allow rules (environment assignment gate, heredoc).
	ForceAsk string
	// Opaque marks shell state the segments do not show: a variable
	// assignment, or a nested sh -c whose arguments reach the inner shell.
	Opaque bool
	// Assign is a non-empty reason when an assignment prefix was stripped
	// off a segment. The rules never see it, so an allow verdict downgrades
	// to ask (CheckCommandFor): GIT_EXTERNAL_DIFF=./x git diff runs ./x.
	Assign string
	// Fail is a non-empty reason when the call must fail outright
	// (unbalanced quotes or parens, emptied redirect target).
	Fail string
}

// envGateNames are the environment-affecting variables whose assignment
// forces ask (curated denylist, case-insensitive).
var envGateNames = []string{
	"path", "bash_env", "env", "ifs", "pythonpath", "pythonhome",
	"pythonstartup", "node_options", "node_path", "rubyopt", "rubylib",
	"perl5opt", "perl5lib", "perllib", "lua_path", "lua_init",
	"shellopts", "bash_xtracefd", "editor", "visual", "pager",
	"git_ssh_command", "git_editor", "git_pager",
}

// shellVarNames are variables the shell itself reads (cd, globbing,
// tracing, startup) or sets between commands ($_ is the last argument), so
// inlineVars never drops their assignment.
var shellVarNames = []string{
	"cdpath", "home", "pwd", "oldpwd", "globignore", "fignore", "ps4",
	"bashopts", "posixly_correct", "histfile", "tmpdir",
	"_", "lineno", "random", "srandom", "seconds", "optind", "optarg", "reply",
	"ppid", "bashpid", "uid", "euid", "groups", "shlvl", "columns", "lines",
	"epochrealtime", "epochseconds", "histcmd", "funcname", "pipestatus",
	"bash_command", "bash_subshell", "bash_lineno", "bash_argv0", "bash_rematch",
}

// EnvAssignmentGate reports whether a name matches the environment-affecting
// denylist (case-insensitive). Job env extras are refused on it outright.
func EnvAssignmentGate(name string) bool {
	n := strings.ToLower(name)
	if slices.Contains(envGateNames, n) {
		return true
	}
	for _, p := range []string{"ld_", "dyld_", "bash_func_", "git_config_"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// Normalize turns a raw bash command into matched-form segments plus the
// redirect gates. It never errors: problems surface in Fail/ForceAsk.
func Normalize(cmd string) Normalized {
	var n Normalized
	cmd = inlineVars(cmd)
	n.scan(cmd)
	if n.Fail == "" && n.ForceAsk == "" && quotedSubstitution(cmd) {
		n.ForceAsk = "command substitution inside quotes"
	}
	return n
}

// inlineVars substitutes leading literal assignments (D=/long/path; ls $D)
// back into the command, so path gates, rules and ReadCommand see the real
// path instead of an opaque expansion. Only a value the shell would leave
// as is qualifies: one word, no quotes, no expansion beyond an earlier
// variable, no glob. A command that assigns anything else is left alone.
func inlineVars(cmd string) string {
	vars := map[string]string{}
	rest := cmd
	for {
		m := leadingAssign.FindStringSubmatch(rest)
		if m == nil || EnvAssignmentGate(m[1]) {
			break
		}
		// An exported name (HOME, GOFLAGS) reaches every child process and a
		// shell variable (CDPATH) steers the shell even unexported, so either
		// assignment stays in the text and keeps the command opaque.
		if _, exported := os.LookupEnv(m[1]); exported || slices.Contains(shellVarNames, strings.ToLower(m[1])) {
			break
		}
		v := substVars(m[2], vars)
		if strings.ContainsRune(v, '$') {
			break
		}
		vars[m[1]] = v
		rest = rest[len(m[0]):]
	}
	if len(vars) == 0 {
		return cmd
	}
	out := substVars(rest, vars)
	// A name written other than as $NAME or ${NAME} may be reassigned
	// (D=, for D, read D, eval $X=), so its references are not the value.
	for name := range vars {
		if regexp.MustCompile(`(^|[^$\w{])` + name + `\b`).MatchString(out) {
			return cmd
		}
	}
	return out
}

// substVars replaces $NAME and ${NAME} outside single quotes with the
// known values; unknown names stay as written.
func substVars(s string, vars map[string]string) string {
	var b strings.Builder
	inSingle := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inSingle:
			inSingle = c != '\''
		case c == '\\':
			b.WriteByte(c)
			if i+1 < len(s) {
				i++
				c = s[i]
			}
		case c == '\'':
			inSingle = true
		case c == '$':
			if m := varRef.FindStringSubmatch(s[i:]); m != nil {
				if v, ok := vars[m[1]+m[2]]; ok {
					b.WriteString(v)
					i += len(m[0]) - 1
					continue
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

var (
	leadingAssign = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)=([A-Za-z0-9_./:@%+,=${}-]+)[ \t]*(;|&&|\n)\s*`)
	varRef        = regexp.MustCompile(`^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)
)

// quotedSubstitution reports a $(, backtick or <( inside double quotes. The
// scanner keeps a quoted run as one literal word, but the shell still runs
// what is in there, so no allow rule may cover it.
func quotedSubstitution(cmd string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case inSingle:
			inSingle = c != '\''
		case c == '\\':
			i++
		case c == '"':
			inDouble = !inDouble
		case c == '\'':
			inSingle = !inDouble
		case inDouble && (c == '`' || ((c == '$' || c == '<') && i+1 < len(cmd) && cmd[i+1] == '(')):
			return true
		}
	}
	return false
}

type tokKind int

const (
	tokWord tokKind = iota
	tokSep
	tokRedirect
	tokTarget // word following a redirect operator
	tokHeredoc
)

type token struct {
	kind tokKind
	text string
	op   string
}

// scanner walks the command producing the token stream. Quote-aware:
// separators and redirects inside quotes are literal text.
type scanner struct {
	toks   []token
	depth  int
	failed bool // unbalanced quotes or parens
}

func (n *Normalized) scan(cmd string) {
	var sc scanner
	var cur strings.Builder
	any := false
	flush := func() {
		if any {
			sc.toks = append(sc.toks, token{kind: tokWord, text: cur.String()})
		}
		cur.Reset()
		any = false
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\'':
			any = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '\''; j++ {
				cur.WriteRune(rs[j])
			}
			if j >= len(rs) {
				sc.failed = true
				flush()
				n.eat(&sc)
				return
			}
			i = j
		case r == '"':
			any = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
					if rs[j] == '\n' { // line continuation
						continue
					}
				}
				cur.WriteRune(rs[j])
			}
			if j >= len(rs) {
				sc.failed = true
				flush()
				n.eat(&sc)
				return
			}
			i = j
		case r == '\\' && i+1 < len(rs) && rs[i+1] == '\n': // line continuation
			i++
		case r == '\\':
			any = true
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
		case r == '`':
			flush()
			sc.toks = append(sc.toks, token{kind: tokSep})
		case (r == '$' || r == '<' || r == '>') && i+1 < len(rs) && rs[i+1] == '(':
			flush()
			i++
			sc.depth++
			sc.toks = append(sc.toks, token{kind: tokSep})
		case r == '$' && i+1 < len(rs) && (rs[i+1] == '\'' || rs[i+1] == '"'):
			// $'..' and $".." quote in bash (the $ goes) but not in dash; a
			// backslash in $'..' can move where bash ends the quote, so ask
			q := rs[i+2:]
			if end := slices.Index(q, '\''); rs[i+1] == '\'' && end >= 0 && slices.Contains(q[:end], '\\') {
				n.ForceAsk = "ANSI-C quoting with a backslash"
			}
		case r == '#' && !any:
			end := slices.Index(rs[i:], '\n')
			if end < 0 {
				end = len(rs) - i
			}
			// after ) or ` the # may be word text ($(..)#), so it stays a word;
			// a quote or backslash there would carry the next line into it
			if i > 0 && (rs[i-1] == ')' || rs[i-1] == '`') {
				if slices.ContainsFunc(rs[i:i+end], func(c rune) bool { return c == '\'' || c == '"' || c == '\\' }) {
					n.ForceAsk = "comment with a quote or backslash"
				}
				any = true
				cur.WriteRune(r)
				continue
			}
			i += end - 1 // sh passes no comment words
		case r == '(':
			flush()
			sc.depth++
			sc.toks = append(sc.toks, token{kind: tokSep})
		case r == ')':
			flush()
			sc.depth--
			if sc.depth < 0 {
				sc.failed = true
			}
			sc.toks = append(sc.toks, token{kind: tokSep})
		case r == ' ' || r == '\t' || r == '\n': // sh splits on these only (NBSP, \v, \r are word text)
			flush()
			if r == '\n' {
				sc.toks = append(sc.toks, token{kind: tokSep})
			}
		case r == ';':
			flush()
			sc.toks = append(sc.toks, token{kind: tokSep})
		case r == '&':
			flush()
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
			}
			sc.toks = append(sc.toks, token{kind: tokSep})
		case r == '|':
			flush()
			if i+1 < len(rs) && rs[i+1] == '|' {
				i++
			}
			sc.toks = append(sc.toks, token{kind: tokSep})
		case r == '>' || r == '<':
			if isAllDigits(cur.String()) { // the fd of 2>/dev/null is no argument
				cur.Reset()
				any = false
			}
			flush()
			op, n2 := readRedirectOp(rs, i)
			i += n2 - 1
			// classify: heredoc forms, fd forms, file forms
			switch op {
			case "<<", "<<-", "<<<":
				sc.toks = append(sc.toks, token{kind: tokHeredoc, op: op})
			default:
				sc.toks = append(sc.toks, token{kind: tokRedirect, op: op})
			}
		default:
			any = true
			cur.WriteRune(r)
		}
	}
	flush()
	if sc.depth != 0 {
		sc.failed = true
	}
	n.eat(&sc)
}

// readRedirectOp reads a redirect operator starting at rs[i]; returns the
// operator text and how many runes it consumed.
func readRedirectOp(rs []rune, i int) (string, int) {
	s := string(rs[i:min(i+3, len(rs))])
	for _, op := range []string{"<<<", "<<-", ">>", ">|", ">&", "<<", "<&", "<>"} {
		if strings.HasPrefix(s, op) {
			return op, len(op)
		}
	}
	return string(rs[i]), 1
}

// eat walks the token stream: redirects take the next word as target,
// separators cut segments, heredocs force ask.
func (n *Normalized) eat(sc *scanner) {
	var seg []string
	toks := sc.toks
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.kind {
		case tokSep:
			n.fold(seg)
			seg = nil
		case tokHeredoc:
			n.ForceAsk = "heredoc or herestring body is shell-executed input"
		case tokRedirect:
			if i+1 < len(toks) && toks[i+1].kind == tokWord {
				n.gate(t.op, toks[i+1].text)
				i++
			} else {
				n.Fail = "empty redirect target"
			}
		case tokWord:
			seg = append(seg, t.text)
		}
	}
	n.fold(seg)
	if sc.failed {
		n.Fail = "unbalanced quotes or parens"
	}
}

// gate classifies one redirect with its target into fs.read/fs.write
// operations (device exemptions applied).
func (n *Normalized) gate(op, target string) {
	if target == "" {
		n.Fail = "empty redirect target"
		return
	}
	if devNull(target) || ((op == ">&" || op == "<&") && (isAllDigits(target) || target == "-")) {
		return
	}
	// < <> <& read; > >> >| &> &>> >& <> write
	if strings.HasPrefix(op, "<") {
		n.ReadPaths = append(n.ReadPaths, target)
	}
	if strings.Contains(op, ">") {
		n.WritePaths = append(n.WritePaths, target)
	}
}

// attachedRedirect handles the glued forms the scanner leaves inside a
// word: `2>file`, `2>>file`, `>&file`, `2>&1`, `<&3`.
func attachedRedirect(word string) (op, target string, ok bool) {
	i := 0
	for i < len(word) && word[i] >= '0' && word[i] <= '9' {
		i++
	}
	rest := word[i:]
	for _, cand := range []string{"&>>", ">>", ">&", "<&", "&>", ">|", "<>", ">", "<"} {
		if strings.HasPrefix(rest, cand) {
			t := rest[len(cand):]
			if t == "" {
				return "", "", false
			}
			// fd duplication (2>&1) stays part of the segment text
			if (cand == ">&" || cand == "<&") && (isAllDigits(t) || t == "-") {
				return "", "", false
			}
			return cand, t, true
		}
	}
	return "", "", false
}

func isAllDigits(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
}

func devNull(p string) bool {
	return p == "/dev/null" || p == "/dev/zero"
}

// fold collapses one segment: assignment gate, prefix unwrap, sh -c
// recursion, then the joined argv becomes the matched form.
func (n *Normalized) fold(seg []string) {
	if len(seg) == 0 {
		return
	}
	// peel redirect-glued words into their own gates first
	var argv []string
	for _, w := range seg {
		if op, target, ok := attachedRedirect(w); ok {
			n.gate(op, target)
			continue
		}
		argv = append(argv, w)
	}
	if len(argv) == 0 {
		return
	}
	// these builtins assign the variables they name (read PATH, printf -v PATH)
	assigns := slices.Contains([]string{"for", "select", "read", "mapfile", "readarray", "getopts", "printf"}, argv[0])
	for _, a := range argv {
		name, ok := assignmentName(a)
		if assigns && !ok {
			name, ok = assignmentName(strings.TrimPrefix(a, "-v") + "=")
		}
		if ok && EnvAssignmentGate(name) {
			n.ForceAsk = "environment assignment: " + name
			return
		}
	}
	for len(argv) > 0 {
		name, ok := assignmentName(argv[0])
		if !ok {
			break
		}
		argv = argv[1:]
		n.Opaque = true
		if n.Assign == "" {
			n.Assign = "environment assignment: " + name
		}
	}
	if len(argv) == 0 {
		return
	}
	if isShell(argv[0]) {
		if body, ok := shellBody(argv[1:]); ok {
			n.absorb(Normalize(body))
			n.Opaque = true
			return
		}
	}
	n.Segments = append(n.Segments, strings.Join(argv, " "))
	n.Args = append(n.Args, argv)
}

func (n *Normalized) absorb(inner Normalized) {
	n.Segments = append(n.Segments, inner.Segments...)
	n.Args = append(n.Args, inner.Args...)
	n.ReadPaths = append(n.ReadPaths, inner.ReadPaths...)
	n.WritePaths = append(n.WritePaths, inner.WritePaths...)
	if inner.Fail != "" && n.Fail == "" {
		n.Fail = inner.Fail
	}
	if inner.ForceAsk != "" && n.ForceAsk == "" {
		n.ForceAsk = inner.ForceAsk
	}
	if inner.Assign != "" && n.Assign == "" {
		n.Assign = inner.Assign
	}
}

// isShell judges a command word by its lowercased base: `/bin/bash` and,
// on a case-folding filesystem, `BASH` run the same shell.
func isShell(word string) bool {
	switch strings.ToLower(filepath.Base(word)) {
	case "sh", "bash", "zsh", "dash", "ksh", "ash":
		return true
	}
	return false
}

// shellBody is the command a shell's argv runs through -c, alone or in a
// bundle (-lc, -ec, the spelling most models write): the word after it.
// A flag that takes a value is stepped over; the first plain word is the
// script the shell runs instead.
func shellBody(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "+o" || a == "-O" || a == "+O" || a == "--rcfile" || a == "--init-file":
			i++
		case !strings.HasPrefix(a, "-") || a == "-" || a == "--":
			return "", false
		case !strings.HasPrefix(a, "--") && strings.Contains(a[1:], "c") && i+1 < len(args):
			return args[i+1], true
		}
	}
	return "", false
}

// assignmentName recognizes NAME=value and NAME+=value, and NAME[i]=value,
// which sets a plain variable NAME as well.
func assignmentName(tok string) (string, bool) {
	i := strings.Index(tok, "=")
	if i <= 0 {
		return "", false
	}
	name := strings.TrimSuffix(tok[:i], "+")
	if j := strings.IndexByte(name, '['); j > 0 && strings.HasSuffix(name, "]") {
		name = name[:j]
	}
	return name, isName(name)
}

func isName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

var secretEnvShape = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "PASSPHRASE", "CREDENTIAL"}

// EnvIsSecret reports whether an env var name looks like it holds a secret
// (case-insensitive); such variables never reach children or Lua. PWD and
// PASS count only as a suffix after _, so PWD, OLDPWD and COMPASS pass.
func EnvIsSecret(name string) bool {
	up := strings.ToUpper(name)
	return strings.HasSuffix(up, "_PWD") || strings.HasSuffix(up, "_PASS") ||
		slices.ContainsFunc(secretEnvShape, func(s string) bool { return strings.Contains(up, s) })
}

func (st *Store) envKept(name string) bool {
	if st == nil || st.EnvKeep == nil {
		return false
	}
	for s := range strings.SplitSeq(st.EnvKeep(), ",") {
		if strings.EqualFold(strings.TrimSpace(s), name) {
			return true
		}
	}
	return false
}

// ChildEnv is a child process environment: zemi's own minus secret-shaped
// names the user did not keep, plus extras that pass the secret and
// assignment gates.
func (st *Store) ChildEnv(extras map[string]any) ([]string, error) {
	var out []string
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && (!EnvIsSecret(name) || st.envKept(name)) {
			out = append(out, kv)
		}
	}
	for k, v := range extras {
		// a key carrying = would set the name before it (PATH=/tmp/x:)
		if !isName(k) || EnvIsSecret(k) || EnvAssignmentGate(k) {
			return nil, fmt.Errorf("env var %q is refused by the environment gate", k)
		}
		out = append(out, k+"="+fmt.Sprint(v))
	}
	return out, nil
}
