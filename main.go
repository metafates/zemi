// zemi: a self-improving, Lua-extensible AI agent harness (DESIGN.md).
// main.go is wiring only: resolve dirs, build the runtime + host + UI,
// assemble the boot steps, run the bubbletea program.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/ui"
	zemiRuntime "github.com/metafates/zemi/runtime"
)

func main() {
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
	rt := lua.NewRuntime(lua.RuntimeOptions{
		Log:       log,
		PathClass: classes,
		RuntimeFS: zemiRuntime.FS,
	})
	rt.SetPrintHook(func(origin, line string) {
		log.Info(origin, "print: "+line, nil)
	})

	host := hostapi.NewHost(rt, log, settings, dirs)
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
	host.EnsureConfigRepo()

	// reload is set before boot starts; /trust calls it after boot
	var reload func()
	steps := []lua.BootStep{
		host.NamespacesStep(),
		host.PermissionsLoadStep(),
		builtinCommandsStep(host, quit, trust.command(func() { reload() })),
		host.MemoryCommandsStep(),
		host.ReflectorCommandsStep(),
		host.SkillsCommandsStep(),
		host.AgentsCommandsStep(),
		host.WorkflowCommandsStep(),
		host.MCPCommandsStep(),
		host.LSPCommandsStep(),
		host.WindowCommandsStep(),
		lua.RequireStep("zemi.defaultui"),
		softStep(host, pluginsStep(dirs)),
		softStep(host, lua.LoadFileStep("init", filepath.Join(dirs.ConfigHome, "init.lua"), lua.OriginUser)),
		softStep(host, host.SkillsAuthoredHooksStep()),
		softStep(host, trust.step()),
		host.MarkdownCommandsStep(),
		readyStep(host, !opts.print),
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
			app.Notify("error", "reload", err.Error())
			return
		}
		app.SignalBoot(nil)
		app.Notify("info", "reload", "config reloaded")
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
			app.Notify("error", "zemi", err.Error())
			return
		}
		if opts.prompt != "" {
			if err := host.AgentSend(opts.prompt); err != nil {
				app.Notify("error", "zemi", err.Error())
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
// time, skipping git data and the permissions.lua zemi writes itself.
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
			case d.IsDir() && d.Name() == ".git":
				return filepath.SkipDir
			case d.IsDir() || d.Name() == "permissions.lua":
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

// builtinCommandsStep registers the Go-owned slash commands; builtin origin
// lets user config replace them.
func builtinCommandsStep(h *hostapi.Host, quit, trust func()) lua.BootStep {
	return lua.BootStep{
		Name: "builtin commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			for _, c := range []*hostapi.Command{
				{Name: "quit", Desc: "quit zemi", GoFn: func(*hostapi.Host, string) error {
					quit()
					return nil
				}},
				{Name: "help", Desc: "show keys and commands", GoFn: func(h *hostapi.Host, args string) error {
					h.OpenHelp()
					return nil
				}},
				{Name: "new", Desc: "start a fresh session", GoFn: func(h *hostapi.Host, _ string) error {
					return h.NewSession()
				}},
				{Name: "sessions", Desc: "list and resume sessions", GoFn: func(h *hostapi.Host, _ string) error {
					return h.OpenSessions()
				}},
				{Name: "resume", Hidden: true, Desc: "resume a session (the latest without an id)", GoFn: func(h *hostapi.Host, args string) error {
					return h.ResumeSession(strings.TrimSpace(args))
				}},
				{Name: "rewind", NArgs: "?", Desc: "undo back to an earlier prompt, files included", GoFn: func(h *hostapi.Host, args string) error {
					if args = strings.TrimSpace(args); args == "" {
						return h.OpenRewind()
					}
					n, err := h.RewindTo(args)
					if err == nil {
						files := "files"
						if n == 1 {
							files = "file"
						}
						h.Notify("info", "rewind", fmt.Sprintf("restored %d %s; the prompt is back in the composer", n, files))
					}
					return err
				}},
				{Name: "effort", Hidden: true, NArgs: "?", Desc: "reasoning effort: low, medium, high or default", GoFn: func(h *hostapi.Host, args string) error {
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
				{Name: "usage", Desc: "tokens and cost", GoFn: func(h *hostapi.Host, _ string) error {
					return h.OpenUsage()
				}},
				{Name: "init", Desc: "write or update AGENTS.md for this repository", GoFn: func(h *hostapi.Host, _ string) error {
					return h.InitProject()
				}},
				{Name: "export", Hidden: true, NArgs: "?", Desc: "write this session as markdown (default zemi-<id>.md)", GoFn: func(h *hostapi.Host, args string) error {
					path, err := h.ExportSession(strings.TrimSpace(args))
					if err == nil {
						h.Notify("info", "export", "wrote "+path)
					}
					return err
				}},
				{Name: "fork", Hidden: true, Desc: "continue this conversation in a new session, keeping the original", GoFn: func(h *hostapi.Host, _ string) error {
					_, err := h.ForkSession()
					return err
				}},
				{Name: "reload", Hidden: true, Desc: "reload config now (zemi reloads on its own when config files change)", GoFn: func(*hostapi.Host, string) error {
					// the composer matches /reload before the registry; this
					// serves the palette, keymaps and zemi.config.reload
					return h.ReloadConfig()
				}},
				{Name: "errors", Hidden: true, NArgs: "*", Desc: "recent runtime errors; enter opens one", GoFn: func(h *hostapi.Host, args string) error {
					return h.OpenErrors(args)
				}},
				{Name: "trust", Desc: "trust this project or stop trusting it", GoFn: func(*hostapi.Host, string) error {
					trust()
					return nil
				}},
				{Name: "health", Desc: "check that everything works", GoFn: func(h *hostapi.Host, _ string) error {
					return h.OpenHealth()
				}},
				{Name: "compact", NArgs: "*", Desc: "summarize the conversation to free context; /compact <what to keep>", GoFn: func(h *hostapi.Host, args string) error {
					return h.CompactNow(args)
				}},
				{Name: "model", Desc: "choose the model", GoFn: func(h *hostapi.Host, args string) error {
					h.OpenModelPicker()
					return nil
				}},
				{Name: "mode", NArgs: "?", Desc: "who approves actions (tab switches)", GoFn: func(h *hostapi.Host, args string) error {
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
			} {
				c.Origin, c.Gen = lua.OriginBuiltin, hostapi.Gen(g.ID)
				if _, err := h.Commands.Add(c); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// softStep keeps a failing config step (a typo in init.lua, a broken plugin
// or project config) from stopping boot: the error goes to the log and
// /errors, and the remaining steps and the runtime still start, like
// neovim reporting an init.lua error and opening anyway.
func softStep(h *hostapi.Host, s lua.BootStep) lua.BootStep {
	run := s.Run
	s.Run = func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
		if err := run(ctx, r, g); err != nil {
			h.Log.Error("boot", s.Name+" failed", map[string]any{"error": err.Error()})
			h.RecordError("boot "+s.Name, err.Error())
		}
		return nil
	}
	return s
}

// pluginsStep loads site/pack/*/start/*/plugin/*.lua sorted, per the boot
// order (DESIGN §5).
func pluginsStep(dirs config.Dirs) lua.BootStep {
	return lua.BootStep{
		Name: "plugins",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			files, err := filepath.Glob(filepath.Join(dirs.PluginsDir(), "*", "start", "*", "plugin", "*.lua"))
			if err != nil {
				return err
			}
			slices.Sort(files)
			for _, f := range files {
				if err := lua.LoadFileStep("plugin "+f, f, lua.OriginPlugin).Run(ctx, r, g); err != nil {
					return err
				}
			}
			return nil
		},
	}
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

// step records the decision and loads <project>/.zemi/init.lua when the
// project is trusted. Agents, commands and workflows read the decision later.
func (tg *trustGate) step() lua.BootStep {
	return lua.BootStep{
		Name: "project trust",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			trusted, err := tg.decide()
			tg.h.SetProjectTrusted(trusted)
			if !trusted {
				return err
			}
			init := filepath.Join(tg.dirs.ProjectConfigDir(), "init.lua")
			if _, serr := os.Stat(init); serr != nil {
				return err
			}
			return errors.Join(err, lua.LoadFileStep("project init", init, lua.OriginProject).Run(ctx, r, g))
		},
	}
}

func (tg *trustGate) decide() (bool, error) {
	if s := tg.session.Load(); s != nil {
		return *s, nil
	}
	root := tg.dirs.TrustRoot()
	if v, ok := readTrust(tg.dirs.TrustFile())[root]; ok {
		return v == "trusted", nil
	}
	if unsaved(root) {
		return false, nil
	}
	return tg.record(root, tg.ask(root, tg.dirs.ProjectDir, ""))
}

// record applies a prompt answer: trust and untrust are saved, "later" holds
// for this process only (a reload does not ask again, the next start does).
func (tg *trustGate) record(root, answer string) (bool, error) {
	ok := answer == "trust"
	if answer != "later" && !unsaved(root) {
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
			ok, err := tg.record(root, answer)
			if err != nil {
				tg.h.Notify("error", "trust", err.Error())
			}
			if ok != was {
				reload()
			}
		})
	}
}

// unsaved reports a root no decision is saved for: the home directory and
// the filesystem root hold everything, so they stay untrusted unless /trust
// or --trust says otherwise for one run.
func unsaved(root string) bool {
	home, err := os.UserHomeDir()
	if err == nil {
		if h, err := filepath.EvalSymlinks(home); err == nil {
			home = h
		}
	}
	return root == "" || filepath.Dir(root) == root || (err == nil && root == home)
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

// readyStep emits app.ready. Print mode skips the startup reflection: the
// process exits before it could finish.
func readyStep(h *hostapi.Host, reflect bool) lua.BootStep {
	return lua.BootStep{
		Name: "app.ready",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			h.EmitOnState(g.L, "app.ready", nil)
			// The last session is reflected in the background at startup,
			// so quitting never waits on the reflector (DESIGN §10.3).
			if reflect {
				h.GoSafe("reflect last session", h.ReflectLastSession)
			}
			return nil
		},
	}
}
