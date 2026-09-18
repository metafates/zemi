package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
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
		{Role: "assistant", Kind: "tool", Tool: "bash", Text: "rm -rf /", Status: "err"},
		{Role: "assistant", Kind: "tool", Tool: "write", Text: "x.go", Status: "err",
			Output: "permission denied (mode): fs.write /x.go: print mode cannot ask"},
		{Role: "assistant", Text: "found"},
	})
	if rep.Result != "looking\n\nfound" || rep.IsError || len(rep.Tools) != 3 || !rep.Tools[0].OK || rep.Tools[1].OK || rep.Tools[0].Detail == "" {
		t.Fatalf("report = %+v", rep)
	}
	if rep.denied != 1 {
		t.Fatalf("denied = %d, want 1", rep.denied)
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
		base, want  string
	}{
		{"manual", 0, true, "manual", "try --mode auto"},
		{"untrusted", 0, false, "auto", "try --trust --mode auto"},
		{"already auto", 0, true, "auto", "auto mode left it to the user"},
		{"outside the sandbox", 1, true, "auto", "outside the sandbox needs the TUI"},
		{"lua script", 0, true, "auto", "only the TUI can approve a Lua script"},
	} {
		rep := report{denied: 1, unsandboxed: c.unsandboxed}
		if c.name == "lua script" {
			rep.tuiOnly = 1
		}
		got := deniedHint(rep, c.trusted, c.base)
		if !strings.Contains(got, c.want) || (c.base == "auto" && c.trusted && strings.Contains(got, "--mode auto")) {
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
	if configFingerprint(dir) != base {
		t.Fatal("git data changed the fingerprint")
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
		tg, _, _ := newTrustGate(t)
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
// command runs it, the way the composer would; anything else is a message.
func TestSendPromptRunsCommands(t *testing.T) {
	_, h, _ := newTrustGate(t)
	got := ""
	if _, err := h.Commands.Add(&hostapi.Command{
		Name: "note", NArgs: "*", Desc: "d", Origin: lua.OriginUser,
		GoFn: func(_ *hostapi.Host, args string) error { got = args; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	if err := sendPrompt(h, "/note the args"); err != nil || got != "the args" {
		t.Fatalf("command prompt: ran with %q, %v", got, err)
	}
	// no such command: the text is the model's, and this host has no model
	if err := sendPrompt(h, "/notes please"); err == nil {
		t.Fatal("an unknown command must reach the agent")
	}
	if got != "the args" {
		t.Fatalf("an unknown command ran one: %q", got)
	}
}
