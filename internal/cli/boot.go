// boot.go builds the runtime + host + UI over this run's dirs and assembles
// the boot steps.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/filelock"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/internal/ui"
	"github.com/metafates/zemi/runtime"
)

// launch opens the TUI.
func launch(o options) int {
	usage := func(msg string) int {
		usageHint(os.Stderr, "zemi", msg)
		return 2
	}
	if err := o.readStdin(os.Stdin); err != nil {
		return usage(err.Error())
	}
	// bubbletea draws to stdout: redirected, the TUI would run the agent unseen
	if !term.IsTerminal(os.Stdout.Fd()) {
		return usage("stdout is not a terminal; the TUI draws on one")
	}
	fail := func(err error) int {
		if le, ok := errors.AsType[*lockedError](err); ok {
			fmt.Fprintln(os.Stderr, "\n"+ui.Busy(os.Stderr, le.pid)+"\n")
			return 1
		}
		fmt.Fprintln(os.Stderr, "zemi:", err)
		return 1
	}
	ctx := context.Background()
	host, trust, log, closeLog, err := bootHost(o, false)
	if err != nil {
		return fail(err)
	}
	defer closeLog()
	app := ui.NewApp(host, log)
	host.SetUIHooks(app)
	trust.ask = app.AskTrust
	host.GoSafe("update check", func() { checkForUpdate(host) })

	steps := hostSteps(host, trust, app)
	p := tea.NewProgram(app)
	app.SetProgram(p)
	markConfig := setReload(host, steps, app)
	// stop ends the watchers below once the TUI returns, so a clean quit leaves nothing running.
	stop := make(chan struct{})

	// While the TUI is up only the renderer may write to the terminal: fd 2
	// goes to the log, so a panic's stack or a chatty library cannot smear
	// over the screen; restore hands the tty back for the lines below.
	restoreStderr := func() {}
	if restore, ok := stderrToLog(host.LogPath()); ok {
		restoreStderr = restore
		defer restore()
	}

	// Boot runs on its own goroutine (it may block on the trust prompt);
	// the outcome reaches the UI through the hook channel.
	go func() {
		// A panic during boot must reach the UI as a boot failure instead
		// of killing the process with the terminal still in raw mode.
		defer func() {
			if r := recover(); r != nil {
				log.Error(fmt.Sprintf("boot panicked: %v", r), "origin", "panic", "stack", string(debug.Stack()))
				app.SignalBoot(fmt.Errorf("boot panicked: %v (stack in %s)", r, host.Dirs.LogDir()))
			}
		}()
		err := host.RT.Boot(ctx, steps)
		app.SignalBoot(err)
		if err != nil {
			// /errors is where both the user and the agent look first
			host.RecordError("boot", "", err.Error())
			return
		}
		markConfig()
		host.KeepPicks.Store(true)
		// here, not in app.ready: once per process, never on a reload
		host.OfferModel()
	}()

	// Closing the terminal sends SIGHUP, which ends the process before the
	// cleanup below, and jobs and servers must never outlive zemi. SIGTERM is
	// not here: bubbletea quits on it and p.Run returns, which restores the
	// terminal and reaches the cleanup anyway.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	go func() {
		select {
		case <-sig:
			proc.KillAll()
			os.Exit(_interrupted)
		case <-stop:
		}
	}()

	_, err = p.Run()
	close(stop)
	// Children run in their own sessions (internal/proc), so the terminal
	// closing does not reach them: stop them here, and start none after (a
	// reload, an MCP retry, a plugin's job), which would outlive zemi. Exit
	// even if some cleanup hangs.
	proc.Refuse()
	proc.KillAll()
	// hand the terminal its normal pointer back (hover sets OSC 22); an error
	// that is not ErrProgramKilled means the program never started
	if err == nil || errors.Is(err, tea.ErrProgramKilled) {
		fmt.Print(ansi.SetPointerShape("default"))
	}
	time.AfterFunc(3*time.Second, func() { os.Exit(0) })
	if err != nil {
		// a panic bubbletea caught is in the log, not on the screen
		if errors.Is(err, tea.ErrProgramPanic) {
			err = fmt.Errorf("%w (stack in %s)", err, host.Dirs.LogDir())
		}
		return fail(err)
	}
	if host.DB != nil {
		host.DB.Touch()
	}
	// and the boot failure, which the TUI showed on a screen that is gone now
	if bootErr := app.BootError(); bootErr != nil {
		fmt.Fprintln(os.Stderr, "zemi: boot failed: "+bootErr.Error())
	}
	if dir, _ := app.RelaunchDir(); dir != "" {
		restoreStderr() // the exec inherits the tty, and a failed one prints here
		closeLog()      // an exec runs no defers
		return fail(relaunch(dir))
	}
	// the parting word that a newer zemi is out, every exit while it is
	if updatePending(host) {
		fmt.Println("\n" + ui.Chip(os.Stdout, "update", "a new version is available; run `zemi update`") + "\n")
	}
	return 0
}

