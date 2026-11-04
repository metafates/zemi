package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/proc"
)

// caseTimeout bounds one case in real time; the clock it reads is fake.
const caseTimeout = 5 * time.Second

// A plugin is code: a tree past these is a mistake, like a home dir.
const (
	copyFiles = 2000
	copyBytes = 32 << 20
)

// startsAtLine is a message that already names file:line.
var startsAtLine = regexp.MustCompile(`^[^\s:]+:\d+:`)

// testPlugins is zemi plugin test <name|dir> [pattern] (DESIGN §5): every
// case of the plugin's tests/*.lua whose "file: name" holds pattern
// runs in a zemi of its own, booted here from temp dirs that hold a copy of
// the plugin and nothing else. It prints the failures and a summary line and
// returns the exit status.
// NOTE: the plugin's skills, agents and workflows load, but only a model
// drives them, so no case can.
func testPlugins(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return testPluginsWithin(ctx, caseTimeout, args, stdout, stderr)
}

// testPluginsWithin is testPlugins with the per-case deadline its caller picks.
func testPluginsWithin(ctx context.Context, timeout time.Duration, args []string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "zemi: plugin test:", err)
		return 1
	}
	pattern := ""
	if len(args) == 2 {
		pattern = args[1]
	}
	src, err := pluginSource(args[0])
	if err != nil {
		return fail(err)
	}
	defer proc.Refuse()()
	tmp, err := os.MkdirTemp("", "zemi-test-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)
	if tmp, err = filepath.EvalSymlinks(tmp); err != nil {
		return fail(err)
	}
	if _, in := perm.Under(src, tmp); in {
		return fail(fmt.Errorf("the temp dir %s is inside the plugin %s", tmp, src))
	}
	name := filepath.Base(src)
	dirs := config.Dirs{ConfigHome: filepath.Join(tmp, "config"), DataHome: filepath.Join(tmp, "data"), CacheHome: filepath.Join(tmp, "cache")}
	copied := filepath.Join(dirs.PluginsRoot(), "local", name)
	if err := copyPlugin(src, copied); err != nil {
		return fail(err)
	}
	if _, err := hostapi.PluginDir(dirs, name); err != nil {
		return fail(fmt.Errorf("%s: zemi would not load it: %w", src, err))
	}
	files, _ := filepath.Glob(filepath.Join(copied, plugin.CodeDir, "tests", "*.lua"))
	if len(files) == 0 {
		fmt.Fprintf(stdout, "no %s/tests/*.lua in %s\n", plugin.CodeDir, src)
		return 1
	}

	boots := 0
	// boot starts a zemi of its own, its store and project fresh too
	boot := func(file string, run int) (*hostapi.PluginTest, func(), error) {
		boots++
		d := dirs
		d.StateHome = filepath.Join(tmp, "boot", strconv.Itoa(boots), "state")
		d.ProjectDir = filepath.Join(tmp, "boot", strconv.Itoa(boots), "project")
		h, pt := hostapi.NewPluginTest(slog.New(slog.DiscardHandler), pathClasses(d), d, file, run)
		end := func() {
			h.Close()
			os.RemoveAll(filepath.Dir(d.ProjectDir))
		}
		if err := os.MkdirAll(d.ProjectDir, 0o755); err != nil {
			return pt, end, err
		}
		idle := func() {}
		bctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		err := h.RT.Boot(bctx, bootSteps(h, mainCommands(idle, idle), nil, h.PluginsStep(), []lua.BootStep{pt.Step()}, false))
		if step, ok := errors.AsType[*lua.StepError](err); ok && errors.Is(bctx.Err(), context.DeadlineExceeded) {
			// Lua names where the deadline stopped it
			step.Err = errors.New(strings.Replace(step.Err.Error(), bctx.Err().Error(), "timed out after "+timeout.String(), 1))
		}
		return pt, end, err
	}

	start := time.Now()
	passed, failed, cases, ran := 0, 0, 0, 0
	for _, file := range files {
		rel := plugin.CodeDir + "/tests/" + filepath.Base(file)
		pt, end, err := boot(file, -1)
		declared := pt.Cases
		end()
		if ctx.Err() != nil {
			return 130
		}
		if err != nil {
			failed++
			if step, ok := errors.AsType[*lua.StepError](err); ok && step.Step == "plugins" {
				// every file would fail the same way
				printFail(stdout, plugin.InitFile, strings.TrimPrefix(bootError(pt, err), "plugin "+name+": "), nil)
				break
			}
			printFail(stdout, rel, bootError(pt, err), nil)
			continue
		}
		if len(declared) == 0 {
			failed++
			printFail(stdout, rel, `declares no case; code the tests share goes in a subdirectory (`+plugin.CodeDir+`/tests/lib/) and loads with require("./lib/<name>")`, nil)
			continue
		}
		cases += len(declared)
		for i, c := range declared {
			if !strings.Contains(rel+": "+c.Name, pattern) {
				continue
			}
			ran++
			pt, end, err := boot(file, i)
			var lines []string
			switch {
			case err != nil:
				lines = []string{bootError(pt, err)}
			case len(pt.Cases) <= i || pt.Cases[i].Name != c.Name:
				lines = []string{fmt.Sprintf("case %d is not %q in this boot: a case name must not depend on the run", i+1, c.Name)}
			default:
				lines = pt.Run(ctx, timeout)
			}
			end()
			if ctx.Err() != nil {
				return 130
			}
			if lines == nil {
				passed++
				continue
			}
			failed++
			printFail(stdout, fmt.Sprintf("%s:%d", rel, c.Line), c.Name, lines)
		}
	}
	switch {
	case ctx.Err() != nil:
		return 130
	case failed == 0 && ran == 0:
		fmt.Fprintf(stdout, "no case matches %q (%d %s in %d %s)\n", pattern, cases, hostapi.Plural(cases, "case", "cases"), len(files), hostapi.Plural(len(files), "file", "files"))
		return 1
	}
	summary := fmt.Sprintf("%d passed", passed)
	if failed > 0 {
		summary += fmt.Sprintf(", %d failed", failed)
	}
	fmt.Fprintf(stdout, "%s (%d %s, %.2fs)\n", summary, len(files), hostapi.Plural(len(files), "file", "files"), time.Since(start).Seconds())
	if failed > 0 {
		return 1
	}
	return 0
}

