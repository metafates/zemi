package perm

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// SecretPathShape matches secret-shaped filesystem paths (§9); reads ask.
var SecretPathShape = []string{".env", ".env.*", ".envrc", "*.pem", "*.key", "id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
	"*credential*", "*secret*", ".netrc"}

// PathIsSecret folds on every OS: a shape that asks one file too many is
// cheaper than a darwin spelling that slips past.
func PathIsSecret(p string) bool {
	base := fold(filepath.Base(p))
	return slices.ContainsFunc(SecretPathShape, func(pat string) bool {
		ok, _ := filepath.Match(pat, base)
		// an ssh public key is meant to be shared
		return ok && !(strings.HasPrefix(pat, "id_") && strings.HasSuffix(base, ".pub"))
	})
}

// ArgUse is how a settled command uses an argument, for the path check.
type ArgUse int

const (
	// ArgRead is read at most, like cat reads a file.
	ArgRead ArgUse = iota
	// ArgRun is the directory cd enters.
	ArgRun
	// ArgWalk is searched recursively when it is a directory, and what
	// matches is printed.
	ArgWalk
	// ArgSearch is ArgWalk minus hidden and gitignored entries below the
	// argument, which rg skips unless told otherwise.
	ArgSearch
	// ArgText is a search pattern, never a path.
	ArgText
)

// ReadCommand reports whether a bash command only reads: every segment runs
// an allowlisted reader named without a path (no build or test tool, which
// runs the project's code, and no git, which runs programs the repository's
// config names), nothing is written through a redirect, the shell expands
// nothing, and every argument passes pathOK, so a reader cannot print secrets
// or files outside the project. A nil pathOK refuses absolute, home and
// parent paths. These settle where no sandbox would hold the command.
func ReadCommand(cmd string, pathOK func(arg string, use ArgUse) bool) bool {
	cmd = inlineVars(cmd)
	if expands(cmd) {
		return false
	}
	if pathOK == nil {
		pathOK = relativeArg
	}
	n := Normalize(cmd)
	// an assignment can steer a reader past its checked flags
	// (RIPGREP_CONFIG_PATH), and sh -c passes arguments nothing here sees
	if n.Fail != "" || n.ForceAsk != "" || n.Opaque || len(n.WritePaths) > 0 || len(n.Segments) == 0 {
		return false
	}
	// a subshell or pipeline may drop a cd, so arguments are judged from
	// every directory the shell may be in
	dirs := []string{"."}
	check := func(a string, use ArgUse) bool {
		for _, d := range dirs {
			if use != ArgText && !pathOK(inDir(d, a), use) {
				return false
			}
		}
		return true
	}
	for _, f := range n.Args {
		if strings.Contains(f[0], "/") || !safeSegment(f) {
			return false
		}
		use, walkCwd := argUses(f)
		positional := 0
		if !safeArgs(f[1:], func(pos int, a string) bool {
			positional = max(positional, pos+1)
			return check(a, use(pos))
		}) || (walkCwd && positional < 2 && !check(".", use(1))) {
			return false
		}
		if f[0] == "cd" {
			for _, d := range dirs {
				dirs = append(dirs, inDir(d, f[1]))
			}
		}
	}
	return safeArgs(n.ReadPaths, func(_ int, a string) bool { return check(a, ArgRead) })
}

func relativeArg(a string, _ ArgUse) bool {
	return !strings.HasPrefix(a, "/") && !strings.HasPrefix(a, "~") && a != ".." && !strings.HasPrefix(a, "../") && !strings.Contains(a, "/../")
}

func inDir(dir, a string) string {
	if dir == "." || filepath.IsAbs(a) || strings.HasPrefix(a, "~") {
		return a
	}
	// not Join: in cd L && cat ../x the .. resolves from L's target
	return dir + string(filepath.Separator) + a
}