// relaunch replaces this process with zemi -C dir. Every
// project fact (trust, origins, stores) is bound at start, so a
// switch is a restart, never a change in place; an exec keeps the PID, so the
// shell's wait and the terminal's foreground group do not change. The one
// start of a program outside internal/proc besides the editor
// probes: the children are reaped (proc.Reaped) and the terminal restored.
// NOTE: Windows has no exec; there it names the command to run instead.
func relaunch(dir string) error {
	args := []string{os.Args[0], "-C", dir}
	proc.Reaped()
	exe, err := os.Executable()
	if err == nil {
		err = syscall.Exec(exe, args, os.Environ())
	}
	return fmt.Errorf("could not start zemi in %s (%w): run zemi -C %s", dir, err, tools.ShellQuote(dir))
}

// newHost builds the runtime and host over dirs, and the trust gate their
// boot reads.
func newHost(dirs config.Dirs, log *slog.Logger, o options) (*hostapi.Host, *trustGate) {
	rt := lua.NewRuntime(lua.RuntimeOptions{
		Log:       log,
		PathClass: pathClasses(dirs),
		RuntimeFS: runtime.FS,
	})
	host := hostapi.NewHost(rt, log, config.NewStore(), dirs)
	catalog := agent.LoadCatalog(dirs.DataHome, os.Getenv)
	host.Builtin.Store(&catalog)
	rt.SetPrintHook(func(line string) {
		log.Info("print: "+line, "origin", "lua")
		host.RecordMessage("print", line)
	})
	host.Version = versionString()
	log.Info("zemi starting", "origin", "boot", "version", host.Version, "project", dirs.ProjectDir)
	if o.dir != "" {
		host.ResumeDir = dirs.ProjectDir // absolute: -C ran first
	}
	trust := &trustGate{h: host, dirs: dirs}
	if o.trust != nil {
		trust.session.Store(o.trust)
		host.SetProjectTrusted(*o.trust) // the flag's run decides at once; the step re-applies it
	}
	return host, trust
}

// hostSteps is the boot of a zemi; app is nil headless (zemi health, a
// completion), where nothing quits and a failing step says so on stderr.
func hostSteps(h *hostapi.Host, trust *trustGate, app *ui.App) []lua.BootStep {
	quit := func() {}
	if app != nil {
		quit = app.Quit
	}
	soft := func(s lua.BootStep) lua.BootStep { return softStep(h, app == nil, s) }
	return bootSteps(h, mainCommands(quit, trust.command()),
		[]lua.BootStep{soft(trust.step())},
		soft(h.PluginsStep()),
		[]lua.BootStep{
			soft(lua.LoadFileStep(_initStep, filepath.Join(h.Dirs.ConfigHome, "init.lua"), lua.OriginUser)),
			soft(h.SkillsUserHooksStep()),
		},
		app != nil,
	)
}

