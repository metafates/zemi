// Package lsptest is a fake language server for tests: every opened or
// changed document gets one "broken" error per line containing BROKEN,
// every known document is republished on each change (as servers do for a
// package), and before publishing the server asks the client for its
// configuration. Like rust-analyzer, it reports itself loading through
// experimental/serverStatus until a document changes.
package lsptest

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/lsp"
)

var wordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// canon is the URI a document lives under: its resolved path, the way
// servers that canonicalize their URIs key them; the client may open a file
// by a spelling a watched-file event spells differently. A path that is
// gone resolves through its directory.
func canon(uri string) string {
	p, ok := strings.CutPrefix(uri, "file://")
	if !ok {
		return uri
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return "file://" + r
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return "file://" + filepath.Join(dir, filepath.Base(p))
	}
	return uri
}

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

// hit is one place a word occurs: the document, the line, and its span in it.
type hit struct {
	uri            string
	line, from, to int
}

// object is the hit as JSON-RPC spells a location: a document and a range.
func (h hit) object() map[string]any { return map[string]any{"uri": h.uri, "range": h.span()} }

// span is the hit's range within its line.
func (h hit) span() map[string]any {
	return map[string]any{
		"start": map[string]any{"line": h.line, "character": h.from},
		"end":   map[string]any{"line": h.line, "character": h.to},
	}
}

