package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestFailPrintJSON: a failure before the turn still reaches a script on
// stdout as the report it parses.
func TestFailPrintJSON(t *testing.T) {
	var out, errOut strings.Builder
	if code := failPrint(errors.New("no model configured"), true, &out, &errOut); code != 1 || errOut.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	var rep report
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil || !rep.IsError || rep.Error != "no model configured" {
		t.Fatalf("stdout = %q (%v)", out.String(), err)
	}
	out.Reset()
	if code := failPrint(errors.New("boom"), false, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "boom") {
		t.Fatalf("text mode: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		code int
		want options
	}{
		{"tui", nil, -1, options{}},
		{"prompt words join", []string{"-m", "a/b", "fix", "the", "test"}, -1, options{model: "a/b", prompt: "fix the test"}},
		{"long print and resume", []string{"--print", "--resume", "s1", "hi"}, -1, options{print: true, resume: "s1", prompt: "hi"}},
		{"dash dash keeps a dashed prompt", []string{"-p", "--", "-v means verbose?"}, -1, options{print: true, prompt: "-v means verbose?"}},
		{"help", []string{"-h"}, 0, options{}},
		{"version", []string{"--version"}, 0, options{}},
		{"unknown flag", []string{"--nope"}, 2, options{}},
		{"continue with resume", []string{"-c", "-r", "s1"}, 2, options{}},
		{"json output", []string{"-p", "--output", "json", "hi"}, -1, options{print: true, output: "json", prompt: "hi"}},
		{"json output needs print", []string{"--output", "json"}, 2, options{}},
		{"bad output", []string{"-p", "--output", "yaml"}, 2, options{}},
		{"health", []string{"--health"}, -1, options{health: true}},
		{"flags after the prompt", []string{"-p", "hi", "--output", "json", "there"}, -1, options{print: true, output: "json", prompt: "hi there"}},
		{"dash dash after a word", []string{"hi", "--", "-m", "x"}, -1, options{prompt: "hi -m x"}},
		{"trust", []string{"--trust", "hi"}, -1, options{prompt: "hi", trust: new(true)}},
		{"trust false", []string{"--trust=false"}, -1, options{trust: new(false)}},
	} {
		got, code := parseArgs(c.args, io.Discard, io.Discard)
		sameTrust := (got.trust == nil) == (c.want.trust == nil) && (got.trust == nil || *got.trust == *c.want.trust)
		got.trust, c.want.trust = nil, nil
		if code != c.code || (code == -1 && (got != c.want || !sameTrust)) {
			t.Errorf("%s: code=%d opts=%+v", c.name, code, got)
		}
	}
}

func TestHealthIgnoresPipedStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w.Close()
	o := options{health: true}
	if err := o.readStdin(r); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "dev",
		"(devel)":                      "dev",
		"v0.0.0-20240101120000-abcdef": "dev",
		"v1.2.3":                       "v1.2.3",
	} {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTurnReport(t *testing.T) {
	rep := turnReport([]hostapi.ChatMessage{
		{Role: "user", Text: "go"},
		{Role: "assistant", Text: "looking"},
		{Role: "assistant", Kind: "tool", Tool: "agent", Text: "find it", Status: "ok", Detail: "explore · 3 tool calls"},
		{Role: "assistant", Kind: "tool", Tool: "bash", Text: "rm -rf /", Status: "err", Output: "rm: /: is a directory\nexit status 1\nchanged: x"},
		{Role: "assistant", Kind: "tool", Tool: "write", Text: "x.go", Status: "err",
			Output: "permission denied (mode): fs.write /x.go: print mode cannot ask"},
		{Role: "assistant", Kind: "tool", Tool: tools.ToolLua, Text: "zemi.mcp.add{}", Status: "err",
			Output: "permission denied (mode): tool.lua: print mode cannot ask"},
		{Role: "assistant", Kind: "tool", Tool: tools.ToolMemory, Text: "forget x", Status: "err",
			Output: "permission denied (default): memory.write: nobody could answer the ask"},
		{Role: "assistant", Kind: "compact", Text: "cleared 40k tokens of old tool output"},
		{Role: "assistant", Text: "found"},
	})
	if rep.Result != "looking\n\nfound" || rep.IsError || rep.Compactions != 1 || len(rep.Tools) != 5 || !rep.Tools[0].OK || rep.Tools[1].OK || rep.Tools[0].Detail == "" {
		t.Fatalf("report = %+v", rep)
	}
	// a failed call says why and whether the gate refused it
	if rt := rep.Tools[1]; rt.Error != "exit status 1" || rt.Denied || rep.Tools[0].Error != "" {
		t.Fatalf("bash row = %+v", rt)
	}
	if rt := rep.Tools[2]; !rt.Denied || !strings.HasPrefix(rt.Error, "permission denied (mode)") {
		t.Fatalf("write row = %+v", rt)
	}
	// an unanswered ask is the gate's to count; the rest were refused
	if rep.refused != 2 {
		t.Fatalf("refused = %d, want 2", rep.refused)
	}
	if rep := turnReport([]hostapi.ChatMessage{{Role: "assistant", Kind: "error", Text: "rate limited"}}); !rep.IsError || rep.Error != "rate limited" {
		t.Fatalf("error report = %+v", rep)
	}
}

func TestDeniedHint(t *testing.T) {
	for _, c := range []struct {
		name        string
		unsandboxed int
		trusted     bool
		want        string
	}{
		{"untrusted", 0, false, "try --trust"},
		{"trusted", 0, true, "the mode left it to the user"},
		{"outside the sandbox", 1, true, "outside the sandbox needs the TUI"},
		{"lua script", 0, true, "only the TUI can approve a Lua script"},
	} {
		rep := report{Denied: 1, unsandboxed: c.unsandboxed}
		if c.name == "lua script" {
			rep.tuiOnly = 1
		}
		if got := deniedHint(rep, c.trusted); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

// TestConfigFingerprint: edits to config files change the fingerprint,
// git data does not.
func TestConfigFingerprint(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("init.lua", "-- one")
	base := configFingerprint(dir)
	write(".git/index", "x")
	write("site/pack/x/start/y/node_modules/z/index.js", "x")
	if configFingerprint(dir) != base {
		t.Fatal("git data or a dependency tree changed the fingerprint")
	}
	write("init.lua", "-- two!")
	if configFingerprint(dir) == base {
		t.Fatal("an init.lua edit went unnoticed")
	}
}

// TestTrustGate: --trust or an earlier "not now" wins over the saved
// decision, which wins over the prompt; only y and n are saved, the prompt
// opens at most once per process, and only a trusted boot loads project
// commands.
func TestTrustGate(t *testing.T) {
	for _, c := range []struct {
		name, saved string
		session     *bool
		answer      string // "" means the prompt must not open
		trusted     bool
		wantSaved   string
	}{
		{"saved trusted", "trusted", nil, "", true, "trusted"},
		{"saved untrusted", "untrusted", nil, "", false, "untrusted"},
		{"flag over saved", "trusted", new(false), "", false, "trusted"},
		{"flag trusts", "", new(true), "", true, ""},
		{"prompt y", "", nil, "trust", true, "trusted"},
		{"prompt n", "", nil, "untrust", false, "untrusted"},
		{"prompt esc", "", nil, "later", false, ""},
	} {
		tg, h, r := newTrustGate(t)
		root := tg.dirs.TrustRoot()
		if c.saved != "" {
			if err := writeTrust(tg.dirs.TrustFile(), map[string]string{root: c.saved}); err != nil {
				t.Fatal(err)
			}
		}
		tg.session.Store(c.session)
		asked := 0
		tg.ask = func(string, string, string) string {
			asked++
			return c.answer
		}
		if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), tg.step(), h.MarkdownCommandsStep()}); err != nil {
			t.Fatal(err)
		}
		loaded := h.Commands.Lookup("x") != nil
		again, _ := tg.decide()
		if h.ProjectTrusted() != c.trusted || loaded != c.trusted || again != c.trusted {
			t.Errorf("%s: trusted=%v loaded=%v again=%v", c.name, h.ProjectTrusted(), loaded, again)
		}
		if want := map[bool]int{true: 1, false: 0}[c.answer != ""]; asked != want {
			t.Errorf("%s: prompt opened %d times", c.name, asked)
		}
		if got := readTrust(tg.dirs.TrustFile())[root]; got != c.wantSaved {
			t.Errorf("%s: saved %q", c.name, got)
		}
	}
}

