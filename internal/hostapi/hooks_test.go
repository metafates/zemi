package hostapi

import (
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/tools"
)

// claudeFixHost writes a Claude-layout plugin under local/fix, hooks.json
// and .mcp.json as given, and boots a host whose plugins step has run.
func claudeFixHost(t *testing.T, hooksJSON, mcpJSON string) (*Host, string) {
	return claudeFixHostFiles(t, hooksJSON, mcpJSON, nil)
}

func claudeFixHostFiles(t *testing.T, hooksJSON, mcpJSON string, extra map[string]string) (*Host, string) {
	t.Helper()
	tmp := t.TempDir()
	dirs := config.Dirs{
		ProjectDir: filepath.Join(tmp, "proj"), ConfigHome: filepath.Join(tmp, "cfg"),
		DataHome: filepath.Join(tmp, "data"), StateHome: filepath.Join(tmp, "state"),
	}
	root := filepath.Join(dirs.PluginsRoot(), "local", "fix")
	files := map[string]string{
		filepath.Join(root, ".claude-plugin", "plugin.json"): `{"name":"fix","version":"1.0.0","description":"fixture"}`,
		filepath.Join(root, "hooks", "hooks.json"):           hooksJSON,
		filepath.Join(root, ".mcp.json"):                     mcpJSON,
		filepath.Join(dirs.ConfigHome, "init.lua"):           "",
	}
	for p, src := range extra {
		files[filepath.Join(root, p)] = src
	}
	for p, src := range files {
		if src == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{
		{Prefix: dirs.ConfigHome, Origin: lua.OriginUser},
		{Prefix: dirs.PluginsRoot(), Origin: lua.OriginPlugin},
	}})
	h := NewHost(r, log, config.NewStore(), dirs)
	if err := r.Boot(t.Context(), []lua.BootStep{
		h.NamespacesStep(), h.PluginsStep(), h.SkillsUserHooksStep(), h.MarkdownCommandsStep(),
	}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	// the grant roots read through symlinks, as h.pluginsRoot does
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return h, root
}

const fixMCP = `{"mcpServers":{
	"echo":{"type":"http","url":"http://127.0.0.1:1/mcp"},
	"skip_sse":{"type":"sse","url":"http://127.0.0.1:1/sse"},
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-long":{"type":"http","url":"http://127.0.0.1:1/mcp"}
}}`

// TestClaudeHooksLoad: the mapped events register, the refused events and
// unsupported hook entries do not, and the MCP servers register under the
// plugin prefix.
func TestClaudeHooksLoad(t *testing.T) {
	hooks := `{"hooks":{
		"PostToolUse":[{"matcher":"Bash|Edit","hooks":[{"type":"command","command":"true"}]}],
		"Stop":[{"hooks":[{"type":"command","command":"true"}]}],
		"PreToolUse":[{"hooks":[{"type":"command","command":"echo pre"}]}],
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"echo up"}]}],
		"Notification":[{"hooks":[{"type":"prompt","prompt":"say"}],"hooks2":null},{"hooks":[{"type":"command","command":"x","if":"Bash(*)"}]}],
		"SessionStart":[{"hooks":[{"type":"command","command":"echo start"}]}]
	}}`
	h, root := claudeFixHost(t, hooks, fixMCP)
	toolPost := h.hookReg.handlers("tool.post")
	if len(toolPost) != 1 || toolPost[0].event != "PostToolUse" || toolPost[0].root != root {
		t.Fatalf("tool.post entries: %+v", toolPost)
	}
	if toolPost[0].match == nil || !toolPost[0].match(tools.ToolShell) || toolPost[0].match(tools.ToolRead) {
		t.Fatalf("matcher: aliased Bash must match shell only: %+v", toolPost[0])
	}
	if len(h.hookReg.handlers("agent.finish")) != 1 || len(h.hookReg.handlers("session.start")) != 1 {
		t.Fatalf("Stop or SessionStart missing: %+v", h.hookReg)
	}
	for _, refused := range []string{"tool.pre", "composer.submit", "session.start2"} {
		if h.hookReg.handlers(refused) != nil {
			t.Fatalf("%s: refused event registered", refused)
		}
	}
	// the hook entries know their data dir, whose parent the boot created
	if fi, err := os.Stat(filepath.Join(h.pluginDataDir("fix"))); err != nil || !fi.IsDir() {
		t.Fatalf("data dir: %v", err)
	}
	g := h.pluginGrant.Load()
	if g == nil || !g.claude[root] {
		t.Fatalf("grant: %+v", g)
	}
	var names []string
	for _, s := range h.mcp.list() {
		names = append(names, s.name)
	}
	if len(names) != 1 || names[0] != "fix-echo" {
		t.Fatalf("mcp servers: %v", names)
	}
}

