package perm

import (
	"cmp"
	"fmt"
	"os"
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
	// (a quote or parenthesis not closed, a redirect with no target).
	Fail string
	// cmd is the text the segments came from, variables inlined, for what
	// only the text shows (ReadCommand's expands)
	cmd string
	// hidden marks a program no segment names, so Programs lists none.
	hidden bool
	// doubt says why the shell may group the text otherwise than the
	// segments show, so Destructive cannot vouch for them either
	doubt string
	// shown are the rows Programs lists when a heredoc makes them differ
	// from Args: a dataReader's body runs only its substitutions while
	// nothing in the command runs code, though the rules judge it as commands.
	shown [][]string
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

// envAssignmentGate reports whether a name matches the environment-affecting
// denylist (case-insensitive). Job env extras are refused on it outright.
func envAssignmentGate(name string) bool {
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
	cmd = inlineVars(cmd)
	n := Normalized{cmd: cmd}
	var sc scanner
	sc.words(&n, []rune(cmd), 0, 0)
	sc.failed = sc.failed || sc.depth != 0
	n.eat(&sc)
	return n
}

// inlineVars substitutes leading literal assignments (D=/long/path; ls $D)
// back into the command, so path gates, rules and ReadCommand see the real
// path instead of an opaque expansion. Only a value the shell would leave
// as is qualifies: one word, no quotes, no expansion beyond an earlier
// variable, no glob. A command that assigns anything else is left alone.
func inlineVars(cmd string) string {
	// the shell expands no heredoc delimiter nor a line it compares against
	// one, so inlining could move where zemi ends a body
	if strings.Contains(cmd, "<<") {
		return cmd
	}
	vars := map[string]string{}
	rest := cmd
	for {
		m := leadingAssign.FindStringSubmatch(rest)
		if m == nil || envAssignmentGate(m[1]) {
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

type tokKind int

const (
	tokWord tokKind = iota
	tokSep
	tokRedirect
	tokHeredoc
	tokBody // a heredoc's body; quoted when its delimiter was
)

type token struct {
	kind   tokKind
	text   string
	op     string
	quoted bool
}

// heredoc is a << or <<- whose body starts after the line's newline.
type heredoc struct {
	delim        string
	quoted, tabs bool
}

// scanner walks the command producing the token stream. Quote-aware:
// separators and redirects inside quotes are literal text.
type scanner struct {
	toks []token
	// held are the tokens of substitutions inside double quotes, emitted
	// where the segment holding the quoted word ends
	held   []token
	docs   []heredoc // opened on the current line
	depth  int
	arith  []int // the depths of the (( )) and $(( )) open, where << shifts
	subs   []int // the depths of the $( <( >( open
	tick   bool  // inside a backtick substitution
	failed bool  // a quote or parenthesis not closed
}

// sep ends a segment; the substitutions its quoted words held follow it.
func (sc *scanner) sep() {
	sc.toks = append(sc.toks, sc.held...)
	sc.held = nil
	sc.toks = append(sc.toks, token{kind: tokSep})
}

// words scans rs from i up to the rune stop outside quotes and parens (0:
// to the end) and returns stop's index, or len(rs) when it never comes.
func (sc *scanner) words(n *Normalized, rs []rune, i int, stop rune) int {
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			sc.toks = append(sc.toks, token{kind: tokWord, text: cur.String()})
		}
		cur.Reset()
		inWord = false
	}
	for ; i < len(rs); i++ {
		r := rs[i]
		if stop != 0 && r == stop && sc.depth == 0 {
			break
		}
		switch {
		case r == '\'':
			inWord = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '\''; j++ {
				cur.WriteRune(rs[j])
			}
			sc.failed = sc.failed || j >= len(rs)
			i = j
		case r == '"':
			inWord = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
					if rs[j] != '\n' { // line continuation
						cur.WriteRune(rs[j])
					}
					continue
				}
				if end, ok := sc.quotedSub(n, rs, j); ok {
					cur.WriteString(string(rs[j:min(end+1, len(rs))]))
					j = end
					continue
				}
				cur.WriteRune(rs[j])
			}
			sc.failed = sc.failed || j >= len(rs)
			i = j
		case r == '\\' && i+1 < len(rs) && rs[i+1] == '\n': // line continuation
			i++
		case r == '\\':
			inWord = true
			// inside backticks \` opens a nested substitution the shell runs,
			// which the words here would keep as text
			if sc.tick && i+1 < len(rs) && rs[i+1] == '`' {
				n.ForceAsk = "nested backtick substitution"
				n.hidden, n.doubt = true, n.ForceAsk
			}
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
		case r == '`':
			if sc.tick {
				sc.closes(n, rs, i)
			} else {
				sc.opens(n, inWord, cur.String())
			}
			sc.tick = !sc.tick
			flush()
			sc.sep()
		case (r == '$' || r == '<' || r == '>') && i+1 < len(rs) && rs[i+1] == '(':
			sc.opens(n, inWord, cur.String())
			flush()
			i++
			sc.depth++
			sc.subs = append(sc.subs, sc.depth)
			sc.sep()
		case r == '$' && i+1 < len(rs) && (rs[i+1] == '\'' || rs[i+1] == '"'):
			// $'..' and $".." quote in bash (the $ goes) but not in dash; a
			// backslash in $'..' can move where bash ends the quote, so ask
			q := rs[i+2:]
			if end := slices.Index(q, '\''); rs[i+1] == '\'' && end >= 0 && slices.Contains(q[:end], '\\') {
				n.ForceAsk = "ANSI-C quoting with a backslash"
				n.hidden, n.doubt = true, n.ForceAsk
			}
		case r == '#' && !inWord:
			end := slices.Index(rs[i:], '\n')
			if end < 0 {
				end = len(rs) - i
			}
			// after ) or ` the # may be word text ($(..)#), so it stays a word;
			// a quote or backslash there would carry the next line into it
			if i > 0 && (rs[i-1] == ')' || rs[i-1] == '`') {
				if slices.ContainsFunc(rs[i:i+end], func(c rune) bool { return c == '\'' || c == '"' || c == '\\' }) {
					n.ForceAsk = "comment with a quote or backslash"
					n.hidden, n.doubt = true, n.ForceAsk
				}
				inWord = true
				cur.WriteRune(r)
				continue
			}
			i += end - 1 // sh passes no comment words
		case r == '(':
			// the words of an array (a=(x y)) or arithmetic are no commands
			if inWord && strings.HasSuffix(cur.String(), "=") {
				n.hidden = true
			}
			flush()
			sc.depth++
			if i > 0 && rs[i-1] == '(' {
				sc.arith = append(sc.arith, sc.depth)
				n.hidden = true
			}
			sc.sep()
		case r == ')':
			flush()
			sc.depth--
			if sc.depth < 0 {
				sc.failed = true
			}
			for len(sc.arith) > 0 && sc.depth < sc.arith[len(sc.arith)-1] {
				sc.arith = sc.arith[:len(sc.arith)-1]
			}
			if k := len(sc.subs); k > 0 && sc.depth < sc.subs[k-1] {
				sc.subs = sc.subs[:k-1]
				sc.closes(n, rs, i)
			}
			sc.sep()
		case r == ' ' || r == '\t' || r == '\n': // sh splits on these only (NBSP, \v, \r are word text)
			flush()
			if r == '\n' {
				sc.sep()
				i = sc.bodies(rs, i)
			}
		case r == ';' || r == '&' || r == '|': // && and || are two empty separators
			flush()
			sc.sep()
		case r == '>' || r == '<':
			if isAllDigits(cur.String()) { // the fd of 2>/dev/null is no argument
				cur.Reset()
				inWord = false
			}
			flush()
			op, n2 := readRedirectOp(rs, i)
			i += n2 - 1
			// classify: heredoc forms, fd forms, file forms
			switch op {
			case "<<", "<<-":
				if len(sc.arith) > 0 { // a shift: the lines below stay commands
					sc.sep()
					break
				}
				d := sc.delimiter(rs, &i)
				d.tabs = op == "<<-"
				sc.docs = append(sc.docs, d)
				sc.toks = append(sc.toks, token{kind: tokHeredoc, op: op, text: d.delim, quoted: d.quoted})
			case "<<<":
				sc.toks = append(sc.toks, token{kind: tokHeredoc, op: op})
			default:
				sc.toks = append(sc.toks, token{kind: tokRedirect, op: op})
			}
		default:
			inWord = true
			cur.WriteRune(r)
		}
	}
	flush()
	sc.sep()
	return i
}

