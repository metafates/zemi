package perm

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// SecretPathShape matches secret-shaped filesystem paths (§9); reads ask.
// The seatbelt policy (internal/sandbox) hides the strict names of this
// list too, and DESIGN §9 lists it: the three move together.
var SecretPathShape = []string{".env", "*.env", ".env.*", ".envrc", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*", "*credential*", "*secret*",
	".netrc", ".npmrc", ".yarnrc.yml", ".pgpass", ".pypirc", ".htpasswd", ".my.cnf"}

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
		use, walk, nonPaths := argUses(f)
		positional := 0
		if !safeArgs(f[1:], func(pos int, flag, a string) bool {
			positional = max(positional, pos+1)
			return check(a, use(pos, flag))
		}) || (walk != ArgRead && positional-nonPaths < 1 && !check(".", walk)) {
			return false
		}
		if f[0] == "cd" {
			for _, d := range dirs {
				dirs = append(dirs, inDir(d, f[1]))
			}
		}
	}
	return safeArgs(n.ReadPaths, func(_ int, _, a string) bool { return check(a, ArgRead) })
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
// position among the operands (as safeArgs counts them, a flag's separate
// value included) or by the flag a value belongs to, how it walks the
// working directory when given no path (ArgRead: it does not), and how many
// operand positions are no path: flag values and the pattern.
func argUses(f []string) (use func(pos int, flag string) ArgUse, walk ArgUse, nonPaths int) {
	name, rest := f[0], f[1:]
	byPos := func(u func(pos int) ArgUse) func(pos int, flag string) ArgUse {
		return func(pos int, flag string) ArgUse {
			if flag != "" {
				return ArgRead
			}
			return u(pos)
		}
	}
	switch {
	case slices.Contains([]string{"grep", "egrep", "fgrep", "rg", "ag"}, name):
		return searchOperands(name, rest)
	// gofmt descends into a directory operand and prints what it parses;
	// tree, du, find, fd and ls -R walk too but print names, never content
	case name == "diff", name == "gofmt":
		return byPos(walkFrom(0, ArgWalk)), ArgRead, 0
	case name == "cd":
		return func(int, string) ArgUse { return ArgRun }, ArgRead, 0
	case name == "sed", name == "awk":
		return byPos(scriptOperands(name, rest)), ArgRead, 0
	}
	return byPos(walkFrom(-1, ArgRead)), ArgRead, 0
}

// searchFlags are the grep, rg and ag options that take a value, and what
// the value is: a count, type, glob or pattern is no path (ArgText); a
// pattern file or ignore file is read. A value the shell passes as its own
// word takes an operand position, which would otherwise shift the pattern
// onto a path and judge the pattern itself as a walk.
var searchFlags = map[string]map[string]ArgUse{
	"grep": {
		"-A": ArgText, "-B": ArgText, "-C": ArgText, "-m": ArgText, "-e": ArgText, "-d": ArgText, "-D": ArgText,
		"--after-context": ArgText, "--before-context": ArgText, "--context": ArgText, "--max-count": ArgText,
		"--regexp": ArgText, "--exclude": ArgText, "--exclude-dir": ArgText, "--include": ArgText, "--label": ArgText,
		"--directories": ArgText, "--devices": ArgText, "--binary-files": ArgText, "--color": ArgText, "--colour": ArgText,
		"-f": ArgRead, "--file": ArgRead, "--exclude-from": ArgRead,
	},
	"rg": {
		"-A": ArgText, "-B": ArgText, "-C": ArgText, "-m": ArgText, "-e": ArgText, "-g": ArgText, "-t": ArgText,
		"-T": ArgText, "-E": ArgText, "-M": ArgText, "-j": ArgText, "-r": ArgText, "-d": ArgText,
		"--after-context": ArgText, "--before-context": ArgText, "--context": ArgText, "--max-count": ArgText,
		"--regexp": ArgText, "--glob": ArgText, "--iglob": ArgText, "--type": ArgText, "--type-not": ArgText,
		"--type-add": ArgText, "--type-clear": ArgText, "--max-columns": ArgText, "--max-depth": ArgText,
		"--max-filesize": ArgText, "--replace": ArgText, "--encoding": ArgText, "--color": ArgText, "--colors": ArgText,
		"--threads": ArgText, "--sort": ArgText, "--sortr": ArgText, "--engine": ArgText, "--dfa-size-limit": ArgText,
		"--regex-size-limit": ArgText, "--context-separator": ArgText, "--field-context-separator": ArgText,
		"--field-match-separator": ArgText, "--path-separator": ArgText,
		"-f": ArgRead, "--file": ArgRead, "--ignore-file": ArgRead,
	},
	"ag": {
		"-A": ArgText, "-B": ArgText, "-C": ArgText, "-m": ArgText, "-G": ArgText, "-g": ArgText,
		"--after": ArgText, "--before": ArgText, "--context": ArgText, "--max-count": ArgText,
		"--file-search-regex": ArgText, "--ignore": ArgText, "--ignore-dir": ArgText, "--workers": ArgText,
		"-p": ArgRead, "--path-to-ignore": ArgRead,
	},
}

