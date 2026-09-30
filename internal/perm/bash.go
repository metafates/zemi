package perm

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/syntax"
)

// Normalized is a command line as the rules judge it (DESIGN §9): the simple
// commands the shell may run, and the gates around them.
type Normalized struct {
	// Segments are the simple commands, each its words joined by a space,
	// matched against rules one by one.
	Segments []string
	// Args are the segments' words: as the program receives them where that
	// is static, quotes removed and an expansion kept as written.
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
	// Assign is a non-empty reason when the command reads a value the rules
	// never see, so an allow verdict downgrades to ask (CheckCommandFor): an
	// assignment prefix stripped off a segment (GIT_EXTERNAL_DIFF=./x git
	// diff runs ./x), or arithmetic naming a variable, whose value bash
	// evaluates as an expression, where a subscript's $(...) runs.
	Assign string
	// Fail is a non-empty reason when the call must fail outright: the
	// shell cannot parse it, or a redirect has no target.
	Fail string
	rows []row
	// expands: the shell substitutes something somewhere in the text: a
	// parameter, a command, arithmetic, a glob, a brace list or a tilde
	expands bool
	// tmpdir: TMPDIR is named other than as a plain $TMPDIR or ${TMPDIR}
	tmpdir bool
	// hidden marks a program no row names, so Programs lists none: an
	// assignment to a gated name picks it, arithmetic evaluates a name's value
	// as code, a heredoc goes to a reader zemi does not know
	hidden bool
	// doubt says why Destructive cannot vouch for the rows either: a shell
	// may read the text otherwise than the parse (doubtful)
	doubt string
	// bash: Fail holds syntax bash runs, whose commands no row shows
	bash bool
	// shown are the rows Programs lists when a heredoc makes them differ
	// from the rows: a dataReader's body runs only its substitutions while
	// nothing in the command runs code, though the rules judge it as commands.
	shown []row
}