// quotedSub scans the $( or backtick substitution at rs[j] inside double
// quotes, which the shell runs like an unquoted one, and returns the index
// of its close.
func (sc *scanner) quotedSub(n *Normalized, rs []rune, j int) (int, bool) {
	from, stop := j+1, '`'
	switch {
	case rs[j] == '$' && j+1 < len(rs) && rs[j+1] == '(':
		from, stop = j+2, ')'
	case rs[j] != '`':
		return 0, false
	}
	sub := scanner{tick: stop == '`'}
	end := sub.words(n, rs, from, stop)
	// the ) of a case pattern ends it early, leaving the rest quoted text
	// the shell runs: a case with no esac fails
	sep, open := true, 0
	for _, t := range sub.toks {
		switch {
		case sep && t.kind == tokWord && t.text == "case":
			open++
		case sep && t.kind == tokWord && t.text == "esac":
			open--
		}
		sep = t.kind == tokSep || t.kind == tokWord && reserved[t.text]
	}
	sc.failed = sc.failed || sub.failed || open > 0 || end >= len(rs)
	sc.held = append(sc.held, token{kind: tokSep})
	sc.held = append(append(sc.held, sub.toks...), token{kind: tokSep})
	if n.ForceAsk == "" {
		n.ForceAsk = "command substitution inside quotes"
	}
	return end, true
}