// TestHookMatcher: the three matcher forms, the alias and the invalid regex.
func TestHookMatcher(t *testing.T) {
	cases := []struct {
		matcher, tool string
		want          bool
	}{
		{"", "shell", true},
		{"*", "shell", true},
		{"Bash|Edit", "shell", true},
		{"Bash|Edit", "edit", true},
		{"Bash|Edit", "read", false},
		{"^sh", "shell", true},
		{"^sh", "read", false},
		{"invalid[", "shell", false},
	}
	for _, c := range cases {
		m := hookMatcher(c.matcher)
		if m == nil {
			m = func(string) bool { return true }
		}
		if got := m(c.tool); got != c.want {
			t.Errorf("matcher %q on %s: %v, want %v", c.matcher, c.tool, got, c.want)
		}
	}
}

// TestClaudeHookChild fires a PostToolUse hook for real: the child sees the
// plugin vars in its environment, the plugin root as its directory and the
// Claude payload on stdin.
func TestClaudeHookChild(t *testing.T) {
	dst := t.TempDir()
	envPath, cwdPath, inPath := filepath.Join(dst, "env"), filepath.Join(dst, "cwd"), filepath.Join(dst, "in")
	hooks := fmt.Sprintf(`{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":
		"env | grep ^CLAUDE > %s; pwd > %s; cat > %s"}]}]}}`, envPath, cwdPath, inPath)
	h, root := claudeFixHost(t, hooks, "{}")
	sid := "s-hook"
	h.agents.mu.Lock()
	h.agents.sessionID = sid
	h.agents.mu.Unlock()

	h.consultHooks("tool.post", map[string]any{"tool_name": "shell", "tool_input": map[string]any{"cmd": "ls"}})
	waitFile(t, inPath)
	env, _ := os.ReadFile(envPath)
	if !strings.Contains(string(env), "CLAUDE_PLUGIN_ROOT="+root) || !strings.Contains(string(env), "CLAUDE_PLUGIN_DATA="+h.pluginDataDir("fix")) {
		t.Fatalf("child env: %q", env)
	}
	if cwd, _ := os.ReadFile(cwdPath); strings.TrimSpace(string(cwd)) != root {
		t.Fatalf("child cwd: %q, want %q", cwd, root)
	}
	var payload map[string]any
	if err := json.Unmarshal(readFile(t, inPath), &payload); err != nil {
		t.Fatalf("stdin: %v", err)
	}
	for k, want := range map[string]string{
		"session_id": sid, "hook_event_name": "PostToolUse", "tool_name": "shell",
		"cwd": h.Dirs.ProjectDir, "permission_mode": "bypassPermissions",
	} {
		if got := fmt.Sprint(payload[k]); got != want {
			t.Errorf("payload %s: %q, want %q", k, got, want)
		}
	}
	if in, _ := payload["tool_input"].(map[string]any); in["cmd"] != "ls" {
		t.Errorf("tool_input: %v", payload["tool_input"])
	}
	if _, err := os.Stat(inPath + "x"); err == nil {
		t.Fatal("matched a non-matching tool")
	}

	// the pipe matcher is aliased: a tool the matcher does not name fires nothing
	os.Remove(inPath)
	h.consultHooks("tool.post", map[string]any{"tool_name": tools.ToolRead, "tool_input": nil})
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(inPath); err == nil {
		t.Fatal("fired for a tool outside the matcher")
	}
}

