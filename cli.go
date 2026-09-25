package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/plugins"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/internal/ui"
)

const usage = `zemi - a terminal coding agent that learns how you work

Usage:
  zemi [flags] [prompt]

  zemi                          open the TUI
  zemi "fix the failing test"   open the TUI and send a first message
  zemi -p "explain main.go"     answer on stdout and exit
  zemi -p "/review HEAD~1"      a prompt naming a command runs it
  git diff | zemi -p "review"   piped stdin is attached to the prompt
  zemi -- why does ls -la fail  flags are read anywhere; after -- it is all prompt

Plugins (a prompt that starts with "plugins" is written zemi -- plugins ...):
  zemi plugins [list]                   name, version, on or off, source
  zemi plugins install <git url> [name] clone it at its highest vX.Y.Z tag
  zemi plugins update [name...]         in the same major version; all without a name
  zemi plugins check                    fetch, then list the updates
  zemi plugins enable|disable|delete <name>

Flags:
  -p, --print         answer the prompt on stdout and exit (no TUI)
  -c, --continue      continue the most recent session
  -r, --resume id     resume the session with this id
  -m, --model id      model for the conversation (provider/model)
      --mode mode     permission mode: auto, yolo or one init.lua adds
      --profile name  agent profile: default, planner or one init.lua defines
      --output fmt    with -p: text (the reply) or json (reply, tools, compactions, usage)
      --trust         trust this project for this run (--trust=false: do not)
      --health        run the /health checks on stdout and exit (1 if any failed)
  -v, --version       print the version and exit
  -h, --help          show this help

With -p nobody can approve a call, so one that needs the user's approval is
denied. A project not trusted in the TUI runs untrusted with -p, where little
runs without approval, unless --trust is given.

Environment:
  ZEMI_CONFIG_HOME    config dir (default ~/.config/zemi)
  ZEMI_DATA_HOME      data dir (default ~/.local/share/zemi)
  ZEMI_STATE_HOME     state dir: sessions, memory, logs (default ~/.local/state/zemi)
  ZEMI_CACHE_HOME     cache dir: language server builds (default ~/.cache/zemi)
                      each defaults to $XDG_<NAME>_HOME/zemi when that is set;
                      a relative path in either is ignored
  ANTHROPIC_API_KEY, OPENAI_API_KEY, ...
                      a builtin provider is on while its key is exported
                      (/model lists them)

Exit status: 0 success, 1 error, 2 usage error, 130 interrupted.
`

// version is set at release time with -ldflags "-X main.version=v1.2.3";
// otherwise the module version from the build info is used.
var version string

type options struct {
	print, cont   bool
	health        bool
	resume, model string
	mode, prompt  string
	output        string
	profile       string
	trust         *bool // --trust for this run; nil when not given
}

// parseArgs parses the command line. code is -1 when zemi should run, or
// the exit status when it is done (help, version, usage error).
func parseArgs(args []string, stdout, stderr io.Writer) (o options, code int) {
	if len(args) > 0 && args[0] == "plugins" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		dirs, err := config.ResolveDirs()
		if err == nil {
			err = plugins.Run(ctx, dirs, args[1:], stdout)
		}
		if ctx.Err() != nil {
			return o, 130
		} else if err != nil {
			fmt.Fprintln(stderr, "zemi: plugins:", err)
			return o, 1
		}
		return o, 0
	}
	fs := flag.NewFlagSet("zemi", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help, ver, trust bool
	for name, p := range map[string]*bool{"p": &o.print, "print": &o.print, "c": &o.cont, "continue": &o.cont, "health": &o.health, "h": &help, "help": &help, "v": &ver, "version": &ver, "trust": &trust} {
		fs.BoolVar(p, name, false, "")
	}
	for name, p := range map[string]*string{"r": &o.resume, "resume": &o.resume, "m": &o.model, "model": &o.model, "mode": &o.mode, "profile": &o.profile, "output": &o.output} {
		fs.StringVar(p, name, "", "")
	}
	bad := func(format string, a ...any) (options, int) {
		fmt.Fprintf(stderr, "zemi: "+format+"\nRun 'zemi --help' for usage.\n", a...)
		return o, 2
	}
	// flag stops at the first word; keep parsing so `zemi "hi" -m x` still
	// sets the model. "--" ends flags, everything after it is prompt.
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return bad("%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			words = append(words, rest...)
			break
		}
		words, args = append(words, rest[0]), rest[1:]
	}
	switch {
	case help:
		fmt.Fprint(stdout, usage)
		return o, 0
	case ver:
		fmt.Fprintln(stdout, "zemi", versionString())
		return o, 0
	case o.cont && o.resume != "":
		return bad("--continue and --resume cannot be combined")
	case o.output != "" && o.output != "text" && o.output != "json":
		return bad("unknown --output %q (want text or json)", o.output)
	case o.output == "json" && !o.print:
		return bad("--output json needs -p")
	}
	// Visit sees only flags that were set, so --trust=false forces untrusted
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "trust" {
			o.trust = &trust
		}
	})
	o.prompt = strings.TrimSpace(strings.Join(words, " "))
	return o, -1
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
// splash and /health all print this. A pseudo-version (untagged build)
// says nothing to a person, so it reads "dev".
func normalizeVersion(v string) string {
	if v == "" || v == "(devel)" || strings.HasPrefix(v, "v0.0.0-") {
		return "dev"
	}
	return v
}

