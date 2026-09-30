// Package sandbox confines a command before it starts: seatbelt on macOS,
// Landlock plus seccomp on Linux. The session and process group proc relies
// on stay the command's own: it is exec'd in place, or with Linux namespaces
// runs under a helper init that shares them.
package sandbox

import (
	"cmp"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"unicode"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
)

// helperArg is the argv[0]+1 a re-exec'd helper answers to.
const helperArg = "__sandbox"

// DispatchHelper exits as the sandbox helper when this process was re-exec'd
// as one. Every binary the probe or Wrap may re-exec (zemi and the test
// binaries) calls it first; one that does not reports no backend.
func DispatchHelper() {
	if len(os.Args) > 2 && os.Args[1] == helperArg {
		os.Exit(helper(os.Args[2:]))
	}
}

// Policy is what a sandboxed command may touch. Everything is readable
// except Hide, where Read and List open parts again; only Write is
// writable, minus Protect.
type Policy struct {
	Write    []string          // writable roots
	Protect  []string          // read-only even inside Write
	Git      string            // a git dir in Protect writable again but for what git runs or follows; seatbelt only
	Hide     []string          // neither readable nor writable
	Read     []string          // readable inside Hide, with all they hold but a deeper Hide
	List     []string          // directories inside Hide whose names are readable, not what they hold
	Loopback bool              // listen on and connect to localhost
	Network  bool              // reach the network: the file rules still apply
	Env      map[string]string // extra environment for the sandboxed command
}

type Backend struct {
	Name string
	// HidesSecretFiles says .env-shaped files are unreadable anywhere,
	// not only under Hide.
	HidesSecretFiles bool
	// Protect paths stay read-only inside Write roots
	EnforcesProtect bool
}

var probeOnce = sync.OnceValues(probe)

// Probe reports the backend this machine supports, checked once processes
// may start (proc.Refuse).
func Probe() (Backend, error) {
	if err := proc.Refused(); err != nil {
		return Backend{}, err
	}
	return probeOnce()
}

// Wrap rewrites cmd to run under p. Dir, Env and SysProcAttr carry over,
// with p.Env set over Env.
func Wrap(cmd *exec.Cmd, p Policy) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	if _, err := Probe(); err != nil {
		return err
	}
	if len(p.Env) > 0 && cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// exec keeps the last of duplicate names
	for _, k := range slices.Sorted(maps.Keys(p.Env)) {
		cmd.Env = append(cmd.Env, k+"="+p.Env[k])
	}
	// the kernel matches resolved paths: /var is /private/var on macOS
	real := func(l []string) []string {
		out := make([]string, len(l))
		for i, path := range l {
			out[i] = cmp.Or(perm.RealPath(path), path)
		}
		return out
	}
	// a protected symlink keeps its target and the link itself, or the link
	// could be swapped for a real dir
	protect := real(p.Protect)
	for _, path := range p.Protect {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if dir := perm.RealPath(filepath.Dir(path)); dir != "" {
				protect = append(protect, filepath.Join(dir, filepath.Base(path)))
			}
		}
	}
	p.Write, p.Protect, p.Hide, p.Read, p.List = real(p.Write), protect, real(p.Hide), real(p.Read), real(p.List)
	if p.Git != "" {
		p.Git = cmp.Or(perm.RealPath(p.Git), p.Git)
	}
	return wrap(cmd, p)
}

// denied matches a denial as the backend's errors print it: the errno text
// after ": " or "] " (C, Go, Node, Python), where a test assertion rarely
// puts it, a failed DNS lookup, or the Linux helper's own failure.
var denied = regexp.MustCompile(`(?i)[:\]] (?:` + deniedErrnos + `)|could not resolve host|` + deniedLookups + `|zemi sandbox:`)

// deniedTail is how much of the output end LikelyDenied reads.
const deniedTail = 8 << 10

// LikelyDenied guesses from a command's output whether the sandbox stopped
// it. With no error only a crash or a denial naming a path counts, which a
// pipe (| head) leaves in the output of a command that exits 0.
// NOTE: a heuristic: a denial earlier than the last 8KB of output is missed,
// and a program's own EPERM reads as denied.
func LikelyDenied(out string, err error) bool {
	if err == nil {
		return Killed(out, nil) || len(DeniedPaths(out)) > 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		switch code := ee.ExitCode(); {
		case code == 126 || code == 127: // not executable, not found
			return false
		case Killed(out, err):
			return true
		case code > 1 && strings.TrimSpace(out) == "":
			// a silent failure (curl -s) has no text to match; exit 1 is
			// grep or test finding nothing
			return true
		}
	}
	return denied.MatchString(out[max(0, len(out)-deniedTail):] + "\n" + err.Error())
}