// TestTrustCommand: /trust saves the answer and reloads only when the
// decision changed; not now changes nothing.
func TestTrustCommand(t *testing.T) {
	for _, c := range []struct {
		answer, wantSaved string
		reloads           bool
	}{
		{"trust", "trusted", true},
		{"untrust", "untrusted", false},
		{"later", "", false},
	} {
		tg, _, _ := newTrustGate(t) // outside the bubble, which waits for every goroutine in it: a host's never exit
		synctest.Test(t, func(t *testing.T) {
			tg.ask = func(_, _, state string) string {
				if state != "not trusted" {
					t.Errorf("/trust shows state %q", state)
				}
				return c.answer
			}
			reloaded := make(chan struct{}, 1)
			tg.command(func() { reloaded <- struct{}{} })()
			select {
			case <-reloaded:
				if !c.reloads {
					t.Errorf("%s: reloaded", c.answer)
				}
			case <-time.After(500 * time.Millisecond):
				if c.reloads {
					t.Errorf("%s: no reload", c.answer)
				}
			}
			if got := readTrust(tg.dirs.TrustFile())[tg.dirs.TrustRoot()]; got != c.wantSaved {
				t.Errorf("%s: saved %q", c.answer, got)
			}
		})
	}
}

// TestHeadlessTrust: a headless run with no decision says what is off and
// how to turn it on, and never trusts.
func TestHeadlessTrust(t *testing.T) {
	var out strings.Builder
	if got := headlessTrust(&out)("/repo", "/repo", ""); got != "later" || !strings.Contains(out.String(), "/repo is not trusted") || !strings.Contains(out.String(), "--trust") {
		t.Fatalf("answer %q, stderr %q", got, out.String())
	}
}

