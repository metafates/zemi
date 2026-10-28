// Package cli is zemi's front end: the command line and the TUI launch.
package cli

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/fantasy"
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
)

// envHelp ends the root's help only.
const envHelp = `{{if not .HasParent}}
Environment:
  ZEMI_CONFIG_HOME  config: init.lua, skills (~/.config/zemi)
  ZEMI_DATA_HOME    data: plugins, model catalog (~/.local/share/zemi)
  ZEMI_STATE_HOME   state: chat databases, logs (~/.local/state/zemi)
  ZEMI_CACHE_HOME   cache: language server builds (~/.cache/zemi)
                    each defaults to $XDG_<NAME>_HOME/zemi when that is set;
                    a relative XDG_<NAME>_HOME is ignored, a relative
                    ZEMI_<NAME>_HOME stops zemi
  ANTHROPIC_API_KEY, OPENAI_API_KEY, ...
                    a builtin provider is on while its key is exported

Exit status: 0 success, 1 error, 2 usage error, 130 interrupted.
{{end}}`

// version is set at release time with -ldflags "-X main.version=v1.2.3";
// otherwise the module version from the build info is used.
var version string

type options struct {
	model  string
	prompt string
	print  bool   // -p: send the prompt, print the final reply, exit
	dir    string // -C
	trust  *bool  // --trust for this run; nil when not given
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

// Main runs the command line for the root main.go's single structured
// os.Exit; the process guards in launch (SIGHUP, the cleanup watchdog) exit
// directly, since a signal must bypass defers and a cleanup hang must not
// hold the exit.
func Main(args []string, stdout, stderr io.Writer) int {
	return execute(args, stdout, stderr, launch)
}

// execute runs the command line and returns the exit status. launch opens
// the TUI.
func execute(args []string, stdout, stderr io.Writer, launch func(options) int) int {
	var o options
	// cobra runs PersistentPreRunE once flags and arguments passed its checks,
	// so an error before it is a usage error and one after it a failure
	parsed := false
	root := newRoot(&o, launch)
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		// -C ran: past this point a failure is not a usage error
		parsed = true
		if f := root.PersistentFlags().Lookup("trust"); f.Changed {
			o.trust = new(f.Value.String() == "true")
		}
		if o.dir != "" {
			if err := enterDir(o.dir); err != nil {
				return err
			}
		}
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
		usageHint(stderr, c.CommandPath(), err.Error())
		return 2
	case c.HasParent():
		headline(stderr, "zemi: "+strings.TrimPrefix(c.CommandPath(), "zemi ")+": ", err.Error())
	default:
		headline(stderr, "zemi: ", err.Error())
	}
	return 1
}

// usageHint reports a usage error, which exits 2, and names the command whose
// --help covers it.
func usageHint(w io.Writer, cmd, msg string) {
	fmt.Fprintf(w, "zemi: %s\nRun '%s --help' for usage.\n", msg, cmd)
}

