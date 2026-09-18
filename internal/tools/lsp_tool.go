package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lsp"
	"github.com/metafates/zemi/internal/perm"
)

const lspToolDesc = `Ask the file's language server about code, by symbol name instead of grep: it understands scopes, so it skips comments, strings and same-named identifiers elsewhere.
- definition: where the symbol under that name in this file is defined (path:line:col and the line).
- references: every use of it across the project, grouped by file.
- rename: rename it everywhere with a true semantic rename (imports and other files included); prefer this over editing each occurrence. Returns the changed files.
- symbols: the file's outline (functions, types, methods with line ranges); cheaper than reading a large file to find its shape.
symbol is the identifier as written in path; when it appears on several lines, line picks the occurrence (otherwise the first is used). Works when a server is installed for the language (gopls, pyright, typescript-language-server, clangd, rust-analyzer or one from config) in a trusted project.`

var lspToolProps = map[string]any{
	"action":   map[string]any{"type": "string", "enum": []string{"definition", "references", "rename", "symbols"}},
	"path":     map[string]any{"type": "string", "description": "file the symbol appears in (or to outline)"},
	"symbol":   map[string]any{"type": "string", "description": "identifier as written in the file; not for symbols"},
	"line":     map[string]any{"type": "number", "description": "1-based line of the occurrence to use when the name appears more than once"},
	"new_name": map[string]any{"type": "string", "description": "rename only"},
}

func lspRequests(in map[string]any) []Request {
	if str(in["action"]) == "rename" {
		return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "rename " + str(in["symbol"]) + " to " + str(in["new_name"])}}
	}
	return []Request{{Subject: "fs.read", Arg: str(in["path"]), Reason: "lsp " + str(in["action"])}}
}

func runLSP(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p == nil || p.LSP == nil {
		return fantasy.NewTextErrorResponse("no language server support"), nil
	}
	action, path, symbol := str(input["action"]), str(input["path"]), str(input["symbol"])
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	s, err := p.LSP(ctx, path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	c, server := s.Client, s.Server
	doc := map[string]any{"uri": c.URI(path)}
	fail := func(err error) (fantasy.ToolResponse, error) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %s %v", server, action, err)), nil
	}
	if action == "symbols" {
		var raw json.RawMessage
		if err := c.Call(ctx, "textDocument/documentSymbol", map[string]any{"textDocument": doc}, &raw); err != nil {
			return fail(err)
		}
		out := formatSymbols(raw)
		if out == "" {
			out = "no symbols"
		}
		return fantasy.NewTextResponse(out), nil
	}
	if symbol == "" {
		return fantasy.NewTextErrorResponse("symbol is required"), nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	line, _ := input["line"].(float64)
	off, note, err := findSymbol(string(text), symbol, int(line))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	params := map[string]any{"textDocument": doc, "position": lsp.Pos(string(text), off)}
	switch action {
	case "definition":
		var raw json.RawMessage
		if err := c.Call(ctx, "textDocument/definition", params, &raw); err != nil {
			return fail(err)
		}
		locs := decodeLocations(raw)
		if len(locs) == 0 {
			return fantasy.NewTextResponse("no definition found for " + symbol + note), nil
		}
		return fantasy.NewTextResponse(formatLocations(locs, 20) + note), nil
	case "references":
		params["context"] = map[string]any{"includeDeclaration": true}
		var raw json.RawMessage
		if err := c.Call(ctx, "textDocument/references", params, &raw); err != nil {
			return fail(err)
		}
		locs := decodeLocations(raw)
		if len(locs) == 0 {
			return fantasy.NewTextResponse("no references to " + symbol + note), nil
		}
		return fantasy.NewTextResponse(fmt.Sprintf("%d references\n%s%s", len(locs), formatLocations(locs, 100), note)), nil
	case "rename":
		newName := str(input["new_name"])
		if newName == "" {
			return fantasy.NewTextErrorResponse("new_name is required"), nil
		}
		params["newName"] = newName
		var edit struct {
			Changes         map[string][]lsp.TextEdit `json:"changes"`
			DocumentChanges []struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version *int   `json:"version"`
				} `json:"textDocument"`
				Edits []lsp.TextEdit `json:"edits"`
				Kind  string         `json:"kind"`
			} `json:"documentChanges"`
		}
		if err := c.Call(ctx, "textDocument/rename", params, &edit); err != nil {
			return fail(err)
		}
		byPath := map[string][]lsp.TextEdit{}
		for uri, es := range edit.Changes {
			byPath[lsp.Location{URI: uri}.Path()] = append(byPath[lsp.Location{URI: uri}.Path()], es...)
		}
		for _, dc := range edit.DocumentChanges {
			if dc.Kind != "" {
				return fantasy.NewTextErrorResponse("the rename would " + dc.Kind + " a file; do that part by hand"), nil
			}
			f := lsp.Location{URI: dc.TextDocument.URI}.Path()
			// the server planned these edits against the text it holds at
			// that version; a newer one on our side means the file moved
			// under it (0 is what some servers send for a file nobody opened)
			if v := dc.TextDocument.Version; v != nil && *v != 0 && c.Version(f) != *v {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s changed while %s planned the rename (version %d, now %d); nothing was changed, retry", workRel(f), server, *v, c.Version(f))), nil
			}
			byPath[f] = append(byPath[f], dc.Edits...)
		}
		if len(byPath) == 0 {
			return fantasy.NewTextResponse("nothing to rename" + note), nil
		}
		// every other file passes the same gate the named one did
		for _, f := range slices.Sorted(maps.Keys(byPath)) {
			if absPath(f) == absPath(path) {
				continue
			}
			if v := p.Check(Request{Tool: "lsp", Subject: "fs.write", Arg: f, Reason: "rename " + symbol + " to " + newName}); v.Decision != perm.Allow {
				return fantasy.NewTextErrorResponse(DeniedText(v, "rename touches "+f+"; nothing was changed")), nil
			}
		}
		var b strings.Builder
		total := 0
		for _, f := range slices.Sorted(maps.Keys(byPath)) {
			n, err := applyTextEdits(p, f, byPath[f])
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %v (files before it were changed)", f, err)), nil
			}
			total += n
			fmt.Fprintf(&b, "%s (%d)\n", f, n)
			// the server must see the new text, or the next query (a rename
			// back, references) answers from the old one
			if err := s.Open(f); err != nil {
				fmt.Fprintf(&b, "  (not re-sent to %s: %v)\n", server, err)
			}
		}
		return fantasy.NewTextResponse(fmt.Sprintf("renamed %s to %s in %d file%s, %d place%s:\n%s", symbol, newName, len(byPath), plural(len(byPath)), total, plural(total), strings.TrimRight(b.String(), "\n")) + p.afterWrite(path)), nil
	}
	return fantasy.NewTextErrorResponse("unknown action " + action), nil
}