// searchOperands is argUses for grep, rg and ag: flag values by the flag
// they belong to, the pattern operand as text (unless a flag gave it), the
// file operands from there as a walk.
func searchOperands(name string, rest []string) (func(pos int, flag string) ArgUse, ArgUse, int) {
	if name == "egrep" || name == "fgrep" {
		name = "grep"
	}
	flags := searchFlags[name]
	// takes says whether the next word is a's value: a long flag without
	// =, or a bundle whose last letter takes one (-nA 3; -A3 is glued)
	takes := func(a string) (ArgUse, bool) {
		if strings.HasPrefix(a, "--") {
			u, ok := flags[a]
			return u, ok
		}
		for j := 1; j < len(a); j++ {
			if u, ok := flags["-"+a[j:j+1]]; ok {
				return u, j == len(a)-1
			}
		}
		return 0, false
	}
	vals, pos := map[int]ArgUse{}, 0
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos++
			continue
		}
		if a == "--" {
			break
		}
		if u, ok := takes(a); ok && i+1 < len(rest) {
			vals[pos] = u
			pos, i = pos+1, i+1
		}
	}
	// a pattern given by flag (-e, --regexp, -f) makes the first operand a
	// path; ag has no such flag
	given := name != "ag" && hasFlag(rest, "-e", "-f", "--regexp", "--file", "--files")
	walk := ArgWalk
	switch {
	// globs, and a whitelist in an --ignore-file, override rg's ignore
	// rules, so they walk everything too
	case name == "rg" && !hasFlag(rest, "-u", "-.", "--unrestricted", "--hidden", "--no-ignore", "--no-ignore-dot",
		"--no-ignore-exclude", "--no-ignore-files", "--no-ignore-global", "--no-ignore-parent", "--no-ignore-vcs",
		"-g", "--glob", "--iglob", "--ignore-file"):
		walk = ArgSearch
	case name == "grep" && !hasFlag(rest, "-r", "-R", "--recursive", "--dereference-recursive", "-d", "--directories"):
		walk = ArgRead
	}
	use := func(pos int, flag string) ArgUse {
		if flag != "" {
			if u, ok := flags[flag]; ok {
				return u
			}
			return ArgRead
		}
		if u, ok := vals[pos]; ok {
			return u
		}
		// the operand index with the flag values before it left out
		operand := pos
		for p := range vals {
			if p < pos {
				operand--
			}
		}
		if operand == 0 && !given {
			return ArgText
		}
		return walk
	}
	nonPaths := len(vals)
	if !given {
		nonPaths++
	}
	return use, walk, nonPaths
}