// headline reports a failure the way the run does: its headline, then the
// detail indented two spaces, under the prefix given.
func headline(w io.Writer, prefix, msg string) {
	head, detail, _ := strings.Cut(msg, "\n")
	fmt.Fprintf(w, "%s%s\n", prefix, hostapi.Headline(head))
	if detail != "" {
		fmt.Fprintln(w, "  "+detail)
	}
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
    #%@@%#+
  #%##%#@@@%#+ @%#
 #*=::   =*###* #%#%#
+*=:    %   ***- -#%@@
++=:  ##*:-  ++-   +###
 ++ @@*+-:   =-:    #%#
   %@#*:     :::-  =@@*
  #%#-  **%%#     %@%*=
  #*=    +#%#@%@%%#%#:
  **==-     *##%**+
   ++=++++**
      --==-
@zemi a terminal coding agent

With no command zemi opens the TUI; a prompt is sent as the first message,
or run when it names a command (zemi "/review HEAD~1"). With -p the prompt
runs headless: the final reply prints and zemi exits.`,
		Example: `  zemi "fix the failing test"
  zemi "/review HEAD~1"
  zemi -- plugins are not loading      after --, it is all prompt
  zemi -p "say hi"                     one-shot: the reply prints, zemi exits`,
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
	root.PersistentFlags().Bool("trust", false, "trust the project for this run; --trust=false does not")
	_ = root.MarkPersistentFlagDirname("cd")
	// -m is persistent, so a subcommand parses it (zemi -m <id> health)
	root.PersistentFlags().StringVarP(&o.model, "model", "m", "", "model for the conversation, by provider/model `id`")
	_ = root.RegisterFlagCompletionFunc("model", completeBooted((*hostapi.Host).ModelIDs))
	root.Flags().BoolVarP(&o.print, "print", "p", false, "send the prompt, print the final reply and exit; the prompt is the arguments or piped stdin")

	var healthJSON *bool
	health := &cobra.Command{
		Use:   "health",
		Short: "run the /health checks; 1 if any failed",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			h, done, err := bootHeadless(*o, false, os.Stderr) // the trust row says it
			if err != nil {
				return err
			}
			defer done()
			return status(printHealth(h, c.OutOrStdout(), *healthJSON))
		},
	}
	healthJSON = jsonFlag(health)

	root.AddCommand(projectsCommand(o), pluginCommand(o), modelsCommand(o), dbCommand(o), health)
	return root
}

// completeBooted offers what only config knows (models, modes),
// read by an inert boot. The dirs are resolved here: cobra runs no hook
// before a completion.
func completeBooted(names func(*hostapi.Host) []string) cobra.CompletionFunc {
	return func(c *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		dirs, err := completionDirs(c)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		h, done, err := bootHeadless(options{dirs: dirs}, true, os.Stderr)
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

// completePlugins offers the plugin names: the first argument of the verbs
// that take one, and test's first may be a directory too.
func completePlugins(c *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	directive := cobra.ShellCompDirectiveNoFileComp
	if c.Name() == "test" {
		directive = cobra.ShellCompDirectiveDefault
	}
	dirs, err := completionDirs(c)
	if err != nil || len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ps, _, _ := plugin.Scan(dirs)
	var out []cobra.Completion
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out, directive
}

// marketplaceList is a completer's prologue: the registered marketplaces, ok
// false when the completion offers nothing.
func marketplaceList(c *cobra.Command, args []string) ([]plugin.MarketplaceInfo, bool) {
	dirs, err := completionDirs(c)
	if err != nil || len(args) > 0 {
		return nil, false
	}
	ms, _ := plugin.Marketplaces(dirs)
	return ms, true
}

// completeMarketplaces offers the registered marketplace names, for
// marketplace remove and update.
func completeMarketplaces(c *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	ms, ok := marketplaceList(c, args)
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []cobra.Completion
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeEntries offers a registered marketplace's entries as the
// name@marketplace install types, for plugin install and add.
func completeEntries(c *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	ms, ok := marketplaceList(c, args)
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []cobra.Completion
	for _, m := range ms {
		for _, e := range m.Entries {
			out = append(out, e.Name+"@"+m.Name)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// finish checks a turn's arguments and joins them into the prompt. A lone
// word that is a near miss of a command is refused: sent, it would open the
// TUI and pay for a model call on a typo.
func (o *options) finish(c *cobra.Command, args []string) error {
	if !c.HasParent() && len(args) == 1 && c.ArgsLenAtDash() < 0 && !o.print {
		if s := c.SuggestionsFor(args[0]); len(s) > 0 {
			return fmt.Errorf("%q is not a command; did you mean %s? (a one-word prompt: zemi -- %s)", args[0], s[0], args[0])
		}
		if args[0] == "version" {
			return errors.New("\"version\" is not a command; did you mean --version?")
		}
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

// takesErr names a command's positional grammar: "db takes <sql> [args...]".
func takesErr(c *cobra.Command) error {
	return fmt.Errorf("%s takes %s", strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" "), strings.TrimPrefix(c.Use, c.Name()+" "))
}

// nargs checks that a command has one to maxN positional arguments, naming
// what its Use line takes.
func nargs(maxN int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) < 1 || len(args) > maxN {
			return takesErr(c)
		}
		return nil
	}
}

// minArgs checks that a command has at least n positional arguments.
func minArgs(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) < n {
			return takesErr(c)
		}
		return nil
	}
}

// maxArgs checks that a command has at most n positional arguments.
func maxArgs(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) > n {
			return takesErr(c)
		}
		return nil
	}
}

// pluginCommand is zemi plugin: no Lua boot, TUI, trust or store, so it
// works beside a running TUI, whose config watcher reloads on the change.
func pluginCommand(o *options) *cobra.Command {
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
		dirs := o.dirs
		var err error
		switch {
		case c.Name() == "plugin" || (c.Name() == "list" && c.Parent() != nil && c.Parent().Name() == "plugin"):
			asJSON, _ := c.Flags().GetBool("json")
			var ps []plugin.Plugin
			var skipped []plugin.Skipped
			if ps, skipped, err = plugin.List(ctx, dirs); err == nil && len(ps) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no plugins yet; zemi plugin marketplace add <source> adds one")
			}
			printPlugins(c.OutOrStdout(), ps, skipped, asJSON)
		case c.Name() == "list":
			asJSON, _ := c.Flags().GetBool("json")
			var ms []plugin.MarketplaceInfo
			if ms, err = plugin.Marketplaces(dirs); err == nil && len(ms) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no marketplaces yet; zemi plugin marketplace add <source> adds one")
			}
			printMarketplaces(c.OutOrStdout(), ms, asJSON)
		case c.Parent().Name() == "marketplace":
			err = plugin.Run(ctx, dirs, append([]string{"marketplace", c.Name()}, args...), c.OutOrStdout())
		default:
			err = plugin.Run(ctx, dirs, append([]string{c.Name()}, args...), c.OutOrStdout())
		}
		if ctx.Err() != nil {
			return exitError(130)
		}
		return err
	}
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "list, install, enable, disable, delete and test plugins",
		Args:  cobra.NoArgs,
		RunE:  run,
	}
	jsonFlag(cmd)
	for _, v := range []struct {
		use, short string
		args       cobra.PositionalArgs
		names      cobra.CompletionFunc
		aliases    []string
	}{
		{"list", "list plugins: name, version, on or off, source", cobra.NoArgs, nil, nil},
		{"install <name>[@<marketplace>]", "install an entry of a registered marketplace; a bare name searches them all", nargs(1), completeEntries, []string{"add"}},
		{"enable <name>", "load the plugin from the next start", nargs(1), completePlugins, nil},
		{"disable <name>", "stop loading the plugin", nargs(1), completePlugins, nil},
	} {
		sub := &cobra.Command{Use: v.use, Aliases: v.aliases, Short: v.short, Args: v.args, ValidArgsFunction: v.names, RunE: run}
		if v.use == "list" { // both spellings list, so both print JSON
			jsonFlag(sub)
		}
		cmd.AddCommand(sub)
	}
	// the only doors installs come through: a marketplace registered here,
	// then its entries installed by name
	mkt := &cobra.Command{
		Use:   "marketplace",
		Short: "add, list, update and remove the marketplaces plugins install from",
		Args:  cobra.NoArgs,
	}
	for _, v := range []struct {
		use, short string
		args       cobra.PositionalArgs
		names      cobra.CompletionFunc
	}{
		{"add <source>", "register a marketplace repo: owner/repo, a git URL, or a local path", nargs(1), nil},
		{"list", "show the marketplaces; --json adds the entries they list", cobra.NoArgs, nil},
		{"remove <name>", "drop a marketplace and its clone", nargs(1), completeMarketplaces},
		{"update [name]", "pull one or all marketplace clones", maxArgs(1), completeMarketplaces},
	} {
		sub := &cobra.Command{Use: v.use, Short: v.short, Args: v.args, ValidArgsFunction: v.names, RunE: run}
		if v.use == "list" {
			jsonFlag(sub)
		}
		mkt.AddCommand(sub)
	}
	cmd.AddCommand(mkt)
	// a local plugin is the user's own code with no copy anywhere, and off a
	// terminal nobody answers, so an agent's command cannot delete one
	del := &cobra.Command{
		Use:               "delete <name>",
		ValidArgsFunction: completePlugins,
		Short:             "delete a plugin and its files; asks first",
		Args: cobra.MatchAll(nargs(1), func(_ *cobra.Command, args []string) error {
			if !yes && !term.IsTerminal(os.Stdin.Fd()) {
				return fmt.Errorf("plugin delete: stdin is not a terminal to ask on; pass --yes to delete %s", args[0])
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

// sessionsCommand and printSessions are gone: the chat is one log per project,
// its transcript opens in the TUI, and a run's rows in /agents.

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
			var unreadable []string
			for _, p := range ps {
				if p.StatsErr != nil {
					unreadable = append(unreadable, fmt.Sprintf("%s: %v", perm.TildePath(p.Root), p.StatsErr))
				}
			}
			if len(unreadable) > 0 {
				fmt.Fprintf(c.ErrOrStderr(), "zemi: skipped %d unreadable %s: %s\n", len(unreadable), hostapi.Plural(len(unreadable), "store", "stores"), strings.Join(unreadable, "; "))
			}
			// the root at column 36, for cut -c36-
			t := table{p: cp, cols: []tableCol{
				{head: "used", role: "hint", width: 16},
				{head: "trust", role: "status", width: 9},
				{head: "sess", width: 4, right: true},
				{head: "root"},
			}}
			for _, p := range ps {
				if *asJSON {
					statsErr := ""
					if p.StatsErr != nil {
						statsErr = p.StatsErr.Error()
					}
					writeJSON(out, struct {
						Root       string    `json:"root"`
						Trust      string    `json:"trust"`
						Used       time.Time `json:"used,omitzero"`
						Sessions   int       `json:"sessions"`
						StatsError string    `json:"stats_error,omitempty"`
					}{Root: p.Root, Trust: p.TrustWord(), Used: p.Used, Sessions: p.Sessions, StatsError: statsErr})
					continue
				}
				used := "-"
				if !p.Used.IsZero() {
					used = p.Used.Format(hostapi.StampLayout)
				}
				t.row(used, p.TrustWord(), strconv.Itoa(p.Sessions), p.Root)
			}
			t.print(out)
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
			if err := checkModelFlag(o.model); err != nil {
				return err
			}
			h, done, err := bootHeadless(*o, true, os.Stderr)
			if err != nil {
				return err
			}
			defer done()
			ids := h.ModelIDs()
			if len(ids) == 0 {
				fmt.Fprintln(c.ErrOrStderr(), "zemi: no model yet;", h.NoModelHint())
				return exitError(1)
			}
			fmt.Fprintln(c.OutOrStdout(), strings.Join(ids, "\n"))
			// a broken init.lua boots soft, so the list is incomplete; health
			// exits 1 on the same state
			if h.InitError() != "" {
				return status(1)
			}
			return nil
		},
	}
	// the catalog picks where each key is sent, so it lives in <data>; nothing boots
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

// jsonFlag adds --json to a listing command: JSON Lines on stdout, one
// object per item.
func jsonFlag(c *cobra.Command) *bool {
	return c.Flags().Bool("json", false, "print one JSON object per line")
}

// writeJSON writes v as one JSON Lines line, what every --json prints. A
// byte that is not UTF-8 becomes U+FFFD, so no line is dropped; the fixed
// struct types leave Marshal nothing else to refuse. Every string the line
// carries, a map key or a nested row's included, passes sanitize here, and
// the caller's value is left alone, so no field needs a per-call list.
func writeJSON(w io.Writer, v any) {
	b, _ := json.Marshal(sanitizeWire(reflect.ValueOf(v)).Interface(), jsontext.AllowInvalidUTF8(true), json.Deterministic(true))
	_, _ = w.Write(append(b, '\n'))
}

// sanitizeWire returns a copy of v whose strings pass sanitize.String,
// descending into its structs, slices, arrays, maps and pointers; unexported
// fields ride along whole, reflect cannot write them one by one.
func sanitizeWire(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		nv := reflect.New(v.Type().Elem())
		nv.Elem().Set(sanitizeWire(v.Elem()))
		return nv
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		nv := reflect.New(v.Type()).Elem()
		nv.Set(sanitizeWire(v.Elem()))
		return nv
	case reflect.Struct:
		nv := reflect.New(v.Type()).Elem()
		nv.Set(v)
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				nv.Field(i).Set(sanitizeWire(v.Field(i)))
			}
		}
		return nv
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		nv := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			nv.Index(i).Set(sanitizeWire(v.Index(i)))
		}
		return nv
	case reflect.Array:
		nv := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			nv.Index(i).Set(sanitizeWire(v.Index(i)))
		}
		return nv
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		nv := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			nv.SetMapIndex(sanitizeWire(iter.Key()), sanitizeWire(iter.Value()))
		}
		return nv
	case reflect.String:
		nv := reflect.New(v.Type()).Elem()
		nv.SetString(sanitize.String(v.String()))
		return nv
	default: // a number, a bool or a flag: nothing on the line to sanitize
		return v
	}
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

// tableCol describes one column of a table.
type tableCol struct {
	head  string // the header row's cell; a table naming none prints no header
	role  string // a role name, or "status" to paint a body cell by its word
	width int    // a column no narrower than this, 0 sizing it to its cells
	right bool   // the cell sits against the column's right edge (a count)
}

// table is one listing's aligned rows: a column pads to its widest cell or
// its own floor, a row's last column stays free so cut and awk read it, and
// the header row is a terminal's alone.
type table struct {
	p      colorprofile.Profile
	cols   []tableCol
	rows   [][]string
	noHead bool // a title above already says what the columns are
	head   bool // the header prints off a terminal too: a query's columns are its answer
}

func (t *table) row(cells ...string) { t.rows = append(t.rows, cells) }

// print writes the table to w.
func (t table) print(w io.Writer) {
	if len(t.rows) == 0 {
		return
	}
	headed := (t.p != colorprofile.NoTTY || t.head) && !t.noHead &&
		slices.ContainsFunc(t.cols, func(c tableCol) bool { return c.head != "" })
	width := make([]int, len(t.cols))
	for i, c := range t.cols {
		width[i] = c.width
		if headed {
			width[i] = max(width[i], ansi.StringWidth(c.head))
		}
	}
	for _, r := range t.rows {
		for i, cell := range r {
			width[i] = max(width[i], ansi.StringWidth(cell))
		}
	}
	if headed {
		cells, roles := make([]string, len(t.cols)), make([]string, len(t.cols))
		for i, c := range t.cols {
			cells[i], roles[i] = c.head, "title"
		}
		t.write(w, cells, roles, width)
	}
	for _, r := range t.rows {
		roles := make([]string, len(t.cols))
		for i, c := range t.cols {
			roles[i] = c.role
			if c.role == "status" {
				roles[i] = statusRole[r[i]]
			}
		}
		t.write(w, r, roles, width)
	}
}

// write renders one row: every column but the last painted in its role and
// padded, the last painted free.
func (t table) write(w io.Writer, cells, roles []string, width []int) {
	var b strings.Builder
	for i, cell := range cells {
		role := roles[i]
		if i == len(cells)-1 {
			b.WriteString(paint(t.p, role, cell))
			break
		}
		gap := width[i] - ansi.StringWidth(cell)
		if t.cols[i].right {
			b.WriteString(strings.Repeat(" ", max(gap, 0)))
			b.WriteString(paint(t.p, role, cell))
		} else {
			b.WriteString(paint(t.p, role, cell))
			b.WriteString(strings.Repeat(" ", max(gap, 0)))
		}
		b.WriteString("  ")
	}
	fmt.Fprintln(w, b.String())
}

// printMarketplaces writes one row per marketplace, the entries it lists only
// in the JSON: the count is what picking an install needs, the entries are
// completion's and the overlay's.
func printMarketplaces(w io.Writer, ms []plugin.MarketplaceInfo, asJSON bool) {
	t := table{p: colorprofile.Detect(w, os.Environ()), cols: []tableCol{
		{head: "name"},
		{head: "source", role: "hint"},
		{head: "plugins", right: true},
		{head: "added", role: "hint"},
	}}
	for _, m := range ms {
		if asJSON {
			entries := make([]marketplaceEntry, len(m.Entries))
			for i, e := range m.Entries {
				entries[i] = marketplaceEntry{
					Name: e.Name, Version: e.Version, Description: e.Description,
					Category: e.Category, Author: e.Author, Homepage: e.Homepage, Uses: e.Uses,
				}
			}
			writeJSON(w, struct {
				Name    string             `json:"name"`
				Source  string             `json:"source"`
				Added   time.Time          `json:"added,omitzero"`
				Entries []marketplaceEntry `json:"entries,omitempty"`
				Error   string             `json:"error,omitempty"`
			}{Name: m.Name, Source: m.Source, Added: m.Added, Entries: entries, Error: m.Err})
			continue
		}
		count, note := strconv.Itoa(len(m.Entries)), ""
		if m.Err != "" {
			count, note = "-", m.Err
		} else if !m.Added.IsZero() {
			note = m.Added.Format(hostapi.StampLayout)
		}
		t.row(m.Name, m.Source, count, note)
	}
	t.print(w)
}

// marketplaceEntry is one entry of a marketplace as the JSON lists it.
type marketplaceEntry struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Author      string   `json:"author,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Uses        []string `json:"uses,omitempty"`
}

