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
	} {
		got, code := parseArgs(c.args, io.Discard, io.Discard)
		if code != c.code || (code == -1 && got != c.want) {
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

// TestConfigFingerprint: edits to config files change the fingerprint,
// zemi's own permissions.lua and git data do not.
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
	write("permissions.lua", "return {}")
	write(".git/index", "x")
	if configFingerprint(dir) != base {
		t.Fatal("zemi-owned files changed the fingerprint")
	}
	write("init.lua", "-- two!")
	if configFingerprint(dir) == base {
		t.Fatal("an init.lua edit went unnoticed")
	}
}

// TestHashTreeFraming: a file whose content spells out another file does
// not hash like the tree holding both, or trust would carry over to it.
func TestHashTreeFraming(t *testing.T) {
	hash := func(files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		h, err := hostapi.HashTree(dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if hash(map[string]string{"a": "x\x00b\x00y"}) == hash(map[string]string{"a": "x", "b": "y"}) {
		t.Fatal("two trees share a hash")
	}
}

// TestProjectTrustResetOnRefusal: a reload whose changed tree is refused
// loads no project markdown command, even after an earlier trusted boot.
func TestProjectTrustResetOnRefusal(t *testing.T) {
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
	hash, err := hostapi.HashTree(dirs.ProjectConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	h.SetProjectTrusted(hash)
	once := func(string, string) string { return "once" }
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), projectConfigStep(h, once, dirs), h.MarkdownCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.Commands.List() {
		if c.Name == "x" {
			t.Fatal("refused project command loaded")
		}
	}
}
