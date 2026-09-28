// zemi: a self-improving, Lua-extensible AI agent harness (DESIGN.md).
// main.go is wiring only: resolve dirs, build the runtime + host + UI,
// assemble the boot steps, run the bubbletea program.
package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/ui"
	zemiRuntime "github.com/metafates/zemi/runtime"
)

func main() {
	sandbox.DispatchHelper()
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr, launch))
}

// launch opens the TUI, or with o.run answers one prompt on stdout.
func launch(o options) int {
	usage := func(msg string) int {
		fmt.Fprintf(os.Stderr, "zemi: %s\nRun '%s --help' for usage.\n", msg, map[bool]string{true: "zemi run", false: "zemi"}[o.run])
		return 2
	}
	if err := o.readStdin(os.Stdin); err != nil {
		return usage(err.Error())
	}
	if o.run {
		if o.prompt == "" {
			return usage("zemi run needs a prompt: an argument, - or piped stdin")
		}
		h, done, err := bootHeadless(o, false, os.Stderr)
		if err != nil {
			return failPrint(err, o.json, os.Stdout, os.Stderr)
		}
		defer done()
		if err := applyOptions(h, o); err != nil {
			return failPrint(err, o.json, os.Stdout, os.Stderr)
		}
		return printTurn(h, o.prompt, o.json, os.Stdout, os.Stderr)
	}
	// bubbletea draws to stdout: redirected, the TUI would run the agent unseen
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		return usage("stdout is not a terminal; zemi run prints the answer")
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "zemi:", err)
		return 1
	}
	ctx := context.Background()
	dirs, err := config.ResolveDirs()
	if err == nil {
		err = dirs.Ensure()
	}
	if err != nil {
		return fail(err)
	}
	log := logging.New(dirs.LogDir())
	defer log.Close()
	host, trust := newHost(dirs, log, o)
	app := ui.NewApp(host, log)
	host.SetUIHooks(app)
	trust.ask = app.AskTrust

	// reload is set before boot starts; /trust calls it after boot
	var reload func()
	steps := hostSteps(host, trust, app, func() { reload() })
	p := tea.NewProgram(app)
	app.SetProgram(p)
	reload = func() {
		if err := host.ForceReload(ctx, steps); err != nil {
			log.Error("reload", "force reload failed", map[string]any{"error": err.Error()})
			host.RecordError("reload", err.Error())
			app.Notify(logging.Error, "Could not reload the config", err.Error())
			return
		}
		app.SignalBoot(nil)
		app.Notify(logging.Info, "Config reloaded", "")
		host.WarnModel()
		host.EmitAsync("config.reloaded", nil)
	}
	app.SetReload(reload)

	// Boot runs on its own goroutine (it may block on the trust prompt);
	// the outcome reaches the UI through the hook channel.
	go func() {
		// A panic during boot must reach the UI as a boot failure instead
		// of killing the process with the terminal still in raw mode.
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic", fmt.Sprintf("boot panicked: %v", r), map[string]any{"stack": string(debug.Stack())})
				app.SignalBoot(fmt.Errorf("boot panicked: %v (stack in %s)", r, dirs.LogDir()))
			}
		}()
		err := host.RT.Boot(ctx, steps)
		app.SignalBoot(err)
		if err != nil {
			// /errors is where both the user and the agent look first
			host.RecordError("boot", err.Error())
			return
		}
		host.GoSafe("config watch", func() { watchConfig(host, dirs, reload) })
		// A bad flag (no session to resume, say) is a toast, not a boot
		// failure: the TUI is still usable.
		if err := applyOptions(host, o); err != nil {
			host.RecordError("flag", err.Error())
			app.Notify(logging.Error, "Could not apply a flag", err.Error())
		}
		host.KeepPicks.Store(true)
		// here, not in app.ready: once per process, after -m, never on a reload
		host.OfferModel()
		if o.prompt != "" {
			if err := sendPrompt(host, o.prompt); err != nil {
				app.Notify(logging.Error, "Could not send", err.Error())
			}
		}
	}()

	// Closing the terminal sends SIGHUP, which ends the process without
	// running the cleanup below, and jobs and servers would outlive zemi.
	// SIGTERM is not here: bubbletea quits on it and p.Run returns, which
	// restores the terminal and reaches the cleanup anyway.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	go func() {
		<-sig
		proc.KillAll()
		os.Exit(130)
	}()

	_, err = p.Run()
	// Children run in their own sessions (internal/proc), so the terminal
	// closing does not reach them: stop them here. Exit even if some cleanup
	// hangs.
	proc.KillAll()
	// hand the terminal its normal pointer back (hover sets OSC 22); an error
	// that is not ErrProgramKilled means the program never started
	if err == nil || errors.Is(err, tea.ErrProgramKilled) {
		fmt.Print(ansi.SetPointerShape("default"))
	}
	time.AfterFunc(3*time.Second, func() { os.Exit(0) })
	if err != nil {
		return fail(err)
	}
	return 0
}

