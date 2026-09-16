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