// bootHost is the prologue a TUI boot and a headless one (zemi health, a
// completion) share: this run's dirs, the log, the host and the trust gate
// its steps read. An inert boot (a completion) only reads
// config: no store, no log file, no process. closeLog releases the log, the
// one-instance lock and, for an inert boot, the process refusal.
func bootHost(o options, inert bool) (*hostapi.Host, *trustGate, *slog.Logger, func(), error) {
	dirs := o.dirs
	if !inert {
		if err := dirs.Ensure(); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	hostDirs, logDir, release := dirs, dirs.LogDir(), func() {}
	if inert {
		hostDirs.StateHome, logDir, release = "", "", proc.Refuse()
	}
	unlock := func() {}
	if !inert {
		u, err := lockInstance(dirs)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		unlock = u
	}
	log, level, path, closeLog := logging.New(logDir)
	host, trust := newHost(hostDirs, log, o)
	host.UseLogFile(level, path)
	host.GoSafe("boot sweep", host.SweepBoot)
	// the saved decision reads this run's dirs, which an inert boot did not open
	trust.dirs = dirs
	return host, trust, log, func() {
		host.Log.Info("zemi stopped")
		closeLog()
		release()
		unlock()
	}, nil
}

// lockedError is the one-instance refusal; the TUI's launch dresses it like
// a tip, the headless paths print its plain words.
type lockedError struct {
	pid int
	msg string
}

func (e *lockedError) Error() string { return e.msg }

// lockInstance holds this project's one-instance lock for the process's life:
// a second zemi on the same project names the holder and stops here. The
// lock lives on an open fd, so a zemi that dies releases it; the file beside
// the store carries the holder's pid for the refusal to name.
func lockInstance(dirs config.Dirs) (func(), error) {
	path := filepath.Join(filepath.Dir(store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot())), "zemi.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	unlock, err := filelock.Lock(path, 0)
	if err != nil {
		if !errors.Is(err, filelock.ErrLocked) {
			return nil, err
		}
		msg := "another zemi is already open on this project; close it to open here"
		pid := 0
		if raw, rerr := os.ReadFile(path); rerr == nil {
			if n, _ := strconv.Atoi(strings.TrimSpace(string(raw))); n > 0 {
				pid, msg = n, fmt.Sprintf("another zemi (pid %d) is already open on this project; close it to open here", n)
			}
		}
		return nil, &lockedError{pid: pid, msg: msg}
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	return unlock, nil
}

// bootHeadless boots zemi with no TUI, for zemi health and the
// flag completions; done stops what boot started (MCP and language servers)
// and closes the log. stderr gets its diagnostics. A non-inert boot applies
// the run's flags; an id no provider names fails the run, where the TUI
// would toast it.
func bootHeadless(o options, inert bool, stderr io.Writer) (*hostapi.Host, func(), error) {
	h, trust, _, closeLog, err := bootHost(o, inert)
	if err != nil {
		return nil, nil, err
	}
	h.Inert, h.Stderr = inert, stderr
	// a project with no saved decision runs untrusted instead of asking; the
	// health row and --trust say what that turns off
	trust.ask = headlessTrust()
	done := func() {
		proc.KillAll()
		h.Close() // the run's stores and runtime end with it
		closeLog()
	}
	steps := hostSteps(h, trust, nil)
	markConfig := func() {}
	if !inert {
		markConfig = setReload(h, steps, nil)
	}
	if err := h.RT.Boot(context.Background(), steps); err != nil {
		done()
		return nil, nil, err
	}
	markConfig()
	return h, done, nil
}

// pathClasses is where each origin's files live (DESIGN §5).
func pathClasses(dirs config.Dirs) lua.PathClasses {
	classes := lua.PathClasses{
		{Prefix: dirs.ConfigHome, Origin: lua.OriginUser},
		{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin},
		{Prefix: dirs.ProjectConfigDir(), Origin: lua.OriginProject},
	}
	for _, dir := range dirs.UserTrees() {
		classes = append(classes, lua.PathClass{Prefix: dir, Origin: lua.OriginUser})
	}
	return classes
}

// coreSteps is the head of the boot order (DESIGN §5): the namespaces and
// the builtin commands, before the plugins and the config.
func coreSteps(h *hostapi.Host, cmds []*hostapi.Command) []lua.BootStep {
	return []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(cmds...)}
}

// bootSteps is the boot order (DESIGN §5): trust is what runs before the
// plugins and config what runs after them, where init.lua does. A plugin
// test runs no trust and its test file as the config.
func bootSteps(h *hostapi.Host, cmds []*hostapi.Command, trust []lua.BootStep,
	plugins lua.BootStep, config []lua.BootStep, reflect bool,
) []lua.BootStep {
	return slices.Concat(
		coreSteps(h, cmds),
		trust, []lua.BootStep{plugins}, config,
		[]lua.BootStep{builderGateStep(h), h.MarkdownCommandsStep(), readyStep(h, reflect)},
	)
}

// builderGateStep releases the chat builder: config has run, so its
// compaction calls can resolve models. Go-only and unable to fail, so no
// soft wrapper; a reload replays it and the release is idempotent.
func builderGateStep(h *hostapi.Host) lua.BootStep {
	return lua.BootStep{
		Name: "builder gate",
		Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
			h.ReleaseBuilder()
			return nil
		},
	}
}

