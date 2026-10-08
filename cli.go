package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"golang.org/x/mod/module"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// envHelp ends the root's help only.
const envHelp = `{{if not .HasParent}}
Environment:
  ZEMI_CONFIG_HOME  config: init.lua, skills (~/.config/zemi)
  ZEMI_DATA_HOME    data: plugins, model catalog (~/.local/share/zemi)
  ZEMI_STATE_HOME   state: sessions, memory, logs (~/.local/state/zemi)
  ZEMI_CACHE_HOME   cache: language server builds (~/.cache/zemi)
                    each defaults to $XDG_<NAME>_HOME/zemi when that is set;
                    a relative path in either is ignored
  ANTHROPIC_API_KEY, OPENAI_API_KEY, ...
                    a builtin provider is on while its key is exported

Exit status: 0 success, 1 error, 2 usage error, 130 interrupted.
{{end}}`

// version is set at release time with -ldflags "-X main.version=v1.2.3";
// otherwise the module version from the build info is used.
var version string

type options struct {
	run, cont, json  bool
	quiet, reasoning bool
	resume, model    string
	mode, profile    string
	prompt           string
	text             bool   // the prompt came from stdin after -: a leading / is text, never a command
	dir              string // -C
	trust            *bool  // --trust for this run; nil when not given
	// dirs are this run's, resolved once by the root's hook after -C; a
	// completion resolves its own, since cobra runs no hook there.
	dirs config.Dirs
}

// exitError is the error of a command that printed its own diagnostic.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func status(code int) error {
	if code == 0 {
		return nil
	}
	return exitError(code)
}

// execute runs the command line and returns the exit status. launch opens
// the TUI or, with options.run, answers one prompt on stdout.
func execute(args []string, stdout, stderr io.Writer, launch func(options) int) int {
	var o options
	// cobra runs PersistentPreRunE once flags and arguments passed its checks,
	// so an error before it is a usage error and one after it a failure
	parsed := false
	root := newRoot(&o, launch)
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		if o.dir != "" {
			if err := enterDir(o.dir); err != nil {
				return err
			}
		}
		parsed = true
		// -C has run, so one resolution serves every command in this run; a
		// failure here is not a usage error
		dirs, err := config.ResolveDirs()
		if err != nil {
			return err
		}
		o.dirs = dirs
		return nil
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	polish(root) // after SetOut: cobra's completion command keeps the writer it is made with
	c, err := root.ExecuteC()
	if s, ok := errors.AsType[exitError](err); ok {
		return int(s)
	}
	switch {
	case err == nil:
		return 0
	case !parsed:
		fmt.Fprintf(stderr, "zemi: %v\nRun '%s --help' for usage.\n", err, c.CommandPath())
		return 2
	case c.HasParent():
		fmt.Fprintf(stderr, "zemi: %s: %v\n", strings.TrimPrefix(c.CommandPath(), "zemi "), err)
	default:
		fmt.Fprintln(stderr, "zemi:", err)
	}
	return 1
}

// enterDir is -C: run as if started in dir. PWD follows, since children get
// an explicit environment that os/exec does not correct.
func enterDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.Chdir(abs); err != nil {
		return fmt.Errorf("-C %s: %w", dir, errors.Unwrap(err)) // the PathError names abs
	}
	return os.Setenv("PWD", abs)
}

