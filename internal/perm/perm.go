// Package perm holds the pieces the permission gate and the file tools
// share: the subjects a request names, path resolution (RealPath, Path and
// its openat walk), the pad and temp roots nothing of the user's depends
// on, the glob a stub match uses, and the secret-shape matchers.
package perm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// The subjects the builtin tools ask about; the set is open (net.<host>,
// lua.<ns.call>, mcp.<server>).
const (
	SubjectShell    = "tool.shell"
	SubjectLua      = "tool.lua"
	SubjectWorkflow = "tool.workflow"
	SubjectRead     = "fs.read"
	SubjectWrite    = "fs.write"
)

const pluginPrefix = "plugin:"

// PluginPrincipal is the principal a plugin's calls and state keys run as.
func PluginPrincipal(name string) string { return pluginPrefix + name }

// CutPluginPrincipal returns the plugin a principal names.
func CutPluginPrincipal(p string) (string, bool) { return strings.CutPrefix(p, pluginPrefix) }

// CanonPath is the form file tools key by: absolute, symlinks and ..
// resolved as the kernel resolves them, and case folded where the
// filesystem folds.
// NOTE: a case-sensitive volume on darwin or windows folds two real files
// into one; a key for one then covers both.
func CanonPath(p string) string { return FoldCase(RealPath(p)) }

// TildePath shortens the home directory, or a path under it, to ~.
func TildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// ExpandHome spells a leading ~, $HOME or ${HOME} out.
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

// Under reports whether path is root or below it, and gives its path
// relative to root. Both must already be resolved; run RealPath first when
// either may hold a symlink. A real entry whose own name starts with ".."
// is inside, which a plain HasPrefix(rel, "..") test gets wrong.
func Under(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// FreeZone reports a path nothing of the user's depends on: inside the pad
// (a request's scratchpad, "" when it has none) or a native temp dir. The
// file tools read, write and delete there with no ask and no prior read.
func FreeZone(pad, path string) bool {
	if InPad(pad, path) {
		return true
	}
	real := CanonPath(path)
	for _, root := range TempRoots() {
		if _, ok := Under(root, real); ok {
			return true
		}
	}
	return false
}

// InPad reports path inside pad, both resolved, so a symlink in the pad
// cannot lead out.
func InPad(pad, path string) bool {
	real := CanonPath(path)
	_, ok := Under(CanonPath(pad), real)
	return ok && real != ""
}

// TempRoots are the machine's own temp dirs, this process's temp dir and
// /tmp, deduped and resolved (/tmp is /private/tmp on macOS).
func TempRoots() []string {
	var roots []string
	for _, r := range []string{os.TempDir(), "/tmp"} {
		if r = CanonPath(r); r != "" && !slices.Contains(roots, r) {
			roots = append(roots, r)
		}
	}
	return roots
}

// RealPath makes p absolute and resolves it one component at a time, so
// L/../x with L a symlink lands where open lands, not beside L. A missing
// component is joined lexically, so a file about to be created is judged by
// its real parent directory, and a later .. climbs back to resolving
// components that exist (new/../L/x). A symlink loop gives
// "", which is inside no root, and so does an empty path: it names nothing,
// where resolving it to the working directory would judge the project.
func RealPath(p string) string {
	if p == "" {
		return ""
	}
	sep := string(filepath.Separator)
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		// not filepath.Abs: it cleans .. before any symlink is seen
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		p = wd + sep + p
	}
	vol := filepath.VolumeName(p)
	real, rest := vol+sep, p[len(vol):]
	hops := 0
	for rest != "" {
		var c string
		c, rest, _ = strings.Cut(rest, sep)
		switch c {
		case "", ".":
			continue
		case "..":
			real = filepath.Dir(real)
			continue
		}
		next := filepath.Join(real, c)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			real = next
			continue
		}
		link, err := os.Readlink(next)
		if hops++; err != nil || hops > 255 {
			return ""
		}
		link = filepath.FromSlash(link)
		if filepath.IsAbs(link) {
			v := filepath.VolumeName(link)
			real, link = v+sep, link[len(v):]
		}
		rest = link + sep + rest
	}
	return real
}

