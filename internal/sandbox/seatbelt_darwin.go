package sandbox

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// sandboxExec is never looked up on PATH: a planted one would run unconfined.
const sandboxExec = "/usr/bin/sandbox-exec"

// seatbelt denies with EPERM; a blocked lookup fails in getaddrinfo
const (
	deniedErrnos  = `operation not permitted`
	deniedLookups = `nodename nor servname`
)

// nestedShapes is a .git or .zemi path component below root; APFS folds
// case, the regex does not. The root itself may sit under one (a worktree in
// .zemi/worktrees), so the match starts after it.
func nestedShapes(root string) string {
	anchor := "^" + regexp.QuoteMeta(root)
	if strings.ContainsAny(root, "\"\n") {
		anchor = "" // unquotable: match anywhere, which only denies more
	}
	return `(regex #"` + anchor + `(/.*)?/\.([gG][iI][tT]|[zZ][eE][mM][iI])(/|$)")`
}

//go:embed profile.sb
var baseProfile string

// secretShapes are the strict shapes of perm.SecretPathShape. The broad
// *secret* and *credential* stay in the ask layer: Go's own stdlib has
// runtime/secret.go, so a kernel rule for them breaks go build.
const secretShapes = `(regex #"/\.env(\.[^/]*)?$" #"/\.envrc$" #"/\.netrc$" #"/\.npmrc$" #"/\.yarnrc\.yml$" #"/\.pgpass$" #"/\.pypirc$" #"/\.htpasswd$" #"/\.my\.cnf$" #"\.(env|pem|key|p12|pfx|jks|keystore)$" #"/id_(rsa|dsa|ecdsa|ed25519)$")`

func probe() (Backend, error) {
	if _, err := os.Stat(sandboxExec); err != nil {
		return Backend{}, err
	}
	// deny default: an allow-default profile applies even inside another sandbox
	err := exec.Command(sandboxExec, "-p", "(version 1)(deny default)(allow process-exec)(allow file-read*)", "/usr/bin/true").Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 71 {
		return Backend{}, errors.New("zemi already runs inside a sandbox, which cannot be nested")
	}
	if err != nil {
		return Backend{}, fmt.Errorf("sandbox-exec: %w", err)
	}
	return Backend{Name: "seatbelt", HidesSecretFiles: true, EnforcesProtect: true}, nil
}

func wrap(cmd *exec.Cmd, p Policy) error {
	prof, defs := profile(p)
	args := append([]string{sandboxExec, "-p", prof}, defs...)
	cmd.Args = append(append(args, "--", cmd.Path), cmd.Args[1:]...)
	cmd.Path = sandboxExec
	return nil
}

