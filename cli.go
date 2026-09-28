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
	"github.com/metafates/zemi/internal/plugins"
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
	run, cont, json bool
	resume, model   string
	mode, profile   string
	prompt          string
	dir             string // -C
	trust           *bool  // --trust for this run; nil when not given
}

// exitStatus is the error of a command that printed its own diagnostic.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func status(code int) error {
	if code == 0 {
		return nil
	}
	return exitStatus(code)
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
	if s, ok := errors.AsType[exitStatus](err); ok {
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
		Long: `Answer one prompt on stdout and exit, with no TUI. Piped stdin is attached
behind the prompt; - reads the prompt itself from stdin. Nobody can approve a
call, so one the mode leaves to the user is denied and stderr counts them.`,
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
	run.Flags().BoolVar(&o.json, "json", false, "print JSON for a script instead of the reply")

	health := &cobra.Command{
		Use:   "health",
		Short: "run the /health checks; 1 if any failed",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			h, done, err := bootHeadless(options{}, io.Discard) // the trust row says it
			if err != nil {
				return err
			}
			defer done()
			return status(printHealth(h, c.OutOrStdout()))
		},
	}

	root.AddCommand(run, sessionsCommand(), pluginsCommand(), health)
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
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		dirs, err := config.ResolveDirs()
		switch {
		case err != nil:
		case c.Name() == "plugins":
			var ps []plugins.Plugin
			if ps, err = plugins.List(ctx, dirs); err == nil && len(ps) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no plugins yet; zemi plugins install <git url> adds one")
			}
			err = errors.Join(err, plugins.Table(c.OutOrStdout(), ps))
		case c.Name() == "delete" && !yes && !confirm(c, "Delete the plugin "+args[0]+" and its files?"):
			return exitStatus(1)
		default:
			err = plugins.Run(ctx, dirs, append([]string{c.Name()}, args...), c.OutOrStdout())
		}
		if ctx.Err() != nil {
			return exitStatus(130)
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
	}{
		{"install <git url> [name]", "clone a plugin at its highest vX.Y.Z tag", cobra.RangeArgs(1, 2)},
		{"update [name...]", "update within the major version; every plugin without a name", cobra.ArbitraryArgs},
		{"check", "fetch, then list the updates", cobra.NoArgs},
		{"enable <name>", "load the plugin from the next start", cobra.ExactArgs(1)},
		{"disable <name>", "stop loading the plugin", cobra.ExactArgs(1)},
	} {
		cmd.AddCommand(&cobra.Command{Use: v.use, Short: v.short, Args: v.args, RunE: run})
	}
	// a local plugin is the user's own code with no copy anywhere, and off a
	// terminal nobody answers, so an agent's command cannot delete one
	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "delete a plugin and its files; asks first",
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
		Use:   "test <name|dir> [pattern]",
		Short: "run its tests, each case in a zemi of its own",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
				fmt.Fprintf(c.ErrOrStderr(), "zemi: no sessions in %s yet\n", hostapi.TildePath(dirs.TrustRoot()))
				return nil
			}
			for _, s := range list {
				title := cmp.Or(strings.Join(strings.Fields(sanitize.String(s.Title)), " "), "(untitled)")
				fmt.Fprintf(c.OutOrStdout(), "%s  %s  %s\n", s.ID, time.UnixMilli(s.UpdatedAt).Format(hostapi.StampLayout), title)
			}
			return err
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "export <id>",
		Short: "print a session as markdown",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dirs, err := config.ResolveDirs()
			if err != nil {
				return err
			}
			db, err := projectStore(dirs)
			md := ""
			if err == nil {
				defer db.Close()
				md, err = hostapi.SessionMarkdown(db, args[0], false)
			}
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no session %q; zemi sessions lists them", args[0])
			}
			fmt.Fprint(c.OutOrStdout(), md)
			return err
		},
	})
	return cmd
}

// projectStore opens this project's store. A missing one is fs.ErrNotExist
// and stays missing, so a look or a tab press creates no database.
func projectStore(dirs config.Dirs) (*store.DB, error) {
	path := store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot())
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return store.Open(path)
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
	case (err != nil || fi.Mode()&os.ModeCharDevice != 0) && dash:
		return errors.New("- reads the prompt from stdin, which is a terminal")
	case err != nil || fi.Mode()&os.ModeCharDevice != 0:
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
		o.prompt = in
	case in != "":
		o.prompt += agent.Attached("stdin", in)
	}
	return nil
}

// headlessTrust answers the trust prompt when nobody can: not now, with one
// line saying what that turns off.
func headlessTrust(stderr io.Writer) func(root, cwd, state string) string {
	return func(root, _, _ string) string {
		fmt.Fprintf(stderr, "zemi: %s is not trusted: project config, instructions and language servers are off and only read-only commands run without asking; pass --trust or run /trust in the TUI\n", hostapi.TildePath(root))
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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

// sendPrompt starts a turn from a command-line prompt. A prompt naming a
// registered command runs it, as the composer would, so /review, /workflow
// and the markdown commands reach a script or a git hook; anything else is
// a message for the model. Its one send builds the tool set, so MCP servers
// still starting get to finish first.
func sendPrompt(h *hostapi.Host, prompt string) error {
	h.WaitMCP(context.Background())
	typed, attached := agent.CutAttached(prompt)
	if rest, ok := strings.CutPrefix(typed, "/"); ok {
		name, args := hostapi.SplitCommand(rest)
		if h.Commands.Lookup(name) != nil {
			return h.RunCommand(context.Background(), name, args+attached)
		}
	}
	return h.AgentSend(prompt)
}
