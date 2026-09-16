package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
)

func TestResolveSchemaRefs(t *testing.T) {
	defs := map[string]any{
		"$defs": map[string]any{
			"Point": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number"}}},
			"Loop":  map[string]any{"$ref": "#/$defs/Loop"},
		},
	}
	with := func(props map[string]any) map[string]any {
		s := map[string]any{"properties": props}
		s["$defs"] = defs["$defs"]
		return s
	}
	for _, c := range []struct {
		name    string
		schema  map[string]any
		want    string
		wantErr string
	}{
		{name: "no properties", schema: map[string]any{"type": "object"}, want: `{}`},
		{
			name:   "nested ref resolves",
			schema: with(map[string]any{"at": map[string]any{"$ref": "#/$defs/Point"}}),
			want:   `{"at":{"properties":{"x":{"type":"number"}},"type":"object"}}`,
		},
		{
			name:   "ref inside an array resolves",
			schema: with(map[string]any{"all": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/Point"}}}),
			want:   `{"all":{"items":{"properties":{"x":{"type":"number"}},"type":"object"},"type":"array"}}`,
		},
		{name: "recursive ref", schema: with(map[string]any{"l": map[string]any{"$ref": "#/$defs/Loop"}}), wantErr: "recursive"},
		{name: "missing target", schema: with(map[string]any{"n": map[string]any{"$ref": "#/$defs/Nope"}}), wantErr: "does not resolve"},
		{name: "external ref", schema: with(map[string]any{"e": map[string]any{"$ref": "https://example.com/s.json"}}), wantErr: "unsupported"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveSchemaRefs(c.schema)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(got)
			if string(b) != c.want {
				t.Fatalf("got %s, want %s", b, c.want)
			}
		})
	}
}

// testPipeline builds a store with the given rules and an overlay that
// answers overlayAllows. Deny by default: nothing executes unless a rule
// allows it.
func testPipeline(t *testing.T, overlayAllows bool, rules ...perm.Rule) (*Pipeline, *[]Request) {
	t.Helper()
	s := perm.NewStore()
	for _, r := range rules {
		s.Add(r)
	}
	var asked []Request
	p := &Pipeline{
		Store: s,
		Ask: func(req Request) (bool, string) {
			asked = append(asked, req)
			return overlayAllows, "overlay"
		},
	}
	return p, &asked
}

func call(t *testing.T, ag fantasy.AgentTool, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := ag.Run(context.Background(), fantasy.ToolCall{ID: "t1", Name: ag.Info().Name, Input: input})
	if err != nil {
		t.Fatalf("Run returned Go error: %v", err)
	}
	return resp
}

func TestBashAllowAndGate(t *testing.T) {
	p, asked := testPipeline(t, false,
		perm.Rule{Subject: "tool.bash", Pattern: "echo *", Decision: perm.Allow})
	ag := All(p)[0]
	resp := call(t, ag, `{"command":"echo hello"}`)
	if resp.IsError || !strings.Contains(resp.Content, "hello") {
		t.Fatalf("echo should run: %+v", resp)
	}
	if len(*asked) != 0 {
		t.Fatalf("allowed command must not ask: %+v", *asked)
	}
	resp = call(t, ag, `{"command":"ls /etc"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "(overlay)") {
		t.Fatalf("unmatched command must hit the overlay and be denied: %+v", resp)
	}
	if len(*asked) != 1 {
		t.Fatalf("overlay consulted once: %+v", *asked)
	}
}

func TestJobLineChecksWhatRuns(t *testing.T) {
	for _, c := range []struct {
		command string
		args    []string
		segs    []string
	}{
		{"echo ready && sleep 30", nil, []string{"echo ready", "sleep 30"}},
		{"sh", []string{"-c", "cat ~/.ssh/id_rsa"}, []string{"cat ~/.ssh/id_rsa"}},
		{"echo", []string{"a;b", "it's"}, []string{"echo a;b it's"}},
	} {
		line := jobLine(c.command, c.args)
		if got := perm.Normalize(line).Segments; !slices.Equal(got, c.segs) {
			t.Errorf("%q %q: %q checks as %q, want %q", c.command, c.args, line, got, c.segs)
		}
	}
}

func TestBashCompoundNeedsEverySegment(t *testing.T) {
	p, asked := testPipeline(t, false,
		perm.Rule{Subject: "tool.bash", Pattern: "ls*", Decision: perm.Allow})
	ag := All(p)[0]
	resp := call(t, ag, `{"command":"ls; rm -rf x"}`)
	if !resp.IsError {
		t.Fatalf("compound must not ride broad allow: %+v", resp)
	}
	if len(*asked) != 1 || (*asked)[0].Arg != "ls; rm -rf x" {
		t.Fatalf("one ask carrying the whole command preview: %+v", *asked)
	}
}

func TestBashAssignmentGateForcesOverlay(t *testing.T) {
	p, asked := testPipeline(t, false,
		perm.Rule{Subject: "tool.bash", Pattern: "*", Decision: perm.Allow})
	ag := All(p)[0]
	resp := call(t, ag, `{"command":"PATH=.:$PATH make"}`)
	if !resp.IsError {
		t.Fatalf("assignment gate must ask; denying overlay stops it: %+v", resp)
	}
	if len(*asked) != 1 {
		t.Fatalf("assignment gate must ask: %+v", *asked)
	}
}

func TestPreHookDenyRewriteReentry(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "tool.bash", Pattern: "*", Decision: perm.Allow})
	denied := 0
	rewrites := 0
	p.Pre = func(tool string, input map[string]any) (string, string, map[string]any, bool) {
		if tool != "bash" {
			return "", "", nil, false
		}
		cmd := str(input["command"])
		if cmd == "danger --x" {
			denied++
			return "deny", "no danger", nil, false
		}
		if cmd == "echo {{TWICE}}" {
			rewrites++
			if rewrites == 1 {
				return "rewrite", "", map[string]any{"command": "echo once"}, false
			}
			t.Fatal("rewrite hook re-fired on re-entry")
		}
		return "", "", nil, false
	}
	ag := All(p)[0]
	resp := call(t, ag, `{"command":"danger --x"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "no danger") {
		t.Fatalf("deny hook: %+v", resp)
	}
	if denied != 1 {
		t.Fatalf("deny hook ran %d times", denied)
	}
	resp = call(t, ag, `{"command":"echo {{TWICE}}"}`)
	if resp.IsError || !strings.Contains(resp.Content, "once") {
		t.Fatalf("rewrite: %+v", resp)
	}
	if rewrites != 1 {
		t.Fatalf("rewrite fired %d times", rewrites)
	}
}

func TestWriteReadEditRoundtrip(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow},
		perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow},
	)
	all := All(p)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")

	write, read, edit := all[2], all[1], all[3]
	resp := call(t, write, `{"path":"`+file+`","content":"one\ntwo\nthree\n"}`)
	if resp.IsError {
		t.Fatalf("write: %+v", resp)
	}
	resp = call(t, read, `{"path":"`+file+`"}`)
	if !strings.Contains(resp.Content, "2\ttwo") {
		t.Fatalf("read: %+v", resp)
	}
	resp = call(t, edit, `{"path":"`+file+`","old_string":"two","new_string":"TWO"}`)
	if resp.IsError {
		t.Fatalf("edit: %+v", resp)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "TWO") || strings.Contains(string(data), "two") {
		t.Fatalf("edit not applied: %q", data)
	}
}

