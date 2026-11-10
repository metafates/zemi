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

func TestReadClaudeMarketplace(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		".claude-plugin/marketplace.json": `{
			"name": "Demo", "owner": {"name": "o", "url": "https://o"},
			"$schema": "https://json.schemastore.org/claude-code-plugin-marketplace.json",
			"metadata": {"whatever": 1},
			"renames": {"old": "Hub"},
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
	m, err := ReadMarketplace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindClaude || m.Name != "Demo" {
		t.Errorf("scalars: %+v", m)
	}
	es := map[string]MarketplacePlugin{}
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
	// an old name the manifest renamed picks its entry; a live name wins
	if e, ok := m.find("old"); !ok || e.Name != "Hub" {
		t.Errorf("find(old): %+v, %v", e, ok)
	}
	if _, ok := m.find("Rel"); !ok {
		t.Error("find(Rel) went through the renames")
	}

	for _, c := range []struct{ name, json, want string }{
		{"a relative source needs the dot", `{"name": "m", "plugins": [{"name": "p", "source": "nope"}]}`, `must start with "./"`},
		{"a relative source stays inside the clone", `{"name": "m", "plugins": [{"name": "p", "source": "./../../secret"}]}`, "stay inside the marketplace"},
		{"a git-subdir path stays inside the clone", `{"name": "m", "plugins": [{"name": "p", "source": {"source": "git-subdir", "url": "https://x/y.git", "path": "../../etc"}}]}`, "stay inside the marketplace"},
		{"a git-subdir path names no path", `{"name": "m", "plugins": [{"name": "p", "source": {"source": "git-subdir", "url": "https://x/y.git"}}]}`, "stay inside the marketplace"},
		{"a source names no kind", `{"name": "m", "plugins": [{"name": "p", "source": {"url": "https://x"}}]}`, "names no kind"},
		{"an entry without a name", `{"name": "m", "plugins": [{"source": "./p"}]}`, "without a name"},
		{"malformed", `{"name": `, "unexpected EOF"},
	} {
		dir := t.TempDir()
		write(t, dir, map[string]string{".claude-plugin/marketplace.json": c.json})
		if _, err := ReadMarketplace(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to hold %q", c.name, err, c.want)
		}
	}
	// the clone root is a source: it installs as a git-kind clone, not a copy
	root := t.TempDir()
	write(t, root, map[string]string{".claude-plugin/marketplace.json": `{"name": "m", "plugins": [{"name": "p", "source": "./"}]}`})
	if m, err := ReadMarketplace(root); err != nil || m.Plugins[0].Source != (ClaudeSource{Kind: SourceRelative, Dir: "./"}) {
		t.Errorf("the clone root as a source: %v, %+v", err, m.Plugins[0].Source)
	}
	if _, err := ReadMarketplace(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing marketplace.json: %v", err)
	}
}

// TestReadMarketplace pins the zemi reader: the zemi manifest wins when a
// repo holds both, and its names and uses are validated at parse.
func TestReadMarketplace(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		".zemi/marketplace.json": `{
			"name": "zed", "owner": {"name": "o"},
			"$schema": "https://example.com/schema.json",
			"metadata": {"whatever": 1},
			"plugins": [
				{"name": "zed-one", "source": "./p/one", "version": "1.0.0", "description": "the one", "uses": ["http", "job"]}
			]
		}`,
		".claude-plugin/marketplace.json": `{"name": "Other Name", "plugins": [{"name": "other-one", "source": "./p/one"}]}`,
	})
	m, err := ReadMarketplace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindZemi || m.Name != "zed" || len(m.Plugins) != 1 {
		t.Fatalf("the zemi manifest did not win: %+v, %v", m, err)
	}
	e := m.Plugins[0]
	if e.Kind != KindZemi || e.Name != "zed-one" || e.Version != "1.0.0" || e.Description != "the one" ||
		e.Source != (ClaudeSource{Kind: "relative", Dir: "./p/one"}) || !slices.Equal(e.Uses, []string{"http", "job"}) {
		t.Errorf("entry: %+v", e)
	}

	dir = t.TempDir()
	write(t, dir, map[string]string{".claude-plugin/marketplace.json": `{"name": "Only Claude", "plugins": []}`})
	if m, err := ReadMarketplace(dir); err != nil || m.Kind != KindClaude || m.Name != "Only Claude" {
		t.Errorf("claude only: %+v, %v", m, err)
	}

	for _, c := range []struct{ name, json, want string }{
		{"a manifest name outside zemi's rule", `{"name": "Zed", "plugins": []}`, "not a marketplace name"},
		{"an entry name outside zemi's rule", `{"name": "zed", "plugins": [{"name": "zed.one", "source": "./p"}]}`, "not a plugin name"},
		{"uses outside the vocabulary", `{"name": "zed", "plugins": [{"name": "zed-one", "source": "./p", "uses": ["fs.read"]}]}`, "not a capability"},
		{"malformed", `{"name": `, "unexpected EOF"},
	} {
		dir := t.TempDir()
		write(t, dir, map[string]string{".zemi/marketplace.json": c.json})
		if _, err := ReadMarketplace(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to hold %q", c.name, err, c.want)
		}
	}
	if _, err := ReadMarketplace(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("neither manifest: %v", err)
	}
}

func TestClaudeProvides(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{name: "nothing", want: nil},
		{
			name: "the conventional dirs and files",
			files: map[string]string{
				".claude-plugin/plugin.json": `{"name": "Demo"}`,
				"skills/a/SKILL.md":          "a",
				"skills/b/SKILL.md":          "b",
				"agents/one.md":              "one",
				"commands/x.md":              "x",
				"commands/nested/y.md":       "y",
				"hooks/hooks.json":           `{"hooks": {"PostToolUse": [], "Stop": []}}`,
				".mcp.json":                  `{"mcpServers": {"acme": {"command": "srv"}}}`,
			},
			want: []string{"2 skills", "1 agent", "2 commands", "mcp: acme", "hooks: PostToolUse,Stop"},
		},
		{
			name: "declared paths replace the conventional dir",
			files: map[string]string{
				".claude-plugin/plugin.json": `{"name": "Demo", "skills": "./more-skills", "agents": ["./agents/a.md"]}`,
				"skills/hidden/SKILL.md":     "not counted",
				"more-skills/SKILL.md":       "counted",
				"agents/a.md":                "a",
			},
			want: []string{"1 skill", "1 agent"},
		},
		{
			name: "workflows count beside the rest",
			files: map[string]string{
				".claude-plugin/plugin.json": `{"name": "Demo"}`,
				".zemi/workflows/a.lua":      "a",
				".zemi/workflows/deep/b.lua": "b",
				".zemi/workflows/c.txt":      "not a workflow",
				"workflows/old.lua":          "the layout load refuses",
			},
			want: []string{"2 workflows"},
		},
		{
			name: "hook events zemi refuses leave no hooks line",
			files: map[string]string{
				".claude-plugin/plugin.json": `{"name": "Demo"}`,
				"hooks/hooks.json":           `{"hooks": {"PreToolUse": [], "PermissionRequest": []}}`,
			},
			want: nil,
		},
		{
			name: "supported hook events survive beside refused ones, deduped",
			files: map[string]string{
				".claude-plugin/plugin.json": `{"name": "Demo", "hooks": "./more-hooks.json"}`,
				"hooks/hooks.json":           `{"hooks": {"PreToolUse": [], "SessionStart": []}}`,
				"more-hooks.json":            `{"hooks": {"SessionStart": []}}`,
			},
			want: []string{"hooks: SessionStart"},
		},
		{
			name: "a malformed manifest provides nothing",
			files: map[string]string{
				".claude-plugin/plugin.json": `{"name": `,
				"skills/a/SKILL.md":          "a",
			},
			want: nil,
		},
	} {
		dir := t.TempDir()
		write(t, dir, c.files)
		if got := claudeProvides(dir, readHead(filepath.Join(dir, ClaudeManifestFile), _headMax)); !slices.Equal(got, c.want) {
			t.Errorf("%s: provides %v, want %v", c.name, got, c.want)
		}
	}
}

// ReadClaude reads the Claude manifest of the plugin in dir; dataDir is what
// ${CLAUDE_PLUGIN_DATA} stands for. A missing manifest is the zero manifest,
// its components still read; a malformed one is an error carrying the zero
// manifest, for the caller to log and load nothing from.
func ReadClaude(dir, dataDir string) (ClaudeManifest, error) {
	return readClaude(dir, dataDir, readHead(filepath.Join(dir, ClaudeManifestFile), _headMax))
}