// row is one simple command.
type row struct {
	args []string
	// loop: the shell may run it more than once, in a loop's body or a
	// while's condition
	loop bool
	pos  uint // where it starts in the text, for heredoc bodies
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
// inlining never drops their assignment.
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

// Normalize parses a command line into the simple commands a POSIX sh runs.
// It is read once as POSIX sh whichever sh runs it, so a parse error, and
// text some sh reads otherwise ($'..', [[, {a,b}), fails the call naming
// the POSIX form. It never errors: problems surface in Fail/ForceAsk.
func Normalize(cmd string) Normalized { return normalize(cmd, false, map[string]Normalized{}) }

// shellText is Normalize for text a shell is handed to run (sh -c, eval),
// which may be bash 3.2 where sh is.
func shellText(text string) Normalized { return normalize(text, true, map[string]Normalized{}) }

// addRows appends each row of src that dst does not hold yet.
func addRows(dst, src []row) []row {
	for _, r := range src {
		if !slices.ContainsFunc(dst, r.same) {
			dst = append(dst, r)
		}
	}
	return dst
}

func (r row) same(s row) bool { return slices.Equal(r.args, s.args) }

// Rows are the command's segments, one Normalized each, for what judges a
// segment on its own (an always answer).
func (n Normalized) Rows() []Normalized {
	out := make([]Normalized, len(n.rows))
	for i, r := range n.rows {
		out[i] = Normalized{Segments: n.Segments[i : i+1], Args: n.Args[i : i+1], rows: []row{r}}
	}
	return out
}

// normalize is Normalize with memo holding the bodies (heredoc, sh -c) this
// top-level call already read, since a reassignment walks the text twice and
// a failed body is read again line by line. Text that parses up to an error
// keeps the statements before it, which a shell runs before it stops there
// (a heredoc body a data reader takes keeps them). shell marks text another
// shell runs.
func normalize(cmd string, shell bool, memo map[string]Normalized) (n Normalized) {
	if strings.ContainsRune(cmd, 0) { // mvdan.cc/sh drops it, exec refuses it
		return Normalized{Fail: "the command holds a NUL byte"}
	}
	defer func() {
		if r := recover(); r != nil {
			n = Normalized{Fail: fmt.Sprint(panicked, r)}
		}
	}()
	stmts, err := parse(cmd)
	w := walker{src: cmd, shell: shell, memo: memo}
	w.inline(stmts)
	w.stmts(stmts)
	// a name that reads other than as $NAME may be reassigned, and a builtin
	// that sets names by a value zemi cannot see may reassign any
	if len(w.vars) > 0 && (w.reassigns(stmts) || slices.ContainsFunc(w.n.rows, func(r row) bool { return ShellState[r.args[0]] })) {
		w = walker{src: cmd, shell: shell, memo: memo}
		w.stmts(stmts)
	}
	n = w.finish(stmts)
	if err != nil {
		var bash bool
		n.Fail, bash = bashism(err, cmd)
		n.bash = n.bash || bash
	}
	// mvdan.cc/sh reads a carriage return as a blank, sh as a character
	if strings.ContainsRune(cmd, '\r') {
		n.doubtful("a carriage return, which sh reads as part of a word")
	}
	for _, r := range n.rows {
		n.Segments = append(n.Segments, strings.Join(r.args, " "))
		n.Args = append(n.Args, r.args)
	}
	return n
}

// posixForms are the POSIX forms of the bash-only syntax models write most,
// by the parser's name for it, or the word.
var posixForms = map[string]string{
	"arrays": `use set -- a b and "$@", or a loop over words`, "`+=` and `a[i]=` assignments": `use x="${x}more", and set -- a b for a list`,
	"`&>` redirects": "use >file 2>&1", "`&>>` redirects": "use >>file 2>&1", "|&": "use 2>&1 |",
	"<( )":  `write each side to a file (cmd >"$TMPDIR/a"), or pipe one in as - (cmd | diff - b)`,
	"$'..'": `use printf for escapes: printf 'a\tb'`, `$".."`: `write a plain ".."`,
	"[[": "use [ ] (test)", "source": "use . file", "let": "use x=$((x+1))", "arithmetic commands": `use $(( )) in a word or [ "$x" -gt 1 ], and ( ( for nested subshells`,
	"{a,b}": "write each word out: a/b a/c, or $(seq 1 3)", "herestrings": `use printf '%s\n' x | cmd`,
	"c-style fors": "use for i in $(seq 1 3)", "slicing": "use cut or ${x#p} and ${x%s}", "search and replace": "use sed, or ${x#p} and ${x%s}",
}

// bashism says why a POSIX sh will not run text whose parse failed, and
// whether bash would.
func bashism(err error, cmd string) (string, bool) {
	if e, ok := errors.AsType[syntax.LangError](err); ok {
		return bashOnly(e.Feature), true
	}
	e, ok := errors.AsType[syntax.ParseError](err)
	if !ok {
		return "the command does not parse: " + err.Error(), false
	}
	switch at := cmd[e.Pos.Offset():]; {
	case strings.HasPrefix(at, "<(") || strings.HasPrefix(at, ">("):
		return bashOnly("<( )"), true
	case strings.HasPrefix(at, "|&"):
		return bashOnly("|&"), true
	}
	return fmt.Sprintf("the command does not parse: line %d col %d: %s", e.Pos.Line(), e.Pos.Col(), e.Text), false
}

// bashOnly is the Fail reason for the bash-only syntax what.
func bashOnly(what string) string {
	return fmt.Sprintf("bash-only syntax (%s): the shell is POSIX sh, so %s", what, cmp.Or(posixForms[what], "write it in POSIX sh"))
}

// refuse fails text bash reads otherwise than the parse.
func (w *walker) refuse(why string) {
	w.n.Fail = cmp.Or(w.n.Fail, why)
	w.n.bash = true
}

const panicked = "zemi could not parse the command: "

// parse parses cmd as POSIX sh, returning the statements before the first
// error. mvdan.cc/sh reads a backslash ending a comment as a line
// continuation, which sh does not, so that backslash is blanked out in
// place and every offset still points into cmd.
func parse(cmd string) ([]*syntax.Stmt, error) {
	p := parsers.Get().(*syntax.Parser)
	defer parsers.Put(p)
	for {
		f, err := p.Parse(strings.NewReader(cmd), "")
		fixed := cmd
		for _, c := range comments(f, cmd) {
			if strings.HasSuffix(c.Text, "\\\n") {
				at := int(c.Hash.Offset()) + len(c.Text) - 1
				fixed = fixed[:at] + " " + fixed[at+1:]
			}
		}
		if fixed != cmd {
			cmd = fixed
			continue
		}
		if err == nil {
			return f.Stmts, nil
		}
		// NOTE: StmtsSeq yields again after a break once it failed, which
		// panics, so the loop runs out instead
		var stmts []*syntax.Stmt
		failed := false
		for s, e := range p.StmtsSeq(strings.NewReader(cmd)) {
			if failed = failed || e != nil; !failed {
				stmts = append(stmts, s)
			}
		}
		return stmts, err
	}
}

// parsers keeps idle parsers: one is large, and a shell call parses a few
// times.
var parsers = sync.Pool{New: func() any { return syntax.NewParser(syntax.Variant(syntax.LangPOSIX), syntax.KeepComments(true)) }}

// comments are f's comments where a backslash may end one; none past a node
// a failed parse left half built.
func comments(f *syntax.File, src string) (cs []*syntax.Comment) {
	if !strings.Contains(src, "\\\n") {
		return nil
	}
	defer func() { _ = recover() }()
	syntax.Walk(f, func(x syntax.Node) bool {
		if c, ok := x.(*syntax.Comment); ok {
			cs = append(cs, c)
		}
		return true
	})
	return cs
}

// walker builds a Normalized from the statements.
type walker struct {
	src   string
	shell bool // another shell runs the text
	n     Normalized
	loop  int // how many loops enclose the walk
	docs  []heredoc
	vars  map[string]string // the inlined assignments
	skip  map[*syntax.Stmt]bool
	memo  map[string]Normalized
}

// inner is shellText(text) for a body inside the command (sh -c, a heredoc),
// read once per top-level call.
func (w *walker) inner(text string) Normalized {
	n, ok := w.memo[text]
	if !ok {
		n = normalize(text, true, w.memo)
		w.memo[text] = n
	}
	// the caller edits its rows and appends to its paths
	n.rows, n.shown = slices.Clone(n.rows), slices.Clone(n.shown)
	n.ReadPaths, n.WritePaths = slices.Clip(n.ReadPaths), slices.Clip(n.WritePaths)
	return n
}

// heredoc is a << body found in the walk.
type heredoc struct {
	pos    uint
	text   string // as written
	quoted bool
	loop   bool
	// reader are the words of the command reading it, nil when no simple
	// command does
	reader []string
	subs   Normalized // an unquoted body's substitutions
}

func (w *walker) stmts(ss []*syntax.Stmt) {
	for _, s := range ss {
		w.stmt(s)
	}
}

func (w *walker) stmt(s *syntax.Stmt) {
	if s == nil || w.skip[s] {
		return
	}
	reader := w.command(s.Cmd)
	for _, r := range s.Redirs {
		w.redirect(r, reader)
	}
}

// command walks c and returns the words of the simple command it is, nil
// for any other.
func (w *walker) command(c syntax.Command) []string {
	switch c := c.(type) {
	case nil:
	case *syntax.CallExpr:
		return w.call(c)
	case *syntax.IfClause:
		w.stmts(c.Cond)
		w.stmts(c.Then)
		if c.Else != nil {
			w.command(c.Else)
		}
	case *syntax.WhileClause:
		w.loop++
		w.stmts(c.Cond)
		w.stmts(c.Do)
		w.loop--
	case *syntax.ForClause:
		it := c.Loop.(*syntax.WordIter) // POSIX has no other
		w.gate(it.Name.Value)
		for _, x := range it.Items {
			w.braces(x)
			w.word(x)
		}
		w.loop++
		w.stmts(c.Do)
		w.loop--
	case *syntax.CaseClause:
		w.word(c.Word)
		for _, it := range c.Items {
			for _, p := range it.Patterns {
				w.word(p)
			}
			w.stmts(it.Stmts)
		}
	case *syntax.Block:
		w.stmts(c.Stmts)
	case *syntax.Subshell:
		// bash reads ((x)) as arithmetic, POSIX sh as a subshell running x
		if strings.HasPrefix(w.src[c.Pos().Offset():], "((") {
			w.refuse(bashOnly("arithmetic commands"))
		}
		w.stmts(c.Stmts)
	case *syntax.BinaryCmd:
		w.stmt(c.X)
		w.stmt(c.Y)
	case *syntax.FuncDecl: // a body may run once per call
		w.loop++
		w.stmt(c.Body)
		w.loop--
	default: // anything newer
		w.subs(c)
	}
	return nil
}

// call walks a simple command.
func (w *walker) call(c *syntax.CallExpr) []string {
	at := len(w.n.rows)
	for _, a := range c.Assigns {
		w.gate(a.Name.Value)
		if a.Value != nil {
			w.word(a.Value)
		}
		w.n.Opaque = true
		w.n.Assign = cmp.Or(w.n.Assign, "environment assignment: "+a.Name.Value)
	}
	// bash assigns x+=1 and a[0]=1, and runs the words after as the command
	if len(c.Args) > 0 && bashAssign.MatchString(w.text(c.Args[0])) {
		w.refuse(bashOnly("`+=` and `a[i]=` assignments"))
	}
	var args []string
	for _, x := range c.Args {
		w.braces(x)
		args = append(args, w.word(x))
	}
	return w.fold(args, c.Pos(), at)
}

var bashAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[.*\]\+?|\+)=`)

// braces fails a word bash would brace-expand, which dash runs as written:
// mkdir a/{b,c} would make one directory named a/{b,c}.
func (w *walker) braces(x *syntax.Word) {
	c := *x
	if syntax.SplitBraces(&c) && slices.ContainsFunc(c.Parts, func(p syntax.WordPart) bool {
		_, ok := p.(*syntax.BraceExp)
		return ok
	}) {
		w.refuse(bashOnly("{a,b}"))
	}
}

// gate forces an ask for an assignment to an environment-affecting name.
func (w *walker) gate(name string) {
	if envAssignmentGate(name) {
		w.n.ForceAsk = "environment assignment: " + name
		w.n.hidden = true
	}
}

// fold adds a simple command's row at index at, before the rows of its
// substitutions: the assignment gate over its words, and a leading shell's -c
// text as the rows it runs.
func (w *walker) fold(args []string, pos syntax.Pos, at int) []string {
	if len(args) == 0 {
		return nil
	}
	// these builtins assign the variables they name (read PATH, printf -v PATH)
	assigns := slices.Contains([]string{"read", "mapfile", "readarray", "getopts", "printf"}, args[0])
	for _, a := range args {
		name, ok := assignmentName(a)
		if assigns && !ok {
			name, ok = assignmentName(strings.TrimPrefix(a, "-v") + "=")
		}
		if ok {
			w.gate(name)
		}
	}
	// sh expands an alias in the lines after it, so later rows may name
	// another program than the one that runs
	if program(args) == "alias" {
		w.n.doubtful("an alias, which sh expands in later lines")
	}
	// bash reads these as a test keyword, a script to run and arithmetic,
	// dash as programs it lacks
	for _, p := range []string{args[0], program(args)} {
		if p == "[[" || p == "source" || p == "let" {
			w.refuse(bashOnly(p))
		}
	}
	if isShell(args[0]) {
		if body, ok := shellBody(args[1:]); ok {
			inner := w.inner(body)
			if inner.Fail != "" {
				inner.Fail = fmt.Sprintf("in the %s -c text: %s", ProgramName(args[0]), inner.Fail)
			}
			for i := range inner.rows {
				inner.rows[i].loop = inner.rows[i].loop || w.loop > 0
				inner.rows[i].pos = pos.Offset()
			}
			w.n.rows = slices.Insert(w.n.rows, at, inner.rows...)
			w.n.take(inner)
			w.n.Opaque = true
			return args
		}
	}
	w.n.rows = slices.Insert(w.n.rows, at, row{args: args, loop: w.loop > 0, pos: pos.Offset()})
	return args
}

// redirect gates one redirect's target, or notes a heredoc for its reader.
func (w *walker) redirect(r *syntax.Redirect, reader []string) {
	if r.Op == syntax.WordHdoc {
		w.n.ForceAsk = cmp.Or(w.n.ForceAsk, "heredoc or herestring input")
		w.word(r.Word)
		return
	}
	if r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc {
		w.n.ForceAsk = cmp.Or(w.n.ForceAsk, "heredoc or herestring input")
		d := heredoc{quoted: quoted(r.Word), loop: w.loop > 0, reader: reader, pos: r.End().Offset()}
		if r.Hdoc != nil {
			d.pos = r.Hdoc.Pos().Offset()
			// End reaches past the delimiter line
			var b strings.Builder
			for _, p := range r.Hdoc.Parts {
				switch l, ok := p.(*syntax.Lit); {
				case ok && d.quoted:
					b.WriteString(l.Value)
				case ok:
					b.WriteString(unescape(l.Value, heredocEscapes))
				default:
					b.WriteString(w.text(p))
				}
			}
			d.text = b.String()
			if !d.quoted {
				sub := walker{src: w.src, shell: w.shell, loop: w.loop, memo: w.memo}
				sub.subs(r.Hdoc)
				d.subs = sub.finish(nil)
			}
		}
		w.docs = append(w.docs, d)
		return
	}
	target := w.word(r.Word)
	op := r.Op.String()
	switch {
	case target == "":
		w.n.Fail = "a redirect has no target"
	case devNull(target) || (op == ">&" || op == "<&") && (isAllDigits(target) || target == "-"):
	default:
		// < <> <& read; > >> >| &> &>> >& <> write
		if strings.HasPrefix(op, "<") {
			w.n.ReadPaths = append(w.n.ReadPaths, target)
		}
		if strings.Contains(op, ">") {
			w.n.WritePaths = append(w.n.WritePaths, target)
		}
	}
}

// quoted reports a heredoc delimiter with any quoting, which makes the body
// literal; mvdan.cc/sh looks only at its last part.
func quoted(delim *syntax.Word) bool {
	return slices.ContainsFunc(delim.Parts, func(p syntax.WordPart) bool {
		l, ok := p.(*syntax.Lit)
		return !ok || strings.Contains(l.Value, `\`)
	})
}

// word is x as the program receives it where that is static, quotes
// removed, with every expansion kept as written; the commands its
// substitutions run become rows.
func (w *walker) word(x *syntax.Word) string {
	var b strings.Builder
	for _, p := range x.Parts {
		w.part(&b, p, false)
	}
	return b.String()
}

// dollarQuoted fails $'..' and $"..", which bash decodes and an older dash
// reads as a $ and a quote.
func (w *walker) dollarQuoted(x *syntax.Word) {
	for i := 1; i < len(x.Parts); i++ {
		l, ok := x.Parts[i-1].(*syntax.Lit)
		if !ok {
			continue
		}
		v, found := strings.CutSuffix(l.Value, "$")
		if !found || oddBackslashes(v) {
			continue
		}
		switch x.Parts[i].(type) {
		case *syntax.SglQuoted:
			w.refuse(bashOnly("$'..'"))
		case *syntax.DblQuoted:
			w.refuse(bashOnly(`$".."`))
		}
	}
}

// oddBackslashes reports text ending in a backslash no other escapes.
func oddBackslashes(s string) bool { return (len(s)-len(strings.TrimRight(s, `\`)))%2 == 1 }

func (w *walker) part(b *strings.Builder, p syntax.WordPart, inDouble bool) {
	switch p := p.(type) {
	case *syntax.Lit:
		escapes := ""
		if inDouble {
			escapes = dquoteEscapes
		}
		// only a backslash ending the text escapes nothing: bash drops it,
		// dash keeps it
		if !inDouble && oddBackslashes(p.Value) {
			w.refuse("the command ends in a backslash, which sh and bash read otherwise")
		}
		b.WriteString(unescape(p.Value, escapes))
	case *syntax.SglQuoted:
		b.WriteString(p.Value)
	case *syntax.DblQuoted:
		for _, q := range p.Parts {
			w.part(b, q, true)
		}
	case *syntax.ParamExp:
		if v, ok := w.vars[paramName(p)]; ok {
			b.WriteString(v)
			return
		}
		b.WriteString(w.text(p))
		w.subs(p)
	default:
		b.WriteString(w.text(p))
		w.subs(p)
	}
}

// The characters a backslash escapes inside double quotes and in an
// unquoted heredoc body; before any other it stays.
const (
	dquoteEscapes  = "$`\"\\\n"
	heredocEscapes = "$`\\\n"
)