func newTrustGate(t *testing.T) (*trustGate, *hostapi.Host, *lua.Runtime) {
	t.Helper()
	project := t.TempDir()
	cmds := filepath.Join(project, ".zemi", "commands")
	if err := os.MkdirAll(cmds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmds, "x.md"), []byte("---\ndescription: x\n---\nhello"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	dirs := config.Dirs{ProjectDir: project, StateHome: t.TempDir()}
	h := hostapi.NewHost(r, log, config.NewStore(), dirs)
	return &trustGate{h: h, dirs: dirs}, h, r
}

// TestSendPromptRunsCommands: a prompt on the command line that names a
// command runs it, the way the composer would, with piped stdin behind the
// marker; anything else is a message.
func TestSendPromptRunsCommands(t *testing.T) {
	_, h, _ := newTrustGate(t)
	got := ""
	if _, err := h.Commands.Add(&hostapi.Command{
		Name: "note", NArgs: "*", Desc: "d", Origin: lua.OriginUser,
		GoFn: func(_ *hostapi.Host, args string) error { got = args; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ prompt, stdin, args string }{
		{"/note the args", "", "the args"},
		{"/note\nnext line", "", "next line"},
		{"/note", "diff --git a/x b/x", hostapi.Attached("stdin", "diff --git a/x b/x")},
	} {
		o := options{print: true, prompt: c.prompt}
		if c.stdin != "" {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			w.WriteString(c.stdin)
			w.Close()
			err = o.readStdin(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		got = "unset"
		if err := sendPrompt(h, o.prompt); err != nil || got != c.args {
			t.Errorf("%q: ran with %q, %v", c.prompt, got, err)
		}
	}
	// no such command: the text is the model's, and this host has no model
	got = "unset"
	if err := sendPrompt(h, "/notes please"); err == nil || got != "unset" {
		t.Fatalf("an unknown command must reach the agent: %v, ran %q", err, got)
	}
}

// TestStdinOverCap: more than 1 MiB of stdin is refused, not cut.
func TestStdinOverCap(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() {
		w.Write(make([]byte, 1<<20+1))
		w.Close()
	}()
	o := options{print: true}
	if err := o.readStdin(r); err == nil || !strings.Contains(err.Error(), "over 1 MiB") {
		t.Fatalf("err = %v", err)
	}
}

// fakeOpenAI is an OpenAI-compatible chat server: "call NAME {json}" calls
// that tool, "say X" replies X, "hang" waits for the request to end, and a
// tool result gets "done".
func fakeOpenAI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream   bool
		Messages []struct {
			Role    string
			Content any
		}
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	last := body.Messages[len(body.Messages)-1]
	text, _ := last.Content.(string)
	if parts, ok := last.Content.([]any); ok {
		for _, p := range parts {
			m, _ := p.(map[string]any)
			s, _ := m["text"].(string)
			text += s
		}
	}
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
	if !body.Stream {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c", "object": "chat.completion", "model": "m", "usage": usage,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(delta map[string]any, fin any) {
		b, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m", "usage": usage,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		w.(http.Flusher).Flush()
	}
	call := regexp.MustCompile(`call (\w+) (\{.*\})`).FindStringSubmatch(text)
	_, say, _ := strings.Cut(text, "say ")
	switch {
	case last.Role == "user" && call != nil:
		ev(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "t1", "type": "function",
			"function": map[string]any{"name": call[1], "arguments": call[2]}}}}, nil)
		ev(map[string]any{}, "tool_calls")
	case last.Role == "user" && text == "hang":
		<-r.Context().Done()
		return
	default:
		ev(map[string]any{"role": "assistant", "content": cmp.Or(say, "done")}, nil)
		ev(map[string]any{}, "stop")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// printHost is a host with /new and a fake model, in an untrusted project.
func printHost(t *testing.T) *hostapi.Host {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fakeOpenAI))
	t.Cleanup(srv.Close)
	_, h, r := newTrustGate(t)
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep(mainCommands(func() {}, func() {})...)}); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`zemi.provider.add({ id = "fake", kind = zemi.provider.kind.OPENAI_COMPAT, api_key = "x", base_url = %q, models = { { id = "m", context_window = 128000 } } })
		zemi.config.set("models.main", "fake/m")`, srv.URL+"/v1")
	if err := r.ExecuteChunk(context.Background(), cfg, "init.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestPrintTurn: what -p prints for a turn, and why calls were denied.
func TestPrintTurn(t *testing.T) {
	for _, c := range []struct {
		name, prompt      string
		first             []string // turns run before, -c style
		setup             func(h *hostapi.Host)
		stdout, stderr    string // wanted substrings
		notStdout, notErr string
		denied            int
	}{
		{name: "escapes are stripped", prompt: "say \x1b]52;c;SGk=\a\x1b[31mhi", stdout: "hi", notStdout: "\x1b"},
		{name: "a deny rule is no ask", prompt: `call bash {"command":"ls -la"}`, setup: func(h *hostapi.Host) {
			h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "ls*", Decision: perm.Deny})
		}, stderr: "1 tool call denied by a rule", notErr: "allow rule"},
		{name: "the planner does not write", prompt: `call write {"path":"x.txt","content":"x"}`, setup: func(h *hostapi.Host) {
			if err := h.SetProfile("planner"); err != nil {
				t.Fatal(err)
			}
		}, stderr: "1 tool call denied by a rule", notErr: "allow rule"},
		{name: "a subagent's unanswered ask counts", prompt: `call agent {"agent":"general","description":"d","prompt":"call write {\"path\":\"zz.txt\",\"content\":\"x\"}"}`,
			stderr: "1 tool call denied; nobody can answer an ask with -p", denied: 1},
		{name: "a swapped-in session is not this run's", first: []string{"say hi", "/new"}, prompt: "/resume", notStdout: "hi"},
	} {
		h := printHost(t)
		if c.setup != nil {
			c.setup(h)
		}
		for _, p := range c.first {
			if printTurn(h, p, false, io.Discard, io.Discard) != 0 {
				t.Fatalf("%s: %q failed", c.name, p)
			}
		}
		var out, errOut strings.Builder
		code := printTurn(h, c.prompt, false, &out, &errOut)
		if code != 0 || !strings.Contains(out.String(), c.stdout) || !strings.Contains(errOut.String(), c.stderr) ||
			c.notStdout != "" && strings.Contains(out.String(), c.notStdout) || c.notErr != "" && strings.Contains(errOut.String(), c.notErr) {
			t.Errorf("%s: code %d, stdout %q, stderr %q", c.name, code, out.String(), errOut.String())
		}
		out.Reset()
		if c.denied > 0 {
			var rep report
			printTurn(h, c.prompt, true, &out, io.Discard)
			if err := json.Unmarshal([]byte(out.String()), &rep); err != nil || rep.Denied != c.denied {
				t.Errorf("%s: json denied = %d (%v)", c.name, rep.Denied, err)
			}
		}
	}
}