// printPlugins writes one aligned line per plugin, the free-text columns last
// so cut and awk read the name, and a header only on a terminal. An entry the
// name rule refused follows the table as its own line.
func printPlugins(w io.Writer, ps []plugin.Plugin, skipped []plugin.Skipped, asJSON bool) {
	t := table{p: colorprofile.Detect(w, os.Environ()), cols: []tableCol{
		{head: "name"},
		{head: "version", role: "hint"},
		{head: "on", role: "status"},
		{head: "source", role: "hint"},
		{head: "marketplace", role: "hint"},
		{head: "uses", role: "hint"},
	}}
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
				Name        string   `json:"name"`
				Version     string   `json:"version"`
				Local       bool     `json:"local"`
				Enabled     bool     `json:"enabled"`
				Source      string   `json:"source"`
				Marketplace string   `json:"marketplace"`
				Uses        []string `json:"uses"`
			}{Name: pl.Name, Version: version, Local: pl.Local, Enabled: pl.Enabled, Source: source, Marketplace: pl.Marketplace, Uses: pl.Uses})
			continue
		}
		t.row(pl.Name, version, on, source, pl.Marketplace, strings.Join(pl.Uses, ","))
	}
	t.print(w)
	// plain lines only: --json stays one Plugin object per line
	if !asJSON {
		for _, s := range skipped {
			fmt.Fprintln(w, s)
		}
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

// readStdin refuses a piped stdin: the TUI needs the terminal on it.
// </dev/null is fine, the TUI opens /dev/tty.
func (o *options) readStdin(stdin *os.File) error {
	fi, err := stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	msg := "stdin is piped; the TUI does not read it"
	if o.prompt == "" {
		msg += " - give the prompt as an argument"
	}
	return errors.New(msg)
}

// headlessTrust answers the trust prompt when nobody can: not now.
func headlessTrust() func(root, cwd, state string) string {
	return func(root, _, _ string) string {
		return "later"
	}
}

// applyOptions applies the flags after boot, so they win over init.lua.
func applyOptions(h *hostapi.Host, o options) error {
	if o.model != "" {
		if err := h.SetTargetModel("default", o.model); err != nil {
			return err
		}
	}
	return nil
}

// checkModelFlag validates -m's syntax for the commands that never boot a
// host (db, models), where applyOptions has nothing to check a catalog
// against: the id carries a slash with both halves named, as ModelID.Split
// reads it everywhere. A well-formed unknown provider stays their no-op.
func checkModelFlag(id string) error {
	if id == "" {
		return nil
	}
	prov, model, ok := agent.ModelID(id).Split()
	if !ok || prov == "" || model == "" {
		return fmt.Errorf("-m %q is not a provider/model id", id)
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
	rows := h.HealthRows()
	t := table{p: p, cols: []tableCol{
		{role: "status", width: 5},
		{role: ""},
		{role: ""},
	}}
	for _, r := range rows {
		if r.Level == hostapi.HealthError {
			code = 1
		}
		if asJSON {
			writeJSON(stdout, struct {
				Level  hostapi.HealthLevel `json:"level"`
				Name   string              `json:"name"`
				Detail string              `json:"detail"`
			}{Level: r.Level, Name: r.Name, Detail: r.Detail})
			continue
		}
		t.row(string(r.Level), r.Name, r.Detail)
	}
	t.print(stdout)
	return code
}

// signalContext ends on an interrupt, a kill or the terminal closing, which
// a command reports as exit status 130.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), stopSignals()...)
}

// stopSignals are the signals that stop a command: an interrupt, a kill and,
// unless a hangup was ignored at start (nohup), the terminal closing, since
// Notify would install a handler for an ignored one.
func stopSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// sendPrompt sends a command-line prompt once no MCP server is starting,
// since its one send builds the tool set, unless ctx ends the wait. The
// prompt held at boot goes as the turn it claimed.
func sendPrompt(ctx context.Context, h *hostapi.Host, prompt string) error {
	if h.WaitMCP(ctx); ctx.Err() != nil {
		return ctx.Err()
	}
	return h.SendHeld(prompt)
}

// runPrint is -p: the one-shot the other harnesses offer (codex exec, pi
// --print, opencode run). It boots headless, sends the prompt - the
// arguments, or piped stdin - and prints the turn's final reply. No UI
// hooks run, so a permission ask fails closed and the turn carries on
// without the denied call.
func runPrint(o options, stdout, stderr io.Writer) int {
	prompt := o.prompt
	if prompt == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			usageHint(stderr, "zemi", "give a prompt: as the arguments or piped on stdin")
			return 2
		}
		prompt = string(b)
	}
	h, done, err := bootHeadless(o, false, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "zemi:", err)
		return 1
	}
	defer done()
	// a hung turn or model is the user's ctrl-c: the signal ends the wait
	// and the deferred cleanup reaps what the turn started
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := sendPrompt(ctx, h, prompt); err != nil {
		fmt.Fprintln(stderr, "zemi:", err)
		return 1
	}
	for h.AgentActive() {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "zemi: interrupted")
			return 1
		case <-time.After(100 * time.Millisecond):
		}
	}
	tr := h.Session().Transcript
	failed := false
	reply := ""
	for i := len(tr) - 1; i >= 0; i-- {
		m := tr[i]
		switch {
		case m.Role == fantasy.MessageRoleUser: // the prompt itself: the turn's rows end here
			i = -1
		case m.Kind == hostapi.RowError:
			failed = true
		case m.Role == fantasy.MessageRoleAssistant && m.Kind == hostapi.RowMessage && reply == "":
			reply = m.Text
		}
	}
	if reply != "" {
		fmt.Fprintln(stdout, reply)
	}
	if failed {
		headline(stderr, "zemi:", "the turn failed; its rows are in /errors")
		return 1
	}
	return 0
}
