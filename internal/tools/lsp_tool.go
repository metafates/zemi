package tools

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lsp"
	"github.com/metafates/zemi/internal/perm"
)

const lspToolDesc = `Ask the file's language server about a symbol by name. Unlike grep it resolves scopes, so it skips comments, strings and other identifiers with the same name.
- definition: where the symbol is defined, as path:line:col and the line.
- implementation: what implements an interface or abstract method.
- references: every use across the project, sorted by file.
- workspace_symbols: find a declaration by name when you do not know its file.
- rename: rename it everywhere, imports and other files included; the changed files count as read.
- symbols: the file's outline, cheaper than reading a large file for its shape.
- call_hierarchy: who calls the symbol (incoming) and what it calls (outgoing).`

// lspActions are what runLSP answers; the switch there reads the same words.
var lspActions = []string{"definition", "implementation", "references", "rename", "symbols", "workspace_symbols", "call_hierarchy"}

var lspToolProps = map[string]any{
	"action":    map[string]any{"type": "string", "enum": lspActions},
	"path":      map[string]any{"type": "string", "description": "a file where the symbol appears (a use is enough for definition); the file to outline for symbols; any file of the language for workspace_symbols"},
	"symbol":    map[string]any{"type": "string", "description": "the identifier as written, or the query for workspace_symbols; not for symbols"},
	"line":      map[string]any{"type": "integer", "description": "1-based line, when the name appears more than once"},
	"new_name":  map[string]any{"type": "string", "description": "rename only"},
	"direction": map[string]any{"type": "string", "enum": []string{"incoming", "outgoing"}, "description": "call_hierarchy only; who calls the symbol or what it calls, omitted = both"},
}

func lspRequests(_ *Pipeline, in map[string]any) ([]Request, error) {
	if str(in["action"]) == "rename" {
		return []Request{{Subject: perm.SubjectWrite, Arg: str(in["path"]), Reason: "rename " + str(in["symbol"]) + " to " + str(in["new_name"])}}, nil
	}
	return []Request{{Subject: perm.SubjectRead, Arg: str(in["path"]), Reason: "lsp " + str(in["action"])}}, nil
}

