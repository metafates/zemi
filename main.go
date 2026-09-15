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
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
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
	// Headless: nothing to quit, and an untrusted project config is skipped
	// instead of asked about.
	quit, askTrust := func() {}, func(string, string) string { return "once" }
	var app *ui.App
	if !opts.print && !opts.health {
		app = ui.NewApp(host, log)
		host.SetUIHooks(app)
		quit, askTrust = app.Quit, app.AskTrust
	}
	host.EnsureConfigRepo()

	steps := []lua.BootStep{
		host.NamespacesStep(),
		host.PermissionsLoadStep(),
		builtinCommandsStep(host, quit),
		host.MemoryCommandsStep(),
		host.ReflectorCommandsStep(),
		host.ConfigCommandsStep(),
		host.SkillsCommandsStep(),
		host.AgentsCommandsStep(),
		host.WorkflowCommandsStep(),
		host.MCPCommandsStep(),
		host.LSPCommandsStep(),
		lua.RequireStep("zemi.defaultui"),
		softStep(host, pluginsStep(dirs)),
		softStep(host, lua.LoadFileStep("init", filepath.Join(dirs.ConfigHome, "init.lua"), lua.OriginUser)),
		softStep(host, host.SkillsAuthoredHooksStep()),
		softStep(host, projectConfigStep(host, askTrust, dirs)),
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
	reload := func() {
		if err := rt.ForceReload(ctx, steps, host.PurgeGeneration); err != nil {
			log.Error("reload", "force reload failed", map[string]any{"error": err.Error()})
			app.Notify("error", "reload", err.Error())
			return
		}
		app.Notify("info", "reload", "config reloaded")
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

// watchConfig reloads when files under the user or project config change,
// once no turn is running (an edit made mid-turn applies after it).
func watchConfig(h *hostapi.Host, dirs config.Dirs, reload func()) {
	last := configFingerprint(dirs.ConfigHome, dirs.ProjectConfigDir())
	for range time.Tick(time.Second) {
		if fp := configFingerprint(dirs.ConfigHome, dirs.ProjectConfigDir()); fp != last && !h.AgentActive() {
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
func builtinCommandsStep(h *hostapi.Host, quit func()) lua.BootStep {
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
					return nil // the composer runs /reload before the registry
				}},
				{Name: "errors", Hidden: true, NArgs: "*", Desc: "recent runtime errors; enter opens one", GoFn: func(h *hostapi.Host, args string) error {
					return h.OpenErrors(args)
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
			sort.Strings(files)
			for _, f := range files {
				if err := lua.LoadFileStep("plugin "+f, f, lua.OriginPlugin).Run(ctx, r, g); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// projectConfigStep loads <project>/.zemi/init.lua behind the one-time
// trust prompt: the config tree is hashed (symlinks refused), the hash
// stored in the state dir, and a changed hash re-prompts before load
// (DESIGN §5).
func projectConfigStep(h *hostapi.Host, askTrust func(path, hash string) string, dirs config.Dirs) lua.BootStep {
	return lua.BootStep{
		Name: "project config",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			pdir := dirs.ProjectConfigDir()
			if pdir == "" {
				return nil
			}
			// Agents, commands and workflows are project config too: any of
			// them triggers the same trust prompt as init.lua.
			initPath := filepath.Join(pdir, "init.lua")
			present := false
			for _, name := range []string{"init.lua", "agents", "commands", "workflows"} {
				if _, err := os.Stat(filepath.Join(pdir, name)); err == nil {
					present = true
				}
			}
			if !present {
				return nil
			}
			hash, err := hashTree(pdir)
			if err != nil {
				return err
			}
			// The trust file maps the dir to the approved hash, or to "!"+hash
			// for a tree the user denied for good; a changed tree asks again.
			trusted := readTrust(dirs.TrustFile())
			switch trusted[pdir] {
			case hash:
			case "!" + hash:
				h.Log.Info("trust", "project config denied earlier; not loading", map[string]any{"dir": pdir})
				return nil
			default:
				answer := askTrust(pdir, hash)
				if answer == "once" {
					h.Log.Warn("trust", "project config refused; not loading", map[string]any{"dir": pdir})
					return nil
				}
				trusted[pdir] = hash
				if answer == "deny" {
					trusted[pdir] = "!" + hash
				}
				if err := writeTrust(dirs.TrustFile(), trusted); err != nil {
					return err
				}
				if answer == "deny" {
					h.Log.Warn("trust", "project config denied; not loading", map[string]any{"dir": pdir})
					return nil
				}
			}
			h.SetProjectTrusted(true)
			if _, err := os.Stat(initPath); err != nil {
				return nil
			}
			return lua.LoadFileStep("project init", initPath, lua.OriginProject).Run(ctx, r, g)
		},
	}
}

// hashTree fingerprints a project config tree: sorted relative paths plus
// file contents over sha256. Symlinks and non-regular files are refused
// (git mode 120000 aliases unreviewed content, DESIGN §10.4).
func hashTree(root string) (string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if t := d.Type(); t&fs.ModeDir == 0 && t != 0 {
			return fmt.Errorf("project config contains a symlink or non-regular file: %s", p)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	sum := sha256.New()
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(sum, "%s\x00", rel)
		sum.Write(data)
		fmt.Fprint(sum, "\x00")
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
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
	return os.WriteFile(path, data, 0o600)
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