// scriptOperands marks a sed or awk program text among the operands as
// safeArgs counts them (a flag's separate value is an operand to it): the
// sed -e values, else the first operand, which "/re/p" makes look like an
// absolute path. awk -F and -v values, and a sed -e value, take a position.
func scriptOperands(name string, rest []string) func(pos int) ArgUse {
	text, pos, named, first := map[int]bool{}, 0, false, -1
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			if first < 0 {
				first = pos
			}
			pos++
			continue
		}
		if a == "--" {
			break
		}
		// -e, -ne and --expression name a script; awk's -F and -v take a word
		script := name == "sed" && (a == "--expression" || !strings.HasPrefix(a, "--") && strings.HasSuffix(a, "e"))
		if (script || name == "awk" && (a == "-F" || a == "-v")) && i+1 < len(rest) {
			text[pos], named = script, named || script
			pos, i = pos+1, i+1
		}
	}
	if !named && first >= 0 {
		text[first] = true
	}
	return func(pos int) ArgUse {
		if text[pos] {
			return ArgText
		}
		return ArgRead
	}
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
// short flag (-f/etc/passwd). pathOK gets the operand's position, or -1
// and the flag for a glued value.
func safeArgs(args []string, pathOK func(pos int, flag, a string) bool) bool {
	ok := func(pos int, flag, a string) bool { return a == "" || pathOK(pos, flag, a) }
	flags, pos := true, 0
	for _, a := range args {
		if flags && strings.HasPrefix(a, "-") {
			if a == "--" {
				flags = false
				continue
			}
			// one glued character is a separator (cut -d/), not a path
			if !strings.HasPrefix(a, "--") && len(a) > 3 && !ok(-1, a[:2], a[2:]) {
				return false
			}
			if flag, v, found := strings.Cut(a, "="); found && !ok(-1, flag, v) {
				return false
			}
			continue
		}
		if !ok(pos, "", a) {
			return false
		}
		pos++
	}
	return true
}

// ReadsSecret reports whether a command's reader (cat, grep, jq, ...)
// names a secret-shaped path, or a word the shell may expand into one
// (cat .e*), judged by secret on each path word; a search pattern is no
// path. Other programs are not judged: an always rule keeps a deleter
// exact and offers a wrapper nothing.
func ReadsSecret(cmd string, secret func(path string) bool) bool {
	n := Normalize(cmd)
	found := false
	for _, f := range n.Args {
		if len(f) == 0 || !readOnlyPrograms[filepath.Base(f[0])] {
			continue
		}
		use, _, _ := argUses(f)
		safeArgs(f[1:], func(pos int, flag, a string) bool {
			if use(pos, flag) != ArgText && (strings.ContainsAny(a, "*?[{") || secret(a)) {
				found = true
			}
			return true
		})
	}
	return found || slices.ContainsFunc(n.ReadPaths, secret)
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

// WrapperPrograms run whatever they are handed, as code (node -e) or as
// another command (sudo, xargs, busybox), so what such a segment does is in
// its arguments: Destructive and GitWrites look past them for the program,
// and an always answer offers no rule for them, since none written from
// the wrapper says what a later call would run.
var WrapperPrograms = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "fish": true, "csh": true, "tcsh": true,
	"node": true, "nodejs": true, "deno": true, "python": true, "python2": true, "python3": true,
	"perl": true, "ruby": true, "php": true, "lua": true, "luajit": true, "julia": true, "rscript": true, "osascript": true,
	"env": true, "sudo": true, "doas": true, "su": true, "xargs": true, "eval": true, "exec": true, "source": true, ".": true,
	"nohup": true, "time": true, "timeout": true, "watch": true, "setsid": true, "stdbuf": true, "script": true,
	"nice": true, "ionice": true, "chrt": true, "caffeinate": true, "parallel": true, "ssh": true, "command": true, "builtin": true,
	"npx": true, "pnpx": true, "bunx": true, "uvx": true, "pipx": true, "busybox": true, "toybox": true,
	"awk": true, // an inline program, like perl -e
}

