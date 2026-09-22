// zemi: a self-improving, Lua-extensible AI agent harness (DESIGN.md).
// main.go is wiring only: resolve dirs, build the runtime + host + UI,
// assemble the boot steps, run the bubbletea program.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
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
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	opts, code := parseArgs(args, os.Stdout, os.Stderr)
	if code >= 0 {
		return code
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "zemi:", err)
		return 1
	}
	if err := opts.readStdin(os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "zemi: %v\nRun 'zemi --help' for usage.\n", err)
		return 2
	}
	if opts.print && opts.prompt == "" {
		fmt.Fprintln(os.Stderr, "zemi: -p needs a prompt (an argument or stdin)\nRun 'zemi --help' for usage.")
		return 2
	}
	ctx := context.Background()

	dirs := config.ResolveDirs()
	if err := dirs.Ensure(); err != nil {
		return fail(err)
	}
	log := logging.New(dirs.LogDir())
	defer log.Close()
	// --health and -p return without reaching the TUI cleanup, and boot may
	// already have started MCP or language servers
	defer proc.KillAll()

	settings := config.NewStore()

	classes := lua.PathClasses{
		{Prefix: dirs.ConfigHome, Origin: lua.OriginUser},
		{Prefix: dirs.PluginsDir(), Origin: lua.OriginPlugin},
		{Prefix: dirs.ProjectConfigDir(), Origin: lua.OriginProject},
		{Prefix: dirs.DataHome + string(filepath.Separator) + "skills", Origin: lua.OriginLearned},
	}
	for _, dir := range dirs.UserTrees() {
		classes = append(classes, lua.PathClass{Prefix: dir, Origin: lua.OriginUser})
	}
	rt := lua.NewRuntime(lua.RuntimeOptions{
		Log:       log,
		PathClass: classes,
		RuntimeFS: zemiRuntime.FS,
	})
	host := hostapi.NewHost(rt, log, settings, dirs)
	rt.SetPrintHook(func(origin, line string) {
		log.Info(origin, "print: "+line, nil)
		host.RecordMessage(origin, line)
	})
	host.Version = versionString()
	// Headless: nothing to quit, and a project with no saved decision runs
	// untrusted instead of asking. Say so, or the run looks like it applied
	// the project's setup.
	quit := func() {}
	trust := &trustGate{h: host, dirs: dirs, ask: headlessTrust(os.Stderr)}
	if opts.trust != nil {
		trust.session.Store(opts.trust)
	}
	var app *ui.App
	if !opts.print && !opts.health {
		app = ui.NewApp(host, log)
		host.SetUIHooks(app)
		quit, trust.ask = app.Quit, app.AskTrust
	}

	// reload is set before boot starts; /trust calls it after boot
	var reload func()
	soft := func(s lua.BootStep) lua.BootStep { return softStep(host, app == nil, s) }
	steps := []lua.BootStep{
		host.NamespacesStep(),
		host.BuiltinCommandsStep(mainCommands(quit, trust.command(func() { reload() }))...),
		lua.RequireStep("zemi.defaultui"),
		soft(trust.step()),
		soft(host.ProjectPermissionsStep()),
		soft(host.PluginsStep()),
		soft(lua.LoadFileStep("init", filepath.Join(dirs.ConfigHome, "init.lua"), lua.OriginUser)),
		soft(host.SkillsAuthoredHooksStep()),
		soft(projectInitStep(host, dirs)),
		host.MarkdownCommandsStep(),
		readyStep(host, app != nil),
		host.SkillsLearnedHooksStep(),
	}

	if opts.health {
		if err := rt.Boot(ctx, steps); err != nil {
			return fail(err)
		}
		return printHealth(host, os.Stdout)
	}

	if opts.print {
		jsonOut := opts.output == "json"
		if err := rt.Boot(ctx, steps); err != nil {
			return failPrint(err, jsonOut, os.Stdout, os.Stderr)
		}
		if err := applyOptions(host, opts); err != nil {
			return failPrint(err, jsonOut, os.Stdout, os.Stderr)
		}
		return printTurn(host, opts.prompt, jsonOut, os.Stdout, os.Stderr)
	}

	p := tea.NewProgram(app)
	app.SetProgram(p)
	reload = func() {
		if err := host.ForceReload(ctx, steps); err != nil {
			log.Error("reload", "force reload failed", map[string]any{"error": err.Error()})
			host.RecordError("reload", err.Error())
			app.Notify("error", "Reload failed", err.Error())
			return
		}
		app.SignalBoot(nil)
		app.Notify("info", "Config reloaded", "")
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
		err := rt.Boot(ctx, steps)
		app.SignalBoot(err)
		if err != nil {
			// /errors is where both the user and the agent look first
			host.RecordError("boot", err.Error())
			return
		}
		host.GoSafe("config watch", func() { watchConfig(host, dirs, reload) })
		// A bad flag (no session to resume, say) is a toast, not a boot
		// failure: the TUI is still usable.
		if err := applyOptions(host, opts); err != nil {
			app.Notify("error", "A flag did not apply", err.Error())
			return
		}
		if opts.prompt != "" {
			if err := sendPrompt(host, opts.prompt); err != nil {
				app.Notify("error", "Could not send the prompt", err.Error())
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

	_, err := p.Run()
	// Children run in their own sessions (internal/proc), so the terminal
	// closing does not reach them: stop them here. Exit even if some cleanup
	// hangs.
	proc.KillAll()
	fmt.Print("\x1b]22;default\x07") // hand the terminal its normal pointer back (hover sets OSC 22)
	time.AfterFunc(3*time.Second, func() { os.Exit(0) })
	if err != nil {
		return fail(err)
	}
	return 0
}

// watchConfig reloads when files under the user or a trusted project's
// config change, once no turn is running (an edit made mid-turn applies
// after it).
func watchConfig(h *hostapi.Host, dirs config.Dirs, reload func()) {
	fingerprint := func() (string, bool) {
		// a sandboxed command that created project config turns trust off first
		h.CheckSandboxed()
		if h.ProjectTrusted() {
			return configFingerprint(dirs.ConfigHome, dirs.ProjectConfigDir()), true
		}
		return configFingerprint(dirs.ConfigHome), false
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

// configFingerprint summarizes config trees by path, size and modification
// time, skipping git data.
func configFingerprint(roots ...string) string {
	sum := sha256.New()
	for _, root := range roots {
		if root == "" {
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return nil
			case d.IsDir() && (d.Name() == ".git" || p == filepath.Join(root, "worktrees")):
				return filepath.SkipDir // checkouts in .zemi/worktrees are not config
			case d.IsDir():
				return nil
			}
			if info, err := d.Info(); err == nil {
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
	return []*hostapi.Command{
		{Name: "quit", Desc: "quit zemi", GoFn: func(*hostapi.Host, string) error {
			quit()
			return nil
		}},
		{Name: "help", Desc: "list keys and commands", GoFn: func(h *hostapi.Host, args string) error {
			h.OpenHelp()
			return nil
		}},
		{Name: "new", Desc: "start a new session", GoFn: func(h *hostapi.Host, _ string) error {
			return h.NewSession()
		}},
		{Name: "sessions", NArgs: "*", Desc: "resume or delete an earlier session", GoFn: func(h *hostapi.Host, args string) error {
			if id, ok := strings.CutPrefix(strings.TrimSpace(args), "delete "); ok {
				return h.DeleteSession(strings.TrimSpace(id))
			}
			return h.OpenSessions()
		}},
		{Name: "resume", Hidden: true, Desc: "resume the latest session", GoFn: func(h *hostapi.Host, args string) error {
			return h.ResumeSession(strings.TrimSpace(args))
		}},
		{Name: "rewind", NArgs: "?", Desc: "go back to an earlier prompt and undo its file changes", GoFn: func(h *hostapi.Host, args string) error {
			if args = strings.TrimSpace(args); args == "" {
				return h.OpenRewind()
			}
			n, err := h.RewindTo(args)
			if err == nil {
				files := "files"
				if n == 1 {
					files = "file"
				}
				h.Notify("info", "Rewound", fmt.Sprintf("restored %d %s; the prompt is back in the composer", n, files))
			}
			return err
		}},
		{Name: "effort", Hidden: true, NArgs: "?", Desc: "set reasoning effort", GoFn: func(h *hostapi.Host, args string) error {
			switch args = strings.TrimSpace(args); args {
			case "":
				cur := h.Effort()
				if cur == "" {
					cur = "default"
				}
				return fmt.Errorf("reasoning effort is %s (want low, medium, high or default)", cur)
			case "default":
				args = ""
			case "low", "medium", "high":
			default:
				return fmt.Errorf("unknown effort %q (want low, medium, high or default)", args)
			}
			_, err := h.Settings.SetQuiet("agent.effort", args)
			return err
		}, GoComplete: func(*hostapi.Host, string, string) []string { return []string{"low", "medium", "high", "default"} }},
		{Name: "profile", NArgs: "?", Desc: "choose the agent profile: model, prompt and tools", GoFn: func(h *hostapi.Host, args string) error {
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
		{Name: "init", Desc: "write AGENTS.md for this repository", GoFn: func(h *hostapi.Host, _ string) error {
			return h.InitProject()
		}},
		{Name: "export", Hidden: true, NArgs: "?", Desc: "save this session as markdown", GoFn: func(h *hostapi.Host, args string) error {
			path, err := h.ExportSession(strings.TrimSpace(args))
			if err == nil {
				h.Notify("info", "Exported", "wrote "+path)
			}
			return err
		}},
		{Name: "copy", NArgs: "*", Desc: "copy the last reply or a code block in it", GoFn: func(h *hostapi.Host, args string) error {
			return h.CopyReply(args)
		}, GoComplete: func(*hostapi.Host, string, string) []string { return []string{"reply", "code"} }},
		{Name: "fork", Hidden: true, Desc: "continue this conversation in a new session", GoFn: func(h *hostapi.Host, _ string) error {
			_, err := h.ForkSession()
			return err
		}},
		{Name: "reload", Hidden: true, Desc: "reload config now", GoFn: func(h *hostapi.Host, _ string) error {
			// the composer matches /reload before the registry; this
			// serves the palette, keymaps and zemi.config.reload
			return h.ReloadConfig()
		}},
		{Name: "errors", NArgs: "*", Desc: "show errors zemi recorded", GoFn: func(h *hostapi.Host, args string) error {
			return h.OpenErrors(args)
		}},
		{Name: "trust", Desc: "change whether this project is trusted", GoFn: func(*hostapi.Host, string) error {
			trust()
			return nil
		}},
		{Name: "health", Desc: "check zemi's setup", GoFn: func(h *hostapi.Host, _ string) error {
			return h.OpenHealth()
		}},
		{Name: "compact", NArgs: "*", Desc: "summarize the conversation to free context", GoFn: func(h *hostapi.Host, args string) error {
			return h.CompactNow(args)
		}},
		{Name: "model", NArgs: "?", Desc: "choose the model", GoFn: func(h *hostapi.Host, args string) error {
			if args = strings.TrimSpace(args); args == "" {
				return h.OpenModelPicker()
			}
			return h.SetModel(args)
		}},
		{Name: "permissions", NArgs: "*", Desc: "see what is allowed and take an always answer back", GoFn: func(h *hostapi.Host, args string) error {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(args), "forget "); ok {
				return h.ForgetRule(rest)
			}
			return h.OpenPermissions()
		}},
		{Name: "mode", NArgs: "?", Desc: "choose who approves actions", GoFn: func(h *hostapi.Host, args string) error {
			if args = strings.TrimSpace(args); args == "" {
				return h.OpenModePicker()
			}
			return h.SetPermMode(args)
		}, GoComplete: func(h *hostapi.Host, _, _ string) []string {
			var names []string
			for _, m := range h.PermModeList() {
				names = append(names, m.Name)
			}
			return names
		}},
	}
}

// softStep keeps a failing config step (a typo in init.lua, a broken plugin
// or project config) from stopping boot: the error goes to the log and
// /errors, and the remaining steps and the runtime still start, like
// neovim reporting an init.lua error and opening anyway. Headless there is
// no /errors to open, so the error goes to stderr as well: otherwise -p
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
			if headless { // no /errors pill to notice, and -p is the scriptable path
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
			return err
		},
	}
}

// projectInitStep loads <project>/.zemi/init.lua in a trusted project.
// Agents, commands and workflows read the decision when they are used.
func projectInitStep(h *hostapi.Host, dirs config.Dirs) lua.BootStep {
	return lua.BootStep{
		Name: "project config",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			init := filepath.Join(dirs.ProjectConfigDir(), "init.lua")
			if _, err := os.Stat(init); err != nil || !h.ProjectTrusted() {
				return nil
			}
			// the nearest .zemi at or above the project, which may not be the repository root's
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
				tg.h.Notify("error", "Could not save trust", err.Error())
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readyStep emits app.ready. Only the TUI reflects at startup: -p and
// --health exit before it could finish.
func readyStep(h *hostapi.Host, reflect bool) lua.BootStep {
	// once per process: a reload replays the step, and the newest session
	// is then the open one
	reflectLast := sync.OnceFunc(func() { h.GoSafe("reflect last session", h.ReflectLastSession) })
	return lua.BootStep{
		Name: "app.ready",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			h.EmitOnState(g.L, "app.ready", nil)
			// The last session is reflected in the background at startup,
			// so quitting never waits on the reflector (DESIGN §10.3).
			if reflect {
				reflectLast()
			}
			return nil
		},
	}
}
