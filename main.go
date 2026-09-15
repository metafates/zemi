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
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/ui"
	zemiRuntime "github.com/metafates/zemi/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "zemi:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	dirs := config.ResolveDirs()
	if err := dirs.Ensure(); err != nil {
		return err
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
	app := ui.NewApp(host, log)
	host.SetUIHooks(app)
	host.EnsureConfigRepo()

	steps := []lua.BootStep{
		host.NamespacesStep(),
		host.PermissionsLoadStep(),
		builtinCommandsStep(host, app),
		host.MemoryCommandsStep(),
		host.ReflectorCommandsStep(),
		host.ConfigCommandsStep(),
		host.SkillsCommandsStep(),
		lua.RequireStep("zemi.defaultui"),
		pluginsStep(dirs),
		lua.LoadFileStep("init", filepath.Join(dirs.ConfigHome, "init.lua"), lua.OriginUser),
		host.SkillsAuthoredHooksStep(),
		projectConfigStep(host, app, dirs),
		crushImportStep(host, settings),
		readyStep(host, settings),
		host.SkillsLearnedHooksStep(),
	}

	p := tea.NewProgram(app)
	app.SetProgram(p)
	app.SetReload(func() {
		if err := rt.ForceReload(ctx, steps, host.PurgeGeneration); err != nil {
			log.Error("reload", "force reload failed", map[string]any{"error": err.Error()})
		}
	})

	// Boot runs on its own goroutine (it may block on the trust prompt);
	// the outcome reaches the UI through the hook channel.
	go func() {
		app.SignalBoot(rt.Boot(ctx, steps))
	}()

	_, err := p.Run()
	return err
}

// crushImportStep imports providers from the user's crush.json (config
// key providers.crush_json, default ~/.config/crush/crush.json) after
// user config ran, so init.lua can point the knob elsewhere or disable
// it. Import problems never fail boot; they are logged.
func crushImportStep(h *hostapi.Host, settings *config.Store) lua.BootStep {
	return lua.BootStep{
		Name: "crush providers import",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			path, _ := settings.Get("providers.crush_json")
			p, _ := path.(string)
			p = expandPath(p)
			if p == "" {
				return nil
			}
			res, err := h.Providers.ImportCrushFile(p)
			if err != nil {
				h.Log.Warn("providers", "crush.json import failed", map[string]any{"error": err.Error()})
				return nil
			}
			for _, id := range res.Imported {
				h.Log.Info("providers", "imported from crush.json", map[string]any{"provider": id})
			}
			for _, w := range res.Warnings {
				h.Log.Warn("providers", w, nil)
			}
			return nil
		},
	}
}

func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// builtinCommandsStep registers the Go-owned shell commands (/quit, /help,
// /model). They carry the builtin origin, so user config can
// rebind them.
func builtinCommandsStep(h *hostapi.Host, app *ui.App) lua.BootStep {
	return lua.BootStep{
		Name: "builtin commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			quit := func(h *hostapi.Host, args string) error {
				app.Quit()
				return nil
			}
			for _, c := range []*hostapi.Command{
				{Name: "quit", Desc: "quit zemi", GoFn: quit, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(g.ID)},
				{Name: "help", Desc: "show keys and commands", GoFn: func(h *hostapi.Host, args string) error {
					h.OpenHelp()
					return nil
				}, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(g.ID)},
				{Name: "new", Desc: "start a fresh session", GoFn: func(h *hostapi.Host, _ string) error {
					return h.NewSession()
				}, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(g.ID)},
				{Name: "sessions", Desc: "list and resume sessions", GoFn: func(h *hostapi.Host, _ string) error {
					return h.OpenSessions()
				}, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(g.ID)},
				{Name: "resume", Desc: "resume a session (the latest without an id)", GoFn: func(h *hostapi.Host, args string) error {
					return h.ResumeSession(strings.TrimSpace(args))
				}, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(g.ID)},
				{Name: "health", Desc: "check model, store, permissions, reflector and runtime", GoFn: func(h *hostapi.Host, _ string) error {
					return h.OpenHealth()
				}, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(g.ID)},
				{Name: "compact", Desc: "summarize the conversation to free context", GoFn: func(h *hostapi.Host, _ string) error {
					return h.CompactNow()
				}, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(g.ID)},
				{Name: "model", Desc: "choose the model", GoFn: func(h *hostapi.Host, args string) error {
					h.OpenModelPicker()
					return nil
				}, Origin: lua.OriginBuiltin, Gen: hostapi.Gen(g.ID)},
			} {
				if _, err := h.Commands.Add(c); err != nil {
					return err
				}
			}
			return nil
		},
	}
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
func projectConfigStep(h *hostapi.Host, app *ui.App, dirs config.Dirs) lua.BootStep {
	return lua.BootStep{
		Name: "project config",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			pdir := dirs.ProjectConfigDir()
			if pdir == "" {
				return nil
			}
			initPath := filepath.Join(pdir, "init.lua")
			if _, err := os.Stat(initPath); err != nil {
				return nil
			}
			hash, err := hashTree(pdir)
			if err != nil {
				return err
			}
			trusted := readTrust(dirs.TrustFile())
			if trusted[pdir] != hash {
				if !app.AskTrust(pdir, hash) {
					h.Log.Warn("trust", "project config refused; not loading", map[string]any{"dir": pdir})
					return nil
				}
				trusted[pdir] = hash
				if err := writeTrust(dirs.TrustFile(), trusted); err != nil {
					return err
				}
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
		if p == root {
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

// readyStep closes the autonomy-knob boot window and emits app.ready; the
// window closes before the handlers run (DESIGN §10.8).
func readyStep(h *hostapi.Host, settings *config.Store) lua.BootStep {
	return lua.BootStep{
		Name: "app.ready",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			settings.CloseBootWindow()
			h.EmitOnState(g.L, "app.ready", nil)
			// The last session is reflected in the background at startup,
			// so quitting never waits on the reflector (DESIGN §10.3).
			go h.ReflectLastSession()
			return nil
		},
	}
}