func newRoot(o *options, launch func(options) int) *cobra.Command {
	root := &cobra.Command{
		Use:   "zemi [prompt...]",
		Short: "zemi - a terminal coding agent",
		Long: `
╭────╮                  ·
│╭──╮│  ╷╷  ━━┓ ┏━┓ ┏┳┓ ╻
│╰──╯│╭─┴┴╮ ┏━┛ ┣━┛ ┃┃┃ ┃
╰────╯╯   ╵ ┗━━ ┗━━ ╹╹╹ ╹
@zemi a terminal coding agent

With no command zemi opens the TUI; a prompt is sent as the first message,
or run when it names a command (zemi "/review HEAD~1").`,
		Example: `  zemi "fix the failing test"
  git diff | zemi run "review this"
  zemi run "/review HEAD~1" > review.md
  zemi -- plugins are not loading      after --, it is all prompt`,
		Version:       versionString(),
		Args:          o.finish,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          func(*cobra.Command, []string) error { return status(launch(*o)) },
	}
	root.SuggestionsMinimumDistance = 2
	root.SetHelpTemplate(root.HelpTemplate() + envHelp)
	root.SetVersionTemplate("zemi {{.Version}}\n")
	root.PersistentFlags().StringVarP(&o.dir, "cd", "C", "", "run in `dir`, as if zemi started there")
	_ = root.MarkPersistentFlagDirname("cd")
	turnFlags(root, o)

	run := &cobra.Command{
		Use:   "run [prompt... | -]",
		Short: "answer one prompt on stdout and exit",
		Long: `Answer one prompt and exit, with no TUI. stdout gets the last message, or
with --json one JSON event per line; stderr shows the run as the chat draws it.
Piped stdin is attached behind the prompt; - reads the prompt itself from
stdin. Nobody can approve a call, so one the mode leaves to the user is denied
and stderr lists them at the end.`,
		Example: `  zemi run "explain main.go"
  git diff | zemi run "review this"
  zemi run - < task.md
  zemi run -c "and the tests?"`,
		Args: o.finish,
		RunE: func(*cobra.Command, []string) error {
			o.run = true
			return status(launch(*o))
		},
	}
	turnFlags(run, o)
	run.Flags().BoolVar(&o.json, "json", false, "print one JSON event per line, the done report last")
	run.Flags().BoolVarP(&o.quiet, "quiet", "q", false, "show no run on stderr, only what went wrong")
	run.Flags().BoolVar(&o.reasoning, "reasoning", false, "show every thought and call whole")

	var healthJSON *bool
	health := &cobra.Command{
		Use:   "health",
		Short: "run the /health checks; 1 if any failed",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			h, done, err := bootHeadless(*o, false, os.Stderr, io.Discard) // the trust row says it
			if err != nil {
				return err
			}
			defer done()
			return status(printHealth(h, c.OutOrStdout(), *healthJSON))
		},
	}
	healthJSON = jsonFlag(health)

	root.AddCommand(run, sessionsCommand(o), projectsCommand(o), pluginsCommand(o), modelsCommand(o), health)
	return root
}

// turnFlags are the flags of a command that runs a turn.
func turnFlags(c *cobra.Command, o *options) {
	f := c.Flags()
	f.BoolVarP(&o.cont, "continue", "c", false, "continue the most recent session")
	f.StringVarP(&o.resume, "resume", "r", "", "resume the session with this `id`")
	f.StringVarP(&o.model, "model", "m", "", "model for the conversation, by provider/model `id`")
	f.StringVar(&o.mode, "mode", "", "permission `mode`: auto, yolo or one init.lua adds")
	f.StringVar(&o.profile, "profile", "", "agent `profile`: default, planner or one of init.lua's")
	f.Bool("trust", false, "trust the project for this run; --trust=false does not")
	_ = c.RegisterFlagCompletionFunc("resume", completeSessions)
	_ = c.RegisterFlagCompletionFunc("model", completeBooted((*hostapi.Host).ModelIDs))
	_ = c.RegisterFlagCompletionFunc("profile", completeBooted((*hostapi.Host).ProfileNames))
	_ = c.RegisterFlagCompletionFunc("mode", completeBooted(modeNames))
}

// completeBooted offers what only config knows (models, profiles, modes),
// read by an inert boot. The dirs are resolved here: cobra runs no hook
// before a completion.
func completeBooted(names func(*hostapi.Host) []string) cobra.CompletionFunc {
	return func(c *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		dirs, err := completionDirs(c)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		h, done, err := bootHeadless(options{dirs: dirs}, true, os.Stderr, io.Discard)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		defer done()
		return names(h), cobra.ShellCompDirectiveNoFileComp
	}
}

// completionDirs are the dirs a completion reads, after -C: cobra runs no
// hook before a completion function.
func completionDirs(c *cobra.Command) (config.Dirs, error) {
	if dir, _ := c.Flags().GetString("cd"); dir != "" {
		if err := enterDir(dir); err != nil {
			return config.Dirs{}, err
		}
	}
	return config.ResolveDirs()
}