// TestPrintInterruptJSON: an interrupted run still prints its report, so a
// script keeps the session id it resumes.
func TestPrintInterruptJSON(t *testing.T) {
	h := printHost(t)
	var out strings.Builder
	code := make(chan int)
	go func() { code <- printTurn(h, "hang", true, &out, io.Discard) }()
	for !h.AgentActive() {
		runtime.Gosched()
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	var rep report
	if c := <-code; c != 130 || json.Unmarshal([]byte(out.String()), &rep) != nil || !rep.IsError || rep.Error != "interrupted" || rep.SessionID == "" {
		t.Fatalf("code %d, stdout %q", c, out.String())
	}
}

// TestModelFlagUnderProfile: -m is refused when --profile names a profile
// with its own model, since the run would measure the profile's.
func TestModelFlagUnderProfile(t *testing.T) {
	_, h, r := newTrustGate(t)
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	if err := r.ExecuteChunk(context.Background(), `zemi.agent.profile("cheap", { model = "a/cheap" })`, "init.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	if err := applyOptions(h, options{model: "a/main", profile: "cheap"}); err == nil || !strings.Contains(err.Error(), "sets its model") {
		t.Fatalf("err = %v", err)
	}
}

// BenchmarkConfigFingerprint walks a config tree with a plugin's
// dependency tree in it, the shape that made the one-second watch cost.
func BenchmarkConfigFingerprint(b *testing.B) {
	dir := b.TempDir()
	deps := filepath.Join(dir, "site", "pack", "x", "start", "y", "node_modules")
	for i := range 2000 {
		p := filepath.Join(deps, fmt.Sprintf("m%d", i%100), fmt.Sprintf("f%d.js", i))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}
	_ = os.WriteFile(filepath.Join(dir, "init.lua"), []byte("-- one"), 0o644)
	for b.Loop() {
		configFingerprint(dir)
	}
}