// setReload gives the host its reload of steps and the check that the
// config trees moved since the last one; app is nil headless. Call the
// returned mark once boot is done: before it, nothing reads as changed, and
// trust, which picks the trees, is not decided.
func setReload(h *hostapi.Host, steps []lua.BootStep, app *ui.App) (mark func()) {
	fingerprint := func() string {
		roots := append(append([]string{h.Dirs.ConfigHome}, h.Dirs.UserTrees()...), pluginRoots(h.Dirs.PluginsRoot())...)
		if h.ProjectTrusted() {
			return configFingerprint(h.Dirs.WorktreesDir(), append(roots, h.Dirs.ProjectConfigDir())...)
		}
		return configFingerprint("", roots...)
	}
	var last atomic.Pointer[string]
	mark = func() { last.Store(new(fingerprint())) }
	h.SetConfigMark(mark)
	h.SetReload(func() error {
		mark()
		if err := h.ForceReload(context.Background(), steps); err != nil {
			h.Log.Error("force reload failed", "origin", "reload", "error", err)
			h.RecordError("reload", "", err.Error())
			h.Notify(slog.LevelError, "Could not reload the config", err.Error())
			return err
		}
		if app != nil {
			app.SignalBoot(nil)
		}
		h.Notify(slog.LevelInfo, "Config reloaded", "")
		h.WarnModel()
		h.EmitAsync(hostapi.EventConfigReloaded, nil)
		return nil
	}, func() bool {
		prev := last.Load()
		return prev != nil && *prev != fingerprint()
	})
	return mark
}

// pluginRoots are what the watcher reads of the plugins tree: every local
// plugin whole, and for an install only the material behind its link, whose
// pull moves it - a marketplace clone's manifest and FETCH_HEAD (which every
// fetch rewrites, a code-only commit included) and each cache clone's
// FETCH_HEAD. marketplaces.json carries the rows, which installs and
// deletes move.
func pluginRoots(root string) []string {
	idx, _ := filepath.Glob(filepath.Join(root, "installed", "[^.]*", ".git", "index"))
	mkt, _ := filepath.Glob(filepath.Join(root, "marketplaces", "[^.]*", ".zemi", "marketplace.json"))
	claude, _ := filepath.Glob(filepath.Join(root, "marketplaces", "[^.]*", ".claude-plugin", "marketplace.json"))
	fetched, _ := filepath.Glob(filepath.Join(root, "marketplaces", "[^.]*", ".git", "FETCH_HEAD"))
	cache, _ := filepath.Glob(filepath.Join(root, "marketplaces", plugin.CacheDirName, "*", "*", ".git", "FETCH_HEAD"))

	return slices.Concat(
		idx,
		mkt,
		claude,
		fetched,
		cache,
		[]string{
			filepath.Join(root, "local"),
			filepath.Join(root, "disabled"),
			filepath.Join(root, "marketplaces.json"),
		},
	)
}

