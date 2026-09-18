// Package sandbox confines a command before it starts: seatbelt on macOS,
// Landlock plus seccomp on Linux. The session and process group proc relies
// on stay the command's own: it is exec'd in place, or with Linux namespaces
// runs under a helper init that shares them.
package sandbox

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/metafates/zemi/internal/perm"
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
// except Hide; only Write is writable, minus Protect.
type Policy struct {
	Write    []string          // writable roots
	Protect  []string          // read-only even inside Write
	Hide     []string          // neither readable nor writable
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

var probed struct {
	once sync.Once
	b    Backend
	err  error
}

// Probe reports the backend this machine supports, checked once.
func Probe() (Backend, error) {
	probed.once.Do(func() { probed.b, probed.err = probe() })
	return probed.b, probed.err
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
			if out[i] = perm.RealPath(path); out[i] == "" {
				out[i] = path
			}
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
	p.Write, p.Protect, p.Hide = real(p.Write), protect, real(p.Hide)
	return wrap(cmd, p)
}

// denied matches a denial as the backend's errors print it: the errno text
// after ": " or "] " (C, Go, Node, Python), where a test assertion rarely
// puts it, a failed DNS lookup, or the Linux helper's own failure.
var denied = regexp.MustCompile(`(?i)[:\]] (?:` + deniedErrnos + `)|could not resolve host|` + deniedLookups + `|zemi sandbox:`)

// deniedTail is how much of the output end LikelyDenied reads.
const deniedTail = 8 << 10

// LikelyDenied guesses from a failed command's output whether the sandbox
// stopped it.
// NOTE: a heuristic: a denial earlier than the last 8KB of output is missed,
// and a program's own EPERM reads as denied.
func LikelyDenied(out string, err error) bool {
	if err == nil {
		return false
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		switch ee.ExitCode() {
		case 126, 127: // not executable, not found
			return false
		}
	}
	return denied.MatchString(out[max(0, len(out)-deniedTail):] + "\n" + err.Error())
}

// network matches the half of a denial that a socket would fix: a failed
// lookup, a refused or unreachable connection, a TLS handshake that never
// started.
var network = regexp.MustCompile(`(?i)could not resolve host|` + deniedLookups +
	`|no such host|name resolution|network (?:is )?unreachable|connection refused|connection reset|` +
	`tls handshake|certificate|dial tcp|getaddrinfo|eai_again|socket: operation not permitted`)

// LikelyNetwork reports a failure the network tier would fix, so the hint
// can offer network: true before the full escape.
func LikelyNetwork(out string, err error) bool {
	if !LikelyDenied(out, err) {
		return false
	}
	return network.MatchString(out[max(0, len(out)-deniedTail):] + "\n" + err.Error())
}

// within reports whether p is root or below it, on the strings as given:
// policy paths are already clean and resolved. perm.Under is the version
// for paths that still need filepath treatment.
func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}