// findSymbol returns the byte offset of name in text as a whole word: the
// occurrence on line when given, else the first outside a comment line (a
// server finds no identifier in a comment), with a note when there were
// others to choose from.
func findSymbol(text, name string, line int) (off int, note string, err error) {
	re := regexp.MustCompile(`(^|[^\pL\pN_])(` + regexp.QuoteMeta(name) + `)($|[^\pL\pN_])`)
	var offs []int
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		offs = append(offs, m[4])
	}
	if len(offs) == 0 {
		return 0, "", errors.New(name + " does not appear in the file as a whole word")
	}
	if line > 0 {
		for _, o := range offs {
			if lsp.Pos(text, o).Line+1 == line {
				return o, "", nil
			}
		}
		return 0, "", fmt.Errorf("%s is not on line %d", name, line)
	}
	if i := slices.IndexFunc(offs, func(o int) bool { return !commentLine(text, o) }); i > 0 {
		offs = append(offs[i:], offs[:i]...)
	}
	if len(offs) > 1 {
		var lines []string
		for _, o := range offs[:min(len(offs), 8)] {
			lines = append(lines, fmt.Sprint(lsp.Pos(text, o).Line+1))
		}
		note = fmt.Sprintf("\n(used the occurrence on line %s; it also appears on lines %s: pass line to pick one)", lines[0], strings.Join(lines[1:], ", "))
	}
	return offs[0], note, nil
}

// commentLine reports whether the line holding off starts like a comment.
func commentLine(text string, off int) bool {
	start := strings.LastIndexByte(text[:off], '\n') + 1
	l := strings.TrimSpace(text[start:off])
	for _, m := range []string{"//", "#", "*", "--", "/*", ";"} {
		if strings.HasPrefix(l, m) {
			return true
		}
	}
	return false
}

func decodeLocations(raw json.RawMessage) []lsp.Location {
	var one lsp.Location
	if json.Unmarshal(raw, &one) == nil && one.URI != "" {
		return []lsp.Location{one}
	}
	var many []lsp.Location
	if json.Unmarshal(raw, &many) == nil && len(many) > 0 && many[0].URI != "" {
		return many
	}
	// LocationLink
	var links []struct {
		URI   string    `json:"targetUri"`
		Range lsp.Range `json:"targetSelectionRange"`
	}
	_ = json.Unmarshal(raw, &links)
	out := make([]lsp.Location, 0, len(links))
	for _, l := range links {
		out = append(out, lsp.Location{URI: l.URI, Range: l.Range})
	}
	return out
}