func runLSP(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.LSP == nil {
		return fantasy.NewTextErrorResponse("no language server support"), nil
	}
	action, path, symbol := str(input["action"]), str(input["path"]), str(input["symbol"])
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
		var raw jsontext.Value
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
	if action == "workspace_symbols" {
		var syms []struct {
			Name     string       `json:"name"`
			Kind     int          `json:"kind"`
			Location lsp.Location `json:"location"`
		}
		if err := c.Call(ctx, "workspace/symbol", map[string]any{"query": symbol}, &syms); err != nil {
			return fail(err)
		}
		if len(syms) == 0 {
			return fantasy.NewTextResponse("no symbol matching " + symbol + " in the project"), nil
		}
		var b strings.Builder
		for _, sym := range syms[:min(len(syms), 50)] {
			fmt.Fprintf(&b, "%s %s  %s:%d\n", symbolKind(sym.Kind), sym.Name, workRel(sym.Location.Path()), sym.Location.Range.Start.Line+1)
		}
		if len(syms) > 50 {
			fmt.Fprintf(&b, "... %d more\n", len(syms)-50)
		}
		return fantasy.NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
	}
	text, err := readWhole(p.Path)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	line, _ := input["line"].(float64)
	off, lines, err := findSymbol(string(text), symbol, int(line))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	note := pickNote(lines)
	params := map[string]any{"textDocument": doc, "position": lsp.Pos(string(text), off)}
	switch action {
	case "definition", "implementation":
		var raw jsontext.Value
		if err := c.Call(ctx, "textDocument/"+action, params, &raw); err != nil {
			return fail(err)
		}
		locs := decodeLocations(raw)
		if len(locs) == 0 {
			return fantasy.NewTextResponse("no " + action + " found for " + symbol + note), nil
		}
		return fantasy.NewTextResponse(formatLocations(locs, 20) + note), nil
	case "references":
		params["context"] = map[string]any{"includeDeclaration": true}
		var raw jsontext.Value
		if err := c.Call(ctx, "textDocument/references", params, &raw); err != nil {
			return fail(err)
		}
		locs := decodeLocations(raw)
		if len(locs) == 0 {
			return fantasy.NewTextResponse("no references to " + symbol + note), nil
		}
		// the list already shows every line the note would name
		if len(lines) > 1 && !slices.ContainsFunc(lines[1:], func(l int) bool {
			return !slices.ContainsFunc(locs, func(loc lsp.Location) bool {
				return perm.CanonPath(loc.Path()) == perm.CanonPath(path) && loc.Range.Start.Line+1 == l
			})
		}) {
			note = ""
		}
		return fantasy.NewTextResponse(fmt.Sprintf("%d references\n%s%s", len(locs), formatLocations(locs, 100), note)), nil
	case "call_hierarchy":
		// the server re-resolves incoming/outgoing from the prepared item
		// (its data field is opaque, selectionRange required), so round-trip
		// it untouched
		var b strings.Builder
		var prepared []jsontext.Value
		if err := c.Call(ctx, "textDocument/prepareCallHierarchy", params, &prepared); err != nil {
			return fail(err)
		}
		if len(prepared) == 0 {
			return fantasy.NewTextResponse("no call hierarchy for " + symbol + note), nil
		}
		dirs := []string{"incoming", "outgoing"}
		if d := str(input["direction"]); slices.Contains(dirs, d) {
			dirs = []string{d}
		}
		for _, dir := range dirs {
			var res []struct {
				From callHierarchyItem `json:"from"`
				To   callHierarchyItem `json:"to"`
			}
			if err := c.Call(ctx, "callHierarchy/"+dir+"Calls", map[string]any{"item": prepared[0]}, &res); err != nil {
				return fail(err)
			}
			title := "callees"
			if dir == "incoming" {
				title = "callers"
			}
			fmt.Fprintf(&b, "%s of %s:\n", title, symbol)
			if len(res) == 0 {
				b.WriteString("  none\n")
			}
			for _, r := range res {
				it := r.To
				if dir == "incoming" {
					it = r.From
				}
				fmt.Fprintf(&b, "  %s:%d: %s\n", workRel(lsp.Location{URI: it.URI}.Path()), it.Range.Start.Line+1, it.Name)
			}
		}
		return fantasy.NewTextResponse(strings.TrimSuffix(b.String(), "\n") + note), nil
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
		// every file is resolved once, judged on that value and written through
		// it; the named one is the path the gate already checked
		reason := "rename " + symbol + " to " + newName
		resolved := map[string]perm.Path{}
		for _, f := range slices.Sorted(maps.Keys(byPath)) {
			if perm.CanonPath(f) == perm.CanonPath(path) {
				resolved[f] = p.Path
				continue
			}
			ps, err := perm.Resolve(f)
			if err != nil {
				return fantasy.NewTextErrorResponse(workRel(f) + ": " + err.Error()), nil
			}
			resolved[f] = ps
			if v := p.CheckDenied(Request{Tool: ToolLSP, Subject: perm.SubjectWrite, Arg: ps.String(), Reason: reason}); v.Decision != perm.Allow {
				return fantasy.NewTextErrorResponse(DeniedText(v, "rename touches "+workRel(f)+"; nothing was changed")), nil
			}
		}
		var b strings.Builder
		total := 0
		for _, f := range slices.Sorted(maps.Keys(byPath)) {
			n, err := applyTextEdits(p, resolved[f], byPath[f])
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s: %v (files before it were changed)", workRel(f), err)), nil
			}
			total += n
			fmt.Fprintf(&b, "%s (%d)\n", workRel(f), n)
			// the server must see the new text, or the next query (a rename
			// back, references) answers from the old one
			if err := s.Open(f); err != nil {
				fmt.Fprintf(&b, "  (not re-sent to %s: %v)\n", server, err)
			}
		}
		// every rewritten file rides AfterWrite, so a brain file a rename
		// edits as a secondary target is scrubbed and linted too (D5)
		var notes strings.Builder
		for _, f := range slices.Sorted(maps.Keys(byPath)) {
			notes.WriteString(p.afterWrite(f))
		}
		return fantasy.NewTextResponse(fmt.Sprintf("renamed %s to %s in %d file%s, %d place%s:\n%s", symbol, newName, len(byPath), plural(len(byPath)), total, plural(total), strings.TrimRight(b.String(), "\n")) + notes.String()), nil
	}
	return fantasy.NewTextErrorResponse("unknown action " + action), nil
}