// argUses says how a checked segment's program uses its arguments by
// position among the operands, and whether it searches the working
// directory when it has no operand after the pattern.
func argUses(f []string) (use func(pos int) ArgUse, walkCwd bool) {
	name, rest := f[0], f[1:]
	// a pattern glued to -e or -f (-eKEY, --regexp=KEY) is not an operand,
	// so the first operand is already searched; -ie bundles only refuse more
	first := 1
	if name != "ag" && slices.ContainsFunc(rest, func(a string) bool {
		if strings.HasPrefix(a, "--") {
			return strings.Contains(a, "=") && hasFlag([]string{a}, "--regexp", "--file")
		}
		i := strings.IndexAny(a, "ef")
		return strings.HasPrefix(a, "-") && i > 0 && i < len(a)-1
	}) {
		first = 0
	}
	search := slices.Contains([]string{"grep", "egrep", "fgrep", "rg", "ag"}, name)
	// the first operand is the pattern ("//|\* " is no path) unless -f may
	// have taken it as a pattern file
	if search && first == 1 && !hasFlag(rest, "-f", "--file", "--files") {
		defer func() {
			files := use
			use = func(pos int) ArgUse {
				if pos == 0 {
					return ArgText
				}
				return files(pos)
			}
		}()
	}
	switch {
	// globs, and a whitelist in an --ignore-file, override rg's ignore
	// rules, so they walk everything too
	case name == "rg" && !hasFlag(rest, "-u", "-.", "--unrestricted", "--hidden", "--no-ignore", "--no-ignore-dot",
		"--no-ignore-exclude", "--no-ignore-files", "--no-ignore-global", "--no-ignore-parent", "--no-ignore-vcs",
		"-g", "--glob", "--iglob", "--ignore-file"):
		return walkFrom(first, ArgSearch), true
	case name == "rg", name == "ag", (name == "grep" || name == "egrep" || name == "fgrep") &&
		hasFlag(rest, "-r", "-R", "--recursive", "--dereference-recursive", "-d", "--directories"):
		return walkFrom(first, ArgWalk), true
	// gofmt descends into a directory operand and prints what it parses;
	// tree, du, find, fd and ls -R walk too but print names, never content
	case name == "diff", name == "gofmt":
		return walkFrom(0, ArgWalk), false
	case name == "cd":
		return func(int) ArgUse { return ArgRun }, false
	}
	return walkFrom(-1, ArgRead), false
}

// walkFrom gives the operands from position first on walk; flag values
// and earlier operands are read.
func walkFrom(first int, walk ArgUse) func(pos int) ArgUse {
	return func(pos int) ArgUse {
		if first < 0 || pos < first {
			return ArgRead
		}
		return walk
	}
}

// expands reports whether the shell would substitute anything: a backtick,
// $ or process substitution outside single quotes, or a glob or brace list
// outside any quotes (cat .e* reads .env past the path check).
func expands(cmd string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case inSingle:
			inSingle = c != '\''
		case c == '\\':
			i++
		case c == '`' || c == '$':
			return true
		case c == '"':
			inDouble = !inDouble
		case inDouble:
		case c == '\'':
			inSingle = true
		case (c == '<' || c == '>') && i+1 < len(cmd) && cmd[i+1] == '(':
			return true
		case c == '*' || c == '?':
			return true
		case c == '[' || c == '{':
			// a bracket class or {a,b} / {1..3} list closed within the word
			word := cmd[i+1 : wordEnd(cmd, i+1)]
			if c == '[' && strings.Contains(word, "]") {
				return true
			}
			if end := strings.IndexByte(word, '}'); c == '{' && end >= 0 && (strings.Contains(word[:end], ",") || strings.Contains(word[:end], "..")) {
				return true
			}
		}
	}
	return false
}

// wordEnd returns where the shell word running through cmd[i:] ends: at the
// first blank or operator that is not quoted or escaped (.e["n "]v is one word).
func wordEnd(cmd string, i int) int {
	var quote byte
	for ; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				i++
			}
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			quote = c
		case strings.IndexByte(" \t\n;&|<>()", c) >= 0:
			return i
		}
	}
	return len(cmd)
}