// globOperand reports an operand the shell may expand to many files.
func globOperand(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return !strings.HasPrefix(a, "-") && strings.ContainsAny(a, "*?[") })
}

// runner reports a program Destructive looks past: a wrapper, or find and
// fd, whose -exec runs what follows.
func runner(prog string) bool {
	prog = strings.ToLower(filepath.Base(prog))
	return WrapperPrograms[prog] || prog == "find" || prog == "fd"
}

// reserved are the shell words a segment may start with before its program:
// Normalize keeps "then rm -rf ." as one segment.
var reserved = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "while": true, "until": true, "do": true, "{": true, "!": true}

// shellBodies are the commands the shells in f run through -c: f's own
// program, or one a wrapper (xargs, env, timeout, find -exec) hands a shell.
// Normalize folds only a segment's leading shell.
func shellBodies(f []string) []string {
	var bodies []string
	for i, w := range f {
		if i > 0 && !runner(f[0]) {
			break
		}
		if isShell(w) {
			if body, ok := shellBody(f[i+1:]); ok {
				bodies = append(bodies, body)
			}
		}
	}
	return bodies
}

// program is f without its leading reserved words; "function name" is
// skipped as a pair (the () spelling makes Normalize split at the brace).
func program(f []string) []string {
	for len(f) > 0 {
		switch {
		case f[0] == "function" && len(f) > 1:
			f = f[2:]
		case reserved[f[0]]:
			f = f[1:]
		default:
			return f
		}
	}
	return f
}

// Subcommand is the index in f of a program's subcommand word, past its
// leading options (git -C dir status, git -c k=v diff, cargo +nightly
// build, npm --prefix web run); -1 when there is none. -C, -c, --prefix
// and git's --git-dir, --work-tree and --namespace take the next word.
func Subcommand(f []string) int {
	for i := 1; i < len(f); i++ {
		switch a := f[i]; {
		case subcommandValues[a]:
			i++
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
		default:
			return i
		}
	}
	return -1
}

var subcommandValues = map[string]bool{"-C": true, "-c": true, "--prefix": true, "--git-dir": true, "--work-tree": true, "--namespace": true}

// plainFlags are leading options known to take no value.
var plainFlags = map[string]bool{
	"--no-pager": true, "-p": true, "--paginate": true, "--bare": true, "--no-replace-objects": true,
	"--literal-pathspecs": true, "--no-optional-locks": true, "-q": true, "--quiet": true, "-v": true,
	"--verbose": true, "--offline": true, "--locked": true, "--frozen": true, "-s": true, "--silent": true,
}

// LeadKnown reports whether Subcommand read every word before f[i] for
// sure: an option it knows takes a value, with that value, a --opt=value, a
// +toolchain or a flag known to take none. An unknown option may take the
// next word (npm -w web test), which Subcommand then calls the subcommand.
func LeadKnown(f []string, i int) bool {
	for j := 1; j < i; j++ {
		switch a := f[j]; {
		case subcommandValues[a]:
			j++
		case strings.HasPrefix(a, "+") || strings.HasPrefix(a, "--") && strings.Contains(a, "=") || plainFlags[a]:
		default:
			return false
		}
	}
	return true
}

// gitWriters are the git subcommands that write .git, which the sandbox
// keeps read-only, so a confined run can only fail on it.
var gitWriters = map[string]bool{
	"add": true, "am": true, "apply": true, "bisect": true, "checkout": true, "cherry-pick": true, "clone": true,
	"commit": true, "commit-tree": true, "fetch": true, "filter-branch": true, "gc": true, "init": true, "maintenance": true,
	"merge": true, "mergetool": true, "mv": true, "pack-refs": true, "prune": true, "pull": true, "push": true,
	"read-tree": true, "rebase": true, "repack": true, "replace": true, "reset": true, "restore": true, "revert": true,
	"rm": true, "sparse-checkout": true, "switch": true, "symbolic-ref": true, "update-index": true, "update-ref": true,
}

