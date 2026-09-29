package perm

import (
	"cmp"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// SecretPathShape matches secret-shaped filesystem paths (§9); reads ask.
// The seatbelt policy (internal/sandbox) hides strictSecretShape, .env
// templates included, and DESIGN §9 lists it: the three move together.
var SecretPathShape = append(slices.Clone(strictSecretShape), "*credential*", "*secret*")

// strictSecretShape is SecretPathShape without the broad names, which also
// match source files (credential.ts, secret_prompt.py).
var strictSecretShape = []string{
	".env", "*.env", ".env.*", ".envrc", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
	".netrc", ".npmrc", ".yarnrc.yml", ".pgpass", ".pypirc", ".htpasswd", ".my.cnf",
}

// PathIsSecret folds on every OS: a shape that asks one file too many is
// cheaper than a darwin spelling that slips past.
func PathIsSecret(p string) bool { return secretShaped(fold(filepath.Base(p)), SecretPathShape) }

// PathIsStrictSecret is what grep and a directory listing hide: the strict
// shapes, minus the committed .env templates a setup step copies.
func PathIsStrictSecret(p string) bool {
	base := fold(filepath.Base(p))
	if ext := filepath.Ext(base); strings.HasPrefix(base, ".env.") && (ext == ".example" || ext == ".sample" || ext == ".template") {
		return false
	}
	return secretShaped(base, strictSecretShape)
}

// secretShaped takes a folded base name.
func secretShaped(base string, shapes []string) bool {
	return slices.ContainsFunc(shapes, func(pat string) bool {
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
func (n Normalized) ReadCommand(pathOK func(arg string, use ArgUse) bool) bool {
	if expands(n.cmd) {
		return false
	}
	if pathOK == nil {
		pathOK = relativeArg
	}
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

// PluginTest reports the one command zemi plugins test <name> [pattern],
// with nothing riding along. The plugin is named, never a directory: the
// runner copies a directory it is given into the temp dir the model reads.
// exe, the running zemi's path, may stand for zemi.
func (n Normalized) PluginTest(exe string) bool {
	if expands(n.cmd) || n.Fail != "" || n.ForceAsk != "" || n.Opaque || n.Assign != "" ||
		len(n.ReadPaths)+len(n.WritePaths) > 0 || len(n.Args) != 1 {
		return false
	}
	f := n.Args[0]
	return (f[0] == "zemi" || f[0] == exe && exe != "") && (len(f) == 4 || len(f) == 5) &&
		f[1] == "plugins" && f[2] == "test" && pluginName.MatchString(f[3])
}

// pluginName is internal/plugin' name shape, which no shell expansion
// (~, {a,b}, a glob) can match.
var pluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

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
func (n Normalized) ReadsSecret(secret func(path string) bool) bool {
	found := false
	for _, f := range n.Args {
		if len(f) == 0 || !readOnlyPrograms[filepath.Base(f[0])] {
			continue
		}
		use, _, _ := argUses(f)
		safeArgs(f[1:], func(pos int, flag, a string) bool {
			if use(pos, flag) != ArgText && (strings.ContainsAny(a, "*?[{") || secret(ExpandHome(a))) {
				found = true
			}
			return true
		})
	}
	return found || slices.ContainsFunc(n.ReadPaths, secret)
}

// ExpandHome is a path word as the shell hands it over: a leading ~ or
// $HOME names the home directory.
func ExpandHome(w string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return w
	}
	for _, p := range []string{"~", "$HOME", "${HOME}"} {
		if w == p || strings.HasPrefix(w, p+"/") {
			return home + w[len(p):]
		}
	}
	return w
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
		if i >= len(s) {
			return true
		}
		if !address() || i < len(s) && s[i] == ',' && func() bool { i++; return !address() }() {
			return false
		}
		space("! \t")
		if i >= len(s) {
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
			// a trailing backslash leaves i past the end
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
	"trap": true, "coproc": true,
	"awk":  true, // an inline program, like perl -e
	"zemi": true, // zemi run runs a model, and plugins install fetches code that runs in every project
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

// ProgramWords is f without its leading reserved words; "function name" is
// skipped as a pair (the () spelling makes Normalize split at the brace).
func ProgramWords(f []string) []string {
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

// SubcommandPrograms are the programs whose second word is a subcommand;
// for any other it may be a file ("cat notes").
var SubcommandPrograms = map[string]bool{
	"git": true, "go": true, "cargo": true, "npm": true, "pnpm": true, "yarn": true, "bun": true,
	"make": true, "docker": true, "kubectl": true, "gh": true, "uv": true,
}

var subcommandWord = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// SubcommandAt is the index in f of the subcommand of a SubcommandPrograms
// program, when the word there is shaped like one; -1 otherwise.
func SubcommandAt(f []string) int {
	i := Subcommand(f)
	if len(f) == 0 || !SubcommandPrograms[strings.ToLower(filepath.Base(f[0]))] || i < 0 || !subcommandWord.MatchString(f[i]) {
		return -1
	}
	return i
}

var subcommandValues = map[string]bool{"-C": true, "-c": true, "--prefix": true, "--git-dir": true, "--work-tree": true, "--namespace": true}

// plainFlags are the leading options known to take no value, per program:
// kubectl's -s and -v take one, so a shared list would misplace the subcommand.
var plainFlags = map[string][]string{
	"git":   {"--no-pager", "-p", "--paginate", "--bare", "--no-replace-objects", "--literal-pathspecs", "--no-optional-locks"},
	"cargo": {"--offline", "--locked", "--frozen", "-q", "--quiet", "-v", "--verbose"},
	"npm":   {"-s", "--silent", "-q", "--quiet"},
	"pnpm":  {"-s", "--silent"},
	"yarn":  {"-s", "--silent"},
	"make":  {"-s", "--silent", "-q", "--quiet"},
	"uv":    {"-q", "--quiet", "-v", "--verbose", "--offline", "--locked", "--frozen"},
}

// LeadKnown reports whether Subcommand read every word before f[i] for
// sure: an option it knows takes a value, with that value, a --opt=value, a
// +toolchain or a flag known to take none. An unknown option may take the
// next word (npm -w web test), which Subcommand then calls the subcommand.
func LeadKnown(f []string, i int) bool {
	plain := plainFlags[strings.ToLower(filepath.Base(f[0]))]
	for j := 1; j < i; j++ {
		switch a := f[j]; {
		case subcommandValues[a]:
			j++
		case strings.HasPrefix(a, "+") || strings.HasPrefix(a, "--") && strings.Contains(a, "=") || slices.Contains(plain, a):
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
func (n Normalized) GitWrites() string {
	if n.Fail != "" {
		return ""
	}
	for _, f := range n.Args {
		f = ProgramWords(f)
		for _, body := range shellBodies(f) {
			if why := Normalize(body).GitWrites(); why != "" {
				return why
			}
		}
		// xargs, find and fd hand git arguments nothing here sees
		for len(f) > 1 && runner(f[0]) && !slices.Contains([]string{"xargs", "find", "fd"}, filepath.Base(f[0])) {
			if g, ok := stepRunner(f); ok {
				f = g
			} else {
				f = f[1:]
			}
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
// substitutions segments of their own. pad is the scratchpad, $TMPDIR in
// every command the model runs, "" for none: an rm whose every operand is
// inside it deletes nothing of the project.
// NOTE: a symlink the same command plants in the pad, one a glob picks
// with a trailing slash, or an arithmetic assignment to a name built at run
// time ($(( $(printf TMP)DIR = 0 ))) still leads out; the run is confined,
// so it can delete no more than any settled command can.
func (n Normalized) Destructive(pad string) string {
	if !n.padIntact() {
		pad = ""
	}
	return n.destructive(false, pad)
}

// padIntact reports that $TMPDIR is the value zemi set: the text names it
// only as $TMPDIR or ${TMPDIR}, unescaped and outside single quotes, nothing
// is assigned, and no builtin runs that sets or unsets a variable by a name
// it may build at run time (read, eval, declare "$n=x").
func (n Normalized) padIntact() bool {
	if n.Fail != "" || n.ForceAsk != "" || n.Opaque || n.Assign != "" {
		return false
	}
	for _, f := range n.Args {
		for f = ProgramWords(f); len(f) > 0; {
			if ShellState[f[0]] || strings.ContainsAny(f[0], "$=") ||
				f[0] == "printf" && slices.ContainsFunc(f, func(a string) bool { return strings.HasPrefix(a, "-v") }) {
				return false
			}
			g, ok := stepRunner(f)
			if !ok {
				break
			}
			f = g
		}
	}
	const name = "TMPDIR"
	inSingle, inDouble := false, false
	for i := 0; i < len(n.cmd); i++ {
		switch c, rest := n.cmd[i], n.cmd[i:]; {
		case inSingle:
			inSingle = c != '\''
			if strings.HasPrefix(rest, name) {
				return false
			}
		case c == '\\':
			i++ // an escaped $ leaves the name to the last case
		case c == '\'' && !inDouble:
			inSingle = true
		case c == '"':
			inDouble = !inDouble
		case strings.HasPrefix(rest, "${"+name+"}"):
			i += len(name) + 2
		case strings.HasPrefix(rest, "$"+name) && !nameByte(rest, len(name)+1):
			i += len(name)
		case strings.HasPrefix(rest, name):
			return false
		}
	}
	return true
}

// ShellState are the builtins that set or unset the shell variables their
// arguments name. bash evaluates a subscript in a name as arithmetic, where
// a quoted $(...) runs.
var ShellState = map[string]bool{
	"eval": true, "source": true, ".": true, "export": true, "declare": true, "typeset": true, "local": true,
	"readonly": true, "unset": true, "read": true, "mapfile": true, "readarray": true, "getopts": true,
	"let": true, "alias": true, "trap": true, "select": true, "enable": true,
}

func nameByte(s string, i int) bool {
	return i < len(s) && (s[i] == '_' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9')
}

// padOperands reports that args has operands and each names a path below
// pad: the literal pad path or $TMPDIR, a slash, then a rest with no climb
// and no expansion but a glob. A word after -- is an operand whatever it
// starts with.
func padOperands(args []string, pad string) bool {
	operands, dashed := 0, false
	for _, a := range args {
		if !dashed && strings.HasPrefix(a, "-") && a != "-" {
			dashed = a == "--"
			continue
		}
		rest, ok := strings.CutPrefix(a, "$TMPDIR/")
		if !ok {
			rest, ok = strings.CutPrefix(a, "${TMPDIR}/")
		}
		// unquoted, a pad path holding a blank or a glob splits or expands
		if ok && strings.ContainsAny(pad, " \t\n*?[") {
			return false
		}
		if !ok {
			rest, ok = strings.CutPrefix(a, strings.TrimSuffix(pad, "/")+"/")
		}
		if !ok || path.Clean("/"+rest) == "/" || strings.ContainsAny(rest, "~\\") || expandsToDotDot("/"+rest) ||
			slices.Contains(strings.Split(rest, "/"), "..") {
			return false
		}
		// a symlink already there leads out
		if _, in := Under(RealPath(pad), RealPath(filepath.Join(pad, rest))); !in && !strings.ContainsAny(rest, "*?[") {
			return false
		}
		operands++
	}
	return operands > 0
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
func (n Normalized) destructive(repeated bool, pad string) string {
	if n.Fail != "" {
		return n.Fail
	}
	loops := 0
	for _, f := range n.Args {
		g := ProgramWords(f)
		if slices.Contains(f[:len(f)-len(g)], "do") {
			loops++
		}
		if len(f) > 0 && f[0] == "done" {
			loops = max(loops-1, 0)
		}
		f = g
		repeated := repeated || loops > 0
		for _, body := range shellBodies(f) {
			if why := Normalize(body).destructive(repeated || len(f) > 0 && repeats(f[0]), ""); why != "" {
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
				if why := Normalize(strings.Join(rest, " ")).destructive(repeated, ""); why != "" {
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
			// behind a runner (xargs, find -exec) more operands come from elsewhere
			if name == "rm" && i == 0 && pad != "" && padOperands(rest, pad) {
				continue
			}
			return "this may delete many files"
		}
	}
	return ""
}

// ShellWords are the syntax and no-op words a segment may start with, which
// run nothing worth naming: Programs lists no segment led by one, and an
// always answer names none it allows.
var ShellWords = map[string]bool{
	"for": true, "select": true, "case": true, "done": true, "fi": true, "esac": true, "}": true,
	"[": true, "set": true, "shopt": true, "true": true, "false": true, ":": true,
	"break": true, "continue": true, "exit": true, "return": true, "wait": true,
}

// Risk is why the permission prompt marks a program.
type Risk string

const (
	RiskDeletes Risk = "deletes"
	RiskCode    Risk = "runs code"
)

// Program is one program a command runs, as the permission prompt names it.
type Program struct {
	Name string // base name, with the subcommand of a SubcommandPrograms one
	Risk Risk
}

// Programs lists what the command runs, each name once in the order it
// first runs, with the strongest mark it has; nil when the parse cannot
// vouch for the list. pad is Destructive's.
// NOTE: a segment's deletes mark sits on the last program it names, so
// find . -exec grep x {} + -delete marks grep; only the first program an
// exec flag runs is named.
func (n Normalized) Programs(pad string) []Program {
	if n.Fail != "" || n.hidden {
		return nil
	}
	if !n.padIntact() {
		pad = ""
	}
	out := []Program{}
	loops := 0
	rows := n.Args
	if n.shown != nil {
		rows = n.shown
	}
	for _, f := range rows {
		g := ProgramWords(f)
		if slices.Contains(f[:len(f)-len(g)], "do") {
			loops++
		}
		if len(f) > 0 && f[0] == "done" {
			loops = max(loops-1, 0)
		}
		seg, ok := segmentPrograms(g)
		if !ok {
			return nil
		}
		if len(seg) > 0 && (Normalized{Args: [][]string{f}}).destructive(loops > 0, pad) != "" {
			seg[len(seg)-1].Risk = RiskDeletes
		}
		for _, p := range seg {
			switch i := slices.IndexFunc(out, func(q Program) bool { return q.Name == p.Name }); {
			case i < 0:
				out = append(out, p)
			case p.Risk == RiskDeletes || out[i].Risk == "":
				out[i].Risk = cmp.Or(p.Risk, out[i].Risk)
			}
		}
	}
	return out
}

// segmentPrograms names the programs one segment runs: a runner and what it
// runs, find or fd and the first program an exec flag runs, a shell's -c body
// or eval's text through Programs. ok is false when the shell still expands
// a program word, so nothing says what runs.
func segmentPrograms(f []string) (_ []Program, ok bool) {
	var out []Program
	// a leading fd redirect (>&2 echo x) stays a word of the segment
	for len(f) > 0 && strings.IndexAny(strings.TrimLeft(f[0], "0123456789"), "<>") == 0 {
		f = f[1:]
	}
	for len(f) > 0 {
		w, base := f[0], filepath.Base(f[0])
		name := strings.ToLower(base)
		_, runs := runnerOpts[name]
		switch {
		case ShellWords[w]:
			return out, true
		case w != "[[" && strings.ContainsAny(w, "$`*?[{"):
			return nil, false
		case isShell(w) || name == "eval":
			body, ok := shellBody(f[1:])
			if name == "eval" {
				body, ok = strings.Join(f[1:], " "), true
			}
			if !ok {
				return append(out, Program{Name: base, Risk: RiskCode}), true
			}
			inner := Normalize(body).Programs("")
			if inner == nil {
				return nil, false
			}
			return append(out, inner...), true
		case runs:
			out = append(out, Program{Name: base})
			g, _ := stepRunner(f)
			if len(g) == 0 || strings.HasPrefix(g[0], "-") {
				return out, true
			}
			f = g
		case name == "find" || name == "fd":
			out = append(out, Program{Name: base})
			i := slices.IndexFunc(f, func(a string) bool { return slices.Contains(execFlags, a) })
			if i < 0 || i+1 >= len(f) {
				return out, true
			}
			f = f[i+1:]
		case WrapperPrograms[name]:
			return append(out, Program{Name: base, Risk: RiskCode}), true
		default:
			if i := SubcommandAt(f); i > 0 && LeadKnown(f, i) {
				base += " " + f[i]
			}
			return append(out, Program{Name: base}), true
		}
	}
	return out, true
}

// execFlags are find's and fd's flags whose next word is a program they run.
var execFlags = []string{"-exec", "-execdir", "-ok", "-okdir", "-x", "-X", "--exec", "--exec-batch"}
