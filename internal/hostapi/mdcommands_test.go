package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

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
// running it sends the expanded body to the agent.
func TestMarkdownCommandSends(t *testing.T) {
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
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), h.MarkdownCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	cmd := h.Commands.Lookup("fix")
	if cmd == nil || cmd.Desc != "fix a file <file>" || cmd.Origin != lua.OriginUser {
		t.Fatalf("command = %+v", cmd)
	}
	if err := h.RunCommand(context.Background(), "fix", "main.go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if got, _ := sent.Load().(string); got != "Fix the bug in main.go." {
		t.Fatalf("sent %q", got)
	}
}

// TestProjectMarkdownCommands: a trusted repository's .zemi, .agents and
// .claude command dirs all load, and the first dir to claim a name keeps it.
func TestProjectMarkdownCommands(t *testing.T) {
	h, r := newTestHostDB(t)
	h.SetProjectTrusted(true)
	for _, c := range []struct{ dir, name, body string }{
		{".zemi", "both", "from zemi"},
		{".claude", "both", "from claude"},
		{".claude", "ctest", "from claude"},
		{".agents", "atest", "from agents"},
	} {
		d := filepath.Join(h.Dirs.ProjectDir, c.dir, "commands")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, c.name+".md"), []byte(c.body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), h.MarkdownCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"both": "from zemi", "ctest": "from claude", "atest": "from agents"} {
		cmd := h.Commands.Lookup(name)
		if cmd == nil || cmd.Desc != want || cmd.Origin != lua.OriginProject {
			t.Fatalf("/%s = %+v, want %q", name, cmd, want)
		}
	}
}
