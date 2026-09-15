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
	runUser(t, r, fmt.Sprintf(`zemi.lsp.add("fake", { command = %q, env = { ZEMI_LSP_FAKE = "1" }, filetypes = { "txt" } })`, os.Args[0]))
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
		{"diagnostics", fmt.Sprintf(`{"path":%q}`, filepath.Join(h.Dirs.ProjectDir, "b.zzz")), "no language server for .zzz"},
	} {
		if got := run(c.tool, c.input); !strings.Contains(got, c.want) {
			t.Fatalf("%s %s = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}