func TestMultieditSequential(t *testing.T) {
	p, _ := testPipeline(t, false, perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("aaa bbb ccc"), 0o644)
	multiedit := All(p)[4]
	resp := call(t, multiedit, `{"path":"`+file+`","edits":[
		{"old_string":"aaa","new_string":"AAA"},
		{"old_string":"bbb","new_string":"BBB"},
		{"old_string":"ccc","new_string":"CCC"}
	]}`)
	if resp.IsError {
		t.Fatalf("multiedit: %+v", resp)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "AAA BBB CCC" {
		t.Fatalf("multiedit result: %q", data)
	}
}

// TestGlobAndGrep runs every case through ripgrep and the Go fallback.
func TestGlobAndGrep(t *testing.T) {
	p, _ := testPipeline(t, false, perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow})
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub", "node_modules"), 0o755)
	for name, body := range map[string]string{
		"main.go": "package main\n", "notes.txt": "notes\n", ".env": "package secret\n",
		".gitignore": "node_modules/\n", "sub/x.go": "package sub\n", "sub/node_modules/y.go": "package y\n",
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	glob, grep := All(p)[5], All(p)[6]
	saved := rgBin
	t.Cleanup(func() { rgBin = saved })
	for _, bin := range []string{saved, ""} {
		rgBin = bin
		for _, c := range []struct {
			tool      fantasy.AgentTool
			input     string
			want, not string
		}{
			{glob, `{"pattern":"*.go"}`, "sub/x.go", "node_modules"},
			{glob, `{"pattern":"*.go"}`, "main.go", "notes"},
			{glob, `{"pattern":"sub/**/*.go"}`, "sub/x.go", "main.go"},
			{glob, `{"pattern":"*.{go,txt}"}`, "notes.txt", "node_modules"},
			{grep, `{"pattern":"package sub"}`, "x.go:1", "main.go"},
			{grep, `{"pattern":"package","include":"[m]*.go"}`, "main.go:1", "x.go"},
			{grep, `{"pattern":"package (y|secret)"}`, "no matches", ".env"},
		} {
			input := strings.Replace(c.input, "{", `{"path":"`+dir+`",`, 1)
			resp := call(t, c.tool, input)
			if !strings.Contains(resp.Content, c.want) || strings.Contains(resp.Content, c.not) {
				t.Errorf("rg=%q %s: want %q, not %q, got %q", bin, c.input, c.want, c.not, resp.Content)
			}
		}
	}
}

// TestEditSafety: write tools refuse files the model has not seen as they
// are, keep CRLF endings, move a whitespace-tolerant replacement to the
// file's indentation, and point a failed match at where it diverges.
func TestEditSafety(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow},
		perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow})
	p.Seen = &sync.Map{}
	all := All(p)
	read, edit, multiedit := all[1], all[3], all[4]
	dir := t.TempDir()
	js := func(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }
	for _, c := range []struct {
		name, file   string
		unread       bool
		input        map[string]any
		want, result string
	}{
		{"unread", "a\n", true, map[string]any{"old_string": "a", "new_string": "b"}, "read ", "a\n"},
		{"crlf", "one\r\ntwo\r\n", false, map[string]any{"old_string": "one\ntwo", "new_string": "1\n2"}, "edited", "1\r\n2\r\n"},
		{"mostly crlf", "one\r\ntwo\r\nthree\n", false, map[string]any{"old_string": "one", "new_string": "1"}, "edited", "1\r\ntwo\r\nthree\r\n"},
		{"mostly lf", "one\ntwo\nthree\r\n", false, map[string]any{"old_string": "one", "new_string": "1"}, "edited", "1\ntwo\nthree\n"},
		{"reindent", "\tif x {\n\t\treturn\n\t}\n", false,
			map[string]any{"old_string": "if x {\n    return\n}", "new_string": "if x {\n    return 1\n}"},
			"check the indentation", "\tif x {\n\t\treturn 1\n\t}\n"},
		{"diverges", "a\nb\nc\n", false, map[string]any{"old_string": "b\nX", "new_string": "y"}, "matches line 2", "a\nb\nc\n"},
		{"nested field", "a\n", false, map[string]any{"edits": []any{map[string]any{"old_string": "a"}}}, `edits[0].new_string`, "a\n"},
	} {
		path := filepath.Join(dir, c.name)
		os.WriteFile(path, []byte(c.file), 0o644)
		if !c.unread {
			call(t, read, js(map[string]any{"path": path}))
		}
		c.input["path"] = path
		tl := edit
		if c.input["edits"] != nil {
			tl = multiedit
		}
		resp := call(t, tl, js(c.input))
		if got, _ := os.ReadFile(path); !strings.Contains(resp.Content, c.want) || string(got) != c.result {
			t.Errorf("%s: want %q, got %q, file %q", c.name, c.want, resp.Content, got)
		}
	}

	path := filepath.Join(dir, "stale")
	os.WriteFile(path, []byte("x\n"), 0o644)
	call(t, read, js(map[string]any{"path": path}))
	os.WriteFile(path, []byte("y\n"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(path, later, later)
	if resp := call(t, edit, js(map[string]any{"path": path, "old_string": "y", "new_string": "z"})); !strings.Contains(resp.Content, "changed since") {
		t.Fatalf("changed file must be read again: %q", resp.Content)
	}
}

// TestReadAndBashShapes: read returns images as images and refuses binaries
// and offsets past the end; bash keeps stdout and stderr in order, strips
// colors, and saves cut output to the scratchpad.
func TestReadAndBashShapes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "i.png"), append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...), 0o644)
	os.WriteFile(filepath.Join(dir, "b.bin"), []byte("a\x00b"), 0o644)
	os.WriteFile(filepath.Join(dir, "t.txt"), []byte("one\n"), 0o644)
	ctx := context.Background()
	p := &Pipeline{ScratchDir: func() string { return filepath.Join(dir, "pad") }}
	for _, c := range []struct {
		name      string
		offset    float64
		typ, want string
	}{
		{"i.png", 0, "image", ""},
		{"b.bin", 0, "text", "binary file"},
		{"t.txt", 5, "text", "past the end"},
	} {
		res, _ := runRead(ctx, p, map[string]any{"path": filepath.Join(dir, c.name), "offset": c.offset})
		if res.Type != c.typ || !strings.Contains(res.Content, c.want) {
			t.Errorf("%s: %+v", c.name, res)
		}
	}
	if res, _ := runRead(ctx, p, map[string]any{"path": os.DevNull}); !strings.Contains(res.Content, "not a regular file") {
		t.Errorf("device: %+v", res)
	}
	p.Images = func() bool { return false }
	if res, _ := runRead(ctx, p, map[string]any{"path": filepath.Join(dir, "i.png")}); res.Type != "text" || !strings.Contains(res.Content, "does not take images") {
		t.Errorf("text-only model: %+v", res)
	}
	if res, _ := runBash(ctx, p, map[string]any{"command": "echo out; echo err >&2; echo out2"}); res.Content != "out\nerr\nout2" {
		t.Fatalf("merged output: %q", res.Content)
	}
	// just over the cap: the cut marker is longer than what it cuts
	res, _ := runBash(ctx, p, map[string]any{"command": `head -c 10002 /dev/zero | tr '\0' x; printf '\n\033[31mEND\033[0m\n'`})
	_, name, _ := strings.Cut(res.Content, "END\n(full output: ")
	if full, err := os.ReadFile(strings.TrimSuffix(name, ")")); err != nil || len(full) < 10002 {
		t.Fatalf("cut output not saved (%v): %q", err, res.Content[len(res.Content)-100:])
	}
}

