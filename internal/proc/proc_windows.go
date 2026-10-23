package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// NOTE: Windows kills only the process itself; children of a shell survive
// until a job object is used.
func setSession(*exec.Cmd) {}

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// Shell is the shell that runs the model's command lines: a Git for Windows
// bash where there is one, else pwsh, else cmd, which knows nothing POSIX.
// System32's bash.exe is the WSL launcher, whose /mnt paths
// do not match zemi's, so it is refused. Looked up once, on PATH like every
// program zemi runs by name.
var Shell = sync.OnceValue(func() string {
	system32 := filepath.Join(os.Getenv("SystemRoot"), "System32") + string(os.PathSeparator)
	for _, name := range []string{"sh", "bash"} {
		if path, err := exec.LookPath(name); err == nil && !strings.HasPrefix(strings.ToLower(path), strings.ToLower(system32)) {
			return path
		}
	}
	for _, dir := range gitRoots() {
		path := filepath.Join(dir, "bin", "bash.exe")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh"
	}
	return "cmd"
})

// gitRoots are where Git for Windows installs itself.
func gitRoots() []string {
	var out []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if v := os.Getenv(env); v != "" {
			out = append(out, filepath.Join(v, "Git"))
		}
	}
	if v := os.Getenv("LocalAppData"); v != "" {
		out = append(out, filepath.Join(v, "Programs", "Git"))
	}
	return out
}

// ShellCommand is Command running line with Shell.
func ShellCommand(line string) *exec.Cmd {
	return ShellCommandContext(context.Background(), line)
}

// ShellCommandContext is ShellCommand cancelled as a group when ctx is.
func ShellCommandContext(ctx context.Context, line string) *exec.Cmd {
	s := Shell()
	if s == "cmd" {
		return CommandContext(ctx, s, "/c", line)
	}
	if s == "pwsh" {
		// NOTE: -Command exits 0 after a failed native command, so the tail
		// hands the real exit code back; the newline keeps a trailing
		// comment from eating it.
		return CommandContext(ctx, s, "-NoProfile", "-NonInteractive", "-Command",
			line+"\nif (-not $?) { exit 1 }; exit $LASTEXITCODE")
	}
	return CommandContext(ctx, s, "-c", line)
}

// SymlinkDir makes newname a directory link to oldname. A stock Windows
// account lacks the symlink privilege, so it falls back to a junction,
// which needs none but stores an absolute target.
func SymlinkDir(oldname, newname string) error {
	err := os.Symlink(oldname, newname)
	if !errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		return err
	}
	target := oldname
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(newname), target)
	}
	c := Command("cmd", "/c", "mklink", "/J", newname, target)
	var errb strings.Builder
	c.Stderr = &errb
	if err := Run(c); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}