// opens notes an unquoted substitution opening in the current segment,
// glued to the word text before it when any: one in the program word runs
// its output, which no segment names; one in an assignment is its value.
func (sc *scanner) opens(n *Normalized, glued bool, word string) {
	if _, assign := assignmentName(word); glued && assign {
		return
	}
	from := len(sc.toks)
	for from > 0 && sc.toks[from-1].kind != tokSep {
		from--
	}
	// the words since the separator, the substitution last as ""
	var f []string
	for i := from; i < len(sc.toks); i++ {
		switch t := sc.toks[i]; {
		case t.kind == tokRedirect && i == len(sc.toks)-1:
			return // the substitution names the redirect's target
		case t.kind == tokRedirect:
			i++
		case t.kind == tokWord:
			f = append(f, t.text)
		}
	}
	f = ProgramWords(append(f, ""))
	for len(f) > 1 {
		if _, assign := assignmentName(f[0]); !assign {
			break
		}
		f = f[1:]
	}
	// a runner (xargs, sudo, timeout 5) leaves the program word open
	if f = stepRunners(f); len(f) == 0 || f[0] == "" { // function $(x) leaves nothing
		n.hidden = true
	}
}

// closes notes an unquoted substitution closing at rs[i]: words after it
// before a separator belong to the command around it, which the scanner
// splits into a segment of their own.
func (sc *scanner) closes(n *Normalized, rs []rune, i int) {
	j := i + 1
	for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
		j++
	}
	if j < len(rs) && !strings.ContainsRune(";&|)\n<>", rs[j]) && (j == i+1 || rs[j] != '#') {
		n.hidden = true
	}
}