// Path is an absolute path resolved as the kernel resolves it, the proof
// Open acts on: every directory component is resolved, and the last one too
// unless Lresolve made it, when Open refuses a link there. Only Resolve and
// Lresolve make one, and the zero value opens nothing.
type Path struct{ s string }

// Resolve resolves p with RealPath.
func Resolve(p string) (Path, error) {
	r := RealPath(p)
	if r == "" {
		return Path{}, fmt.Errorf("%s cannot be resolved", p)
	}
	return Path{r}, nil
}

// Lresolve is Resolve leaving the last component as named, like lstat(2):
// a symlink there is the link itself, which Open refuses and Remove deletes.
func Lresolve(p string) (Path, error) {
	sep := string(filepath.Separator)
	dir, name, ok := strings.CutLast(p, sep)
	if !ok {
		dir, name = ".", p
	}
	if name == "" || name == "." || name == ".." {
		return Resolve(p)
	}
	r, err := Resolve(dir + sep)
	if err != nil {
		return Path{}, err
	}
	return Path{filepath.Join(r.s, name)}, nil
}

func (p Path) String() string { return p.s }

// FoldCase folds p on darwin and windows, where the filesystem ignores case.
func FoldCase(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return fold(p)
	}
	return p
}

// fold compares names the way APFS does: full Unicode case folding over the
// canonical decomposition, so U+017F (long s) matches s and U+00E9 matches e+U+0301.
func fold(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool { return r >= utf8.RuneSelf }) {
		// lowercase last: Apache/Cherokee case pairs fold to each other
		// (U+AB83 <-> U+13B3), so folding alone would oscillate and leave no
		// stable canonical; ToLower lands both spellings on one representative
		return strings.ToLower(norm.NFC.String(cases.Fold().String(norm.NFD.String(s))))
	}
	return strings.ToLower(s)
}

// Match reports whether the glob pattern matches the whole of s: `*` spans
// any characters (separators included), `?` matches one. The one caller is
// a cold path (a plugin test's stub matcher), so no cache.
func Match(pattern, s string) bool {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return regexp.MustCompile(b.String()).MatchString(s)
}

// SecretPathShape matches secret-shaped filesystem names. The attach step
// refuses one, and a diff or a listing may name it rather than print it.
var SecretPathShape = []string{
	".env", "*.env", ".env.*", ".envrc", "*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore",
	"id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*",
	".netrc", ".npmrc", ".yarnrc.yml", ".pgpass", ".pypirc", ".htpasswd", ".my.cnf",
	"*credential*", "*secret*",
}

// PathIsSecret folds on every OS: a shape that flags one file too many is
// cheaper than a darwin spelling that slips past.
func PathIsSecret(p string) bool { return secretShaped(fold(filepath.Base(p)), SecretPathShape) }

// secretShaped takes a folded base name.
func secretShaped(base string, shapes []string) bool {
	return slices.ContainsFunc(shapes, func(pat string) bool {
		ok, _ := filepath.Match(pat, base)
		// an ssh public key is meant to be shared
		return ok && !(strings.HasPrefix(pat, "id_") && strings.HasSuffix(base, ".pub"))
	})
}

// EnvIsSecret reports whether an env var name looks like it holds a secret
// (case-insensitive); such variables reach Lua through zemi.env.get for the
// user's and zemi's own code only. PWD and PASS count only as a suffix
// after _, so PWD, OLDPWD and COMPASS pass.
func EnvIsSecret(name string) bool {
	up := strings.ToUpper(name)
	return strings.HasSuffix(up, "_PWD") || strings.HasSuffix(up, "_PASS") ||
		slices.ContainsFunc(secretEnvShape, func(s string) bool { return strings.Contains(up, s) })
}

var secretEnvShape = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "PASSPHRASE", "CREDENTIAL"}