// unescape drops the backslashes the shell removes from literal text: those
// before a character in escapes, or before any when escapes is "" (outside
// quotes). A backslash and a newline both go.
func unescape(s, escapes string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (escapes == "" || strings.IndexByte(escapes, s[i+1]) >= 0) {
			if i++; s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// subs walks the substitutions inside node into rows.
func (w *walker) subs(node syntax.Node) {
	syntax.Walk(node, func(x syntax.Node) bool {
		if x, ok := x.(*syntax.CmdSubst); ok {
			w.stmts(x.Stmts)
			return false
		}
		return true
	})
}

// text is node as written.
func (w *walker) text(node syntax.Node) string {
	return w.src[node.Pos().Offset():node.End().Offset()]
}

// inline takes the leading plain assignments (D=/long/path; ls $D), whose
// values the words then read in place of $D and ${D}, so path gates, rules
// and ReadCommand see the path instead of an expansion. Only a value the
// shell would leave as is qualifies: no quotes, no expansion but an earlier
// inlined name or $TMPDIR, bare or in double quotes, no glob or brace. A
// set -e before them is stepped over. Heredoc delimiters stay as written: sh
// expands none.
func (w *walker) inline(stmts []*syntax.Stmt) {
	w.vars, w.skip = map[string]string{}, map[*syntax.Stmt]bool{}
	vars := w.vars
	leading(stmts, func(s *syntax.Stmt) bool {
		c, ok := s.Cmd.(*syntax.CallExpr)
		if ok && setsOptions(c) && len(s.Redirs) == 0 {
			return true
		}
		if !ok || len(c.Args) > 0 || len(c.Assigns) != 1 || len(s.Redirs) > 0 || s.Negated || s.Background {
			return false
		}
		a := c.Assigns[0]
		if a.Value == nil || envAssignmentGate(a.Name.Value) {
			return false
		}
		// an exported name (HOME, GOFLAGS) reaches every child process and a
		// shell variable (CDPATH) steers the shell even unexported
		if _, exported := os.LookupEnv(a.Name.Value); exported || slices.Contains(shellVarNames, strings.ToLower(a.Name.Value)) {
			return false
		}
		var v strings.Builder
		parts := a.Value.Parts
		// "$TMPDIR/x" is the same value quoted
		if q, ok := parts[0].(*syntax.DblQuoted); ok && len(parts) == 1 {
			parts = q.Parts
		}
		for _, p := range parts {
			switch p := p.(type) {
			case *syntax.Lit:
				if !plainValue.MatchString(p.Value) {
					return false
				}
				v.WriteString(p.Value)
			case *syntax.ParamExp:
				known, ok := vars[paramName(p)]
				// the pad stays a name, which padOperands reads
				if paramName(p) == "TMPDIR" {
					known, ok = "$TMPDIR", true
				}
				if !ok {
					return false
				}
				v.WriteString(known)
			default:
				return false
			}
		}
		vars[a.Name.Value], w.skip[s] = v.String(), true
		return true
	})
	// the last statement's assignment sets nothing any command reads
	if len(vars) > 0 && !slices.ContainsFunc(stmts, func(s *syntax.Stmt) bool { return !w.skipped(s) }) {
		s := last(stmts[len(stmts)-1])
		delete(vars, s.Cmd.(*syntax.CallExpr).Assigns[0].Name.Value)
		delete(w.skip, s)
	}
}

// setsOptions reports a set -e, -u or -x, which assigns nothing: set -a
// would export what follows, and set -- sets the arguments.
func setsOptions(c *syntax.CallExpr) bool {
	if len(c.Assigns) > 0 || len(c.Args) < 2 || c.Args[0].Lit() != "set" {
		return false
	}
	return !slices.ContainsFunc(c.Args[1:], func(w *syntax.Word) bool { return !setOption.MatchString(w.Lit()) })
}

var setOption = regexp.MustCompile(`^[-+][eux]+$`)

var plainValue = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=-]+$`)

// leading calls fn on the statements the shell runs first, in order and
// unconditionally (a list and the left of an && chain), until it says stop.
func leading(stmts []*syntax.Stmt, fn func(*syntax.Stmt) bool) bool {
	for _, s := range stmts {
		if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && b.Op == syntax.AndStmt && !s.Negated && !s.Background && len(s.Redirs) == 0 {
			if !leading([]*syntax.Stmt{b.X, b.Y}, fn) {
				return false
			}
			continue
		}
		if !fn(s) {
			return false
		}
	}
	return true
}

// skipped reports a statement inlining took whole.
func (w *walker) skipped(s *syntax.Stmt) bool {
	if b, ok := s.Cmd.(*syntax.BinaryCmd); ok {
		return w.skipped(b.X) && w.skipped(b.Y)
	}
	return w.skip[s]
}

// last is the statement an && chain ends in.
func last(s *syntax.Stmt) *syntax.Stmt {
	if b, ok := s.Cmd.(*syntax.BinaryCmd); ok {
		return last(b.Y)
	}
	return s
}

// paramName is the name a $NAME or ${NAME} reads, "" for any longer expansion.
func paramName(p *syntax.ParamExp) string {
	if p.Param == nil || p.Excl || p.Length || p.Width || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Names != 0 || p.Exp != nil {
		return ""
	}
	return p.Param.Value
}

// reassigns reports a word past the inlined statements naming an inlined
// variable other than as $NAME: D=, for D, read D, eval $X=/ with X=D.
func (w *walker) reassigns(stmts []*syntax.Stmt) bool {
	for name := range w.vars {
		re := regexp.MustCompile(`(^|[^$\w{])` + name + `\b`)
		found := slices.ContainsFunc(w.n.rows, func(r row) bool { return slices.ContainsFunc(r.args, re.MatchString) })
		for _, s := range stmts {
			syntax.Walk(s, func(x syntax.Node) bool {
				switch x := x.(type) {
				case *syntax.Stmt:
					return !w.skipped(x) && !found
				case *syntax.ParamExp:
					return paramName(x) != name // $D itself
				case *syntax.Lit:
					found = found || re.MatchString(x.Value)
				}
				return !found
			})
		}
		if found {
			return true
		}
	}
	return false
}

// finish takes the heredoc bodies and the text-wide flags.
// A heredoc body is a script unless every program in the command runs none
// (inert); then only an unquoted one's substitutions run. Its rows go where
// the body is. The command is judged whole, so one other program (mkdir,
// bash x.sh) makes every body a script, but a body fails the call only when
// a shell may run it. Programs names what runs by the body's reader instead
// (shown), while no shell or interpreter in the command could run a data
// reader's body.
// NOTE: a body that does not parse is judged whole and line by line, so a
// reader no known shell is ($SHELL, make) may still run a command spanning
// lines the parse does not show; the confined run is the limit there, as
// for any settled code.
func (w *walker) finish(stmts []*syntax.Stmt) Normalized {
	for _, s := range stmts {
		if !w.skipped(s) {
			w.scan(s)
		}
	}
	n := &w.n
	docs := w.docs
	// a body is a script unless every program only reads it
	script := slices.ContainsFunc(n.rows, func(r row) bool { return !inert(r.args) })
	// a data reader's body is data only while nothing in the command can run
	// it: cat <<EOF | sh, or cat > x.sh <<EOF then bash x.sh
	code := slices.ContainsFunc(n.rows, func(r row) bool { return WrapperPrograms[program(r.args)] })
	foreign := slices.ContainsFunc(n.rows, func(r row) bool {
		p := program(r.args)
		return p != "" && !inert(stepRunners(r.args)) && !isShell(p)
	})
	if len(docs) > 0 {
		n.shown = slices.Clone(n.rows)
	}
	for _, d := range slices.Backward(docs) {
		var body Normalized
		switch {
		case script:
			body = w.inner(d.text)
			if body.Fail == "" {
				break
			}
			body.Fail = fmt.Sprintf("in the heredoc body from line %d: %s", strings.Count(w.src[:d.pos], "\n")+1, body.Fail)
			// a reader may still run it line by line (make runs each recipe
			// line in a shell of its own), and sh expands an unquoted one first
			more := []Normalized{d.subs}
			for line := range strings.Lines(d.text) {
				more = append(more, w.inner(line))
			}
			for _, m := range more {
				body.rows = addRows(body.rows, m.rows)
				m.Fail = ""
				body.take(m)
			}
		case !d.quoted:
			body = d.subs
		}
		show := body
		reader := ""
		if len(d.reader) > 0 {
			reader = ProgramName(stepRunners(d.reader)[0])
		}
		data := dataReader(stepRunners(d.reader))
		switch {
		case isShell(reader):
		case data && code: // listed as the script it may be, unless another language reads it
			show.hidden = foreign
		case data && d.quoted:
			show = Normalized{}
		case data:
			show = d.subs
		// a body an interpreter reads is its code, which the reader's own
		// runs-code mark covers; only an unquoted one's substitutions run first
		case WrapperPrograms[reader] && d.quoted:
			show = Normalized{}
		case WrapperPrograms[reader]:
			show = d.subs
		default:
			show.hidden = true
		}
		body.hidden = show.hidden // Programs lists what is shown
		// an apostrophe in prose or another language's code is no shell error
		if !isShell(reader) && !slices.Contains([]string{"", "source", "."}, reader) && !(data && code && !foreign) {
			// but a line bash would run shows no row, and a reader other than
			// an interpreter may be a shell ($SHELL, make, busybox sh)
			if body.bash && (!WrapperPrograms[reader] || slices.ContainsFunc(d.reader, isShell)) {
				body.doubtful("bash-only syntax in a heredoc body a shell may run")
			}
			body.Fail, body.bash = "", false
		}
		at := func(rows []row) int {
			return len(slices.DeleteFunc(slices.Clone(rows), func(r row) bool { return r.pos >= d.pos }))
		}
		for _, r := range []*[]row{&body.rows, &show.rows} {
			for i := range *r {
				(*r)[i].pos, (*r)[i].loop = d.pos, (*r)[i].loop || d.loop
			}
		}
		n.rows = slices.Insert(n.rows, at(n.rows), body.rows...)
		n.shown = slices.Insert(n.shown, at(n.shown), show.rows...)
		body.rows = nil
		n.take(body)
	}
	return *n
}

// take merges an inner body's gates and flags into n; the rows are the
// caller's to place.
func (n *Normalized) take(inner Normalized) {
	n.ReadPaths = append(n.ReadPaths, inner.ReadPaths...)
	n.WritePaths = append(n.WritePaths, inner.WritePaths...)
	n.Fail = cmp.Or(n.Fail, inner.Fail)
	n.ForceAsk = cmp.Or(n.ForceAsk, inner.ForceAsk)
	n.Assign = cmp.Or(n.Assign, inner.Assign)
	n.doubt = cmp.Or(n.doubt, inner.doubt)
	n.Opaque = n.Opaque || inner.Opaque
	n.expands = n.expands || inner.expands
	n.tmpdir = n.tmpdir || inner.tmpdir
	n.hidden = n.hidden || inner.hidden
	n.bash = n.bash || inner.bash
}

// scan sets the text-wide flags: expands, tmpdir, and for arithmetic
// hidden, and Assign when it names a variable. It fails $'..' and $"..".
func (w *walker) scan(s *syntax.Stmt) {
	n := &w.n
	body, subst := false, 0
	var visit func(x syntax.Node) bool
	visit = func(x syntax.Node) bool {
		switch x := x.(type) {
		case *syntax.Redirect:
			if x.Op == syntax.Hdoc || x.Op == syntax.DashHdoc {
				w.dollarQuoted(x.Word)
				// bash 3.2, which runs sh -c text on macOS, ends a $( )
				// at a ) in a heredoc body inside it and runs the rest
				if w.shell && subst > 0 {
					n.doubtful("a heredoc inside $( ), which bash 3.2 (macOS sh) may end early")
				}
				if x.Hdoc != nil {
					body = true
					syntax.Walk(x.Hdoc, visit)
					body = false
				}
				return false
			}
		case *syntax.Word:
			w.dollarQuoted(x)
			n.expands = n.expands || !body && globs(x)
		case *syntax.ParamExp:
			if v, inlined := w.vars[paramName(x)]; inlined {
				// a value from $TMPDIR reads as $TMPDIR written in its place
				n.expands = n.expands || strings.Contains(v, "$")
				return false
			}
			n.expands = true
			if x.Index != nil {
				w.arith(x)
			}
			if paramName(x) == "TMPDIR" {
				return false
			}
		case *syntax.CmdSubst:
			n.expands = true
			if !x.Backquotes {
				subst++
			}
			for _, s := range x.Stmts {
				syntax.Walk(s, visit)
			}
			if !x.Backquotes {
				subst--
			}
			return false
		case *syntax.ArithmExp:
			n.expands = true
			w.arith(x)
		case *syntax.SglQuoted:
			n.tmpdir = n.tmpdir || strings.Contains(x.Value, "TMPDIR")
		case *syntax.Lit:
			n.tmpdir = n.tmpdir || strings.Contains(x.Value, "TMPDIR")
		}
		return true
	}
	syntax.Walk(s, visit)
}

// doubtful forces an ask for text a shell may read otherwise than the
// parse, which Destructive and Programs cannot vouch for either.
func (n *Normalized) doubtful(why string) {
	n.ForceAsk = cmp.Or(n.ForceAsk, why)
	n.doubt = cmp.Or(n.doubt, why)
	n.hidden = true
}

// arith notes arithmetic: Programs cannot vouch for it, and one naming a
// variable reads a value the rules never see.
func (w *walker) arith(node syntax.Node) {
	w.n.hidden = true
	var read func(x syntax.Node) bool
	read = func(x syntax.Node) bool {
		switch x := x.(type) {
		case *syntax.BinaryArithm:
			if x.Op == syntax.Assgn { // n=1 reads no n
				syntax.Walk(x.Y, read)
				return false
			}
		case *syntax.ParamExp:
			w.n.Assign = cmp.Or(w.n.Assign, "arithmetic evaluates a variable's value")
		case *syntax.Lit:
			if isName(x.Value) {
				w.n.Assign = cmp.Or(w.n.Assign, "arithmetic evaluates a variable's value")
			}
		}
		return w.n.Assign == ""
	}
	syntax.Walk(node, read)
}

// globs reports a word the shell may expand past its quotes: a glob, a
// bracket class or brace list closed within the word (.e["n "]v and
// {"x y",.env} included), or a tilde.
func globs(x *syntax.Word) bool {
	// the word with every quoted or escaped character as q
	var b strings.Builder
	for _, p := range x.Parts {
		l, ok := p.(*syntax.Lit)
		if !ok {
			b.WriteByte('q')
			continue
		}
		for i := 0; i < len(l.Value); i++ {
			if l.Value[i] == '\\' {
				i++
				b.WriteByte('q')
			} else {
				b.WriteByte(l.Value[i])
			}
		}
	}
	s := b.String()
	if strings.ContainsAny(s, "*?") || strings.HasPrefix(s, "~") || strings.Contains(s, "=~") || strings.Contains(s, ":~") {
		return true
	}
	if i := strings.IndexByte(s, '['); i >= 0 && strings.Contains(s[i:], "]") {
		return true
	}
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if end := strings.IndexByte(s[i:], '}'); end >= 0 && (strings.Contains(s[i:i+end], ",") || strings.Contains(s[i:i+end], "..")) {
			return true
		}
	}
	return false
}

// program is the ProgramName a row runs, past runners (sudo tee), "" when
// none is named yet.
func program(args []string) string {
	if f := stepRunners(args); len(f) > 0 {
		return ProgramName(f[0])
	}
	return ""
}

// dataReader reports that the program f starts with runs none of its input,
// so a heredoc it reads is data (cat > f); anything else may run it (fish,
// make -f -, $SHELL, sudo sh). git counts only as a subcommand that reads a
// message or a patch, named first: before it -c can define an alias that
// runs a shell.
func dataReader(f []string) bool {
	if len(f) == 0 {
		return false
	}
	if p := ProgramName(f[0]); p != "git" {
		return slices.Contains([]string{"cat", "tee", "echo", "printf", "gh", "patch"}, p)
	}
	return len(f) > 1 && slices.Contains([]string{"commit", "tag", "notes", "apply", "am", "hash-object", "mktree", "mktag"}, f[1])
}

// inert reports that the program f starts with runs no heredoc in the
// command: a dataReader, or a git builtin no alias can shadow that runs no
// program its words name (git add, unlike push --receive-pack or rebase -x).
func inert(f []string) bool {
	plain := []string{"add", "status", "diff", "log", "show", "branch", "switch", "checkout", "restore", "reset", "rm", "mv", "stash", "init", "rev-parse", "ls-files"}
	return dataReader(f) || len(f) > 1 && ProgramName(f[0]) == "git" && slices.Contains(plain, f[1])
}

func isAllDigits(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
}

func devNull(p string) bool {
	return p == "/dev/null" || p == "/dev/zero"
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
