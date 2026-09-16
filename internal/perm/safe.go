package perm

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// secretPathShape matches secret-shaped filesystem paths (§9); reads ask.
var secretPathShape = []string{".env", ".env.*", ".envrc", "*.pem", "*.key", "id_rsa*", "*credential*", "*secret*", ".netrc"}

func PathIsSecret(p string) bool {
	base := strings.ToLower(filepath.Base(p))
	return slices.ContainsFunc(secretPathShape, func(pat string) bool {
		ok, _ := filepath.Match(strings.ToLower(pat), base)
		return ok
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
		// git reads rev:path from the tree, so the path after the colon counts too
		_, sub, rev := strings.Cut(a, ":")
		for _, d := range dirs {
			if !pathOK(inDir(d, a), use) || (rev && sub != "" && !pathOK(inDir(d, sub), use)) {
				return false
			}
		}
		return true
	}
	for _, seg := range n.Segments {
		f := strings.Fields(seg)
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
		if filepath.Base(f[0]) == "cd" {
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
	return filepath.Join(dir, a)
}

// argUses says how a checked segment's program uses its arguments by
// position among the operands, and whether it searches the working
// directory when it has no operand after the pattern.
func argUses(f []string) (use func(pos int) ArgUse, walkCwd bool) {
	name, rest := filepath.Base(f[0]), f[1:]
	switch {
	// globs override rg's ignore rules, so they walk everything too
	case name == "rg" && !hasFlag(rest, "-u", "-.", "--unrestricted", "--hidden", "--no-ignore", "--no-ignore-dot",
		"--no-ignore-exclude", "--no-ignore-files", "--no-ignore-global", "--no-ignore-parent", "--no-ignore-vcs",
		"-g", "--glob", "--iglob"):
		return walkFrom(1, ArgSearch), true
	case name == "rg", name == "ag", (name == "grep" || name == "egrep" || name == "fgrep") &&
		hasFlag(rest, "-r", "-R", "--recursive", "--dereference-recursive", "-d", "--directories"):
		return walkFrom(1, ArgWalk), true
	case name == "diff":
		return walkFrom(0, ArgWalk), false
	case name == "cd", !readOnlyPrograms[name] && name != "sed" && name != "awk" && name != "uniq":
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
			word := cmd[i+1:]
			if end := strings.IndexAny(word, " \t\n;&|<>()"); end >= 0 {
				word = word[:end]
			}
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
// variable, run a program it names or load code from a file.
var refusedFlags = map[string][]string{
	"sort":          {"-o", "--output", "--compress-program"},
	"gofmt":         {"-w"},
	"printf":        {"-v"},
	"tree":          {"-o", "-R"},
	"file":          {"-C", "--compile"},
	"fd":            {"-x", "-X", "--exec", "--exec-batch"},
	"rg":            {"--pre"},
	"ag":            {"--pager"},
	"find":          {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls"},
	"sed":           {"-i", "--in-place", "-f", "--file"},
	"awk":           {"-f", "--file", "-i", "--include", "-l", "--load", "-E", "--exec"},
	"jq":            {"-f", "--from-file", "-L", "--library-path"},
	"make":          {"-f", "--file", "--makefile", "-C", "--directory", "-E", "--eval"},
	"cargo":         {"--config", "--fix", "--out-dir", "--artifact-dir", "--open"},
	"prettier":      {"-w", "--write"},
	"eslint":        {"--fix"},
	"ruff":          {"--fix", "--unsafe-fixes"},
	"golangci-lint": {"--fix"},
	"jest":          {"-u", "--updateSnapshot"},
	"vitest":        {"-u", "--update"},
	"tsc":           {"--init"},
}

// goRefusedFlags run a program, load source from elsewhere or write a file.
// Go flags never bundle, and -flag and --flag are the same flag.
var goRefusedFlags = []string{"-exec", "-toolexec", "-vettool", "-overlay", "-modfile", "-o", "-coverprofile",
	"-cpuprofile", "-memprofile", "-blockprofile", "-mutexprofile", "-trace", "-outputdir", "-fuzz", "-update"}

// test and lint tools that only touch the project
// NOTE: a linter whose own config turns fixes on (ruff fix = true,
// golangci-lint fix: true) still writes; only the flags are checked.
var projectTools = map[string]bool{
	"pytest": true, "tox": true, "mypy": true, "eslint": true, "prettier": true,
	"vitest": true, "jest": true, "golangci-lint": true, "staticcheck": true,
}

// sedFileCmd matches sed commands that write or read a file or run one: w
// and r (`w file`, the no-space `w/tmp/out` GNU accepts, after an address
// like 1w or $r, or as the s///w flag) and e. The character after w or r
// must not be a letter, or every path holding a word like "web" would read
// as a write.
// NOTE: `rsecret.pem` (a letter right after r) still reads a file inside
// the project; telling it from a path needs a real sed script parser.
var sedFileCmd = regexp.MustCompile(`(^|[;{}\s/0-9$,!])([wWrR](\s+\S|[/.])|e($|[\s;}]))`)

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
	case hasFlag(rest, refusedFlags[name]...):
		return false
	case name == "cd": // with no directory or - it leaves the project
		return len(rest) == 1 && !strings.HasPrefix(sub, "-")
	case name == "jq":
		return !jqUnsafe.MatchString(strings.Join(rest, " "))
	case readOnlyPrograms[name], projectTools[name]:
		return true
	case name == "ruff": // format and check --fix rewrite files
		return sub == "check" || hasFlag(rest, "--check", "--diff")
	case name == "tsc": // it writes the compiled files next to the sources
		return hasFlag(rest, "--noEmit")
	case name == "sed":
		// quotes are gone by now, so the script and the file names are one
		// string; sedFileCmd is written to tell a w command from a path
		return !sedFileCmd.MatchString(strings.Join(rest, " "))
	case name == "awk":
		return !awkUnsafe.MatchString(strings.Join(rest, " "))
	case name == "git":
		return safeGit(rest)
	// go run and cargo run execute whatever they are pointed at, so they ask
	case name == "go":
		if slices.ContainsFunc(rest, func(a string) bool {
			f, _, _ := strings.Cut("-"+strings.TrimLeft(a, "-"), "=")
			return strings.HasPrefix(a, "-") && slices.Contains(goRefusedFlags, f)
		}) {
			return false
		}
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
		if hasFlag(rest, "--script-shell", "--node-options") {
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
			case "pytest", "unittest", "py_compile", "mypy":
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
	case "remote":
		return next == "" || next == "-v" || next == "show" || next == "get-url"
	case "stash", "worktree":
		return next == "list" || (sub == "stash" && next == "show")
	case "reflog":
		return next == "" || next == "show"
	case "config":
		return slices.ContainsFunc(rest, func(a string) bool {
			return slices.Contains([]string{"--get", "--get-all", "--get-regexp", "--list", "-l"}, a)
		})
	case "submodule":
		return next == "" || next == "status"
	}
	return false
}

// hasFlag reports whether any argument may set one of flags: the flag with
// or without =value, a short flag bundled or with a glued value (sed -ni,
// sort -uo), or an abbreviated long flag, which GNU tools and git accept
// (--in-pl). It errs toward yes, so it only refuses, never allows.
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
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