// TestSessionStartCapture: the capture stores the hooks' text for the session
// and drops a pinned prompt so the next send rebuilds with it.
func TestSessionStartCapture(t *testing.T) {
	cases := []struct {
		name      string
		command   string
		sessionID string
		want      string
	}{
		{"additionalContext", `echo '{"hookSpecificOutput":{"additionalContext":"CTX line"}}'`, "s1", "CTX line"},
		{"plain stdout", `echo plain text`, "s1", "plain text"},
		{"a hook the timeout killed", "sleep 30; echo late", "s1", ""},
		{"a capture for a session gone", `echo gone`, "s2", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			timeout := ""
			if c.name == "a hook the timeout killed" {
				timeout = `"timeout":1,`
			}
			hooks := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command",%s"command":%q}]}]}}`, timeout, c.command)
			h, _ := claudeFixHost(t, hooks, "{}")
			h.agents.mu.Lock()
			h.agents.sessionID = c.sessionID
			h.agents.pin.key = "pin"
			h.agents.mu.Unlock()
			h.captureSessionContext("s1")
			hookText := func() string { h.agents.mu.Lock(); defer h.agents.mu.Unlock(); return h.agents.hookCtx }
			if c.want == "" {
				waitQuiet(t, hookText)
				if got := hookText(); got != "" {
					t.Fatalf("hookCtx: %q", got)
				}
				h.agents.mu.Lock()
				defer h.agents.mu.Unlock()
				if h.agents.pin.key != "pin" {
					t.Fatal("pin dropped without text")
				}
				return
			}
			waitFor(t, func() error {
				if hookText() == "" {
					return fmt.Errorf("no context captured")
				}
				return nil
			})
			h.agents.mu.Lock()
			defer h.agents.mu.Unlock()
			if !strings.Contains(h.agents.hookCtx, c.want) {
				t.Fatalf("hookCtx: %q", h.agents.hookCtx)
			}
			if !strings.HasPrefix(h.agents.hookCtx, "\n\n# Plugin context\n") {
				t.Fatalf("hookCtx unheaded: %q", h.agents.hookCtx)
			}
			if h.agents.pin.key != "" {
				t.Fatal("a late capture must drop the pin")
			}
		})
	}
}

// TestClaudePluginLayers: a Claude plugin's skills, commands and agents load
// under the plugin prefix, the commands as model-invocable skill rows beside
// their /command, the agent's Claude tool names aliased and its unknown
// model dropped, and the bodies carry the plugin root.
func TestClaudePluginLayers(t *testing.T) {
	extra := map[string]string{
		"skills/greet/SKILL.md": "---\nuser-invocable: false\n---\n\nGreet the user warmly.\n\nSay hi from ${CLAUDE_PLUGIN_ROOT}.\n",
		"commands/pr.md":        "---\ndescription: Draft a PR description\n---\nDraft a PR for $ARGUMENTS.\n",
		"agents/reviewer.md":    "---\ndescription: Reviews changes\ntools: bash, read\nmodel: sonnet\n---\nReview the diff at ${CLAUDE_PLUGIN_ROOT}.\n",
	}
	h, root := claudeFixHostFiles(t, "{}", "{}", extra)
	skills := map[string]skill.Skill{}
	for _, s := range h.skillsReg.list() {
		skills[s.Name] = s
	}
	greet := skills["fix-greet"]
	if greet.Name == "" || greet.Description != "Greet the user warmly." || greet.Root != root || greet.UserInvocable {
		t.Fatalf("fix-greet: %+v", greet)
	}
	pr := skills["fix-pr"]
	if pr.Name == "" || pr.UserInvocable || pr.DisableModelInvocation || pr.File == "" || pr.Description != "Draft a PR description" {
		t.Fatalf("fix-pr: %+v", pr)
	}
	if c := h.Commands.Lookup("fix-pr"); c == nil || c.Path != skillFile(pr) {
		t.Fatalf("the /fix-pr command: %+v", c)
	}
	var reviewer AgentDef
	for _, d := range h.agentDefs() {
		if d.Name == "fix-reviewer" {
			reviewer = d
		}
	}
	if reviewer.Name == "" || !slices.Equal(reviewer.Tools, []string{"shell", "read"}) || reviewer.Model != "" {
		t.Fatalf("fix-reviewer: %+v", reviewer)
	}
	if !strings.Contains(reviewer.System, root) {
		t.Fatal("the agent body did not get the plugin root")
	}
	text, err := tools.SkillText(tools.SkillInfo{Name: greet.Name, Path: greet.Dir, Root: greet.Root, Data: greet.Data})
	if err != nil || !strings.Contains(text, "Say hi from "+root) {
		t.Fatalf("skill text: %q %v", text, err)
	}
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	waitFor(t, func() error {
		if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
			return fmt.Errorf("%s not written yet", path)
		}
		return nil
	})
}

// waitQuiet gives the goroutine its chance and fails only when it produced
// what it should not have.
func waitQuiet(t *testing.T, get func() string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if got := get(); got != "" {
		t.Fatalf("the empty capture wrote %q", got)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