// configFingerprint summarizes config trees by path, size and modification
// time. Git data, dot-directories, node_modules, skip (the checkouts in
// .agents/worktrees) and the editor configs zemi writes itself are not
// config, and a plugin's dependency tree would make every walk cost
// milliseconds.
func configFingerprint(skip string, roots ...string) string {
	sum := sha256.New()
	if s, err := filepath.EvalSymlinks(skip); err == nil {
		skip = s // the roots are walked resolved
	}
	// resolved, so a symlinked config home or user tree (dotfiles) is walked:
	// WalkDir follows no link below its root
	var walk []string
	for _, r := range roots {
		if r == "" {
			continue // EvalSymlinks would make it "."
		}
		if r, err := filepath.EvalSymlinks(r); err == nil && !slices.Contains(walk, r) {
			walk = append(walk, r)
		}
	}
	// ignored says whether a walked directory holds nothing the fingerprint
	// reads: a dot-dir but a plugin's code dir, node_modules, the skip tree
	// or another walked root.
	ignored := func(d fs.DirEntry, p string) bool {
		return d.Name()[0] == '.' && d.Name() != plugin.CodeDir || d.Name() == "node_modules" || p == skip || slices.Contains(walk, p)
	}
	for _, root := range walk {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return nil
			case d.IsDir() && p != root && ignored(d, p):
				return filepath.SkipDir // a root is walked once
			case d.IsDir() || d.Name() == ".luarc.json" || d.Name() == ".emmyrc.json":
				return nil
			}
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				fmt.Fprintf(sum, "%s|%d|%d\n", p, info.Size(), info.ModTime().UnixNano())
			}
			return nil
		})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// mainCommands are the Go-owned slash commands that need the program's own
// hooks; the rest come from the host (hostapi.BuiltinCommandsStep).
func mainCommands(quit, trust func()) []*hostapi.Command {
	var efforts []string
	for _, e := range agent.Efforts {
		efforts = append(efforts, string(e))
	}
	efforts = append(efforts, "default")
	return []*hostapi.Command{
		{Name: "quit", Desc: "quit zemi", GoFn: func(*hostapi.Host, string, bool) error {
			quit()
			return nil
		}},
		{Name: "help", Desc: "list keys and commands", GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			return h.OpenHelp()
		}},
		{Name: "effort", Hidden: true, NArgs: hostapi.NArgsOptional, Desc: "set reasoning effort", GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			if args = strings.TrimSpace(args); args == "" {
				return h.OpenEffortPicker()
			}
			return h.SetConfig(config.KeyAgentEffort, args)
		}, GoComplete: func(*hostapi.Host, string, string) []string { return efforts }},
		{Name: "usage", Desc: "show tokens and cost", GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			return h.OpenUsage()
		}},
		{Name: "prompt", Desc: "show the system prompt the model gets", GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			return h.OpenPrompt()
		}},
		{Name: "logs", Desc: "show recent print and zemi.log lines", GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			return h.OpenLogs()
		}},
		{Name: "init", Desc: "write AGENTS.md for this project", GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			return h.InitProject()
		}},
		{Name: "compact", Desc: "compact the chat's context now", GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			h.CompactNow()
			return nil
		}},
		{Name: "copy", NArgs: hostapi.NArgsAny, Desc: "copy the last reply or one of its code blocks", GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			return h.CopyReply(args)
		}, GoComplete: hostapi.SubcommandCompleter([]string{"reply", "code"}, nil)},
		{Name: "reload", Hidden: true, Desc: "reload config now", GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			// the composer matches /reload before the registry; this
			// serves the palette, keymaps and zemi.command.run
			return h.ReloadConfig()
		}},
		{Name: "errors", NArgs: hostapi.NArgsAny, Desc: "show errors zemi recorded", GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			return h.OpenErrors(args)
		}},
		{Name: "trust", Desc: "change whether this project is trusted", GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			if _, err := h.Screen(); err != nil {
				return fmt.Errorf("%w; --trust trusts the project", err)
			}
			trust()
			return nil
		}},
		{Name: "health", Desc: "check zemi's setup", GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			return h.OpenHealth()
		}},
		{Name: "goal", NArgs: hostapi.NArgsAny, Desc: "keep working until a condition holds; with no argument, shows it", GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			return h.SetGoal(args)
		}, GoComplete: stopCompleter(func(h *hostapi.Host) bool { _, ok := h.Goal(); return ok })},
		{Name: "loop", NArgs: hostapi.NArgsAny, Desc: "run a prompt again on a timer; with no argument, shows it", GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			return h.SetLoop(args)
		}, GoComplete: stopCompleter(func(h *hostapi.Host) bool { _, ok := h.Loop(); return ok })},
		{Name: "btw", NArgs: hostapi.NArgsSome, Desc: "ask a side question the conversation does not keep", GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			return h.Btw(args)
		}},
		{Name: "model", NArgs: hostapi.NArgsAny, Desc: "choose the model, or one per role", GoFn: func(h *hostapi.Host, args string, confirm bool) error {
			return h.SetModel(args)
		}, GoComplete: (*hostapi.Host).CompleteModel},
	}
}