// completeSessions offers this project's session ids, titled.
func completeSessions(c *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	var out []cobra.Completion
	dirs, err := completionDirs(c)
	if err != nil || c.Name() == "export" && len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	db, err := projectStore(dirs)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer db.Close()
	list, _ := db.ListSessions(-1)
	for _, s := range list {
		title := []rune(sanitize.Line(s.Title))
		out = append(out, cobra.CompletionWithDesc(s.ID, string(title[:min(len(title), 60)])))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completePlugins offers the plugin names: every argument of update, the
// first of the other verbs, and test's first may be a directory too.
func completePlugins(c *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	directive := cobra.ShellCompDirectiveNoFileComp
	if c.Name() == "test" {
		directive = cobra.ShellCompDirectiveDefault
	}
	dirs, err := completionDirs(c)
	if err != nil || c.Name() != "update" && len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ps, _ := plugin.Scan(dirs)
	var out []cobra.Completion
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out, directive
}

// finish checks a turn's arguments and joins them into the prompt. A lone
// word that is a near miss of a command is refused: sent, it would open the
// TUI and pay for a model call on a typo.
func (o *options) finish(c *cobra.Command, args []string) error {
	if !c.HasParent() && len(args) == 1 && c.ArgsLenAtDash() < 0 {
		if s := c.SuggestionsFor(args[0]); len(s) > 0 {
			return fmt.Errorf("%q is not a command; did you mean %s? (a one-word prompt: zemi -- %s)", args[0], s[0], args[0])
		}
	}
	if o.cont && o.resume != "" {
		return errors.New("--continue and --resume cannot be combined")
	}
	if f := c.Flags().Lookup("trust"); f.Changed {
		o.trust = new(f.Value.String() == "true")
	}
	o.prompt = strings.TrimSpace(strings.Join(args, " "))
	return nil
}

// polish adds cobra's own commands and flags, in zemi's words.
func polish(c *cobra.Command) {
	c.InitDefaultHelpFlag()
	c.Flags().Lookup("help").Usage = "show this help"
	if !c.HasParent() {
		c.InitDefaultHelpCmd()
		c.InitDefaultCompletionCmd()
		c.InitDefaultVersionFlag()
		c.Flags().Lookup("version").Usage = "print the version"
	}
	for _, s := range c.Commands() {
		switch s.Name() {
		case "help":
			s.Short = "show the help of a command"
		case "completion":
			s.Short = "print a shell completion script"
			s.Long = s.Short + "; each shell's help says where it goes"
			for _, sh := range s.Commands() {
				sh.Short = "print the " + sh.Name() + " completion script"
			}
		}
		polish(s)
	}
}

// nargs checks that a command has one to maxN positional arguments, naming
// what its Use line takes.
func nargs(maxN int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) < 1 || len(args) > maxN {
			return fmt.Errorf("%s takes %s", strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" "), strings.TrimPrefix(c.Use, c.Name()+" "))
		}
		return nil
	}
}