// GitWrites says why a git command cannot run with .git read-only: its
// subcommand writes the repository. Empty when it only reads, or is no git.
func GitWrites(cmd string) string {
	n := Normalize(cmd)
	if n.Fail != "" {
		return ""
	}
	for _, f := range n.Args {
		f = program(f)
		for _, body := range shellBodies(f) {
			if why := GitWrites(body); why != "" {
				return why
			}
		}
		// xargs, find and fd hand git arguments nothing here sees
		for len(f) > 1 && runner(f[0]) && !slices.Contains([]string{"xargs", "find", "fd"}, filepath.Base(f[0])) {
			f = f[1:]
		}
		i := Subcommand(f)
		if len(f) == 0 || filepath.Base(f[0]) != "git" || i < 0 {
			continue
		}
		sub, rest := f[i], f[i+1:]
		var words []string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				words = append(words, a)
			}
		}
		first := ""
		if len(words) > 0 {
			first = words[0]
		}
		writes := gitWriters[sub]
		switch sub {
		case "branch", "tag":
			writes = len(words) > 0 || hasFlag(rest, "-d", "-D", "-m", "-M", "-c", "-C", "-u", "--delete", "--move", "--copy", "--set-upstream-to", "--unset-upstream", "--edit-description", "-f")
		case "stash":
			writes = first != "list" && first != "show"
		case "remote":
			writes = len(words) > 0 && first != "show" && first != "get-url"
		case "worktree", "submodule", "notes":
			// bare, they list
			writes = first != "" && first != "list" && first != "status" && first != "summary" && first != "show"
		case "hash-object":
			writes = hasFlag(rest, "-w")
		case "config":
			writes = len(words) > 1 || hasFlag(rest, "--unset", "--unset-all", "--add", "--replace-all", "--edit", "-e", "--remove-section", "--rename-section")
		case "reflog":
			writes = first == "expire" || first == "delete"
		}
		if writes {
			return "git " + sub + " writes .git, which stays read-only in the sandbox; allowing runs it unconfined"
		}
	}
	return ""
}

// Destructive says why a command still goes to the user when a sandbox
// confines it to the project: it may delete or empty many files at once.
// Empty when it may run. Normalize already made sh -c bodies and
// substitutions segments of their own.
func Destructive(cmd string) string {
	return destructive(cmd, false)
}

// repeats are the runners that run what they are handed once per input,
// so a bare rm in their shell body deletes in bulk.
func repeats(prog string) bool {
	switch strings.ToLower(filepath.Base(prog)) {
	case "xargs", "find", "fd", "parallel":
		return true
	}
	return false
}

// destructive is Destructive with repeated set for a shell body a
// repeating runner hands a shell (xargs sh -c 'rm "$@"'). A loop body
// between do and done repeats too.
func destructive(cmd string, repeated bool) string {
	n := Normalize(cmd)
	if n.Fail != "" {
		return n.Fail
	}
	loops := 0
	for _, f := range n.Args {
		g := program(f)
		if slices.Contains(f[:len(f)-len(g)], "do") {
			loops++
		}
		if len(f) > 0 && f[0] == "done" {
			loops = max(loops-1, 0)
		}
		f = g
		repeated := repeated || loops > 0
		for _, body := range shellBodies(f) {
			if why := destructive(body, repeated || len(f) > 0 && repeats(f[0])); why != "" {
				return why
			}
		}
		for i, w := range f {
			if i > 0 && !runner(f[0]) {
				break
			}
			name, rest := filepath.Base(w), f[i+1:]
			switch {
			case name == "eval":
				if why := destructive(strings.Join(rest, " "), repeated); why != "" {
					return why
				}
				continue
			case name == "rm" && (i > 0 || repeated || hasFlag(rest, "-r", "-R", "-f", "--recursive", "--force") || globOperand(rest)):
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