// Killed reports a command that died from a signal a confined program gets
// when a service it needs is denied, printing no path: the process itself,
// a child the shell reports by its 128+n exit, or the shell's report line,
// which a pipe (| tail) leaves in the output of a command that exits 0.
func Killed(out string, err error) bool {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		sig := syscall.Signal(ee.ExitCode() - 128)
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig = ws.Signal()
		}
		if sig > 0 && crashSignal.MatchString(sig.String()) {
			return true
		}
	}
	return shellReport.MatchString(out[max(0, len(out)-deniedTail):])
}

// crash is a signal a denial crashes a program with, as strsignal spells it
// on macOS and Linux; never a hangup (129 is git's usage exit), an interrupt
// or a term, which come from outside.
const crash = `(?:Abort trap|Aborted|Killed|Bad system call|Trace/BPT trap|Trace/breakpoint trap|Segmentation fault|Bus error|Illegal instruction)(?:: \d+)?(?: \(core dumped\))?`

var (
	crashSignal = regexp.MustCompile(`(?i)^` + crash + `$`)
	// dash's "Abort trap: 6", bash's "sh: line 1: 4242 Abort trap: 6   chrome"
	shellReport = regexp.MustCompile(`(?m)^(?:` + crash + `|.*: line \d+: +\d+ ` + crash + ` .*)$`)
)

// network matches the half of a denial that a socket would fix: a failed
// lookup, a refused or unreachable connection, a TLS handshake that never
// started.
var network = regexp.MustCompile(`(?i)could not resolve host|` + deniedLookups +
	`|no such host|name resolution|network (?:is )?unreachable|connection refused|connection reset|` +
	`tls handshake|certificate|dial tcp|getaddrinfo|eai_again|socket: operation not permitted`)

// LikelyNetwork reports a failure the network tier would fix, so the hint
// can offer network: true before the full escape.
func LikelyNetwork(out string, err error) bool {
	if err == nil || !LikelyDenied(out, err) {
		return false
	}
	return network.MatchString(out[max(0, len(out)-deniedTail):] + "\n" + err.Error())
}

// DeniedPaths are the path words on the output lines that carry a denial:
// a word holding a / or starting with a dot.
func DeniedPaths(out string) []string {
	var paths []string
	for line := range strings.Lines(out[max(0, len(out)-deniedTail):]) {
		if !denied.MatchString(line) {
			continue
		}
		for w := range strings.FieldsFuncSeq(line, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`:'"()`, r) }) {
			if strings.Contains(w, "/") || strings.HasPrefix(w, ".") {
				paths = append(paths, w)
			}
		}
	}
	return paths
}

// systemTemp matches the shared temp dirs, which the policy's own $TMPDIR
// replaces.
var systemTemp = regexp.MustCompile(`^(?:/private)?/(?:tmp|var/folders)(?:/|$)`)

// SystemTemp reports a path in the shared temp dirs.
func SystemTemp(path string) bool { return systemTemp.MatchString(path) }

// WriteNames names the Write roots as a sentence would ("the project,
// $TMPDIR and $GOCACHE"): "the project" for project, the Env variable that
// holds a root ($TMPDIR before the others), a path inside the project
// relative to it, else the path.
func (p Policy) WriteNames(project string) string {
	keys := slices.Sorted(maps.Keys(p.Env))
	names := make([]string, len(p.Write))
	for i, w := range p.Write {
		k := slices.IndexFunc(keys, func(k string) bool { return p.Env[k] == w })
		rel, err := filepath.Rel(project, w)
		switch {
		case w == project:
			names[i] = "the project"
		case p.Env["TMPDIR"] == w:
			names[i] = "$TMPDIR"
		case k >= 0:
			names[i] = "$" + keys[k]
		case project != "" && err == nil && filepath.IsLocal(rel):
			names[i] = rel
		default:
			names[i] = w
		}
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// within reports whether p is root or below it, on the strings as given:
// policy paths are already clean and resolved. perm.Under is the version
// for paths that still need filepath treatment.
func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}