// delimiter reads the word after the << ending at rs[*i], leaving *i on its
// last rune: quotes and backslashes go, and any of them makes the body
// literal.
func (sc *scanner) delimiter(rs []rune, i *int) heredoc {
	var d heredoc
	var b strings.Builder
	j := *i + 1
	for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
		j++
	}
	for ; j < len(rs) && !strings.ContainsRune(" \t\n;&|<>()", rs[j]); j++ {
		switch q := rs[j]; q {
		case '\'', '"':
			d.quoted = true
			for j++; j < len(rs) && rs[j] != q; j++ {
				b.WriteRune(rs[j])
			}
			sc.failed = sc.failed || j >= len(rs)
		case '\\':
			d.quoted = true
			if j+1 < len(rs) {
				j++
				b.WriteRune(rs[j])
			}
		default:
			b.WriteRune(q)
		}
	}
	d.delim = b.String()
	*i = j - 1
	return d
}

// bodies takes the bodies of the heredocs opened on the line whose newline
// is rs[i], each up to its delimiter line or the end, and returns the index
// of the last rune taken.
func (sc *scanner) bodies(rs []rune, i int) int {
	for _, d := range sc.docs {
		from := i + 1
		body := rs[min(from, len(rs)):]
		i = len(rs) - 1
		for p := from; p < len(rs); {
			e := len(rs)
			if k := slices.Index(rs[p:], '\n'); k >= 0 {
				e = p + k
			}
			line := string(rs[p:e])
			if d.tabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == d.delim {
				body, i = rs[from:p], min(e, len(rs)-1)
				break
			}
			p = e + 1
		}
		sc.toks = append(sc.toks, token{kind: tokBody, text: string(body), quoted: d.quoted})
	}
	sc.docs = nil
	return i
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
// separators cut segments, heredocs force ask. A heredoc body is a script
// unless every program in the command is a dataReader; then only an
// unquoted one's substitutions run. Its segments go where the body is.
// The command is judged whole, so one other program (mkdir, bash x.sh) makes
// every body a script, but a body fails the call only when a shell may run
// it. Programs names what runs by the body's reader instead (shown), while no
// shell or interpreter in the command could run a data reader's body.
// NOTE: a data body with an open quote hides the lines after it from
// Destructive; the confined run is the limit there, as for any settled code.
func (n *Normalized) eat(sc *scanner) {
	var seg []string
	type doc struct {
		at int
		token
		reader string // the program whose << it is, "" when no word before it names one
	}
	var docs []doc
	var readers []string
	toks := sc.toks
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.kind {
		case tokSep:
			n.fold(seg)
			seg = nil
		case tokHeredoc:
			n.ForceAsk = "heredoc or herestring input"
			if t.op != "<<<" && t.text == "" && !t.quoted {
				n.Fail = "a redirect has no target"
			}
			if t.op != "<<<" {
				readers = append(readers, program(seg))
			}
		case tokBody:
			n.fold(seg)
			seg = nil
			d := doc{at: len(n.Args), token: t}
			if len(docs) < len(readers) {
				d.reader = readers[len(docs)]
			}
			docs = append(docs, d)
		case tokRedirect:
			if i+1 < len(toks) && toks[i+1].kind == tokWord {
				n.gate(t.op, toks[i+1].text)
				i++
			} else {
				n.Fail = "a redirect has no target"
			}
		case tokWord:
			seg = append(seg, t.text)
		}
	}
	n.fold(seg)
	if sc.failed {
		n.Fail = "a quote or parenthesis is not closed"
	}
	// a body is a script unless every program only reads it
	script := slices.ContainsFunc(n.Args, func(f []string) bool {
		f = ProgramWords(f)
		return len(f) > 0 && !slices.Contains(dataReaders, f[0])
	})
	// a data reader's body is data only while nothing in the command can run
	// it: cat <<EOF | sh, or cat > x.sh <<EOF then bash x.sh
	code := slices.ContainsFunc(n.Args, func(f []string) bool { return WrapperPrograms[program(f)] })
	foreign := slices.ContainsFunc(n.Args, func(f []string) bool {
		p := program(f)
		return p != "" && !slices.Contains(dataReaders, p) && !isShell(p)
	})
	if len(docs) > 0 {
		n.shown = slices.Clone(n.Args)
	}
	for _, d := range slices.Backward(docs) {
		var body Normalized
		switch {
		case script:
			body = Normalize(d.text)
		case !d.quoted:
			body = substitutions(d.text)
		}
		show := body
		data := slices.Contains(dataReaders, d.reader)
		switch {
		case isShell(d.reader):
		case data && code: // listed as the script it may be, unless another language reads it
			show.hidden = foreign
		case data && d.quoted:
			show = Normalized{}
		case data:
			show = substitutions(d.text)
		// a body an interpreter reads is its code, which the reader's own
		// runs-code mark covers; only an unquoted one's substitutions run first
		case WrapperPrograms[d.reader] && d.quoted:
			show = Normalized{}
		case WrapperPrograms[d.reader]:
			show = substitutions(d.text)
		default:
			show.hidden = true
		}
		body.hidden = show.hidden // Programs lists what is shown
		// an apostrophe in prose or another language's code is no shell error
		if !isShell(d.reader) && !slices.Contains([]string{"", "source", "."}, d.reader) && !(data && code && !foreign) {
			body.Fail = ""
		}
		n.absorb(d.at, body)
		n.shown = slices.Insert(n.shown, d.at, show.Args...)
	}
}