// pluginsCommand is zemi plugins: no Lua boot, TUI, trust or store, so it
// works beside a running TUI, whose config watcher reloads on the change.
func pluginsCommand(o *options) *cobra.Command {
	yes := false
	var asJSON *bool
	// git clones and fetches: an interrupt stops them and exits with 130
	run := func(c *cobra.Command, args []string) error {
		// asked before the signal context, so ctrl+c at the prompt ends zemi
		if c.Name() == "delete" && !yes && !confirm(c, "Delete the plugin "+args[0]+" and its files?") {
			fmt.Fprintf(c.ErrOrStderr(), "zemi: kept the plugin %s\n", args[0]) // a declined prompt, as rm -i: not a failure
			return nil
		}
		ctx, stop := signalContext()
		defer stop()
		dirs := o.dirs
		var err error
		switch {
		case c.Name() == "plugins":
			var ps []plugin.Plugin
			if ps, err = plugin.List(ctx, dirs); err == nil && len(ps) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no plugins yet; zemi plugins install <git url> adds one")
			}
			printPlugins(c.OutOrStdout(), ps, *asJSON)
		default:
			err = plugin.Run(ctx, dirs, append([]string{c.Name()}, args...), c.OutOrStdout())
		}
		if ctx.Err() != nil {
			return exitError(130)
		}
		return err
	}
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "list plugins: name, version, on or off, source",
		Args:  cobra.NoArgs,
		RunE:  run,
	}
	asJSON = jsonFlag(cmd)
	for _, v := range []struct {
		use, short string
		args       cobra.PositionalArgs
		names      cobra.CompletionFunc
	}{
		{"install <git url> [name]", "clone a plugin at its highest vX.Y.Z tag", nargs(2), nil},
		{"update [name...]", "update within the major version; every plugin without a name", cobra.ArbitraryArgs, completePlugins},
		{"check", "fetch, then list the updates", cobra.NoArgs, nil},
		{"enable <name>", "load the plugin from the next start", nargs(1), completePlugins},
		{"disable <name>", "stop loading the plugin", nargs(1), completePlugins},
	} {
		cmd.AddCommand(&cobra.Command{Use: v.use, Short: v.short, Args: v.args, ValidArgsFunction: v.names, RunE: run})
	}
	// a local plugin is the user's own code with no copy anywhere, and off a
	// terminal nobody answers, so an agent's command cannot delete one
	del := &cobra.Command{
		Use:               "delete <name>",
		ValidArgsFunction: completePlugins,
		Short:             "delete a plugin and its files; asks first",
		Args: cobra.MatchAll(nargs(1), func(_ *cobra.Command, args []string) error {
			if !yes && !term.IsTerminal(os.Stdin.Fd()) {
				return fmt.Errorf("plugins delete: stdin is not a terminal to ask on; pass --yes to delete %s", args[0])
			}
			return nil
		}),
		RunE: run,
	}
	del.Flags().BoolVar(&yes, "yes", false, "delete without asking")
	cmd.AddCommand(del)
	cmd.AddCommand(&cobra.Command{
		Use:               "test <name|dir> [pattern]",
		ValidArgsFunction: completePlugins,
		Short:             "run its tests, each case in a zemi of its own",
		Args:              nargs(2),
		RunE: func(c *cobra.Command, args []string) error {
			ctx, stop := signalContext()
			defer stop()
			return status(testPlugins(ctx, args, c.OutOrStdout(), c.ErrOrStderr()))
		},
	})
	return cmd
}

func sessionsCommand(o *options) *cobra.Command {
	var asJSON *bool
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "list this project's sessions, newest first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dirs := o.dirs
			var list []*store.Session
			db, err := projectStore(dirs)
			if err == nil {
				defer db.Close()
				list, err = db.ListSessions(-1)
			}
			if errors.Is(err, fs.ErrNotExist) || err == nil && len(list) == 0 {
				fmt.Fprintf(c.ErrOrStderr(), "zemi: no sessions in %s yet\n", perm.TildePath(dirs.TrustRoot()))
				return nil
			}
			out, p := c.OutOrStdout(), colorprofile.Detect(c.OutOrStdout(), os.Environ())
			if !*asJSON && p != colorprofile.NoTTY {
				fmt.Fprintln(out, paint(p, "title", fmt.Sprintf("%-8s  %-16s  %s", "session", "updated", "title")))
			}
			for _, s := range list {
				updated := time.UnixMilli(s.UpdatedAt)
				if *asJSON {
					writeJSON(out, struct {
						ID      string    `json:"id"`
						Title   string    `json:"title"`
						Updated time.Time `json:"updated"`
						CostUSD float64   `json:"cost_usd"`
					}{s.ID, sanitize.Line(s.Title), updated, s.Cost})
					continue
				}
				fmt.Fprintf(out, "%s  %s  %s\n", paint(p, "hint", s.ID), paint(p, "hint", updated.Format(hostapi.StampLayout)), ansi.Truncate(cmp.Or(sanitize.Line(s.Title), "(untitled)"), 60, "…"))
			}
			return err
		},
	}
	asJSON = jsonFlag(cmd)
	full := false
	export := &cobra.Command{
		Use:               "export <id>",
		ValidArgsFunction: completeSessions,
		Short:             "print a session as markdown",
		Args:              nargs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dirs := o.dirs
			db, err := projectStore(dirs)
			md := ""
			if err == nil {
				defer db.Close()
				md, err = hostapi.SessionMarkdown(db, args[0], full)
			}
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no session %q; zemi sessions lists them", args[0])
			}
			fmt.Fprint(c.OutOrStdout(), md)
			return err
		},
	}
	export.Flags().BoolVar(&full, "full", false, "add reasoning, every tool call expanded and the agent runs")
	cmd.AddCommand(export)
	return cmd
}