func TestValidationErrors(t *testing.T) {
	p, _ := testPipeline(t, false)
	ag := All(p)[0]
	resp := call(t, ag, `{"wrong":1}`)
	if !resp.IsError || !strings.Contains(resp.Content, "missing required field") {
		t.Fatalf("validation: %+v", resp)
	}
	resp = call(t, ag, `not json`)
	if !resp.IsError || !strings.Contains(resp.Content, "invalid tool input") {
		t.Fatalf("bad json: %+v", resp)
	}
}

func TestNoOverlayFailsClosed(t *testing.T) {
	s := perm.NewStore()
	p := &Pipeline{Store: s}
	ag := All(p)[0]
	resp := call(t, ag, `{"command":"echo hi"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "ask failed") {
		t.Fatalf("no overlay must fail closed: %+v", resp)
	}
}

func TestRewriteLoosensAllows(t *testing.T) {
	p, asked := testPipeline(t, false,
		perm.Rule{Subject: "tool.bash", Pattern: "*", Decision: perm.Allow})
	p.Pre = func(tool string, input map[string]any) (string, string, map[string]any, bool) {
		if tool == "bash" && str(input["command"]) == "rewrite-me" {
			return "rewrite", "", map[string]any{"command": "echo hi"}, true
		}
		return "", "", nil, false
	}
	ag := All(p)[0]
	resp := call(t, ag, `{"command":"rewrite-me"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "permission denied (overlay)") {
		t.Fatalf("loosened rewrite must ask (deny overlay): %+v", resp)
	}
	if len(*asked) != 1 {
		t.Fatalf("re-entered allow must consult the overlay: %+v", *asked)
	}
}

func TestPostHookRuns(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "tool.bash", Pattern: "echo *", Decision: perm.Allow})
	var posts []string
	p.Post = func(tool string, input map[string]any, ok bool, output string) {
		posts = append(posts, fmt.Sprintf("%s ok=%v %s", tool, ok, output))
	}
	ag := All(p)[0]
	call(t, ag, `{"command":"echo posted"}`)
	call(t, ag, `{"command":"echo nope; rm x"}`)
	if len(posts) != 2 {
		t.Fatalf("post must run for success and denial: %+v", posts)
	}
	if !strings.Contains(posts[0], "ok=true") || !strings.Contains(posts[0], "posted") {
		t.Fatalf("success post: %+v", posts)
	}
	if !strings.Contains(posts[1], "ok=false") {
		t.Fatalf("denial post: %+v", posts)
	}
}

