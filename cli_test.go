package main

import (
	"io"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

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
		{"bad mode", []string{"--mode", "wild"}, 2, options{}},
		{"continue with resume", []string{"-c", "-r", "s1"}, 2, options{}},
		{"json output", []string{"-p", "--output", "json", "hi"}, -1, options{print: true, output: "json", prompt: "hi"}},
		{"json output needs print", []string{"--output", "json"}, 2, options{}},
		{"bad output", []string{"-p", "--output", "yaml"}, 2, options{}},
	} {
		got, code := parseArgs(c.args, io.Discard, io.Discard)
		if code != c.code || (code == -1 && got != c.want) {
			t.Errorf("%s: code=%d opts=%+v", c.name, code, got)
		}
	}
}

func TestTurnReport(t *testing.T) {
	rep := turnReport([]hostapi.ChatMessage{
		{Role: "user", Text: "go"},
		{Role: "assistant", Text: "looking"},
		{Role: "assistant", Kind: "tool", Tool: "agent", Text: "find it", Status: "ok", Detail: "explore · 3 tool calls"},
		{Role: "assistant", Kind: "tool", Tool: "bash", Text: "rm -rf /", Status: "err"},
		{Role: "assistant", Text: "found"},
	})
	if rep.Result != "looking\n\nfound" || rep.IsError || len(rep.Tools) != 2 || !rep.Tools[0].OK || rep.Tools[1].OK || rep.Tools[0].Detail == "" {
		t.Fatalf("report = %+v", rep)
	}
	if rep := turnReport([]hostapi.ChatMessage{{Role: "assistant", Kind: "error", Text: "rate limited"}}); !rep.IsError || rep.Error != "rate limited" {
		t.Fatalf("error report = %+v", rep)
	}
}