// projectsCommand is zemi projects: one line per project, the path last and
// absolute at column 36, so cut -c36- hands it to zemi -C.
func projectsCommand(o *options) *cobra.Command {
	var asJSON *bool
	cmd := &cobra.Command{
		Use:   "projects",
		Short: "list the projects zemi keeps state for, last used first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ps := hostapi.ListProjects(o.dirs, true)
			if len(ps) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no projects yet")
			}
			out, cp := c.OutOrStdout(), colorprofile.Detect(c.OutOrStdout(), os.Environ())
			if !*asJSON && cp != colorprofile.NoTTY && len(ps) > 0 {
				fmt.Fprintln(out, paint(cp, "title", fmt.Sprintf("%-16s  %-9s  %4s  %s", "used", "trust", "sess", "root")))
			}
			for _, p := range ps {
				if *asJSON {
					writeJSON(out, struct {
						Root     string    `json:"root"`
						Trust    string    `json:"trust"`
						Used     time.Time `json:"used,omitzero"`
						Sessions int       `json:"sessions"`
						Memories int       `json:"memories"`
					}{p.Root, p.TrustWord(), p.Used, p.Sessions, p.Memories})
					continue
				}
				used := "-"
				if !p.Used.IsZero() {
					used = p.Used.Format(hostapi.StampLayout)
				}
				fmt.Fprintf(out, "%s  %s  %4d  %s\n", pad(cp, "hint", used, 16), pad(cp, statusRole[p.TrustWord()], p.TrustWord(), 9), p.Sessions, p.Root)
			}
			return nil
		},
	}
	asJSON = jsonFlag(cmd)
	return cmd
}

func modelsCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "list the models -m takes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			h, done, err := bootHeadless(*o, true, os.Stderr, io.Discard)
			if err != nil {
				return err
			}
			defer done()
			ids := h.ModelIDs()
			if len(ids) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no models yet;", h.NoModelHint())
				return exitError(1)
			}
			fmt.Fprintln(c.OutOrStdout(), strings.Join(ids, "\n"))
			return nil
		},
	}
	// the catalog picks where each key is sent, so it lives in <data>, which
	// no sandbox writes; nothing boots
	cmd.AddCommand(&cobra.Command{
		Use:   "update",
		Short: "fetch the latest model catalog",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx, stop := signalContext()
			defer stop()
			cat, changed, err := agent.UpdateCatalog(ctx, agent.CatalogURL, o.dirs.DataHome, os.Getenv)
			switch {
			case ctx.Err() != nil:
				return exitError(130)
			case err != nil:
				return err
			case changed:
				fmt.Fprintln(c.OutOrStdout(), "Model catalog updated:", cat.Info.Summary())
			default:
				fmt.Fprintln(c.OutOrStdout(), "Model catalog is up to date")
			}
			return nil
		},
	})
	return cmd
}

// projectStore opens this project's store read-only, so a look or a tab
// press writes nothing, a missing store included (fs.ErrNotExist).
func projectStore(dirs config.Dirs) (*store.DB, error) {
	return store.OpenRead(store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot()))
}

// jsonFlag adds --json to a listing command: JSON Lines on stdout, one
// object per item, as zemi run --json prints its events.
func jsonFlag(c *cobra.Command) *bool {
	return c.Flags().Bool("json", false, "print one JSON object per line")
}

// roleStyles are the CLI's colors: basic ANSI ones, so the terminal's own
// palette reads on a light or a dark ground. Any other role is plain.
var roleStyles = map[string]uv.Style{
	"ok":     {Fg: ansi.Green},
	"warn":   {Fg: ansi.Yellow},
	"danger": {Fg: ansi.Red},
	"hint":   {Fg: ansi.BrightBlack},
	"title":  {Fg: ansi.Blue, Attrs: uv.AttrBold},
}

