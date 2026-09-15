// Package lsptest is a fake language server for tests: every opened or
// changed document gets one "broken" error per line containing BROKEN, and
// before publishing the server asks the client for its configuration.
package lsptest

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/metafates/zemi/internal/lsp"
)

// Serve answers one client until it exits or in closes.
func Serve(in io.Reader, out io.Writer) {
	r := bufio.NewReader(in)
	write := func(v any) {
		b, _ := json.Marshal(v)
		_ = lsp.WriteMessage(out, b)
	}
	// Document versions must arrive in order; once they do not, every later
	// publish carries the regression so the client's last read sees it.
	seen := map[string]int{}
	regressed := false
	for {
		body, err := lsp.ReadMessage(r)
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Text    string `json:"text"`
					Version int    `json:"version"`
				} `json:"textDocument"`
				ContentChanges []struct {
					Text string `json:"text"`
				} `json:"contentChanges"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		switch m.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"capabilities": map[string]any{"textDocumentSync": 1}}})
		case "shutdown":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		case "exit":
			return
		case "textDocument/didOpen", "textDocument/didChange":
			// A .quiet document is one the server has nothing to say about.
			if strings.HasSuffix(m.Params.TextDocument.URI, ".quiet") {
				continue
			}
			uri0 := m.Params.TextDocument.URI
			if v := m.Params.TextDocument.Version; v <= seen[uri0] {
				regressed = true
			} else {
				seen[uri0] = v
			}
			text := m.Params.TextDocument.Text
			if n := len(m.Params.ContentChanges); n > 0 {
				text = m.Params.ContentChanges[n-1].Text
			}
			write(map[string]any{"jsonrpc": "2.0", "id": "cfg", "method": "workspace/configuration", "params": map[string]any{"items": []any{map[string]any{}}}})
			diags := []any{}
			for i, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "BROKEN") {
					diags = append(diags, map[string]any{
						"range":    map[string]any{"start": map[string]any{"line": i, "character": 0}, "end": map[string]any{"line": i, "character": 6}},
						"severity": 1, "message": "broken", "source": "fake",
					})
				}
			}
			if regressed {
				diags = append(diags, map[string]any{
					"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}},
					"severity": 1, "message": "version regression", "source": "fake",
				})
			}
			// Publish the symlink-resolved path, the way servers that
			// canonicalize their URIs do.
			uri := m.Params.TextDocument.URI
			if p, err := filepath.EvalSymlinks(strings.TrimPrefix(uri, "file://")); err == nil {
				uri = "file://" + p
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{"uri": uri, "diagnostics": diags}})
		}
	}
}