// formatLocations renders path:line:col with the source line, grouped by
// file in path order, at most limit rows.
func formatLocations(locs []lsp.Location, limit int) string {
	sort.SliceStable(locs, func(i, j int) bool {
		if pi, pj := locs[i].Path(), locs[j].Path(); pi != pj {
			return pi < pj
		}
		return locs[i].Range.Start.Line < locs[j].Range.Start.Line
	})
	files := map[string][]string{}
	var b strings.Builder
	n := 0
	for _, l := range locs {
		if n == limit {
			fmt.Fprintf(&b, "... %d more\n", len(locs)-n)
			break
		}
		n++
		path := l.Path()
		lines, ok := files[path]
		if !ok {
			data, _ := os.ReadFile(path)
			lines = strings.Split(string(data), "\n")
			files[path] = lines
		}
		src := ""
		if l.Range.Start.Line < len(lines) {
			src = strings.TrimSpace(lines[l.Range.Start.Line])
		}
		fmt.Fprintf(&b, "%s:%d:%d: %s\n", workRel(path), l.Range.Start.Line+1, l.Range.Start.Character+1, src)
	}
	return strings.TrimRight(b.String(), "\n")
}

// symbolKinds names the LSP SymbolKind values.
var symbolKinds = [...]string{"", "file", "module", "namespace", "package", "class", "method", "property", "field", "constructor", "enum", "interface", "function", "variable", "constant", "string", "number", "boolean", "array", "object", "key", "null", "enum member", "struct", "event", "operator", "type parameter"}

// formatSymbols renders a documentSymbol result, hierarchical or flat.
func formatSymbols(raw json.RawMessage) string {
	type sym struct {
		Name     string `json:"name"`
		Detail   string `json:"detail"`
		Kind     int    `json:"kind"`
		Range    lsp.Range
		Location struct {
			Range lsp.Range `json:"range"`
		} `json:"location"`
		Children []json.RawMessage `json:"children"`
	}
	var b strings.Builder
	var walk func(items []json.RawMessage, depth int)
	walk = func(items []json.RawMessage, depth int) {
		for _, it := range items {
			var s sym
			// fields and properties are the file's noise, not its shape
			if json.Unmarshal(it, &s) != nil || s.Kind == 7 || s.Kind == 8 {
				continue
			}
			r := s.Range
			if r.End == (lsp.Position{}) {
				r = s.Location.Range
			}
			kind := "symbol"
			if s.Kind > 0 && s.Kind < len(symbolKinds) {
				kind = symbolKinds[s.Kind]
			}
			fmt.Fprintf(&b, "%s%s %s", strings.Repeat("  ", depth), kind, s.Name)
			if s.Detail != "" {
				fmt.Fprintf(&b, " %s", s.Detail)
			}
			if r.End.Line > r.Start.Line {
				fmt.Fprintf(&b, "  lines %d-%d\n", r.Start.Line+1, r.End.Line+1)
			} else {
				fmt.Fprintf(&b, "  line %d\n", r.Start.Line+1)
			}
			walk(s.Children, depth+1)
		}
	}
	var items []json.RawMessage
	_ = json.Unmarshal(raw, &items)
	walk(items, 0)
	return strings.TrimRight(b.String(), "\n")
}

// workRel shortens a path to the working directory when it is inside it.
func workRel(path string) string {
	if rel, ok := perm.Under(workdir(), path); ok {
		return rel
	}
	return path
}

// applyTextEdits writes a server's edits to one file under its lock and
// checkpoint, last range first so earlier offsets stay valid.
func applyTextEdits(p *Pipeline, path string, edits []lsp.TextEdit) (int, error) {
	defer lockFile(path)()
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	text := string(data)
	// bottom-up so earlier offsets stay valid; edits at one position keep
	// the server's order, the later one applied first as the protocol says
	slices.SortStableFunc(edits, func(a, b lsp.TextEdit) int {
		return lsp.Offset(text, b.Range.Start) - lsp.Offset(text, a.Range.Start)
	})
	for i := 0; i < len(edits); {
		j := i + 1
		for j < len(edits) && edits[j].Range.Start == edits[i].Range.Start {
			j++
		}
		slices.Reverse(edits[i:j])
		i = j
	}
	for _, e := range edits {
		s, t := lsp.Offset(text, e.Range.Start), lsp.Offset(text, e.Range.End)
		text = text[:s] + e.NewText + text[t:]
	}
	p.checkpoint(path)
	if err := os.WriteFile(path, []byte(text), fi.Mode().Perm()); err != nil {
		return 0, err
	}
	fi, _ = os.Stat(path)
	p.markRead(path, fi)
	return len(edits), nil
}
