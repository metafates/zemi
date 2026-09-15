package perm

import (
	"path/filepath"
	"regexp"
	"strings"
)

// SafeCommand reports whether a bash command only reads, builds or tests:
// every segment runs an allowlisted program with harmless arguments, nothing
// is written through a redirect, and no command substitution runs. Auto mode
// approves these without asking.
func SafeCommand(cmd string) bool {
	for _, bad := range []string{"`", "$(", "<(", ">("} {
		if strings.Contains(cmd, bad) {
			return false
		}
	}
	n := Normalize(cmd)
	if n.Fail != "" || n.ForceAsk != "" || len(n.WritePaths) > 0 || len(n.Segments) == 0 {
		return false
	}
	for _, seg := range n.Segments {
		if !safeSegment(strings.Fields(seg)) {
			return false
		}
	}
	return true
}

var readOnlyPrograms = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "uniq": true, "cut": true,
	"tr": true, "pwd": true, "which": true, "whoami": true, "date": true, "echo": true,
	"printf": true, "true": true, "false": true, "test": true, "[": true, "file": true,
	"stat": true, "du": true, "df": true, "tree": true, "basename": true, "dirname": true,
	"realpath": true, "readlink": true, "jq": true, "diff": true, "cmp": true, "nl": true,
	"od": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "fd": true,
	"cd": true, "uname": true, "id": true, "hostname": true, "column": true, "comm": true,
	"paste": true, "fold": true, "sha256sum": true, "shasum": true, "md5sum": true, "type": true,
	"sleep": true, "seq": true,
}

// test and lint tools that only touch the project
var projectTools = map[string]bool{
	"pytest": true, "tox": true, "mypy": true, "ruff": true, "eslint": true, "prettier": true,
	"tsc": true, "vitest": true, "jest": true, "golangci-lint": true, "staticcheck": true,
}

var safeMakeTarget = regexp.MustCompile(`^(all|build|test[\w.-]*|check[\w.-]*|lint[\w.-]*|fmt|format|vet|compile|bench[\w.-]*|cover(age)?)$`)

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
	case name == "sort":
		return !hasFlag(rest, "-o", "--output")
	case readOnlyPrograms[name], projectTools[name]:
		return true
	case name == "find":
		return !hasFlag(rest, "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls")
	case name == "sed":
		return !hasFlag(rest, "-i", "--in-place") && !strings.Contains(strings.Join(rest, " "), "w ")
	case name == "awk":
		script := strings.Join(rest, " ")
		return !strings.ContainsAny(script, ">|") && !strings.Contains(script, "system")
	case name == "gofmt":
		return !hasFlag(rest, "-w")
	case name == "git":
		return safeGit(rest)
	case name == "go":
		switch sub {
		case "build", "test", "vet", "list", "version", "doc", "fmt", "run", "mod", "work":
			return true
		case "env":
			return !hasFlag(rest, "-w", "-u")
		}
	case name == "cargo":
		switch sub {
		case "build", "check", "test", "clippy", "fmt", "doc", "tree", "metadata", "run", "bench":
			return true
		}
	case name == "npm" || name == "pnpm" || name == "yarn" || name == "bun":
		switch sub {
		case "test", "run", "lint", "build", "typecheck", "ls", "list", "outdated", "why":
			return true
		}
	case name == "make":
		for _, t := range rest {
			if !strings.HasPrefix(t, "-") && !safeMakeTarget.MatchString(t) {
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
			case "pytest", "unittest", "json.tool", "py_compile", "mypy":
				return true
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
	if len(args) == 0 || hasFlag(args, "--output") {
		return false
	}
	sub, rest := args[0], args[1:]
	next := ""
	if len(rest) > 0 {
		next = rest[0]
	}
	switch sub {
	case "status", "diff", "log", "show", "blame", "describe", "rev-parse", "ls-files", "ls-tree",
		"grep", "shortlog", "cat-file", "merge-base", "rev-list", "show-ref", "for-each-ref", "fetch",
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
		return len(rest) == 0 || hasFlag(rest, "-l", "--list", "-n")
	case "remote":
		return next == "" || next == "-v" || next == "show" || next == "get-url"
	case "stash", "worktree":
		return next == "list" || (sub == "stash" && next == "show")
	case "reflog":
		return next == "" || next == "show"
	case "config":
		return hasFlag(rest, "--get", "--get-all", "--get-regexp", "--list", "-l")
	case "submodule":
		return next == "" || next == "status"
	}
	return false
}

// hasFlag reports whether any argument is one of flags, or starts with a
// flag followed by "=" or (for short flags) an attached value.
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") || (len(f) == 2 && strings.HasPrefix(a, f) && !strings.HasPrefix(a, "--")) {
				return true
			}
		}
	}
	return false
}