// readStdin attaches piped stdin to the prompt in print mode, behind the
// marker that ends what the user typed; --health ignores stdin. The TUI
// needs the terminal on stdin, so a pipe there is a usage error.
func (o *options) readStdin(stdin *os.File) error {
	fi, err := stdin.Stat()
	if o.health || err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	if !o.print {
		return errors.New("stdin is not a terminal; use -p to answer from a pipe")
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
	if o.prompt != "" {
		select {
		case <-first:
		case <-time.After(3 * time.Second):
			fmt.Fprintln(os.Stderr, "zemi: no stdin within 3s, continuing without it")
			return nil
		}
	}
	in := <-got
	if len(in) > 1<<20 {
		return errors.New("stdin is over 1 MiB; write it to a file and name the file in the prompt")
	}
	if in := strings.TrimSpace(string(in)); in != "" {
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
		if err := h.SetModel("chat "+o.model, !o.health); err != nil {
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
		if r.Level == "error" {
			code = 1
		}
		fmt.Fprintf(stdout, "%-5s %-12s %s\n", r.Level, r.Name, r.Detail)
	}
	return code
}

// failPrint reports a print-mode failure. A script reading stdout for
// --output json gets the same report shape it gets for a failed turn.
func failPrint(err error, jsonOut bool, stdout, stderr io.Writer) int {
	if !jsonOut {
		fmt.Fprintln(stderr, "zemi:", hostapi.Headline(err.Error()))
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report{IsError: true, Error: err.Error(), Tools: []reportTool{}})
	return 1
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
		name, args := ui.SplitCommand(rest)
		if h.Commands.Lookup(name) != nil {
			return h.RunCommand(context.Background(), name, args+attached)
		}
	}
	return h.AgentSend(prompt)
}

// printTurn runs one turn without the TUI: the reply goes to stdout (or a
// JSON report with --output json), a failure to stderr with status 1, an
// interrupt ends with 130.
func printTurn(h *hostapi.Host, prompt string, jsonOut bool, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	h.WaitMCP(ctx)
	sid, start := h.AgentSessionID(), len(h.Session().Transcript)
	began, before, denied := time.Now(), h.AgentStream(), h.HeadlessDenials()
	interrupted := ctx.Err() != nil
	if !interrupted {
		if err := sendPrompt(h, prompt); err != nil {
			if errors.Is(err, context.Canceled) {
				return 130
			}
			if !jsonOut && errors.Is(err, hostapi.ErrNoModel) && h.InitError() != "" {
				return 1 // the init failure on stderr is the cause
			}
			h.Log.Error("print", err.Error(), nil) // stderr gets the headline, the log the traceback
			return failPrint(err, jsonOut, stdout, stderr)
		}
	}
	// a command that swapped the session (/new, /resume) ran no turn, and
	// none of the rows or usage it brought are this run's.
	// NOTE: a command whose turn starts a session and ends before this check
	// (a markdown command's quick reply) reads as a swap and prints nothing;
	// a swap the command reported itself would tell them apart.
	if strings.HasPrefix(prompt, "/") && h.AgentSessionID() != sid && !h.AgentActive() {
		start, before = len(h.Session().Transcript), h.AgentStream()
	}
wait:
	for h.AgentActive() {
		select {
		case <-ctx.Done():
			if interrupted {
				break wait
			}
			interrupted = true
			h.AgentCancel()
			// the turn gets a moment to stop, then print mode leaves anyway
			ctx, stop = context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
		case <-time.After(20 * time.Millisecond):
		}
	}
	rows := h.Session().Transcript
	rep := turnReport(rows[min(start, len(rows)):])
	if interrupted {
		proc.KillAll()
		rep.IsError, rep.Error = true, "interrupted"
	}
	d := h.HeadlessDenials()
	rep.Denied, rep.unsandboxed, rep.tuiOnly = d.Asked-denied.Asked, d.Unsandboxed-denied.Unsandboxed, d.TUIOnly-denied.TUIOnly
	if jsonOut {
		after := h.AgentStream()
		rep.SessionID, rep.DurationMS = h.AgentSessionID(), time.Since(began).Milliseconds()
		rep.Usage.Input, rep.Usage.Output = after.SessionIn-before.SessionIn, after.SessionOut-before.SessionOut
		rep.Usage.CostUSD = after.SessionCost - before.SessionCost
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		if rep.IsError {
			head, detail, _ := strings.Cut(rep.Error, "\n")
			fmt.Fprintln(stderr, "zemi:", head)
			if detail != "" {
				fmt.Fprintln(stderr, "  "+detail)
			}
		}
		// The denied calls are in the JSON report but nowhere in the text
		// reply, so a run that did nothing looks like a lazy model.
		if n := rep.Denied; n > 0 {
			fmt.Fprintf(stderr, "zemi: %d tool %s denied; nobody can approve a call with -p, %s\n", n, plural(n, "call", "calls"), deniedHint(rep, h.ProjectTrusted()))
		}
		if n := rep.refused; n > 0 {
			fmt.Fprintf(stderr, "zemi: %d tool %s denied by a rule, the mode, the profile or a permission.request handler\n", n, plural(n, "call", "calls"))
		}
		if rep.Result != "" {
			fmt.Fprintln(stdout, sanitize.String(rep.Result))
		}
	}
	switch {
	case interrupted:
		return 130
	case rep.IsError:
		return 1
	}
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// deniedHint says what would let print mode's denied calls through.
func deniedHint(rep report, trusted bool) string {
	const rule = "an allow rule (zemi.permission.rule.add in init.lua) lets it through"
	switch {
	case rep.unsandboxed > 0:
		return "and running outside the sandbox needs the TUI; " + rule
	case rep.tuiOnly > 0:
		return "and only the TUI can approve a Lua script or a memory edit; " + rule
	case !trusted:
		return "try --trust (untrusted, auto mode asks for almost everything)"
	}
	return "and the mode left it to the user; " + rule
}

// report is the --output json document for one print-mode turn.
type report struct {
	Result     string       `json:"result"`
	IsError    bool         `json:"is_error"`
	Error      string       `json:"error,omitempty"`
	SessionID  string       `json:"session_id"`
	DurationMS int64        `json:"duration_ms"`
	Tools      []reportTool `json:"tools"`
	// asks nobody could answer, subagents' and workflows' included
	Denied int `json:"denied"`
	// times the turn compacted or cleared old tool output to fit the window
	Compactions int `json:"compactions"`
	Usage       struct {
		Input   int64   `json:"input_tokens"`
		Output  int64   `json:"output_tokens"`
		CostUSD float64 `json:"cost_usd"`
	} `json:"usage"`

	unsandboxed int // of Denied, asks to run outside the sandbox
	tuiOnly     int // of Denied, asks no permission mode ever settles
	refused     int // main-agent calls a rule, mode, profile or handler denied
}

type reportTool struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Error is the first line of a failed call's output, so a script driving
	// -p reads why; Denied marks the ones the permission gate refused
	Error  string `json:"error,omitempty"`
	Denied bool   `json:"denied,omitempty"`
}

// failureLine is why a failed call failed: a bash result ends in its
// exit status, signal or "stopped:" line under the command's output, so the last such
// line (the output may hold one too), else the last line; other tools
// lead with it.
func failureLine(tool, output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if tool != tools.ToolBash {
		return lines[0]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := lines[i]; strings.HasPrefix(l, "exit status ") || strings.HasPrefix(l, "signal: ") || strings.HasPrefix(l, "stopped: ") || strings.HasPrefix(l, "timed out ") {
			return l
		}
	}
	return lines[len(lines)-1]
}

// turnReport summarizes the transcript rows one turn added.
func turnReport(rows []hostapi.ChatMessage) report {
	rep := report{Tools: []reportTool{}}
	var replies []string
	for _, m := range rows {
		switch {
		case m.Role != "assistant":
		case m.Kind == "":
			replies = append(replies, strings.TrimSpace(m.Text))
		case m.Kind == "tool":
			rt := reportTool{Name: m.Tool, Input: m.Text, OK: m.Status == "ok", Detail: m.Detail}
			if !rt.OK {
				rt.Error = failureLine(m.Tool, m.Output)
				// the gate's DeniedText; a bare "permission denied" is also EACCES
				rt.Denied = strings.HasPrefix(strings.TrimSpace(m.Output), "permission denied (")
			}
			// an unanswered ask is counted at the gate (Host.HeadlessDenials)
			if by, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(m.Output), "permission denied ("), ")"); rt.Denied && by != "default" && by != "unsandboxed" && by != "network" {
				rep.refused++
			}
			rep.Tools = append(rep.Tools, rt)
		case m.Kind == "compact":
			rep.Compactions++
		case m.Kind == "error":
			rep.IsError, rep.Error = true, m.Text
		}
	}
	rep.Result = strings.Join(replies, "\n\n")
	return rep
}