// safeArgs checks every argument that could name a file: plain arguments,
// everything after --, the value of --flag=value, and a value glued to a
// short flag (-f/etc/passwd). pathOK gets the operand's position, or -1 for
// a flag value.
func safeArgs(args []string, pathOK func(pos int, a string) bool) bool {
	ok := func(pos int, a string) bool { return a == "" || pathOK(pos, a) }
	flags, pos := true, 0
	for _, a := range args {
		if flags && strings.HasPrefix(a, "-") {
			if a == "--" {
				flags = false
				continue
			}
			// one glued character is a separator (cut -d/), not a path
			if !strings.HasPrefix(a, "--") && len(a) > 3 && !ok(-1, a[2:]) {
				return false
			}
			if _, v, found := strings.Cut(a, "="); found && !ok(-1, v) {
				return false
			}
			continue
		}
		if !ok(pos, a) {
			return false
		}
		pos++
	}
	return true
}

var readOnlyPrograms = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "cut": true,
	"tr": true, "pwd": true, "which": true, "whoami": true, "date": true, "echo": true,
	"printf": true, "true": true, "false": true, "test": true, "[": true, "file": true,
	"stat": true, "du": true, "df": true, "tree": true, "basename": true, "dirname": true,
	"realpath": true, "readlink": true, "jq": true, "diff": true, "cmp": true, "nl": true,
	"od": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "fd": true,
	"cd": true, "uname": true, "id": true, "hostname": true, "column": true, "comm": true,
	"paste": true, "fold": true, "sha256sum": true, "shasum": true, "md5sum": true, "type": true,
	"sleep": true, "seq": true, "sort": true, "gofmt": true, "find": true,
	"sed": true, "awk": true, "uniq": true,
}

