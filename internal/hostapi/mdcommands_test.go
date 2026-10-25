package hostapi

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
)

func TestExpandPromptArgs(t *testing.T) {
	for _, c := range []struct{ body, args, want string }{
		{"Fix $ARGUMENTS", "  the flaky test ", "Fix the flaky test"},
		{"Move $1 to $2", "a.go b.go", "Move a.go to b.go"},
		{"Move $1 to $2", "a.go", "Move a.go to "},
		{"no args", "ignored", "no args"},
	} {
		if got := expandPromptArgs(c.body, c.args); got != c.want {
			t.Fatalf("expand(%q, %q) = %q, want %q", c.body, c.args, got, c.want)
		}
	}
}

// TestMarkdownCommandSends: a commands/<name>.md file becomes /<name>, and
// running it sends the expanded body behind the typed line, so only what
// the user typed reads as their words; attached text stays behind it too.
func TestMarkdownCommandSends(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	h.Dirs.ConfigHome = t.TempDir()
	dir := filepath.Join(h.Dirs.ConfigHome, "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\ndescription: fix a file\nargument-hint: <file>\n---\nFix the bug in $1.\n"
	if err := os.WriteFile(filepath.Join(dir, "fix.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var sent atomic.Value
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			sent.Store(last)
			return textParts("ok")
		}}, nil
	}
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.MarkdownCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	cmd := h.Commands.Lookup("fix")
	if cmd == nil || cmd.Desc != "fix a file <file>" || cmd.Origin != lua.OriginUser {
		t.Fatalf("command = %+v", cmd)
	}
	body = agent.Attached("command", "Fix the bug in main.go.")
	for _, c := range []struct{ args, want string }{
		{"main.go", "/fix main.go" + body},
		{"main.go" + agent.Attached("stdin", "panic: x"), "/fix main.go" + body + agent.Attached("stdin", "panic: x")},
	} {
		if err := h.RunCommand(t.Context(), "fix", c.args, false); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, h)
		if got, _ := sent.Load().(string); got != c.want {
			t.Fatalf("sent %q, want %q", got, c.want)
		}
	}
}

// TestProjectMarkdownCommands: a trusted repository's .agents/commands load
// as project commands; .claude/commands is not read.
func TestProjectMarkdownCommands(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	h.SetProjectTrusted(true)
	for _, c := range []struct{ dir, name string }{{".agents", "atest"}, {".claude", "ctest"}} {
		d := filepath.Join(h.Dirs.ProjectDir, c.dir, "commands")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, c.name+".md"), []byte("from "+c.dir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.MarkdownCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	if cmd := h.Commands.Lookup("atest"); cmd == nil || cmd.Desc != "from .agents" || cmd.Origin != lua.OriginProject {
		t.Fatalf("/atest = %+v", cmd)
	}
	if cmd := h.Commands.Lookup("ctest"); cmd != nil {
		t.Fatalf(".claude/commands loaded: %+v", cmd)
	}
}
