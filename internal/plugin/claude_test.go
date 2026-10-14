package plugin

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"Demo Tools":        "demo-tools",
		"demo_tools":        "demo-tools",
		"--Demo--Tools--":   "demo-tools",
		"demo.tools.v2":     "demo-tools-v2",
		"123":               "123",
		"---":               "",
		"":                  "",
		"demo_tools-go.QA2": "demo-tools-go-qa2",
	} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandClaude(t *testing.T) {
	t.Setenv("ZEMI_TEST_TOKEN", "tok")
	t.Setenv("CLAUDE_PLUGIN_ROOT", "/elsewhere")
	root, data := "/plugins/installed/demo", "/data/demo"
	for _, c := range []struct{ in, want string }{
		{"${CLAUDE_PLUGIN_ROOT}/run.sh", root + "/run.sh"},
		{"${CLAUDE_PLUGIN_DATA}/cache.db", data + "/cache.db"},
		{"${ZEMI_TEST_TOKEN}", "tok"},
		{"${ZEMI_TEST_UNSET:-fallback}", "fallback"},
		{"${ZEMI_TEST_UNSET:-}", ""},
		{"${ZEMI_TEST_UNSET}", "${ZEMI_TEST_UNSET}"},
		{"${ZEMI_TEST_UNSET:-a b}/c", "a b/c"},
		{"no vars here", "no vars here"},
	} {
		if got := ExpandClaude(c.in, root, data); got != c.want {
			t.Errorf("ExpandClaude(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// write writes the files of a fixture plugin dir, making parents.
func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadClaude(t *testing.T) {
	t.Setenv("ZEMI_TEST_TOKEN", "tok")
	dir := t.TempDir()
	data := "/data/demo"
	write(t, dir, map[string]string{
		".claude-plugin/plugin.json": `{
			"name": "Demo", "version": "1.2.0", "description": "the demo plugin",
			"author": {"name": "Demo"},
			"$schema": "https://json.schemastore.org/claude-code-plugin.json",
			"commands": "./commands-extra",
			"agents": ["./agents-extra/a.md"],
			"hooks": "./.claude-plugin/hooks/hooks.json",
			"mcpServers": {"inline": {"command": "uvx", "args": ["m"], "env": {"T": "${ZEMI_TEST_TOKEN}"}}}
		}`,
		".mcp.json": `{"mcpServers": {"from-file": {"command": "${CLAUDE_PLUGIN_ROOT}/bin/srv"}}}`,
		"hooks/hooks.json": `{"hooks": {"PostToolUse": [
			{"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "${CLAUDE_PLUGIN_ROOT}/fmt.sh", "timeout": 30}]},
			{"matcher": "", "hooks": [{"type": "command", "command": "notify.sh"}]}
		]}}`,
		".claude-plugin/hooks/hooks.json": `{"hooks": {"SessionStart": [
			{"hooks": [{"type": "command", "command": "rules.mjs"}]}
		]}}`,
	})
	m, err := ReadClaude(dir, data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Demo" || m.Version != "1.2.0" || m.Description != "the demo plugin" || m.Author.Name != "Demo" {
		t.Errorf("scalars: %+v", m)
	}
	if !m.CommandsSet || !m.AgentsSet || m.SkillsSet {
		t.Errorf("declared flags: commands %v, agents %v, skills %v", m.CommandsSet, m.AgentsSet, m.SkillsSet)
	}
	if len(m.Commands) != 1 || m.Commands[0] != filepath.Join(dir, "commands-extra") {
		t.Errorf("commands paths: %v", m.Commands)
	}
	if len(m.Agents) != 1 || m.Agents[0] != filepath.Join(dir, "agents-extra/a.md") {
		t.Errorf("agents paths: %v", m.Agents)
	}
	// .mcp.json first, the manifest's inline server second, both kept
	if len(m.MCP) != 2 || m.MCP[0].Name != "from-file" || m.MCP[1].Name != "inline" {
		t.Fatalf("mcp servers: %+v", m.MCP)
	}
	if m.MCP[0].Command != filepath.Join(dir, "bin/srv") || m.MCP[0].Type != "stdio" {
		t.Errorf("from-file: %+v", m.MCP[0])
	}
	if m.MCP[1].Env["T"] != "tok" || m.MCP[1].Type != "stdio" {
		t.Errorf("inline: %+v", m.MCP[1])
	}
	// hooks/hooks.json plus the manifest's own path, events sorted
	if len(m.Hooks) != 2 || m.Hooks[0].Event != "PostToolUse" || m.Hooks[1].Event != "SessionStart" {
		t.Fatalf("hook events: %+v", m.Hooks)
	}
	if m.Hooks[0].Matchers[0].Matcher != "Edit|Write" || m.Hooks[0].Matchers[0].Hooks[0].Command != filepath.Join(dir, "fmt.sh") ||
		m.Hooks[0].Matchers[0].Hooks[0].Timeout != 30 || m.Hooks[0].Matchers[1].Matcher != "" {
		t.Errorf("PostToolUse group: %+v", m.Hooks[0])
	}
}

func TestReadClaudeShapes(t *testing.T) {
	t.Setenv("ZEMI_TEST_TOKEN", "tok")
	dir := t.TempDir()
	data := "/data/demo"
	write(t, dir, map[string]string{
		// the bare .mcp.json form, a manifest hooks path, an array of paths
		".mcp.json": `{"bare-http": {"type": "http", "url": "https://x/${ZEMI_TEST_UNSET:-9}/", "headers": {"A": "${ZEMI_TEST_TOKEN}"}, "oauth": {"clientId": "cid", "callbackPort": 8765}}}`,
		".claude-plugin/plugin.json": `{
			"name": "Shapes",
			"mcpServers": ["./more-mcp.json"],
			"hooks": ["./.claude-plugin/own-hooks.json"]
		}`,
		"more-mcp.json":                 `{"stdio-one": {"command": "srv", "args": ["--fast"]}}`,
		".claude-plugin/own-hooks.json": `{"Stop": [{"hooks": [{"type": "command", "command": "done.sh"}]}]}`,
	})
	m, err := ReadClaude(dir, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.MCP) != 2 || m.MCP[0].Name != "bare-http" || m.MCP[1].Name != "stdio-one" {
		t.Fatalf("mcp: %+v", m.MCP)
	}
	h := m.MCP[0]
	if h.Type != "http" || h.URL != "https://x/9/" || h.Headers["A"] != "tok" || h.ClientID != "cid" || h.CallbackPort != 8765 {
		t.Errorf("bare-http: %+v", h)
	}
	if len(m.Hooks) != 1 || m.Hooks[0].Event != "Stop" || m.Hooks[0].Matchers[0].Hooks[0].Command != "done.sh" {
		t.Fatalf("hooks: %+v", m.Hooks)
	}
}

func TestReadClaudeMissing(t *testing.T) {
	m, err := ReadClaude(t.TempDir(), "")
	if err != nil || m.Name != "" || m.MCP != nil || m.Hooks != nil {
		t.Errorf("missing manifest: %+v, %v", m, err)
	}
}

func TestReadClaudeMalformed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{".claude-plugin/plugin.json": `{"name": `})
	m, err := ReadClaude(dir, "")
	if err == nil {
		t.Error("a malformed manifest is an error")
	}
	if m.Name != "" || m.MCP != nil {
		t.Errorf("the error carries the zero manifest: %+v", m)
	}
}

// TestClaudeMCPDropsInvalidEntries pins Claude's rule that a bad server entry
// is dropped, not fatal: its neighbors still load.
func TestClaudeMCPDropsInvalidEntries(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		".mcp.json": `{"good": {"command": "srv"}, "bad": "not an object"}`,
	})
	m, err := ReadClaude(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(m.MCP, func(s ClaudeMCPServer) bool { return s.Name == "good" }) {
		t.Errorf("the good entry was dropped: %+v", m.MCP)
	}
}

func TestReadClaudeMarketplace(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		".claude-plugin/marketplace.json": `{
			"name": "Demo", "owner": {"name": "o", "url": "https://o"},
			"$schema": "https://json.schemastore.org/claude-code-plugin-marketplace.json",
			"metadata": {"whatever": 1},
			"plugins": [
				{"name": "Rel", "source": "./p/rel", "version": "1.0.0", "author": {"name": "a"}, "category": "x", "strict": false},
				{"name": "Git", "source": {"source": "git", "url": "https://x/y.git", "ref": "main", "sha": "abc"}},
				{"name": "Hub", "source": {"source": "github", "repo": "me/hub"}},
				{"name": "Sub", "source": {"source": "git-subdir", "url": "https://x/y.git", "path": "p/sub"}},
				{"name": "Npm", "source": {"source": "npm", "package": "x"}},
				{"name": "Comp", "source": "./p/comp", "commands": "./cmds", "hooks": {"Stop": []}}
			]
		}`,
	})
	m, err := ReadClaudeMarketplace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Demo" || m.Owner.Name != "o" || m.Owner.URL != "https://o" {
		t.Errorf("scalars: %+v", m)
	}
	es := map[string]ClaudeMarketplaceEntry{}
	for _, e := range m.Plugins {
		es[e.Name] = e
	}
	if len(es) != 6 {
		t.Fatalf("entries: %v", m.Plugins)
	}
	if e := es["Rel"]; e.Source != (ClaudeSource{Kind: "relative", Dir: "./p/rel"}) ||
		e.Version != "1.0.0" || e.Author.Name != "a" || e.Strict {
		t.Errorf("Rel: %+v", e)
	}
	if e := es["Git"]; e.Source != (ClaudeSource{Kind: "git", URL: "https://x/y.git", Ref: "main", SHA: "abc"}) {
		t.Errorf("Git: %+v", e.Source)
	}
	if e := es["Hub"]; e.Source != (ClaudeSource{Kind: "github", URL: "https://github.com/me/hub"}) {
		t.Errorf("Hub: %+v", e.Source)
	}
	if e := es["Sub"]; e.Source != (ClaudeSource{Kind: "git-subdir", URL: "https://x/y.git", Dir: "p/sub"}) {
		t.Errorf("Sub: %+v", e.Source)
	}
	if e := es["Npm"]; e.Source.Kind != "npm" || e.Source.URL != "" {
		t.Errorf("Npm: %+v", e.Source)
	}
	// an unwritten strict defaults true; the component fields ride as written
	if e := es["Comp"]; !e.Strict || string(e.Commands) != `"./cmds"` || string(e.Hooks) != `{"Stop": []}` {
		t.Errorf("Comp: %+v", e)
	}
	if e := es["Rel"]; e.Strict {
		t.Errorf("Rel: strict %v, want false", e.Strict)
	}

	for _, c := range []struct{ name, json, want string }{
		{"a relative source needs the dot", `{"name": "m", "plugins": [{"name": "p", "source": "nope"}]}`, `must start with "./"`},
		{"a source names no kind", `{"name": "m", "plugins": [{"name": "p", "source": {"url": "https://x"}}]}`, "names no kind"},
		{"an entry without a name", `{"name": "m", "plugins": [{"source": "./p"}]}`, "without a name"},
		{"malformed", `{"name": `, "unexpected EOF"},
	} {
		dir := t.TempDir()
		write(t, dir, map[string]string{".claude-plugin/marketplace.json": c.json})
		if _, err := ReadClaudeMarketplace(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to hold %q", c.name, err, c.want)
		}
	}
	if _, err := ReadClaudeMarketplace(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing marketplace.json: %v", err)
	}
}