// newHost builds the runtime and host over dirs, and the trust gate their
// boot reads.
func newHost(dirs config.Dirs, log *logging.Logger, o options) (*hostapi.Host, *trustGate) {
	rt := lua.NewRuntime(lua.RuntimeOptions{
		Log:       log,
		PathClass: pathClasses(dirs),
		RuntimeFS: zemiRuntime.FS,
	})
	host := hostapi.NewHost(rt, log, config.NewStore(), dirs)
	catalog := agent.LoadCatalog(dirs.DataHome, os.Getenv)
	host.Builtin.Store(&catalog)
	rt.SetPrintHook(func(origin, line string) {
		log.Info(origin, "print: "+line, nil)
		host.RecordMessage(origin, line)
	})
	host.Version = versionString()
	trust := &trustGate{h: host, dirs: dirs}
	if o.trust != nil {
		trust.session.Store(o.trust)
	}
	return host, trust
}

// hostSteps is the boot of a zemi; app is nil headless, where nothing
// quits and a failing step says so on stderr.
func hostSteps(h *hostapi.Host, trust *trustGate, app *ui.App, reload func()) []lua.BootStep {
	quit := func() {}
	if app != nil {
		quit = app.Quit
	}
	soft := func(s lua.BootStep) lua.BootStep { return softStep(h, app == nil, s) }
	return bootSteps(h, mainCommands(quit, trust.command(reload)),
		[]lua.BootStep{soft(trust.step()), soft(h.ProjectPermissionsStep())},
		soft(h.PluginsStep()),
		[]lua.BootStep{
			soft(lua.LoadFileStep("init", filepath.Join(h.Dirs.ConfigHome, "init.lua"), lua.OriginUser)),
			soft(h.SkillsUserHooksStep()),
			soft(projectInitStep(h, h.Dirs)),
		},
		app != nil)
}

// bootHeadless boots zemi with no TUI, for zemi run and zemi health; done
// stops what boot started (MCP and language servers) and closes the log.
// An inert boot (zemi models, completion) only reads config: no store, no
// log file, no process, and Host.Inert's guards.
func bootHeadless(o options, inert bool, notice io.Writer) (*hostapi.Host, func(), error) {
	dirs, err := config.ResolveDirs()
	if err == nil && !inert {
		err = dirs.Ensure()
	}
	if err != nil {
		return nil, nil, err
	}
	hostDirs, logDir, release := dirs, dirs.LogDir(), func() {}
	if inert {
		hostDirs.StateHome, logDir, release = "", "", proc.Refuse()
	}
	log := logging.New(logDir)
	h, trust := newHost(hostDirs, log, o)
	h.Inert = inert
	trust.dirs = dirs // the saved decision; a headless answer saves none
	// a project with no saved decision runs untrusted instead of asking; say
	// so, or the run looks like it applied the project's setup
	trust.ask = headlessTrust(notice)
	done := func() {
		proc.KillAll()
		log.Close()
		release()
	}
	if err := h.RT.Boot(context.Background(), hostSteps(h, trust, nil, func() {})); err != nil {
		done()
		return nil, nil, err
	}
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

// bootSteps is the boot order (DESIGN §5): trust is what runs before the
// plugins and config what runs after them, where init.lua does. A plugin
// test runs no trust and its test file as the config.
func bootSteps(h *hostapi.Host, cmds []*hostapi.Command, trust []lua.BootStep, plugins lua.BootStep, config []lua.BootStep, reflect bool) []lua.BootStep {
	return slices.Concat(
		[]lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(cmds...), lua.RequireStep("zemi.tui")},
		trust, []lua.BootStep{plugins}, config,
		[]lua.BootStep{h.MarkdownCommandsStep(), readyStep(h, reflect)})
}

