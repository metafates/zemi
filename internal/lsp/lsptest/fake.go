// Package lsptest is a fake language server for tests: every opened or
// changed document gets one "broken" error per line containing BROKEN,
// every known document is republished on each change (as servers do for a
// package), and before publishing the server asks the client for its
// configuration.
package lsptest

import (
	"bufio"
	"encoding/json"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/lsp"
)

var wordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// wordAt is the identifier covering character on line, "" when none.
func wordAt(text string, line, char int) string {
	lines := strings.Split(text, "\n")
	if line >= len(lines) {
		return ""
	}
	for _, m := range wordRe.FindAllStringIndex(lines[line], -1) {
		if m[0] <= char && char <= m[1] {
			return lines[line][m[0]:m[1]]
		}
	}
	return ""
}

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
	texts := map[string]string{}
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
				Position struct {
					Line, Character int
				} `json:"position"`
				NewName string `json:"newName"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		switch m.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"capabilities": map[string]any{"textDocumentSync": 1}}})
		case "textDocument/definition", "textDocument/references", "textDocument/rename":
			uri := m.Params.TextDocument.URI
			text := texts[uri]
			word := wordAt(text, m.Params.Position.Line, m.Params.Position.Character)
			hits := func(uri, text string) []map[string]any {
				var out []map[string]any
				for i, line := range strings.Split(text, "\n") {
					for _, m := range wordRe.FindAllStringIndex(line, -1) {
						if line[m[0]:m[1]] == word {
							out = append(out, map[string]any{"uri": uri, "range": map[string]any{
								"start": map[string]any{"line": i, "character": m[0]}, "end": map[string]any{"line": i, "character": m[1]}}})
						}
					}
				}
				return out
			}
			var result any
			switch m.Method {
			case "textDocument/definition":
				// the definition is the first "func WORD" line, if any
				for _, h := range hits(uri, text) {
					l := h["range"].(map[string]any)["start"].(map[string]any)["line"].(int)
					if strings.HasPrefix(strings.Split(text, "\n")[l], "func "+word) {
						result = []any{h}
					}
				}
			case "textDocument/references":
				var all []map[string]any
				for _, u := range slices.Sorted(maps.Keys(texts)) {
					all = append(all, hits(u, texts[u])...)
				}
				result = all
			case "textDocument/rename":
				changes := map[string]any{}
				for u, t := range texts {
					var edits []any
					for _, h := range hits(u, t) {
						edits = append(edits, map[string]any{"range": h["range"], "newText": m.Params.NewName})
					}
					if len(edits) > 0 {
						changes[u] = edits
					}
				}
				result = map[string]any{"changes": changes}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		case "textDocument/documentSymbol":
			var syms []any
			for i, line := range strings.Split(texts[m.Params.TextDocument.URI], "\n") {
				if name, ok := strings.CutPrefix(line, "func "); ok {
					name, _, _ = strings.Cut(name, "(")
					syms = append(syms, map[string]any{"name": name, "kind": 12, "range": map[string]any{
						"start": map[string]any{"line": i, "character": 0}, "end": map[string]any{"line": i + 1, "character": 0}}})
				}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": syms})
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
			texts[uri0] = text
			write(map[string]any{"jsonrpc": "2.0", "id": "cfg", "method": "workspace/configuration", "params": map[string]any{"items": []any{map[string]any{}}}})
			for uri, text := range texts {
				version := seen[uri]
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
				if p, err := filepath.EvalSymlinks(strings.TrimPrefix(uri, "file://")); err == nil {
					uri = "file://" + p
				}
				write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
					"params": map[string]any{"uri": uri, "version": version, "diagnostics": diags}})
			}
		}
	}
}
