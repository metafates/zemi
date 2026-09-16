package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestLSPDiagnostics: a server added from Lua reports on files the write
// tool changes, and the diagnostics tool reads them back.
func TestLSPDiagnostics(t *testing.T) {
	h, r := newTestHostDB(t)
	defer h.lsp.stopAll()
	runUser(t, r, fmt.Sprintf(`zemi.lsp.add("fake", { command = %q, env = { ZEMI_LSP_FAKE = "1" }, filetypes = { "txt", "quiet" } })`, os.Args[0]))
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	byName := map[string]fantasy.AgentTool{}
	for _, tl := range tools.All(h.PipelineFor()) {
		byName[tl.Info().Name] = tl
	}
	path := filepath.Join(h.Dirs.ProjectDir, "a.txt")
	run := func(tool, input string) string {
		t.Helper()
		resp, err := byName[tool].Run(context.Background(), fantasy.ToolCall{ID: "1", Name: tool, Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Content
	}
	for _, c := range []struct{ tool, input, want string }{
		{"write", fmt.Sprintf(`{"path":%q,"content":"ok\nBROKEN"}`, path), "error a.txt:2:1: broken"},
		{"diagnostics", fmt.Sprintf(`{"path":%q}`, path), `<diagnostics source="fake">`},
		{"write", fmt.Sprintf(`{"path":%q,"content":"fixed"}`, path), "fake reports no problems in a.txt"},
		{"write", fmt.Sprintf(`{"path":%q,"content":"BROKEN"}`, filepath.Join(h.Dirs.ProjectDir, "b.txt")), "error b.txt:1:1"},
		{"write", fmt.Sprintf(`{"path":%q,"content":"still fine"}`, path), "errors in other files (a caller this change broke, or already broken):\nerror b.txt:1:1: broken"},
		{"diagnostics", fmt.Sprintf(`{"path":%q}`, filepath.Join(h.Dirs.ProjectDir, "b.zzz")), "no language server for .zzz"},
	} {
		if got := run(c.tool, c.input); !strings.Contains(got, c.want) {
			t.Fatalf("%s %s = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
	first := h.lsp.running["fake"]
	for i := range lspMaxStalls {
		run("write", fmt.Sprintf(`{"path":%q,"content":"nothing"}`, filepath.Join(h.Dirs.ProjectDir, fmt.Sprintf("q%d.quiet", i))))
	}
	if h.lsp.running["fake"] != nil || first.stalls != lspMaxStalls {
		t.Fatalf("a silent server must be dropped for a fresh start: %v", h.lspRows())
	}
	if got := run("write", fmt.Sprintf(`{"path":%q,"content":"BROKEN"}`, path)); !strings.Contains(got, "error a.txt:1:1") {
		t.Fatalf("restarted server = %q", got)
	}
}

// TestLSPTool: symbols, definition, references and rename go through the
// file's server by symbol name; rename writes every file the server names.
func TestLSPTool(t *testing.T) {
	h, r := newTestHostDB(t)
	defer h.lsp.stopAll()
	runUser(t, r, fmt.Sprintf(`zemi.lsp.add("fake", { command = %q, env = { ZEMI_LSP_FAKE = "1" }, filetypes = { "txt" } })`, os.Args[0]))
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	var tool fantasy.AgentTool
	for _, tl := range tools.All(h.PipelineFor()) {
		if tl.Info().Name == "lsp" {
			tool = tl
		}
	}
	a, b := filepath.Join(h.Dirs.ProjectDir, "a.txt"), filepath.Join(h.Dirs.ProjectDir, "b.txt")
	os.WriteFile(a, []byte("func Foo()\nFoo()\nfunc Bar()\n"), 0o644)
	os.WriteFile(b, []byte("x := Foo()\n"), 0o644)
	run := func(input string) string {
		t.Helper()
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "lsp", Input: input})
		if err != nil {
			t.Fatal(err)
		}
		// paths come back relative to the process working directory, which
		// is not the temp project here
		return strings.ReplaceAll(resp.Content, h.Dirs.ProjectDir+string(filepath.Separator), "")
	}
	for _, c := range []struct{ input, want string }{
		{fmt.Sprintf(`{"action":"symbols","path":%q}`, a), "function Foo  lines 1-2\nfunction Bar  lines 3-4"},
		{fmt.Sprintf(`{"action":"definition","path":%q,"symbol":"Foo","line":2}`, a), "a.txt:1:6: func Foo()"},
		{fmt.Sprintf(`{"action":"definition","path":%q,"symbol":"Foo"}`, a), "also appears on lines 2: pass line"},
		{fmt.Sprintf(`{"action":"references","path":%q,"symbol":"Foo"}`, b), "3 references\na.txt:1:6: func Foo()\na.txt:2:1: Foo()\nb.txt:1:6: x := Foo()"},
		{fmt.Sprintf(`{"action":"references","path":%q,"symbol":"Nope"}`, b), "does not appear"},
		{fmt.Sprintf(`{"action":"rename","path":%q,"symbol":"Foo","new_name":"Baz"}`, b), "renamed Foo to Baz in 2 files, 3 places"},
	} {
		if got := run(c.input); !strings.Contains(got, c.want) {
			t.Fatalf("%s = %q, want %q", c.input, got, c.want)
		}
	}
	if got, _ := os.ReadFile(a); string(got) != "func Baz()\nBaz()\nfunc Bar()\n" {
		t.Fatalf("a.txt after rename: %q", got)
	}
	if got, _ := os.ReadFile(b); string(got) != "x := Baz()\n" {
		t.Fatalf("b.txt after rename: %q", got)
	}
}

// TestLSPConfig: user config replaces a builtin by name with its own args
// and settings, disables one, and list shows what the project can use.
func TestLSPConfig(t *testing.T) {
	h, r := newTestHostDB(t)
	defer h.lsp.stopAll()
	runUser(t, r, `
		zemi.lsp.add("gopls", { command = "gopls-wrapper", filetypes = { "go" }, settings = { gopls = { staticcheck = true } }, init_options = { x = 1 } })
		zemi.lsp.add("zls", { command = "zls", filetypes = { "zig" } })
		zemi.lsp.disable("zls")
	`)
	defs := h.lspDefs()
	if len(defs) != 1 || defs[0].Name != "gopls" || defs[0].Command != "gopls-wrapper" {
		t.Fatalf("defs = %+v", defs)
	}
	if cfg, _ := defs[0].Config.(map[string]any); cfg["gopls"].(map[string]any)["staticcheck"] != true || defs[0].Init.(map[string]any)["x"] != float64(1) {
		t.Fatalf("settings = %+v init = %+v", defs[0].Config, defs[0].Init)
	}
	if d, ok := h.lspFor("main.go"); !ok || d.Command != "gopls-wrapper" {
		t.Fatalf("lspFor(main.go) = %+v %v", d, ok)
	}
	runUser(t, r, `
		local rows = zemi.lsp.list()
		assert(#rows == 1 and rows[1].name == "gopls" and rows[1].status == "available" and rows[1].filetypes == ".go", "list: " .. tostring(rows[1] and rows[1].status))
	`)
}