// statusRole is the role of each status word the CLI prints.
var statusRole = map[string]string{
	"ok": "ok", "on": "ok", "trusted": "ok",
	"warn": "warn", "undecided": "warn",
	"error": "danger", "missing": "danger",
	"off": "hint", "untrusted": "hint",
}

// paint styles text in role for the output p describes: unchanged off a
// terminal, bold alone under NO_COLOR.
func paint(p colorprofile.Profile, role, text string) string {
	st := uv.ConvertStyle(roleStyles[role], p)
	return st.Styled(text)
}

// pad is text painted in role and padded to w cells by its visible width,
// so a column lines up with the escapes around it.
func pad(p colorprofile.Profile, role, text string, w int) string {
	return paint(p, role, text) + strings.Repeat(" ", max(w-ansi.StringWidth(text), 0))
}

// printPlugins writes one aligned line per plugin, the free-text source
// last so cut and awk read the name, and a header only on a terminal.
func printPlugins(w io.Writer, ps []plugin.Plugin, asJSON bool) {
	p := colorprofile.Detect(w, os.Environ())
	var rows [][4]string
	if !asJSON && p != colorprofile.NoTTY && len(ps) > 0 {
		rows = append(rows, [4]string{"name", "version", "on", "source"})
	}
	for _, pl := range ps {
		version, source, on := pl.Version, pl.URL, "on"
		if pl.Local {
			version, source = cmp.Or(version, "local"), pl.Dir
		}
		if !pl.Enabled {
			on = "off"
		}
		if asJSON {
			writeJSON(w, struct {
				Name    string `json:"name"`
				Version string `json:"version"`
				Local   bool   `json:"local"`
				Enabled bool   `json:"enabled"`
				Source  string `json:"source"`
			}{pl.Name, pl.Version, pl.Local, pl.Enabled, source})
			continue
		}
		rows = append(rows, [4]string{pl.Name, version, on, source})
	}
	var width [3]int
	for _, r := range rows {
		for i := range width {
			width[i] = max(width[i], ansi.StringWidth(r[i])+2)
		}
	}
	for i, r := range rows {
		roles := [4]string{"", "hint", statusRole[r[2]], "hint"}
		if i == 0 && len(rows) > len(ps) {
			roles = [4]string{"title", "title", "title", "title"}
		}
		fmt.Fprintln(w, pad(p, roles[0], r[0], width[0])+pad(p, roles[1], r[1], width[1])+pad(p, roles[2], r[2], width[2])+paint(p, roles[3], r[3]))
	}
}