// Serve answers one client until it exits or in closes.
func Serve(in io.Reader, out io.Writer) {
	r := bufio.NewReader(in)
	write := func(v any) {
		b, _ := json.Marshal(v, json.Deterministic(true))
		_ = lsp.WriteMessage(out, b)
	}
	// Document versions must arrive in order; once they do not, every later
	// publish carries the regression so the client's last read sees it.
	seen := map[string]int{}
	texts := map[string]string{}
	regressed, hung, loading := false, false, false
	saves, events := 0, 0
	for {
		body, err := lsp.ReadMessage(r)
		if err != nil {
			return
		}
		var m struct {
			ID     jsontext.Value `json:"id"`
			Method string         `json:"method"`
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
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"position"`
				NewName      string `json:"newName"`
				Query        string `json:"query"`
				Capabilities struct {
					Experimental struct {
						ServerStatusNotification bool `json:"serverStatusNotification"`
					} `json:"experimental"`
				} `json:"capabilities"`
				Changes []struct {
					URI  string `json:"uri"`
					Type int    `json:"type"`
				} `json:"changes"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &m) != nil || hung {
			continue
		}
		m.Params.TextDocument.URI = canon(m.Params.TextDocument.URI)
		for i := range m.Params.Changes {
			m.Params.Changes[i].URI = canon(m.Params.Changes[i].URI)
		}
		switch m.Method {
		default:
			if m.Method != "" && len(m.ID) > 0 {
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			}
		case "initialize":
			if loading = m.Params.Capabilities.Experimental.ServerStatusNotification; loading {
				write(map[string]any{"jsonrpc": "2.0", "method": "experimental/serverStatus", "params": map[string]any{"health": "ok", "quiescent": false}})
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"capabilities": map[string]any{"textDocumentSync": 1}}})
		case "initialized":
			// the way gopls does it: ask the client to report .txt and .lua changes
			write(map[string]any{"jsonrpc": "2.0", "id": "reg", "method": "client/registerCapability", "params": map[string]any{"registrations": []any{
				map[string]any{"id": "w1", "method": "workspace/didChangeWatchedFiles", "registerOptions": map[string]any{"watchers": []any{
					map[string]any{"globPattern": "**/*.txt"}, map[string]any{"globPattern": "**/*.lua"},
				}}},
			}}})
		case "workspace/didChangeWatchedFiles":
			events += len(m.Params.Changes)
			// a file nobody opened is read from disk only when told it changed
			for _, ch := range m.Params.Changes {
				p := strings.TrimPrefix(ch.URI, "file://")
				if ch.Type == 3 {
					delete(texts, ch.URI)
				} else if _, open := seen[ch.URI]; !open {
					if b, err := os.ReadFile(p); err == nil {
						texts[ch.URI] = string(b)
					}
				}
			}
		case "textDocument/didClose":
			delete(texts, m.Params.TextDocument.URI)
			delete(seen, m.Params.TextDocument.URI)
		case "textDocument/definition", "textDocument/implementation", "textDocument/references", "textDocument/rename":
			uri := m.Params.TextDocument.URI
			text := texts[uri]
			word := wordAt(text, m.Params.Position.Line, m.Params.Position.Character)
			hits := func(uri, text string) []hit {
				var out []hit
				for i, line := range strings.Split(text, "\n") {
					for _, m := range wordRe.FindAllStringIndex(line, -1) {
						if line[m[0]:m[1]] == word {
							out = append(out, hit{uri: uri, line: i, from: m[0], to: m[1]})
						}
					}
				}
				return out
			}
			var result any
			switch m.Method {
			case "textDocument/definition", "textDocument/implementation":
				// the definition is the first "func WORD" line, if any
				lines := strings.Split(text, "\n")
				for _, h := range hits(uri, text) {
					if strings.HasPrefix(lines[h.line], "func "+word) {
						result = []any{h.object()}
					}
				}
			case "textDocument/references":
				var all []map[string]any
				for _, u := range slices.Sorted(maps.Keys(texts)) {
					for _, h := range hits(u, texts[u]) {
						all = append(all, h.object())
					}
				}
				result = all
			case "textDocument/rename":
				changes := map[string]any{}
				for u, t := range texts {
					var edits []any
					for _, h := range hits(u, t) {
						edits = append(edits, map[string]any{"range": h.span(), "newText": m.Params.NewName})
					}
					if len(edits) > 0 {
						changes[u] = edits
					}
				}
				result = map[string]any{"changes": changes}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		case "workspace/symbol":
			var syms []any
			for _, u := range slices.Sorted(maps.Keys(texts)) {
				for i, line := range strings.Split(texts[u], "\n") {
					name, ok := strings.CutPrefix(line, "func ")
					if name, _, _ = strings.Cut(name, "("); !ok || !strings.Contains(name, m.Params.Query) {
						continue
					}
					syms = append(syms, map[string]any{"name": name, "kind": 12, "location": map[string]any{"uri": u, "range": map[string]any{
						"start": map[string]any{"line": i, "character": 0}, "end": map[string]any{"line": i, "character": len(line)},
					}}})
				}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": syms})
		case "textDocument/documentSymbol":
			var syms []any
			for i, line := range strings.Split(texts[m.Params.TextDocument.URI], "\n") {
				if name, ok := strings.CutPrefix(line, "func "); ok {
					name, _, _ = strings.Cut(name, "(")
					syms = append(syms, map[string]any{"name": name, "kind": 12, "range": map[string]any{
						"start": map[string]any{"line": i, "character": 0}, "end": map[string]any{"line": i + 1, "character": 0},
					}})
				}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": syms})
		case "shutdown":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		case "exit":
			return
		case "textDocument/didSave":
			saves++
		case "fake/saves":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": saves})
		case "fake/events":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": events})
		case "fake/keys":
			write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": slices.Sorted(maps.Keys(texts))})
		case "textDocument/didOpen", "textDocument/didChange":
			if loading && m.Method == "textDocument/didChange" {
				loading = false
				write(map[string]any{"jsonrpc": "2.0", "method": "experimental/serverStatus", "params": map[string]any{"health": "ok", "quiescent": true}})
			}
			// A .hang document wedges the server: it reads on and answers nothing.
			if strings.HasSuffix(m.Params.TextDocument.URI, ".hang") {
				hung = true
				continue
			}
			// A .quiet document is one the server has nothing to say about.
			if strings.HasSuffix(m.Params.TextDocument.URI, ".quiet") {
				continue
			}
			uri0 := m.Params.TextDocument.URI
			// Like gopls, a change to a document never opened is dropped.
			if _, open := seen[uri0]; !open && m.Method == "textDocument/didChange" {
				continue
			}
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
				write(map[string]any{
					"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
					"params": map[string]any{"uri": uri, "version": version, "diagnostics": diags},
				})
			}
		}
	}
}