// refusedFlags make an otherwise read-only program write a file, assign a
// variable, run a program it names, load code from a file or read paths
// listed in a file the path check never sees.
var refusedFlags = map[string][]string{
	"sort":   {"-o", "--output", "--compress-program", "--files0-from"},
	"printf": {"-v"},
	"tree":   {"-o", "-R"},
	"file":   {"-C", "--compile", "-f", "--files-from"},
	"du":     {"--files0-from"},
	"wc":     {"--files0-from"},
	"diff":   {"--from-file", "--to-file"},
	"fd":     {"-x", "-X", "--exec", "--exec-batch"},
	"rg":     {"--pre", "--hostname-bin"},
	"ag":     {"--pager"},
	"find":   {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls", "-files0-from"},
	"sed":    {"-i", "-I", "--in-place", "-f", "--file"},
	"awk":    {"-f", "--file", "-i", "--include", "-l", "--load", "-E", "--exec", "-W", "-o", "--pretty-print", "-p", "--profile", "-d", "--dump-variables", "-D", "--debug"},
	"jq":     {"-f", "--from-file", "-L", "--library-path"},
	"gofmt":  {"-w", "--w", "-cpuprofile", "--cpuprofile"},
}

// sedSafe reports whether every sed script (the -e values, or else the
// first operand) parses into commands that write, read and run nothing.
func sedSafe(args []string) bool {
	var scripts, operands []string
	next := func(i *int) bool {
		if *i++; *i < len(args) {
			scripts = append(scripts, args[*i])
		}
		return *i < len(args)
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			operands = append(operands, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--") && hasFlag([]string{a}, "--expression"):
			if _, v, ok := strings.Cut(a, "="); ok {
				scripts = append(scripts, v)
			} else if !next(&i) {
				return false
			}
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && a != "-":
			// -ne p and -nep: the value-taking short flags besides -e are refused
			if _, v, ok := strings.Cut(a, "e"); ok && v != "" {
				scripts = append(scripts, v)
			} else if ok && !next(&i) {
				return false
			}
		default:
			operands = append(operands, a)
		}
	}
	if len(scripts) == 0 && len(operands) > 0 {
		scripts = operands[:1]
	}
	for _, s := range scripts {
		if !sedScriptSafe(s, false) || !sedScriptSafe(s, true) {
			return false
		}
	}
	return len(scripts) > 0
}

// sedScriptSafe parses one sed script. brackets says whether a [...] class
// in a regex hides the delimiter (BSD sed) or not (GNU sed). Anything it
// cannot parse is unsafe: sed compiles the whole script before it runs, so
// only a script sed accepts matters.
func sedScriptSafe(s string, brackets bool) bool {
	i := 0
	space := func(set string) {
		for i < len(s) && strings.IndexByte(set, s[i]) >= 0 {
			i++
		}
	}
	// delimited moves past the text up to the closing delimiter d
	delimited := func(d byte, regex bool) bool {
		for ; i < len(s); i++ {
			switch {
			case s[i] == d:
				i++
				return true
			case regex && brackets && s[i] == '[':
				end := bracketEnd(s[i:])
				if end < 0 {
					return false
				}
				i += end
			case s[i] == '\\' && !(regex && brackets && i+1 < len(s) && s[i+1] == '['):
				i++
			}
		}
		return false
	}
	// a line number, $, first~step, +N, /re/ or \cREc, with I and M flags
	address := func() bool {
		space(" \t")
		switch {
		case i < len(s) && s[i] == '/':
			i++
			if !delimited('/', true) {
				return false
			}
		case i+1 < len(s) && s[i] == '\\':
			i += 2
			if !delimited(s[i-1], true) {
				return false
			}
		default:
			space("0123456789$+~")
		}
		space("IM \t")
		return true
	}
	for {
		space(" \t\n;")
		if i == len(s) {
			return true
		}
		if !address() || i < len(s) && s[i] == ',' && func() bool { i++; return !address() }() {
			return false
		}
		space("! \t")
		if i == len(s) {
			return false
		}
		cmd := s[i]
		i++
		switch {
		case cmd == '{':
			continue
		case strings.IndexByte("}=dDgGhHnNpPxzF", cmd) >= 0:
		case strings.IndexByte("lLqQ", cmd) >= 0:
			space("0123456789 \t")
		case strings.IndexByte(":btTv", cmd) >= 0:
			// GNU ends a label at ; and BSD only at a newline: stopping early
			// parses more of the script as commands
			space(" \t")
			for i < len(s) && strings.IndexByte(" \t\n;}", s[i]) < 0 {
				i++
			}
		case cmd == 'a' || cmd == 'i' || cmd == 'c' || cmd == '#':
			for ; i < len(s) && s[i] != '\n'; i++ {
				if s[i] == '\\' && cmd != '#' {
					i++
				}
			}
		case cmd == 's' || cmd == 'y':
			if i == len(s) || s[i] == '\\' || s[i] == '\n' {
				return false
			}
			d := s[i]
			i++
			if !delimited(d, true) || !delimited(d, cmd == 'y') {
				return false
			}
			// BSD skips blanks between flags; w and e are left to the check below
			if cmd == 's' {
				space("gpiImM0123456789 \t")
			}
		default: // w, W, r, R and e write, read or run
			return false
		}
		space(" \t")
		if i < len(s) && strings.IndexByte("\n;}#", s[i]) < 0 {
			return false
		}
	}
}

// bracketEnd returns the index of the ] closing the bracket expression t
// starts with, or -1.
func bracketEnd(t string) int {
	j := 1
	if j < len(t) && t[j] == '^' {
		j++
	}
	if j < len(t) && t[j] == ']' {
		j++
	}
	for ; j < len(t); j++ {
		if t[j] == ']' {
			return j
		}
		// [:alpha:], [=a=] and [.x.] may hold a ]
		if t[j] == '[' && j+1 < len(t) && strings.IndexByte(".:=", t[j+1]) >= 0 {
			k := strings.Index(t[j+2:], string(t[j+1])+"]")
			if k < 0 {
				return -1
			}
			j += k + 3
		}
	}
	return -1
}

// getline and ARGV read files the arguments do not name, system and @
// (gawk's @include and indirect calls) run code, ENVIRON and PROCINFO print
// the environment, SYMTAB reaches all of them. A bare < is a comparison.
var awkUnsafe = regexp.MustCompile(`[>|@]|getline|system|ENVIRON|ARGV|PROCINFO|SYMTAB`)

// env and $ENV print the environment; import and include load modules.
var jqUnsafe = regexp.MustCompile(`\$ENV|\b(env|import|include)\b`)

func safeSegment(a []string) bool {
	if len(a) == 0 {
		return true
	}
	name, rest := a[0], a[1:]
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch {
	case name == "uniq": // a second operand is the output file
		return len(slices.DeleteFunc(slices.Clone(rest), func(a string) bool { return strings.HasPrefix(a, "-") })) < 2
	case hasFlag(rest, refusedFlags[name]...):
		return false
	case name == "cd": // with no directory or - it leaves the project; from .git/hooks go build writes a hook
		return len(rest) == 1 && !strings.HasPrefix(sub, "-") &&
			!slices.ContainsFunc(strings.Split(sub, "/"), func(p string) bool { return strings.EqualFold(p, ".git") })
	case name == "jq":
		return !jqUnsafe.MatchString(strings.Join(rest, " "))
	case name == "sed":
		return sedSafe(rest)
	case name == "awk":
		return !awkUnsafe.MatchString(strings.Join(rest, " "))
	}
	return readOnlyPrograms[name]
}

// hasFlag reports whether any argument may set one of flags: the flag with
// or without =value, a short flag bundled or with a glued value (sed -ni,
// sort -uo), or an abbreviated long flag, which GNU tools and git accept
// (--in-pl). It errs toward yes, so it only refuses, never allows.
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if strings.HasPrefix(name, "--") { // --output.json.path sets --output
			name, _, _ = strings.Cut(name, ".")
		}
		for _, f := range flags {
			switch {
			case name == f:
			case len(f) == 2 && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a[1:], f[1:]):
			case len(name) > 2 && strings.HasPrefix(name, "--") && strings.HasPrefix(f, name):
			default:
				continue
			}
			return true
		}
	}
	return false
}

