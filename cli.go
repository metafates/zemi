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

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/proc"
)

const usage = `zemi - a terminal coding agent that learns how you work

Usage:
  zemi [flags] [prompt]

  zemi                          open the TUI
  zemi "fix the failing test"   open the TUI and send a first message
  zemi -p "explain main.go"     answer on stdout and exit
  git diff | zemi -p "review"   piped stdin is appended to the prompt
  zemi -- why does ls -la fail  flags are read anywhere; after -- it is all prompt

Flags:
  -p, --print         answer the prompt on stdout and exit (no TUI)
  -c, --continue      continue the most recent session
  -r, --resume id     resume the session with this id
  -m, --model id      model to use (provider/model)
      --mode mode     permission mode: manual, plan, auto or yolo
      --output fmt    with -p: text (the reply) or json (reply, tools, usage)
      --trust         trust this project for this run (--trust=false: do not)
      --health        run the /health checks on stdout and exit (1 if any failed)
  -v, --version       print the version and exit
  -h, --help          show this help

With -p nobody can answer a permission ask, so asks are denied unless
--mode is auto (a model approves routine actions) or yolo. A project you
have not trusted in the TUI runs untrusted with -p unless --trust is given.

Environment:
  ZEMI_CONFIG_HOME    config dir (default ~/.config/zemi)
  ZEMI_DATA_HOME      data dir (default ~/.local/share/zemi)
  ZEMI_STATE_HOME     state dir: sessions, memory, logs (default ~/.local/state/zemi)

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
	trust         *bool // --trust for this run; nil when not given
}

// parseArgs parses the command line. code is -1 when zemi should run, or
// the exit status when it is done (help, version, usage error).
func parseArgs(args []string, stdout, stderr io.Writer) (o options, code int) {
	fs := flag.NewFlagSet("zemi", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help, ver, trust bool
	for name, p := range map[string]*bool{"p": &o.print, "print": &o.print, "c": &o.cont, "continue": &o.cont, "health": &o.health, "h": &help, "help": &help, "v": &ver, "version": &ver, "trust": &trust} {
		fs.BoolVar(p, name, false, "")
	}
	for name, p := range map[string]*string{"r": &o.resume, "resume": &o.resume, "m": &o.model, "model": &o.model, "mode": &o.mode, "output": &o.output} {
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

// readStdin appends piped stdin to the prompt in print mode; --health
// ignores stdin. The TUI needs
// the terminal on stdin, so a pipe there is a usage error.
func (o *options) readStdin(stdin *os.File) error {
	fi, err := stdin.Stat()
	if o.health || err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	if !o.print {
		return errors.New("stdin is not a terminal; use -p to answer from a pipe")
	}
	// NOTE: stdin is capped at 1 MiB; larger input belongs in a file the
	// agent reads.
	first, got := make(chan struct{}), make(chan []byte, 1)
	go func() {
		head := make([]byte, 1)
		n, _ := stdin.Read(head)
		close(first)
		rest, _ := io.ReadAll(io.LimitReader(stdin, 1<<20-1))
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
	if in := strings.TrimSpace(string(<-got)); in != "" {
		o.prompt = strings.TrimSpace(o.prompt + "\n\n" + in)
	}
	return nil
}

// headlessTrust answers the trust prompt when nobody can: not now, with one
// line saying what that turns off.
func headlessTrust(stderr io.Writer) func(root, cwd, state string) string {
	return func(root, _, _ string) string {
		fmt.Fprintf(stderr, "zemi: %s is not trusted: project config, instructions and language servers are off and only read-only commands run without asking; pass --trust or run /trust in the TUI\n", root)
		return "later"
	}
}

// applyOptions applies the flags after boot, so they win over init.lua.
func applyOptions(h *hostapi.Host, o options) error {
	if o.model != "" {
		if err := h.Settings.SetHost("agent.model", o.model); err != nil {
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
// check makes the exit status 1, so a script can gate on it.
func printHealth(h *hostapi.Host, stdout io.Writer) int {
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
		fmt.Fprintln(stderr, "zemi:", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report{IsError: true, Error: err.Error(), Tools: []reportTool{}})
	return 1
}

// printTurn runs one turn without the TUI: the reply goes to stdout (or a
// JSON report with --output json), a failure to stderr with status 1, an
// interrupt ends with 130.
func printTurn(h *hostapi.Host, prompt string, jsonOut bool, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	start := len(h.Session().Transcript)
	began, before := time.Now(), h.AgentStream()
	if err := h.AgentSend(prompt); err != nil {
		return failPrint(err, jsonOut, stdout, stderr)
	}
	interrupted := false
	for h.AgentActive() {
		select {
		case <-ctx.Done():
			if !interrupted {
				interrupted = true
				h.AgentCancel()
				// the turn gets a moment to stop, then print mode leaves anyway
				ctx, stop = context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
			} else {
				proc.KillAll()
				return 130
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if interrupted {
		proc.KillAll()
		return 130
	}
	rep := turnReport(h.Session().Transcript[start:])
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
		if n := rep.denied; n > 0 {
			calls := "calls"
			if n == 1 {
				calls = "call"
			}
			base := ""
			for _, d := range h.PermModeList() {
				if d.Name == h.PermMode() {
					base = d.Base
				}
			}
			fmt.Fprintf(stderr, "zemi: %d tool %s denied; nobody can answer an ask with -p, %s\n", n, calls, deniedHint(rep, h.ProjectTrusted(), base))
		}
		if rep.Result != "" {
			fmt.Fprintln(stdout, rep.Result)
		}
	}
	if rep.IsError {
		return 1
	}
	return 0
}

// deniedHint says what would let print mode's denied calls through.
func deniedHint(rep report, trusted bool, base string) string {
	const rule = "an allow rule (zemi.permission.rule in init.lua) settles it"
	switch {
	case rep.unsandboxed > 0:
		return "and running outside the sandbox needs the TUI; " + rule
	case !trusted:
		return "try --trust --mode auto (auto mode settles little in an untrusted project)"
	case base == "auto":
		return "and auto mode left it to the user; " + rule
	}
	return "try --mode auto"
}

// report is the --output json document for one print-mode turn.
type report struct {
	Result     string       `json:"result"`
	IsError    bool         `json:"is_error"`
	Error      string       `json:"error,omitempty"`
	SessionID  string       `json:"session_id"`
	DurationMS int64        `json:"duration_ms"`
	Tools      []reportTool `json:"tools"`
	Usage      struct {
		Input   int64   `json:"input_tokens"`
		Output  int64   `json:"output_tokens"`
		CostUSD float64 `json:"cost_usd"`
	} `json:"usage"`

	denied      int // tool calls the permission gate refused; text mode notes them
	unsandboxed int // of those, asks to run outside the sandbox
}

type reportTool struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
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
			if m.Status != "ok" && strings.Contains(m.Output, "permission denied") {
				rep.denied++
				if strings.Contains(m.Output, "permission denied (unsandboxed)") {
					rep.unsandboxed++
				}
			}
			rep.Tools = append(rep.Tools, reportTool{Name: m.Tool, Input: m.Text, OK: m.Status == "ok", Detail: m.Detail})
		case m.Kind == "error":
			rep.IsError, rep.Error = true, m.Text
		}
	}
	rep.Result = strings.Join(replies, "\n\n")
	return rep
}