// confirm asks on stderr and reads the answer from stdin; only y or yes is
// a yes.
func confirm(c *cobra.Command, question string) bool {
	fmt.Fprint(c.ErrOrStderr(), question+" [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func versionString() string {
	if version != "" {
		return normalizeVersion(version)
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		return normalizeVersion(bi.Main.Version)
	}
	return normalizeVersion("")
}

// normalizeVersion is the one name a build answers to: --version, the
// splash and /health all print this. A pseudo-version (a commit past a tag,
// or no tag at all) says nothing to a person, so it reads "dev".
func normalizeVersion(v string) string {
	if v == "" || v == "(devel)" || module.IsPseudoVersion(v) {
		return "dev"
	}
	return v
}

// readStdin reads piped stdin for zemi run: behind the marker that ends what
// the user typed, or as the typed prompt itself when the prompt is "-". The
// TUI needs the terminal on stdin, so a pipe there is a usage error.
func (o *options) readStdin(stdin *os.File) error {
	fi, err := stdin.Stat()
	dash := o.prompt == "-"
	switch {
	case dash && (err != nil || term.IsTerminal(stdin.Fd())):
		return errors.New("- reads the prompt from stdin, which is a terminal")
	// the TUI opens /dev/tty when stdin is </dev/null
	case !dash && (err != nil || fi.Mode()&os.ModeCharDevice != 0):
		return nil
	case !o.run:
		return errors.New("stdin is not a terminal; zemi run answers from a pipe")
	}
	// NOTE: stdin over 1 MiB is refused; larger input belongs in a file the
	// agent reads.
	first, got := make(chan struct{}), make(chan []byte, 1)
	go func() {
		head := make([]byte, 1)
		n, _ := stdin.Read(head)
		close(first)
		rest, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
		got <- append(head[:n], rest...)
	}()
	// Schedulers and editors often leave an idle pipe on stdin: with a
	// prompt argument, stdin that stays silent for 3s is skipped. A slow
	// producer is fine once its first byte arrives.
	if o.prompt != "" && !dash {
		select {
		case <-first:
		case <-time.After(3 * time.Second):
			fmt.Fprintln(os.Stderr, "zemi: no stdin within 3s, continuing without it")
			return nil
		}
	}
	b := <-got
	if len(b) > 1<<20 {
		return errors.New("stdin is over 1 MiB; write it to a file and name the file in the prompt")
	}
	switch in := strings.TrimSpace(string(b)); {
	case dash && in == "":
		return errors.New("- reads the prompt from stdin, which is empty")
	case dash:
		o.prompt, o.text = in, true
	case in != "":
		o.prompt += agent.Attached("stdin", in)
	}
	return nil
}

// headlessTrust answers the trust prompt when nobody can: not now, with one
// line saying what that turns off.
func headlessTrust(stderr io.Writer) func(root, cwd, state string) string {
	return func(root, _, _ string) string {
		fmt.Fprintf(stderr, "zemi: %s is not trusted: no .agents or AGENTS.md, only read-only commands run; --trust\n", perm.TildePath(root))
		return "later"
	}
}

// applyOptions applies the flags after boot, so they win over init.lua.
func applyOptions(h *hostapi.Host, o options) error {
	if o.profile != "" {
		if err := h.SetProfile(o.profile); err != nil {
			return err
		}
	}
	if o.model != "" {
		if err := h.SetModel("chat " + o.model); err != nil {
			return err
		}
	}
	if o.mode != "" {
		// checked here, after init.lua: it may add modes
		if err := h.SetPermMode(o.mode); err != nil {
			return err
		}
	}
	if o.cont || o.resume != "" {
		return h.ResumeSession(o.resume)
	}
	return nil
}

// printHealth writes the /health checks for a headless run; any failed
// check makes the exit status 1, so a script can gate on it. MCP servers
// settle first, so their row says what a run would get.
func printHealth(h *hostapi.Host, stdout io.Writer, asJSON bool) int {
	ctx, stop := signalContext()
	defer stop()
	if h.WaitMCP(ctx); ctx.Err() != nil {
		return 130
	}
	code, p := 0, colorprofile.Detect(stdout, os.Environ())
	for _, r := range h.HealthRows() {
		if r.Level == hostapi.HealthError {
			code = 1
		}
		if asJSON {
			writeJSON(stdout, struct {
				Level  hostapi.HealthLevel `json:"level"`
				Name   string              `json:"name"`
				Detail string              `json:"detail"`
			}{r.Level, r.Name, r.Detail})
			continue
		}
		fmt.Fprintf(stdout, "%s %s %s\n", pad(p, statusRole[string(r.Level)], string(r.Level), 5), pad(p, "", r.Name, 12), r.Detail)
	}
	return code
}

// signalContext ends on an interrupt, a kill or the terminal closing, which
// a command reports as exit status 130. A hangup ignored at start (nohup)
// stays ignored, since Notify would install a handler for it.
func signalContext() (context.Context, context.CancelFunc) {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return signal.NotifyContext(context.Background(), sigs...)
}

// sendPrompt sends a command-line prompt (Host.SendPrompt, or as text to
// the model when text) once no MCP server is starting, since its one send
// builds the tool set, unless ctx ends the wait.
func sendPrompt(ctx context.Context, h *hostapi.Host, prompt string, text bool) error {
	if h.WaitMCP(ctx); ctx.Err() != nil {
		return ctx.Err()
	}
	if text {
		// the command line's prompt is the user's own: one that joins a running
		// turn hands that turn's foreground calls off. Headless marks nothing,
		// since no UI would take the report a handoff delivers (Host.UserSpoke).
		return h.AgentSendUser(prompt)
	}
	return h.SendPrompt(prompt)
}