// watchConfig reloads when files under the user or a trusted project's
// config or a plugin change, once no turn is running (an edit made mid-turn
// applies after it).
func watchConfig(h *hostapi.Host, dirs config.Dirs, reload func()) {
	fingerprint := func() (string, bool) {
		// a sandboxed command that created project config turns trust off first
		h.CheckSandboxed()
		roots := append(append([]string{dirs.ConfigHome}, dirs.UserTrees()...), pluginRoots(dirs.PluginsRoot())...)
		if h.ProjectTrusted() {
			return configFingerprint(dirs.WorktreesDir(), append(roots, dirs.ProjectConfigDir())...), true
		}
		return configFingerprint("", roots...), false
	}
	last, trusted := fingerprint()
	for range time.Tick(time.Second) {
		fp, t := fingerprint()
		switch {
		case t != trusted:
			// /trust reloaded already; only the watched trees changed
			last, trusted = fp, t
		case fp != last && !h.AgentActive():
			last = fp
			reload()
		}
	}
}

// pluginRoots are what the watcher reads of the plugins tree: every local
// plugin whole, and for a clone only its git index, which install, update
// and delete move, so a big installed tree costs no walk.
func pluginRoots(root string) []string {
	idx, _ := filepath.Glob(filepath.Join(root, "installed", "[^.]*", ".git", "index"))
	return append(idx, filepath.Join(root, "local"), filepath.Join(root, "disabled"))
}

