package perm

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestRowsCoverShells runs each command under every sh, dash and bash found,
// in an empty dir with a PATH of stubs that log their argv, and checks that
// each program a shell ran is a row: the parse never hides a command.
func TestRowsCoverShells(t *testing.T) {
	t.Parallel()
	cmds := []string{
		"git log # \\\nrm -rf a",
		"echo $'\\' ; rm -rf b ; #'",
		"echo $'\\'' ; rm -rf c ; #'",
		"[[ x == y || rm == rm ]]",
		"((rm -rf))",
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
		"cat <<< $(rm r)",
		"(cd sub && make) | tee log >/dev/null",
		"sudo -u root rm s; timeout 5 rm t",
		"rm u\\",
	}
	stubs := t.TempDir()
	names := map[string]bool{}
	for _, c := range cmds {
		for _, n := range regexp.MustCompile(`[a-z][a-z0-9-]*`).FindAllString(c, -1) {
			names[n] = true
		}
	}
	stub := "#!/bin/sh\n{ printf '%s\\037' \"${0##*/}\" \"$@\"; printf '\\036'; } >> \"$ZLOG\"\n[ \"${0##*/}\" = cat ] && [ $# = 0 ] && exec /bin/cat\nexit 0\n"
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
	for _, sh := range []string{"/bin/sh", "/bin/dash", "/bin/bash"} {
		if _, err := os.Stat(sh); err != nil {
			continue
		}
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
				if rec != "" && n.doubt == "" && !slices.ContainsFunc(n.Args, func(row []string) bool { return runs(row, argv) }) {
					t.Errorf("%s ran %q, which no row of %q names: %q", sh, argv, c, n.Args)
				}
			}
		}
	}
}

// runs reports that argv is what row, or the command a runner in it
// leads to, runs: a word holding an expansion stands for any words.
func runs(row, argv []string) bool {
	for k := range row {
		if matches(row[k:], argv) {
			return true
		}
	}
	return false
}

func matches(row, argv []string) bool {
	if len(row) == 0 {
		return len(argv) == 0
	}
	if strings.ContainsAny(row[0], "$`*?[{~") {
		for i := range len(argv) + 1 {
			if matches(row[1:], argv[i:]) {
				return true
			}
		}
		return false
	}
	return len(argv) > 0 && row[0] == argv[0] && matches(row[1:], argv[1:])
}
