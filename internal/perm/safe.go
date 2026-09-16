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
	// ArgRun may hold code the program runs: a test file, a build
	// directory, a config, the directory cd enters.
	ArgRun
	// ArgWalk is searched recursively when it is a directory, and what
	// matches is printed.
	ArgWalk
	// ArgSearch is ArgWalk minus hidden and gitignored entries below the
	// argument, which rg skips unless told otherwise.
	ArgSearch
	// ArgRepo is the directory of a repository whose tracked files a git
	// command may print without naming them (git grep, git show HEAD).
	ArgRepo
)

// SafeCommand reports whether a bash command only reads, builds or tests:
// every segment runs an allowlisted program with harmless arguments, nothing
// is written through a redirect, the shell expands nothing, and every
// argument passes pathOK, so a read-only program cannot reach secrets or
// files outside the project. A nil pathOK refuses absolute, home and parent
// paths. Auto and plan mode run these without asking.
func SafeCommand(cmd string, pathOK func(arg string, use ArgUse) bool) bool {
	cmd = inlineVars(cmd)
	if expands(cmd) {
		return false
	}
	if pathOK == nil {
		pathOK = relativeArg
	}
	n := Normalize(cmd)
	// an assignment can steer a program past its checked flags (GOFLAGS,
	// RIPGREP_CONFIG_PATH), and sh -c passes arguments nothing here sees
	if n.Fail != "" || n.ForceAsk != "" || n.Opaque || len(n.WritePaths) > 0 || len(n.Segments) == 0 {
		return false
	}
	// a subshell or pipeline may drop a cd, so arguments are judged from
	// every directory the shell may be in
	dirs := []string{"."}
	check := func(a string, use ArgUse) bool {
		// git reads rev:path from the tree and :0:path from the index, so the
		// text after every colon counts too
		paths := []string{a}
		for rest, ok := a, true; ok; {
			if _, rest, ok = strings.Cut(rest, ":"); ok && rest != "" {
				paths = append(paths, rest)
			}
		}
		for _, d := range dirs {
			for _, p := range paths {
				if !pathOK(inDir(d, p), use) {
					return false
				}
			}
		}
		return true
	}
	for _, f := range n.Args {
		if !safeSegment(f) {
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
		if repo, ok := gitContentDir(f); ok && !check(repo, ArgRepo) {
			return false
		}
		if filepath.Base(f[0]) == "cd" {
			for _, d := range dirs {
				dirs = append(dirs, inDir(d, f[1]))
			}
		}
	}
	return safeArgs(n.ReadPaths, func(_ int, a string) bool { return check(a, ArgRead) })
}

// ReadCommand is SafeCommand limited to programs that only read, found on
// PATH by name: no build or test tool, which runs the project's code, and no
// git, which runs programs the repository's config names (core.fsmonitor on
// status, diff.external and textconv on diff). An untrusted project settles
// only these.
func ReadCommand(cmd string, pathOK func(arg string, use ArgUse) bool) bool {
	if !SafeCommand(cmd, pathOK) {
		return false
	}
	for _, f := range Normalize(inlineVars(cmd)).Args {
		if len(f) > 0 && (!reader(f[0]) || strings.ContainsRune(f[0], '/')) {
			return false
		}
	}
	return true
}

// reader reports a program that only reads its arguments; safeSegment
// refuses the flags and scripts that would make one write or run code.
func reader(name string) bool {
	return readOnlyPrograms[name] || name == "sed" || name == "awk" || name == "uniq"
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
	name, rest := filepath.Base(f[0]), f[1:]
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
	case name == "diff":
		return walkFrom(0, ArgWalk), false
	case name == "cd", !reader(name):
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
}

// refusedFlags make an otherwise read-only program write a file, assign a
// variable, run a program it names, load code from a file or read paths
// listed in a file the path check never sees.
var refusedFlags = map[string][]string{
	"sort":          {"-o", "--output", "--compress-program", "--files0-from"},
	"printf":        {"-v"},
	"tree":          {"-o", "-R"},
	"file":          {"-C", "--compile", "-f", "--files-from"},
	"du":            {"--files0-from"},
	"wc":            {"--files0-from"},
	"diff":          {"--from-file", "--to-file"},
	"fd":            {"-x", "-X", "--exec", "--exec-batch"},
	"rg":            {"--pre", "--hostname-bin"},
	"ag":            {"--pager"},
	"find":          {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls", "-files0-from"},
	"sed":           {"-i", "-I", "--in-place", "-f", "--file"},
	"awk":           {"-f", "--file", "-i", "--include", "-l", "--load", "-E", "--exec", "-W", "-o", "--pretty-print", "-p", "--profile", "-d", "--dump-variables", "-D", "--debug"},
	"jq":            {"-f", "--from-file", "-L", "--library-path"},
	"make":          {"-f", "--file", "--makefile", "-C", "--directory", "-E", "--eval", "-t", "--touch"},
	"cargo":         {"--config", "--fix", "--out-dir", "--artifact-dir", "--open", "--logfile", "-Z"},
	"prettier":      {"-w", "--write", "--cache-location"},
	"eslint":        {"--fix", "-o", "--output-file", "--init", "--inspect-config", "--cache-location"},
	"ruff":          {"--fix", "--fix-only", "--unsafe-fixes", "--add-noqa", "-o", "--output-file", "--config", "--cache-dir"},
	"golangci-lint": {"--fix", "--output", "--out-format", "--cpu-profile-path", "--mem-profile-path", "--trace-path"},
	"jest":          {"-u", "--updateSnapshot", "--outputFile", "--coverageDirectory", "--init"},
	"vitest":        {"-u", "--update", "--outputFile", "--ui", "--api", "--open", "--inspect", "--inspect-brk"},
	// --basetemp removes the directory it names
	"pytest": {"--basetemp", "--junitxml", "--junit-xml", "--pastebin", "-o", "--override-ini", "--debug",
		"--log-file", "--report-log", "--result-log", "--cov-report", "--pdbcls"},
	"mypy": {"--python-executable", "--install-types", "--junit-xml", "--html-report", "--txt-report", "--xml-report",
		"--linecount-report", "--linecoverage-report", "--lineprecision-report", "--memory-xml-report",
		"--cobertura-xml-report", "--any-exprs-report"},
	"tox": {"-x", "--override", "--installpkg", "--devenv", "--discover", "--result-json", "--workdir", "--root"},
}

// goRefusedFlags run a program, load source from elsewhere or write a file.
// Go flags never bundle, and -flag and --flag are the same flag.
var goRefusedFlags = map[string][]string{
	"go": {"-exec", "-toolexec", "-vettool", "-overlay", "-modfile", "-o", "-coverprofile", "-cpuprofile",
		"-memprofile", "-blockprofile", "-mutexprofile", "-trace", "-outputdir", "-fuzz", "-update", "-pkgdir",
		"-testlogfile", "-gocoverdir", "-fuzzcachedir", "-artifacts", "-debug-actiongraph", "-debug-trace",
		"-debug-runtime-trace", "-http", "-gcflags", "-ldflags", "-asmflags", "-gccgoflags"},
	"gofmt":       {"-w", "-cpuprofile"},
	"staticcheck": {"-debug"},
}

// test and lint tools that only touch the project
// NOTE: a linter whose own config turns fixes on (ruff fix = true,
// golangci-lint fix: true) still writes; only the flags are checked.
var projectTools = map[string]bool{
	"pytest": true, "tox": true, "mypy": true, "eslint": true, "prettier": true,
	"vitest": true, "jest": true, "golangci-lint": true, "staticcheck": true,
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

// npm script names routinely carry a colon: test:unit.
var safeMakeTarget = regexp.MustCompile(`^(all|build[\w.:-]*|test[\w.:-]*|check[\w.:-]*|lint[\w.:-]*|vet|compile|bench[\w.:-]*|cover(age)?)$`)

// fixing and formatting targets rewrite sources (lint:fix)
var writingTarget = regexp.MustCompile(`fix|fmt|format|write`)

func safeTarget(t string) bool {
	return safeMakeTarget.MatchString(t) && !writingTarget.MatchString(t)
}

func safeSegment(a []string) bool {
	if len(a) == 0 {
		return true
	}
	name, rest := filepath.Base(a[0]), a[1:]
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch {
	case name == "uniq": // a second operand is the output file
		return len(slices.DeleteFunc(slices.Clone(rest), func(a string) bool { return strings.HasPrefix(a, "-") })) < 2
	case hasFlag(rest, refusedFlags[name]...), goFlag(rest, goRefusedFlags[name]...):
		return false
	case name == "cd": // with no directory or - it leaves the project; from .git/hooks go build writes a hook
		return len(rest) == 1 && !strings.HasPrefix(sub, "-") &&
			!slices.ContainsFunc(strings.Split(sub, "/"), func(p string) bool { return strings.EqualFold(p, ".git") })
	case name == "tox": // exec and devenv run what they are given, and -- reaches the commands
		return !slices.ContainsFunc(rest, func(a string) bool {
			return slices.Contains([]string{"--", "exec", "e", "devenv", "d", "quickstart", "q", "legacy", "le", "schema"}, a)
		})
	case name == "golangci-lint": // fmt, migrate and custom rewrite files or build code
		return slices.Contains([]string{"", "run", "linters", "formatters", "config", "version", "help"}, sub)
	case name == "jq":
		return !jqUnsafe.MatchString(strings.Join(rest, " "))
	case name == "jest": // --config, --transform and --haste take inline JSON naming modules to run
		return !slices.ContainsFunc(rest, func(a string) bool { return strings.Contains(a, "{") })
	case readOnlyPrograms[name], projectTools[name]:
		return true
	case name == "ruff": // format and check --fix rewrite files; after -- a flag is a path
		opts := rest
		if i := slices.Index(rest, "--"); i >= 0 {
			opts = rest[:i]
		}
		return sub == "check" || hasFlag(opts, "--check", "--diff")
	case name == "tsc":
		// it writes the compiled files next to the sources; options ignore
		// case and a boolean one takes a following false
		lower := make([]string, len(rest))
		for i, a := range rest {
			lower[i] = strings.ToLower(a)
		}
		return hasFlag(lower, "--noemit") && !slices.Contains(rest, "false") && !hasFlag(lower, "--init", "-b", "--build",
			"-i", "--incremental", "--composite", "--tsbuildinfofile", "--generatetrace", "--generatecpuprofile")
	case name == "sed":
		return sedSafe(rest)
	case name == "awk":
		return !awkUnsafe.MatchString(strings.Join(rest, " "))
	case name == "git":
		return safeGit(rest)
	// go run and cargo run execute whatever they are pointed at, so they ask
	case name == "go":
		switch sub {
		case "build", "test", "vet", "list", "version", "doc":
			return true
		case "mod":
			return len(rest) > 1 && (rest[1] == "graph" || rest[1] == "why" || rest[1] == "verify")
		case "env":
			return !hasFlag(rest, "-w", "-u")
		}
	case name == "cargo":
		switch sub {
		case "build", "check", "test", "clippy", "doc", "tree", "metadata", "bench":
			return true
		case "fmt":
			return hasFlag(rest, "--check")
		}
	case name == "npm" || name == "pnpm" || name == "yarn" || name == "bun":
		// flags after the command configure npm (--script-shell) or reach the
		// program a script runs (-- --fix)
		if slices.ContainsFunc(rest[min(1, len(rest)):], func(a string) bool { return strings.HasPrefix(a, "-") }) {
			return false
		}
		switch sub {
		case "test", "lint", "build", "typecheck", "ls", "list", "outdated", "why":
			return true
		case "run": // only the usual build and test scripts, like make
			return len(rest) > 1 && safeTarget(rest[1])
		}
	case name == "make":
		for _, t := range rest {
			if !strings.HasPrefix(t, "-") && !safeTarget(t) {
				return false
			}
		}
		return true
	case name == "python" || name == "python3":
		if sub == "--version" || sub == "-V" {
			return true
		}
		if sub == "-m" && len(rest) > 1 {
			switch rest[1] {
			case "pytest", "mypy":
				return !hasFlag(rest[2:], refusedFlags[rest[1]]...)
			case "unittest", "py_compile":
				return true
			case "json.tool": // a second operand is the output file
				return len(slices.DeleteFunc(slices.Clone(rest[2:]), func(a string) bool { return strings.HasPrefix(a, "-") })) < 2
			}
		}
	case name == "node":
		return len(rest) == 1 && (sub == "--version" || sub == "-v")
	}
	return false
}

// safeGit allows subcommands that read the repository (and fetch, which
// only updates remote-tracking refs).
func safeGit(args []string) bool {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "--no-pager", "--no-optional-locks":
			args = args[1:]
		case "-C":
			if len(args) < 2 {
				return false
			}
			args = args[2:]
		default: // -c runs configured programs, --exec-path and friends too
			return false
		}
	}
	// --upload-pack and friends name a program for git to run
	if len(args) == 0 || hasFlag(args, "--output", "--upload-pack", "--receive-pack", "--exec", "--exec-path", "--no-index") {
		return false
	}
	sub, rest := args[0], args[1:]
	next := ""
	if len(rest) > 0 {
		next = rest[0]
	}
	switch sub {
	// fetch is not here: it talks to the network and runs transport helpers
	case "grep": // -O names a program to open the matches with
		return !hasFlag(rest, "-O", "--open-files-in-pager", "--untracked")
	case "status", "diff", "log", "show", "blame", "describe", "rev-parse", "ls-files", "ls-tree",
		"shortlog", "cat-file", "merge-base", "rev-list", "show-ref", "for-each-ref",
		"name-rev", "count-objects":
		return true
	case "branch":
		for i := 0; i < len(rest); i++ {
			switch a := rest[i]; {
			case a == "--contains" || a == "--merged" || a == "--no-merged" || a == "--points-at":
				i++
			case !strings.HasPrefix(a, "-") || hasFlag([]string{a}, "-d", "-D", "-m", "-M", "-c", "-C", "-f", "--delete", "--move", "--copy", "--force", "--set-upstream-to", "-u", "--unset-upstream", "--edit-description"):
				return false
			}
		}
		return true
	case "tag":
		return len(rest) == 0 || slices.ContainsFunc(rest, func(a string) bool {
			return a == "--list" || strings.HasPrefix(a, "-l") || strings.HasPrefix(a, "-n")
		})
	case "remote": // -v may come before add or remove
		rest = slices.DeleteFunc(rest, func(a string) bool { return a == "-v" || a == "--verbose" })
		// show queries the remote, which may be a model-chosen host:path, unless -n
		return len(rest) == 0 || rest[0] == "get-url" ||
			rest[0] == "show" && slices.Contains(rest, "-n") && !slices.Contains(rest, "--")
	case "stash", "worktree":
		return next == "list" || (sub == "stash" && next == "show")
	case "reflog":
		return next == "" || next == "show"
	case "config":
		// only --local: other scopes and includes read ~/.gitconfig and
		// friends, which may hold tokens from outside the project
		return slices.Contains(rest, "--local") && !hasFlag(rest, "--global", "--system", "--file", "-f", "--blob", "--includes") &&
			slices.ContainsFunc(rest, func(a string) bool {
				return slices.Contains([]string{"--get", "--get-all", "--get-regexp", "--list", "-l"}, a)
			})
	case "submodule":
		return next == "" || next == "status"
	}
	return false
}

// gitContentDir returns the directory (the -C values) a git segment reads
// the repository from when it may print file content, and whether it may.
func gitContentDir(f []string) (string, bool) {
	if len(f) == 0 || filepath.Base(f[0]) != "git" {
		return "", false
	}
	dir, args := ".", f[1:]
	for len(args) > 1 && strings.HasPrefix(args[0], "-") {
		if args[0] == "-C" {
			dir, args = inDir(dir, args[1]), args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return "", false
	}
	sub, rest := args[0], args[1:]
	return dir, slices.Contains([]string{"grep", "show", "diff", "cat-file"}, sub) ||
		sub == "stash" && slices.Contains(rest, "show") ||
		(sub == "log" || sub == "reflog") && hasFlag(rest, "-p", "-u", "-U", "--patch", "--unified", "--binary", "-L", "--word-diff",
			"--color-words", "--cc", "-c", "--dd", "--diff-merges", "--remerge-diff", "--patch-with-stat", "--patch-with-raw")
}

// goFlag reports whether args set one of flags the way Go's flag package
// reads them: -flag and --flag alike, never bundled, and a test binary's
// -test.flag too. A dotted name (-debug.cpuprofile) sets its first part.
func goFlag(args []string, flags ...string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		f, _, _ := strings.Cut("-"+strings.TrimPrefix(strings.TrimLeft(a, "-"), "test."), "=")
		f, _, _ = strings.Cut(f, ".")
		return strings.HasPrefix(a, "-") && slices.Contains(flags, f)
	})
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