// configFingerprint summarizes config trees by path, size and modification
// time. Git data, dot-directories, node_modules, skip (the checkouts in
// .agents/worktrees) and the .luarc.json zemi writes itself are not config,
// and a plugin's dependency tree would make every walk cost milliseconds.
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
	for _, root := range walk {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return nil
			case d.IsDir() && p != root && (d.Name()[0] == '.' || d.Name() == "node_modules" || p == skip || slices.Contains(walk, p)):
				return filepath.SkipDir // a root is walked once
			case d.IsDir() || d.Name() == ".luarc.json":
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
		{Name: "quit", Desc: "quit zemi", GoFn: func(*hostapi.Host, string) error {
			quit()
			return nil
		}},
		{Name: "help", Desc: "list keys and commands", GoFn: func(h *hostapi.Host, args string) error {
			return h.OpenHelp()
		}},
		{Name: "new", Desc: "start a new session", GoFn: func(h *hostapi.Host, _ string) error {
			return h.NewSession()
		}},
		{Name: "sessions", NArgs: hostapi.NArgsAny, Desc: "resume or delete an earlier session", GoFn: func(h *hostapi.Host, args string) error {
			if id, ok := strings.CutPrefix(strings.TrimSpace(args), "delete "); ok {
				return h.DeleteSession(strings.TrimSpace(id))
			}
			return h.OpenSessions()
		}},
		{Name: "resume", Hidden: true, NArgs: hostapi.NArgsOptional, Desc: "resume the latest session, or the one with this id", GoFn: func(h *hostapi.Host, args string) error {
			return h.ResumeSession(strings.TrimSpace(args))
		}},
		{Name: "rewind", NArgs: hostapi.NArgsOptional, Desc: "go back to an earlier prompt and undo its file changes", GoFn: func(h *hostapi.Host, args string) error {
			if args = strings.TrimSpace(args); args == "" {
				return h.OpenRewind()
			}
			n, err := h.RewindTo(args)
			if err == nil {
				files := "files"
				if n == 1 {
					files = "file"
				}
				h.Notify(logging.Info, "Session rewound", fmt.Sprintf("restored %d %s; the prompt is back in the composer", n, files))
			}
			return err
		}},
		{Name: "effort", Hidden: true, NArgs: hostapi.NArgsOptional, Desc: "set reasoning effort", GoFn: func(h *hostapi.Host, args string) error {
			if args = strings.TrimSpace(args); args == "" {
				// the readout is a toast, which a headless run drops
				if _, err := h.Screen(); err != nil {
					return err
				}
				pick := h.Settings.Str("agent.effort")
				cur := cmp.Or(pick, "default")
				if eff := h.Effort(h.AgentStream().Model); eff != pick {
					cur += " (" + cmp.Or(eff, "none") + " on this model)"
				}
				h.Notify(logging.Info, "Reasoning effort is "+cur, "/effort "+config.OrList(efforts)+" sets it")
				return nil
			}
			return h.SetConfig("agent.effort", args)
		}, GoComplete: func(*hostapi.Host, string, string) []string { return efforts }},
		{Name: "profile", NArgs: hostapi.NArgsOptional, Desc: "choose the agent profile: model, prompt and tools", GoFn: func(h *hostapi.Host, args string) error {
			if args = strings.TrimSpace(args); args == "" {
				return h.OpenProfilePicker()
			}
			return h.SetProfile(args)
		}, GoComplete: func(h *hostapi.Host, _, _ string) []string { return h.ProfileNames() }},
		{Name: "usage", Desc: "show tokens and cost", GoFn: func(h *hostapi.Host, _ string) error {
			return h.OpenUsage()
		}},
		{Name: "prompt", Desc: "show the system prompt the model gets", GoFn: func(h *hostapi.Host, _ string) error {
			return h.OpenPrompt()
		}},
		{Name: "messages", Desc: "show recent print and zemi.log lines", GoFn: func(h *hostapi.Host, _ string) error {
			return h.OpenMessages()
		}},
		{Name: "init", Desc: "write AGENTS.md for this project", GoFn: func(h *hostapi.Host, _ string) error {
			return h.InitProject()
		}},
		{Name: "export", Hidden: true, NArgs: hostapi.NArgsAny, Desc: "save this session as markdown", GoFn: func(h *hostapi.Host, args string) error {
			path, full := strings.CutPrefix(strings.TrimSpace(args)+" ", "full ")
			_, err := h.ExportSession(strings.TrimSpace(path), full)
			return err
		}, GoComplete: hostapi.SubcommandCompleter([]string{"full"}, nil)},
		{Name: "copy", NArgs: hostapi.NArgsAny, Desc: "copy the last reply or a code block in it", GoFn: func(h *hostapi.Host, args string) error {
			return h.CopyReply(args)
		}, GoComplete: hostapi.SubcommandCompleter([]string{"reply", "code"}, nil)},
		{Name: "fork", Hidden: true, Desc: "continue this conversation in a new session", GoFn: func(h *hostapi.Host, _ string) error {
			_, err := h.ForkSession()
			return err
		}},
		{Name: "reload", Hidden: true, Desc: "reload config now", GoFn: func(h *hostapi.Host, _ string) error {
			// the composer matches /reload before the registry; this
			// serves the palette, keymaps and zemi.config.reload
			return h.ReloadConfig()
		}},
		{Name: "errors", NArgs: hostapi.NArgsAny, Desc: "show errors zemi recorded", GoFn: func(h *hostapi.Host, args string) error {
			return h.OpenErrors(args)
		}},
		{Name: "trust", Desc: "change whether this project is trusted", GoFn: func(h *hostapi.Host, _ string) error {
			if _, err := h.Screen(); err != nil {
				return fmt.Errorf("%w; --trust trusts the project for one run", err)
			}
			trust()
			return nil
		}},
		{Name: "health", Desc: "check zemi's setup", GoFn: func(h *hostapi.Host, _ string) error {
			return h.OpenHealth()
		}},
		{Name: "compact", NArgs: hostapi.NArgsAny, Desc: "summarize the conversation to free context; text after it says what to keep", GoFn: func(h *hostapi.Host, args string) error {
			return h.CompactNow(args)
		}},
		{Name: "btw", NArgs: hostapi.NArgsSome, Desc: "ask a side question the conversation does not keep", GoFn: func(h *hostapi.Host, args string) error {
			return h.Btw(args)
		}},
		{Name: "model", NArgs: hostapi.NArgsAny, Desc: "choose the model, or one per role", GoFn: func(h *hostapi.Host, args string) error {
			return h.SetModel(args, true)
		}, GoComplete: (*hostapi.Host).CompleteModel},
		{Name: "permissions", NArgs: hostapi.NArgsAny, Desc: "see the rules and delete one you allowed", GoFn: func(h *hostapi.Host, args string) error {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(args), "forget "); ok {
				return h.ForgetRule(rest)
			}
			return h.OpenPermissions()
		}},
		{Name: "mode", NArgs: hostapi.NArgsOptional, Desc: "choose the permission mode", GoFn: func(h *hostapi.Host, args string) error {
			if args = strings.TrimSpace(args); args == "" {
				return h.OpenModePicker()
			}
			return h.SetPermMode(args)
		}, GoComplete: func(h *hostapi.Host, _, _ string) []string { return modeNames(h) }},
	}
}

func modeNames(h *hostapi.Host) []string {
	var names []string
	for _, m := range h.PermModeList() {
		names = append(names, m.Name)
	}
	return names
}