// TestOutputLimits: read pages a long file with a continuation offset, grep
// returns every matching line up to its cap, and command output keeps its end.
func TestOutputLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	lines := make([]string, readMaxLines+5)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d match", i+1)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, _ := runRead(ctx, nil, map[string]any{"path": path})
	if !strings.HasSuffix(res.Content, "... (5 more lines; continue with offset 2001)\n") {
		t.Fatalf("first page tail: %q", res.Content[len(res.Content)-80:])
	}
	res, _ = runRead(ctx, nil, map[string]any{"path": path, "offset": float64(2001)})
	if strings.Contains(res.Content, "more lines") || !strings.HasSuffix(res.Content, "line 2005 match\n") {
		t.Fatalf("last page: %q", res.Content)
	}
	res, _ = runGrep(ctx, nil, map[string]any{"pattern": "match", "path": dir})
	if n := strings.Count(res.Content, "\n"); n != grepMaxMatches || !strings.Contains(res.Content, "stopped at 100 matches") {
		t.Fatalf("grep lines = %d: %q", n, res.Content[len(res.Content)-120:])
	}
	if out := truncMiddle(strings.Repeat("a", 20000) + "the error"); len(out) > 10100 || !strings.HasSuffix(out, "the error") {
		t.Fatalf("truncMiddle kept %d bytes, tail %q", len(out), out[len(out)-20:])
	}
}

// TestToolSchemasAreProperties guards the fantasy contract: Parameters is
// the properties map (fantasy adds type/properties around it) and arrays
// declare items, or strict providers refuse every request.
func TestToolSchemasAreProperties(t *testing.T) {
	for _, tl := range All(&Pipeline{}) {
		info := tl.Info()
		if info.Required == nil {
			t.Fatalf("%s: nil Required marshals as null", info.Name)
		}
		for name, spec := range info.Parameters {
			m, ok := spec.(map[string]any)
			if !ok {
				t.Fatalf("%s.%s: property spec is %T, want an object", info.Name, name, spec)
			}
			if m["type"] == "array" && m["items"] == nil {
				t.Fatalf("%s.%s: array without items", info.Name, name)
			}
		}
	}
}