// stopCompleter offers the "stop" argument to a command while it is set.
func stopCompleter(set func(*hostapi.Host) bool) func(h *hostapi.Host, lead, line string) []string {
	return func(h *hostapi.Host, lead, line string) []string {
		if !set(h) {
			return nil
		}
		return hostapi.SubcommandCompleter([]string{"stop"}, nil)(h, lead, line)
	}
}

// _initStep is the user init.lua step's name, which softStep blames a broken
// config on (Host.SetInitError).
const _initStep = "init"

// softStep keeps a failing config step (a typo in init.lua, a broken plugin)
// from stopping boot: the error goes to the log and
// /errors, and the remaining steps and the runtime still start, like
// neovim reporting an init.lua error and opening anyway. Headless there is
// no /errors to open, so the error goes to stderr as well: otherwise a
// zemi health run reads green over a boot that failed to load its config.
func softStep(h *hostapi.Host, headless bool, s lua.BootStep) lua.BootStep {
	run := s.Run
	s.Run = func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
		err := run(ctx, r, g)
		if s.Name == _initStep {
			h.SetInitError("")
		}
		if err != nil {
			if s.Name == _initStep {
				h.SetInitError(hostapi.Headline(err.Error()))
			}
			h.Log.Error(s.Name+" failed", "origin", "boot", "error", err)
			h.RecordError("boot", s.Name, err.Error())
			if headless { // no /errors pill to notice
				fmt.Fprintln(h.Stderr, "zemi: "+s.Name+": "+hostapi.Headline(err.Error()))
			}
		}
		return nil
	}
	return s
}

// trustGate decides whether the user trusts the project (DESIGN §5): a
// decision held for this process (--trust, "not now"), then the saved one
// in the state dir, then the prompt. Trust is keyed by Dirs.TrustRoot.
type trustGate struct {
	h    *hostapi.Host
	dirs config.Dirs
	ask  func(root, cwd, state string) string
	// session outranks the saved decision; nil when there is none
	session atomic.Pointer[bool]
}

// step records the decision. It runs before plugins and user init.lua,
// which may already reach what it gates (project agents and workflows,
// language servers). MCP servers do not depend on it: they always start in
// DataHome with ZEMI_PROJECT_DIR set.
func (tg *trustGate) step() lua.BootStep {
	return lua.BootStep{
		Name: "project trust",
		Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
			trusted, err := tg.decide()
			tg.h.SetProjectTrusted(trusted)
			// .agents/worktrees/.gitignore, so git status never lists the
			// checkouts. An inert boot (a completion) writes nothing.
			if trusted && !tg.h.Inert {
				if _, werr := tg.dirs.WorktreesIgnore(); werr != nil {
					tg.h.Log.Warn("cannot write .agents/worktrees/.gitignore", "origin", "trust", "error", werr)
				}
			}
			return err
		},
	}
}