// runners start the program their arguments name, so Destructive looks for
// one past them.
var runners = []string{"xargs", "find", "fd", "parallel", "env", "sudo", "nohup", "time", "timeout", "nice", "command", "exec"}

// Destructive says why a command still goes to the user when a sandbox
// confines it to the project: it may delete or empty many files at once.
// Empty when it may run. Normalize already made sh -c bodies and
// substitutions segments of their own.
func Destructive(cmd string) string {
	n := Normalize(cmd)
	if n.Fail != "" {
		return n.Fail
	}
	for _, f := range n.Args {
		for i, w := range f {
			if i > 0 && !slices.Contains(runners, filepath.Base(f[0])) {
				break
			}
			name, rest := filepath.Base(w), f[i+1:]
			switch {
			case name == "eval":
				if why := Destructive(strings.Join(rest, " ")); why != "" {
					return why
				}
				continue
			case name == "rm" && (i > 0 || hasFlag(rest, "-r", "-R", "-f", "--recursive", "--force")):
			case name == "git" && slices.Contains(rest, "clean"):
			case name == "find" && hasFlag(rest, "-delete"):
			case name == "rsync" && slices.ContainsFunc(rest, func(a string) bool { return strings.HasPrefix(a, "--del") || strings.HasPrefix(a, "--remove") }):
			case name == "shred", name == "truncate", name == "dd":
			default:
				continue
			}
			return strings.Join(f, " ") + " may delete many files"
		}
	}
	return ""
}