// softStep keeps a failing config step (a typo in init.lua, a broken plugin
// or project config) from stopping boot: the error goes to the log and
// /errors, and the remaining steps and the runtime still start, like
// neovim reporting an init.lua error and opening anyway. Headless there is
// no /errors to open, so the error goes to stderr as well: otherwise zemi run
// answers as if the config had loaded, and a broken provider registration
// then blames a config line that is already correct.
func softStep(h *hostapi.Host, headless bool, s lua.BootStep) lua.BootStep {
	run := s.Run
	s.Run = func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
		err := run(ctx, r, g)
		if s.Name == "init" {
			h.SetInitError("")
		}
		if err != nil {
			if s.Name == "init" {
				h.SetInitError(hostapi.Headline(err.Error()))
			}
			h.Log.Error("boot", s.Name+" failed", map[string]any{"error": err.Error()})
			h.RecordError("boot "+s.Name, err.Error())
			if headless { // no /errors pill to notice, and zemi run is the scriptable path
				fmt.Fprintln(os.Stderr, "zemi: "+s.Name+": "+hostapi.Headline(err.Error()))
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
			tg.h.CheckSandboxed()
			trusted, err := tg.decide()
			tg.h.SetProjectTrusted(trusted)
			if trusted { // so git status never lists the checkouts, sandbox or not
				if _, werr := tg.dirs.WorktreesIgnore(); werr != nil {
					tg.h.Log.Warn("trust", "cannot write .agents/worktrees/.gitignore", map[string]any{"error": werr.Error()})
				}
			}
			return err
		},
	}
}

// projectInitStep loads <repo>/.agents/init.lua in a trusted project.
// Agents, commands and workflows read the decision when they are used.
func projectInitStep(h *hostapi.Host, dirs config.Dirs) lua.BootStep {
	return lua.BootStep{
		Name: "project config",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			init := filepath.Join(dirs.ProjectConfigDir(), "init.lua")
			if _, err := os.Stat(init); err != nil || !h.ProjectTrusted() {
				return nil
			}
			h.Log.Info("boot", "loading project config", map[string]any{"path": init})
			return lua.LoadFileStep("project init", init, lua.OriginProject).Run(ctx, r, g)
		},
	}
}

func (tg *trustGate) decide() (bool, error) {
	if tg.h.TrustHeld() {
		return false, nil
	}
	if s := tg.session.Load(); s != nil {
		return *s, nil
	}
	root := tg.dirs.TrustRoot()
	if v, ok := readTrust(tg.dirs.TrustFile())[root]; ok {
		return v == "trusted", nil
	}
	if hostapi.TrustUnsaved(root) {
		return false, nil
	}
	return tg.record(root, tg.ask(root, tg.dirs.ProjectDir, ""))
}

// record applies a prompt answer: trust and untrust are saved, "later" holds
// for this process only (a reload does not ask again, the next start does).
func (tg *trustGate) record(root, answer string) (bool, error) {
	ok := answer == "trust"
	if answer != "later" && !hostapi.TrustUnsaved(root) {
		saved := readTrust(tg.dirs.TrustFile())
		saved[root] = map[bool]string{true: "trusted", false: "untrusted"}[ok]
		err := writeTrust(tg.dirs.TrustFile(), saved)
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
func (tg *trustGate) command(reload func()) func() {
	return func() {
		// the prompt blocks until answered: off the UI goroutine
		tg.h.GoSafe("trust", func() {
			root, was := tg.dirs.TrustRoot(), tg.h.ProjectTrusted()
			answer := tg.ask(root, tg.dirs.ProjectDir, map[bool]string{true: "trusted", false: "not trusted"}[was])
			if answer == "later" {
				return
			}
			tg.h.ReleaseTrust()
			ok, err := tg.record(root, answer)
			if err != nil {
				tg.h.Notify(logging.Error, "Could not save the trust decision", err.Error())
			}
			// set before the reload: a boot that fails before the trust step
			// must not leave the old decision in force
			tg.h.SetProjectTrusted(ok)
			if ok != was {
				reload()
			}
		})
	}
}

func readTrust(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

func writeTrust(path string, m map[string]string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, data, 0o600)
}

// readyStep drops the builtin providers when init.lua turned them off
// (config has run by now, and a replay re-seeds them first) and emits
// app.ready. Only the TUI reflects at startup: zemi run and zemi health
// exit before it could finish.
func readyStep(h *hostapi.Host, reflect bool) lua.BootStep {
	// once per process: a reload replays the step
	reflectLast := sync.OnceFunc(func() { h.GoSafe("reflect sweep", h.ReflectSweep) })
	return lua.BootStep{
		Name: "app.ready",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			if !h.Settings.Bool("providers.builtin") {
				h.Providers.DropBuiltin()
			}
			h.EmitOnState(g.L, "app.ready", nil)
			// idle sessions are reflected in the background at startup,
			// so quitting never waits on the reflector (DESIGN §10.3)
			if reflect {
				reflectLast()
			}
			return nil
		},
	}
}