// profile renders p as seatbelt rules. Paths go in as -D params, so the
// profile text never needs escaping.
func profile(p Policy) (string, []string) {
	var b strings.Builder
	var defs []string
	param := func(kind string, i int, path string) string {
		name := fmt.Sprintf("%s_%d", kind, i)
		defs = append(defs, "-D"+name+"="+path)
		return fmt.Sprintf("(param %q)", name)
	}
	b.WriteString(baseProfile)
	switch {
	case p.Network:
		// the file rules still hold, so this is a socket, not the machine; IP
		// only, since a unix socket (docker.sock, ssh-agent) is the machine.
		// mDNSResponder answers DNS and trustd verifies TLS.
		b.WriteString("(allow network-outbound (remote ip))\n(allow network-bind network-inbound (local ip))\n")
		b.WriteString("(allow network-outbound (remote unix-socket (path-literal \"/private/var/run/mDNSResponder\")))\n")
		b.WriteString("(allow mach-lookup (global-name \"com.apple.trustd\" \"com.apple.trustd.agent\"))\n")
	case p.Loopback:
		// NOTE: seatbelt's localhost also matches this machine's LAN addresses,
		// so a service bound to them is reachable too.
		b.WriteString("(allow network-bind network-inbound (local ip \"localhost:*\"))\n")
		b.WriteString("(allow network-outbound (remote ip \"localhost:*\"))\n")
	}
	// the last matching rule wins, so rules go shallow to deep (a nested path
	// is longer): a root inside a protected or hidden dir, like the scratchpad
	// under the state dir, is open again, and a protected or hidden path inside
	// a root stays shut. At the same path Hide beats Protect beats Write.
	type rule struct{ path, text string }
	var rules []rule
	for i, w := range p.Write {
		rules = append(rules, rule{w, "(allow file-read* file-write* (subpath " + param("WRITE", i, w) + "))"})
	}
	for i, pr := range p.Protect {
		rules = append(rules, rule{pr, "(deny file-write* (subpath " + param("PROTECT", i, pr) + "))"})
	}
	for i, h := range p.Hide {
		rules = append(rules, rule{h, "(deny file-read* file-write* (subpath " + param("HIDE", i, h) + "))"})
	}
	slices.SortStableFunc(rules, func(a, b rule) int { return len(a.path) - len(b.path) })
	for _, r := range rules {
		b.WriteString(r.text + "\n")
	}
	// realpath and cargo stat every parent of a root, so the hidden parents of
	// a root (the state dir over the sandbox cache) give back their metadata
	// only: listing them and reading what else they hold stays denied.
	// Landlock has no right for stat or path walks, so Linux needs none of this.
	var parents []string
	for _, w := range p.Write {
		for d := filepath.Dir(w); ; d = filepath.Dir(d) {
			if slices.ContainsFunc(p.Hide, func(h string) bool { return within(d, h) }) {
				parents = append(parents, d)
			}
			if d == "/" || d == "." {
				break
			}
		}
	}
	slices.Sort(parents)
	for i, d := range slices.Compact(parents) {
		fmt.Fprintf(&b, "(allow file-read-metadata (literal %s))\n", param("PARENT", i, d))
	}
	// renaming a root or a protected path's parent would move the protection away
	keep := slices.Clone(p.Write)
	for _, pr := range p.Protect {
		for d := filepath.Dir(pr); slices.ContainsFunc(p.Write, func(w string) bool { return within(d, w) }); d = filepath.Dir(d) {
			keep = append(keep, d)
			if d == "/" {
				break
			}
		}
	}
	slices.Sort(keep)
	for i, d := range slices.Compact(keep) {
		fmt.Fprintf(&b, "(deny file-write-unlink (require-all (literal %s) (vnode-type DIRECTORY)))\n", param("KEEP", i, d))
	}
	// A nested .git or .zemi runs what it names once git or zemi starts in
	// that dir outside the sandbox. Only roots holding a protected path (the
	// project) get this, so tests can still git init in the temp dirs.
	// NOTE: a bare repo (HEAD, objects, refs, config) is found by git under
	// any name and cannot be matched here.
	// A root inside another root's protected path (.zemi/worktrees) is opened
	// again after that root's rule, shallow to deep, and gets its own.
	roots := slices.Clone(p.Write)
	slices.SortStableFunc(roots, func(a, b string) int { return len(a) - len(b) })
	for i, w := range roots {
		hole := slices.ContainsFunc(p.Protect, func(pr string) bool {
			return within(w, pr) && slices.ContainsFunc(roots, func(o string) bool { return o != w && within(pr, o) })
		})
		if hole {
			fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", param("NESTED", i, w))
		}
		if hole || slices.ContainsFunc(p.Protect, func(pr string) bool { return pr != w && within(pr, w) }) {
			fmt.Fprintf(&b, "(deny file-write* (require-all (subpath %s) %s))\n", param("NESTED", i, w), nestedShapes(w))
		}
	}
	b.WriteString("(deny file-read* file-write* " + secretShapes + ")\n")
	if p.Network { // CA bundles end in .pem
		b.WriteString(`(allow file-read* (subpath "/private/etc/ssl") (regex #"/certifi/cacert\.pem$" #"^/(opt/homebrew|usr/local)/etc/(ca-certificates|openssl@3)/cert\.pem$"))` + "\n")
	}
	return b.String(), defs
}