// bootError is why a boot stopped, its step's name and Lua's traceback of
// Go frames left out.
func bootError(pt *hostapi.PluginTest, err error) string {
	if step, ok := errors.AsType[*lua.StepError](err); ok {
		err = step.Err
	}
	msg, _, _ := strings.Cut(pt.Rel(err.Error()), "\nstack traceback:")
	return msg
}

// printFail writes one failure: FAIL, where and what, then each report line,
// its later lines indented deeper. A what that names its own file:line
// stands alone.
func printFail(w io.Writer, where, what string, lines []string) {
	what, more, _ := strings.Cut(what, "\n")
	if !startsAtLine.MatchString(what) {
		what = where + ": " + what
	}
	fmt.Fprintln(w, "FAIL "+what)
	if more != "" {
		lines = append([]string{more}, lines...)
	}
	for _, l := range lines {
		first, rest, _ := strings.Cut(l, "\n")
		fmt.Fprintln(w, "  "+first)
		if rest == "" {
			continue
		}
		for r := range strings.SplitSeq(rest, "\n") {
			fmt.Fprintln(w, "    "+r)
		}
	}
}

// pluginSource is the plugin directory arg names: a path, or a name the
// plugins step would load.
func pluginSource(arg string) (string, error) {
	dir := arg
	if !strings.ContainsRune(arg, filepath.Separator) && arg != "." && arg != ".." {
		dirs, err := config.ResolveDirs()
		if err != nil {
			return "", err
		}
		if dir, err = hostapi.PluginDir(dirs, arg); err != nil {
			return "", err
		}
	}
	dir, err := filepath.Abs(dir)
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return "", err
	}
	if old := plugin.OldLayout(dir); old != nil {
		return "", fmt.Errorf("%s: %w", dir, old)
	}
	if _, err := os.Stat(filepath.Join(dir, plugin.CodeDir)); err != nil {
		return "", fmt.Errorf("%s is no plugin: %w", dir, err)
	}
	return dir, nil
}

// copyPlugin copies the plugin at src to dst the way a test zemi should
// load it: its code (plugin.Code, plugin.Kept), a symbolic link only when it
// is relative and stays inside the plugin, at most copyFiles files and
// copyBytes bytes.
func copyPlugin(src, dst string) error {
	files, size := 0, int64(0)
	inside := func(p string) bool { _, ok := perm.Under(src, p); return ok }
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// the root's entries are the plugin's layout (plugin.Code); below them
		// the code is whole, minus what zemi hides anywhere (plugin.Kept)
		if p != src {
			atRoot := filepath.Dir(p) == src
			if atRoot && !plugin.Code(d.Name()) || !atRoot && !plugin.Kept(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if real, err := filepath.EvalSymlinks(p); err != nil || filepath.IsAbs(target) || !inside(real) || !inside(filepath.Join(filepath.Dir(p), target)) {
				return nil
			}
			return os.Symlink(target, to)
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if files, size = files+1, size+info.Size(); files > copyFiles || size > copyBytes {
			return fmt.Errorf("%s holds over %d files or %d MiB, which no plugin does", src, copyFiles, copyBytes>>20)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, info.Mode().Perm())
	})
}
