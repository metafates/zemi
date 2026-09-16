// Package sandbox confines a command before it starts: seatbelt on macOS,
// Landlock plus seccomp on Linux. Both exec the command in place, so the
// pid, session and process group proc relies on stay the command's own.
package sandbox

import (
	"errors"
	"os/exec"
	"strings"
	"sync"

	"github.com/metafates/zemi/internal/perm"
)

// Policy is what a sandboxed command may touch. Everything is readable
// except Hide; only Write is writable, minus Protect.
type Policy struct {
	Write    []string // writable roots
	Protect  []string // read-only even inside Write
	Hide     []string // neither readable nor writable
	Loopback bool     // listen on and connect to localhost
}

type Backend struct {
	Name string
	// HidesSecretFiles says .env-shaped files are unreadable anywhere,
	// not only under Hide.
	HidesSecretFiles bool
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

// Wrap rewrites cmd to run under p. Dir, Env and SysProcAttr carry over.
func Wrap(cmd *exec.Cmd, p Policy) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	if _, err := Probe(); err != nil {
		return err
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
	p.Write, p.Protect, p.Hide = real(p.Write), real(p.Protect), real(p.Hide)
	return wrap(cmd, p)
}

var deniedWords = []string{
	"operation not permitted", "permission denied", "read-only file system", "seccomp",
	"sandbox", "landlock", "failed to write file", "network is unreachable", "could not resolve",
	"name resolution", "nodename nor servname", "getaddrinfo", "enotfound", "eai_again",
}

// LikelyDenied guesses from a failed command's output whether the sandbox
// stopped it.
// NOTE: a heuristic (codex's); a command that prints these words for its own
// reasons reads as denied.
func LikelyDenied(out string, err error) bool {
	if err == nil {
		return false
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		// usage error, not executable, not found
		switch ee.ExitCode() {
		case 2, 126, 127:
			return false
		}
	}
	text := strings.ToLower(out + "\n" + err.Error())
	for _, w := range deniedWords {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// within reports whether p is root or below it.
func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}
