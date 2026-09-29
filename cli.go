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
		Short: "zemi - a terminal coding agent, context included",
		Long: `zemi - a terminal coding agent, context included

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
	run.Flags().BoolVar(&o.reasoning, "reasoning", false, "show reasoning, every call and whole command output, as ctrl+o does")

	health := &cobra.Command{
		Use:   "health",
		Short: "run the /health checks; 1 if any failed",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			h, done, err := bootHeadless(options{}, false, os.Stderr, io.Discard) // the trust row says it
			if err != nil {
				return err
			}
			defer done()
			return status(printHealth(h, c.OutOrStdout()))
		},
	}

	root.AddCommand(run, sessionsCommand(), projectsCommand(), pluginsCommand(), modelsCommand(), health)
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
// read by an inert boot.
func completeBooted(names func(*hostapi.Host) []string) cobra.CompletionFunc {
	return func(c *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if _, err := completionDirs(c); err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		h, done, err := bootHeadless(options{}, true, os.Stderr, io.Discard)
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
		title := []rune(sessionTitle(s))
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
		}
		polish(s)
	}
}

// pluginsCommand is zemi plugins: no Lua boot, TUI, trust or store, so it
// works beside a running TUI, whose config watcher reloads on the change.
func pluginsCommand() *cobra.Command {
	yes := false
	// git clones and fetches: an interrupt stops them and exits with 130
	run := func(c *cobra.Command, args []string) error {
		// asked before the signal context, so ctrl+c at the prompt ends zemi
		if c.Name() == "delete" && !yes && !confirm(c, "Delete the plugin "+args[0]+" and its files?") {
			fmt.Fprintf(c.ErrOrStderr(), "zemi: kept the plugin %s\n", args[0]) // a declined prompt, as rm -i: not a failure
			return nil
		}
		ctx, stop := signalContext()
		defer stop()
		dirs, err := config.ResolveDirs()
		switch {
		case err != nil:
		case c.Name() == "plugins":
			var ps []plugin.Plugin
			if ps, err = plugin.List(ctx, dirs); err == nil && len(ps) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no plugins yet; zemi plugins install <git url> adds one")
			}
			err = errors.Join(err, plugin.Table(c.OutOrStdout(), ps))
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
	for _, v := range []struct {
		use, short string
		args       cobra.PositionalArgs
		names      cobra.CompletionFunc
	}{
		{"install <git url> [name]", "clone a plugin at its highest vX.Y.Z tag", cobra.RangeArgs(1, 2), nil},
		{"update [name...]", "update within the major version; every plugin without a name", cobra.ArbitraryArgs, completePlugins},
		{"check", "fetch, then list the updates", cobra.NoArgs, nil},
		{"enable <name>", "load the plugin from the next start", cobra.ExactArgs(1), completePlugins},
		{"disable <name>", "stop loading the plugin", cobra.ExactArgs(1), completePlugins},
	} {
		cmd.AddCommand(&cobra.Command{Use: v.use, Short: v.short, Args: v.args, ValidArgsFunction: v.names, RunE: run})
	}
	// a local plugin is the user's own code with no copy anywhere, and off a
	// terminal nobody answers, so an agent's command cannot delete one
	del := &cobra.Command{
		Use:               "delete <name>",
		ValidArgsFunction: completePlugins,
		Short:             "delete a plugin and its files; asks first",
		Args: cobra.MatchAll(cobra.ExactArgs(1), func(_ *cobra.Command, args []string) error {
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
		Args:              cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			ctx, stop := signalContext()
			defer stop()
			return status(testPlugins(ctx, args, c.OutOrStdout(), c.ErrOrStderr()))
		},
	})
	return cmd
}

func sessionsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "list this project's sessions, newest first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dirs, err := config.ResolveDirs()
			if err != nil {
				return err
			}
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
			for _, s := range list {
				title := cmp.Or(sessionTitle(s), "(untitled)")
				fmt.Fprintf(c.OutOrStdout(), "%s  %s  %s\n", s.ID, time.UnixMilli(s.UpdatedAt).Format(hostapi.StampLayout), title)
			}
			return err
		},
	}
	full := false
	export := &cobra.Command{
		Use:               "export <id>",
		ValidArgsFunction: completeSessions,
		Short:             "print a session as markdown",
		Args:              cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dirs, err := config.ResolveDirs()
			if err != nil {
				return err
			}
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
func projectsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "projects",
		Short: "list the projects zemi keeps state for, last used first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dirs, err := config.ResolveDirs()
			if err != nil {
				return err
			}
			ps := hostapi.ListProjects(dirs, true)
			if len(ps) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no projects yet")
			}
			for _, p := range ps {
				used := "-"
				if !p.Used.IsZero() {
					used = p.Used.Format(hostapi.StampLayout)
				}
				fmt.Fprintf(c.OutOrStdout(), "%-16s  %-9s  %4d  %s\n", used, p.TrustWord(), p.Sessions, p.Root)
			}
			return nil
		},
	}
}

func modelsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "list the models -m takes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			h, done, err := bootHeadless(options{}, true, os.Stderr, io.Discard)
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
			dirs, err := config.ResolveDirs()
			if err != nil {
				return err
			}
			cat, changed, err := agent.UpdateCatalog(ctx, agent.CatalogURL, dirs.DataHome, os.Getenv)
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

// sessionTitle is a session's title on one line.
func sessionTitle(s *store.Session) string {
	return strings.Join(strings.Fields(sanitize.String(s.Title)), " ")
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
		fmt.Fprintf(stderr, "zemi: %s is not trusted: project config, instructions and language servers are off and only read-only commands run without asking; pass --trust or run /trust in the TUI\n", perm.TildePath(root))
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
		if err := h.SetModel("chat "+o.model, true); err != nil {
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
func printHealth(h *hostapi.Host, stdout io.Writer) int {
	ctx, stop := signalContext()
	defer stop()
	if h.WaitMCP(ctx); ctx.Err() != nil {
		return 130
	}
	code := 0
	for _, r := range h.HealthRows() {
		if r.Level == hostapi.HealthError {
			code = 1
		}
		fmt.Fprintf(stdout, "%-5s %-12s %s\n", r.Level, r.Name, r.Detail)
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
		return h.AgentSend(prompt)
	}
	return h.SendPrompt(prompt)
}