// program is the ProgramName a segment's words run, past reserved words and
// runners (sudo tee), "" when none is named yet.
func program(seg []string) string {
	if f := stepRunners(ProgramWords(seg)); len(f) > 0 {
		return ProgramName(f[0])
	}
	return ""
}

// dataReaders run none of their input, so a heredoc they read is data
// (cat > f, git commit -F -); anything else may run it (fish, make -f -,
// $SHELL, sudo sh).
var dataReaders = []string{"cat", "tee", "echo", "printf", "git", "gh", "patch"}

// substitutions parses the $( and backtick substitutions of text the shell
// expands but does not split, an unquoted heredoc body.
func substitutions(body string) Normalized {
	var n Normalized
	var sc scanner
	rs := []rune(body)
	for j := 0; j < len(rs); j++ {
		if rs[j] == '\\' {
			j++
		} else if end, ok := sc.quotedSub(&n, rs, j); ok {
			j = end
		}
	}
	sc.toks = sc.held
	n.eat(&sc)
	return n
}

// gate classifies one redirect with its target into fs.read/fs.write
// operations (device exemptions applied).
func (n *Normalized) gate(op, target string) {
	if target == "" {
		n.Fail = "a redirect has no target"
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
		if ok && envAssignmentGate(name) {
			n.ForceAsk = "environment assignment: " + name
			n.hidden = true
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
			n.absorb(len(n.Args), Normalize(body))
			n.Opaque = true
			return
		}
	}
	n.Segments = append(n.Segments, strings.Join(argv, " "))
	n.Args = append(n.Args, argv)
}

// absorb puts inner's segments at index at and takes its gates.
func (n *Normalized) absorb(at int, inner Normalized) {
	n.Segments = slices.Insert(n.Segments, at, inner.Segments...)
	n.Args = slices.Insert(n.Args, at, inner.Args...)
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
	n.doubt = cmp.Or(n.doubt, inner.doubt)
	n.hidden = n.hidden || inner.hidden
}

func isShell(word string) bool {
	switch ProgramName(word) {
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

func (s *Store) envKept(name string) bool {
	if s == nil || s.EnvKeep == nil {
		return false
	}
	for k := range strings.SplitSeq(s.EnvKeep(), ",") {
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return true
		}
	}
	return false
}

// ChildEnv is a child process environment: zemi's own minus secret-shaped
// names the user did not keep, plus extras that pass the secret and
// assignment gates.
func (s *Store) ChildEnv(extras map[string]any) ([]string, error) {
	var out []string
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && (!EnvIsSecret(name) || s.envKept(name)) {
			out = append(out, kv)
		}
	}
	for k, v := range extras {
		// a key carrying = would set the name before it (PATH=/tmp/x:)
		if !isName(k) || EnvIsSecret(k) || envAssignmentGate(k) {
			return nil, fmt.Errorf("env var %q is refused by the environment gate", k)
		}
		out = append(out, k+"="+fmt.Sprint(v))
	}
	return out, nil
}
