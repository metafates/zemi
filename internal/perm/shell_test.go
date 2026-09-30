package perm

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/proc"
)

// TestRowsCoverShells runs each command under proc.Shell, in an empty dir
// with a PATH of stubs that log their argv, and checks that each program it
// ran is a row of the parse: the parse never hides a command. A stub named
// for a shell runs the real one, so the bodies it hands on run too.
func TestRowsCoverShells(t *testing.T) {
	t.Parallel()
	cmds := []string{
		"git log # \\\nrm -rf a",
		"cat <<'EOF' | sh\nrm -rf d\nEOF",
		`echo "$(rm e)" "$(echo "f $(rm f)")"`,
		"echo `echo \\`rm g\\``",
		`x="$(case a in a) rm h ;; esac)"`,
		"echo $((1<<2))\nrm i",
		"cat <<EOF\n$(rm j)\nEOF",
		"cat <<'EOF'\n$(rm k)\nEOF",
		"echo $(true)#'\nrm l #'",
		"if grep -q x y; then ls; elif false; then :; else cat z; fi",
		"for f in a b; do rm \"$f\"; done; while false; do rm m; done",
		"f() { rm n; }; f",
		"D=./build/o; rm -rf $D",
		"sh -c 'rm p && echo q' && bash -c \"git push\"",
		"xargs rm < list; find . -exec rm {} +",
		"(cd sub && make) | tee log >/dev/null",
		"sudo -u root rm s; timeout 5 rm t",
		"sh <<EOF\necho \\`rm -rf z\\`\nEOF",
		"alias l=rm\nl -rf aa",
		// bash 3.2 ends the $( ) at the body's ), which proc.Shell does not
		"echo $(cat <<EOF\n) ; rm ab\nEOF\n)",
		"echo \"$(cat <<EOF\n) \" ; rm ac ; \"\nEOF\n)\"",
	}
	stubs := t.TempDir()
	names := map[string]bool{}
	for _, c := range cmds {
		for _, n := range regexp.MustCompile(`[a-z][a-z0-9-]*`).FindAllString(c, -1) {
			names[n] = true
		}
	}
	// one printf per record: a stub run in the background writes beside another
	stub := "#!/bin/sh\nr=$(printf '%s\\037' \"${0##*/}\" \"$@\")\nprintf '%s\\036' \"$r\" >> \"$ZLOG\"\n[ \"${0##*/}\" = cat ] && [ $# = 0 ] && exec /bin/cat\nexit 0\n"
	for n := range names {
		if err := os.WriteFile(filepath.Join(stubs, n), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, sh := range []string{"sh", "dash", "bash"} {
		if _, err := os.Stat("/bin/" + sh); err != nil {
			continue
		}
		// a stub named for a shell runs the real one, which runs the body
		os.Remove(filepath.Join(stubs, sh))
		os.Symlink("/bin/"+sh, filepath.Join(stubs, sh))
	}
	sh := proc.Shell()
	for _, c := range cmds {
		dir := t.TempDir()
		log := filepath.Join(dir, "log")
		cmd := exec.Command(sh, "-c", c)
		cmd.Dir, cmd.Env = dir, []string{"PATH=" + stubs, "HOME=" + dir, "ZLOG=" + log}
		cmd.Run()
		b, _ := os.ReadFile(log)
		n := Normalize(c)
		for rec := range strings.SplitSeq(strings.TrimSuffix(string(b), "\x1e"), "\x1e") {
			argv := strings.Split(strings.TrimSuffix(rec, "\x1f"), "\x1f")
			// a call that fails never runs
			if rec != "" && n.doubt == "" && n.Fail == "" && !slices.ContainsFunc(n.rows, func(r row) bool { return runs(r.args, argv) }) {
				t.Errorf("%s ran %q, which no row of %q names: %v", sh, argv, c, n.rows)
			}
		}
	}
}

// runs reports that argv is what row, or the command a runner leading it
// (sudo, timeout 5) hands on, runs.
func runs(row, argv []string) bool {
	for ok := true; ok && len(row) > 0; row, ok = stepRunner(row) {
		if matches(row, argv) {
			return true
		}
	}
	return false
}

// matches reports that row spells argv: a word holding an expansion stands
// for one word, a bare $@ or $* for any number.
func matches(row, argv []string) bool {
	switch {
	case len(row) == 0:
		return len(argv) == 0
	case row[0] == "$@" || row[0] == "$*":
		for i := range len(argv) + 1 {
			if matches(row[1:], argv[i:]) {
				return true
			}
		}
		return false
	case len(argv) == 0:
		return false
	case strings.ContainsAny(row[0], "$`*?[{~"):
		return matches(row[1:], argv[1:])
	}
	return row[0] == argv[0] && matches(row[1:], argv[1:])
}