func (tg *trustGate) decide() (bool, error) {
	if s := tg.session.Load(); s != nil {
		return *s, nil
	}
	root := tg.dirs.TrustRoot()
	if v, ok := tg.dirs.ReadTrust()[root]; ok {
		return v == config.Trusted, nil
	}
	if hostapi.TrustUnsaved(root) {
		return false, nil
	}
	return tg.record(root, tg.ask(root, tg.dirs.ProjectDir, ""))
}

// record applies a prompt answer: trust and untrust are saved, "later" holds
// for this process only (a reload does not ask again, the next start does).
func (tg *trustGate) record(root, answer string) (bool, error) {
	ok := answer == ui.TrustYes
	if answer != ui.TrustLater && !hostapi.TrustUnsaved(root) {
		saved := tg.dirs.ReadTrust()
		mark := config.Untrusted
		if ok {
			mark = config.Trusted
		}
		saved[root] = mark
		err := tg.dirs.WriteTrust(saved)
		if err == nil {
			tg.session.Store(nil)
			return ok, nil
		}
		tg.session.Store(&ok)
		return ok, err
	}
	tg.session.Store(&ok)
	return ok, nil
}

// command is /trust: it always opens the prompt, so neither Lua nor the
// model can change trust without the user's keypress.
func (tg *trustGate) command() func() {
	return func() {
		// the prompt blocks until answered: off the UI goroutine
		tg.h.GoSafe("trust", func() {
			root, was := tg.dirs.TrustRoot(), tg.h.ProjectTrusted()
			mark := config.Untrusted
			if was {
				mark = config.Trusted
			}
			answer := tg.ask(root, tg.dirs.ProjectDir, mark)
			if answer == ui.TrustLater {
				return
			}
			ok, err := tg.record(root, answer)
			if err != nil {
				tg.h.Notify(slog.LevelError, "Could not save the trust decision", err.Error())
			}
			tg.h.Retrust(ok)
		})
	}
}

// readyStep drops the builtin providers when init.lua turned them off
// (config has run by now, and a replay re-seeds them first), names an
// override no tool answers to, and emits app.ready.
func readyStep(h *hostapi.Host, reflect bool) lua.BootStep {
	// once per process: a reload replays the step
	updateLast := sync.OnceFunc(func() {
		h.GoSafe("plugin auto-update", func() {
			ctx := context.WithoutCancel(context.Background())
			auto := h.Settings.Bool(config.KeyPluginsAutoUpdate)
			news, moved, err := plugin.AutoUpdate(ctx, h.Dirs, auto)
			switch {
			case err != nil:
				h.Notify(slog.LevelWarn, "Could not update the plugins", err.Error())
			case len(news) > 0 && auto:
				h.Notify(slog.LevelInfo, "Plugins updated", strings.Join(news, "; "))
			case len(news) > 0:
				h.Notify(slog.LevelInfo, "Plugin updates available", strings.Join(news, "; "))
			case moved:
			default:
				// a fetch that moved nothing still rewrote the clones'
				// FETCH_HEAD, which the reload's fingerprint reads: the
				// quiet run is not a config change
				h.MarkConfig()
			}
		})
	})
	return lua.BootStep{
		Name: hostapi.EventAppReady,
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			if !h.Settings.Bool(config.KeyProvidersBuiltin) {
				h.Providers.DropBuiltin()
			}
			h.CheckToolNames()
			h.EmitOnState(g.L, hostapi.EventAppReady, nil)
			// the marketplace pull runs off the boot, and only for the TUI:
			// a headless boot stays quiet, its stderr is the work
			if reflect {
				updateLast()
			}
			return nil
		},
	}
}
