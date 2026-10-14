package proc

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// gitTimeout bounds a git call whose context has no deadline of its own.
const gitTimeout = 15 * time.Second

// Git is one git command in a repository whose config cannot name a
// program: fsmonitor, hooks (an index refresh runs post-index-change),
// submodules, filter drivers, LFS smudging, signature checks in log, the
// system gitconfig and every transport but https, ssh and file are off.
// zemi's own checks need this in a trusted project too, since a shell
// command can plant a nested repository there, and a plugin clone is
// someone else's repository.
// NOTE: with a filter off, status compares a stat-dirty filtered file's raw
// bytes with the cleaned blob and lists it as modified (git-crypt, nbstripout).
type Git struct {
	Dir string
	Env []string // added to the environment
	// Foreign blanks the user's global filter drivers too, which zemi's own
	// checks keep so status sees what the user's git sees: a plugin's
	// .gitattributes must not run a program the user installed.
	Foreign bool
	CPU     *time.Duration // when set, gains the CPU time the call's processes took
}

// noFilter holds the commands that run no filter driver as zemi calls them,
// which skip the driver probe; cat-file would with --filters or --textconv.
var noFilter = map[string]bool{"cat-file": true, "config": true, "fetch": true, "ls-files": true, "rev-list": true, "rev-parse": true, "tag": true}

// Run runs git with args and returns its stdout; stderr goes only into the
// error. Without a deadline on ctx the call gets 15 seconds.
func (g Git) Run(ctx context.Context, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, gitTimeout)
		defer cancel()
	}
	safe := []string{
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
		"-c", "diff.ignoreSubmodules=all", "-c", "submodule.recurse=false",
		"-c", "log.showSignature=false",
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always", "-c", "protocol.file.allow=user",
	}
	if !noFilter[subcommand(args)] {
		out, _ := g.run(ctx, "config", "--show-scope", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process)$`)
		for line := range strings.Lines(out) {
			scope, key, ok := strings.Cut(strings.TrimSpace(line), "\t")
			if !ok || scope == "global" && !g.Foreign {
				continue
			}
			// a required driver left blank fails status outright (git lfs install --local)
			name, _, _ := strings.CutLast(key, ".")
			safe = append(safe, "-c", key+"=", "-c", name+".required=false")
		}
	}
	out, err := g.run(ctx, append(safe, args...)...)
	if err != nil {
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (g Git) run(ctx context.Context, args ...string) (string, error) {
	if err := Refused(); err != nil {
		return "", err
	}
	cmd := CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	// status leaves a repository's index alone
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	cmd.Env = append(cmd.Env, g.Env...)
	// a warning on stderr ("could not open directory") must not land in a
	// -z listing or a blob's content
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if g.CPU != nil && cmd.ProcessState != nil {
		*g.CPU += cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	}
	if err != nil {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), err
}

// subcommand is the git command args run, past leading -c options.
func subcommand(args []string) string {
	for i := 0; i < len(args); i += 2 {
		if args[i] != "-c" {
			return args[i]
		}
	}
	return ""
}