// findSymbol returns the byte offset of name in text as a whole word: the
// occurrence on line when given, else the first outside a comment line (a
// server finds no identifier in a comment). lines lists the used line and up
// to seven others when there was a choice.
func findSymbol(text, name string, line int) (off int, lines []int, err error) {
	re := regexp.MustCompile(`(^|[^\pL\pN_])(` + regexp.QuoteMeta(name) + `)($|[^\pL\pN_])`)
	var offs []int
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		// the last segment of http.Get: the first would ask about the package
		offs = append(offs, m[4]+strings.LastIndexAny(name, ".:>")+1)
	}
	if len(offs) == 0 {
		return 0, nil, errors.New(name + " does not appear in the file as a whole word")
	}
	if line > 0 {
		for _, o := range offs {
			if lsp.Pos(text, o).Line+1 == line {
				return o, nil, nil
			}
		}
		return 0, nil, fmt.Errorf("%s is not on line %d", name, line)
	}
	if i := slices.IndexFunc(offs, func(o int) bool { return !commentLine(text, o) }); i > 0 {
		offs = append(offs[i:], offs[:i]...)
	}
	if len(offs) > 1 {
		for _, o := range offs[:min(len(offs), 8)] {
			lines = append(lines, lsp.Pos(text, o).Line+1)
		}
	}
	return offs[0], lines, nil
}

// pickNote tells the model which occurrence findSymbol used and where else
// the name appears.
func pickNote(lines []int) string {
	if len(lines) < 2 {
		return ""
	}
	others := strings.Trim(fmt.Sprint(lines[1:]), "[]")
	return fmt.Sprintf("\n(used the occurrence on line %d; it also appears on line%s %s: pass line to pick one)", lines[0], plural(len(lines)-1), strings.ReplaceAll(others, " ", ", "))
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

func decodeLocations(raw jsontext.Value) []lsp.Location {
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
	slices.SortStableFunc(locs, func(a, b lsp.Location) int {
		return cmp.Or(cmp.Compare(a.Path(), b.Path()), cmp.Compare(a.Range.Start.Line, b.Range.Start.Line))
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

// callHierarchyItem is what the tool prints of an LSP CallHierarchyItem.
type callHierarchyItem struct {
	Name  string    `json:"name"`
	URI   string    `json:"uri"`
	Range lsp.Range `json:"range"`
}

// symbolKind names an LSP SymbolKind value.
func symbolKind(k int) string {
	kinds := [...]string{"", "file", "module", "namespace", "package", "class", "method", "property", "field", "constructor", "enum", "interface", "function", "variable", "constant", "string", "number", "boolean", "array", "object", "key", "null", "enum member", "struct", "event", "operator", "type parameter"}
	if k > 0 && k < len(kinds) {
		return kinds[k]
	}
	return "symbol"
}

// formatSymbols renders a documentSymbol result, hierarchical or flat.
func formatSymbols(raw jsontext.Value) string {
	type sym struct {
		Name     string    `json:"name"`
		Detail   string    `json:"detail"`
		Kind     int       `json:"kind"`
		Range    lsp.Range `json:"range"`
		Location struct {
			Range lsp.Range `json:"range"`
		} `json:"location"`
		Children []jsontext.Value `json:"children"`
	}
	var b strings.Builder
	var walk func(items []jsontext.Value, depth int)
	walk = func(items []jsontext.Value, depth int) {
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
			fmt.Fprintf(&b, "%s%s %s", strings.Repeat("  ", depth), symbolKind(s.Kind), s.Name)
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
	var items []jsontext.Value
	_ = json.Unmarshal(raw, &items)
	walk(items, 0)
	return strings.TrimRight(b.String(), "\n")
}

// workRel shortens a resolved path to the working directory when it is
// inside it, resolving the directory too: a symlinked cwd (/tmp on macOS)
// would otherwise never match.
func workRel(path string) string {
	if rel, ok := perm.Under(perm.RealPath(workdir()), path); ok {
		return rel
	}
	return path
}

// readWhole reads a file the gate resolved: not through a symlink swapped in
// since the check, and not blocking on a FIFO the model named.
func readWhole(path perm.Path) ([]byte, error) {
	f, _, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	return data, cmp.Or(err, f.Close())
}

// applyTextEdits writes a server's edits to one file under its lock and
// checkpoint, last range first so earlier offsets stay valid.
func applyTextEdits(p *Pipeline, path perm.Path, edits []lsp.TextEdit) (int, error) {
	defer lockFile(path.String())()
	fi, err := os.Stat(path.String())
	if err != nil {
		return 0, err
	}
	data, err := readWhole(path)
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
	p.checkpoint(path.String())
	if err := WriteFile(path, []byte(text), fi.Mode().Perm()); err != nil {
		return 0, err
	}
	fi, _ = os.Stat(path.String())
	p.markRead(path.String(), fi)
	return len(edits), nil
}
