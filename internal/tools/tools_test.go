package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sandbox"
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
		Ask: func(req Request) (bool, string, string) {
			asked = append(asked, req)
			return overlayAllows, "overlay", ""
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

// onPath is the per-run copy Run hands a file tool for path.
func onPath(t *testing.T, p *Pipeline, path string) *Pipeline {
	t.Helper()
	var run Pipeline
	if p != nil {
		run = *p
	}
	var err error
	if run.Path, err = perm.Resolve(path); err != nil {
		t.Fatal(err)
	}
	return &run
}

func TestBashGate(t *testing.T) {
	allow := func(pattern string) perm.Rule {
		return perm.Rule{Subject: "tool.bash", Pattern: pattern, Decision: perm.Allow}
	}
	rewrite := func(tool string, input map[string]any) (PreVerdict, string, map[string]any, bool) {
		if str(input["command"]) == "rewrite-me" {
			return "rewrite", "", map[string]any{"command": "echo hi"}, true
		}
		return "", "", nil, false
	}
	for _, c := range []struct {
		name      string
		rule      perm.Rule
		pre       func(string, map[string]any) (PreVerdict, string, map[string]any, bool)
		noOverlay bool
		command   string
		want      string
		wantErr   bool
		wantAsks  []string
	}{
		{name: "allowed runs without asking", rule: allow("echo *"), command: "echo hello", want: "hello"},
		{name: "unmatched asks the denying overlay", rule: allow("echo *"), command: "ls /etc", want: "(overlay)", wantErr: true, wantAsks: []string{"ls /etc"}},
		{name: "compound needs every segment", rule: allow("ls*"), command: "ls; rm -rf x", wantErr: true, wantAsks: []string{"ls; rm -rf x"}},
		{name: "no overlay fails closed", noOverlay: true, command: "echo hi", want: "ask failed", wantErr: true},
		{name: "loosened rewrite re-asks under allow", rule: allow("*"), pre: rewrite, command: "rewrite-me", want: "permission denied (overlay)", wantErr: true, wantAsks: []string{"echo hi"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, asked := testPipeline(t, false, c.rule)
			if c.noOverlay {
				p.Ask = nil
			}
			p.Pre = c.pre
			resp := call(t, All(p)[0], `{"command":"`+c.command+`"}`)
			if resp.IsError != c.wantErr || !strings.Contains(resp.Content, c.want) {
				t.Fatalf("got %+v", resp)
			}
			var args []string
			for _, r := range *asked {
				args = append(args, r.Arg)
			}
			if !slices.Equal(args, c.wantAsks) {
				t.Fatalf("asks = %q, want %q", args, c.wantAsks)
			}
		})
	}
}

// TestEditsOnOneFileKeepCallOrder: two edits to one file in one step, the
// second depending on the first, apply in call order whichever goroutine
// runs first.
func TestEditsOnOneFileKeepCallOrder(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow},
		perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow})
	p.Seen = &sync.Map{}
	read, edit := toolNamed(t, "read", p), toolNamed(t, "edit", p)
	path := filepath.Join(perm.RealPath(t.TempDir()), "f.txt")
	for i := range 20 {
		os.WriteFile(path, []byte("a\n"), 0o644)
		call(t, read, `{"path":"`+path+`","limit":0}`)
		first := fmt.Sprintf(`{"path":%q,"old_string":"a","new_string":"b"}`, path)
		second := fmt.Sprintf(`{"path":%q,"old_string":"b","new_string":"c"}`, path)
		CallStart(context.Background(), "c1", "edit", first)
		CallStart(context.Background(), "c2", "edit", second)
		var wg sync.WaitGroup
		var r1, r2 fantasy.ToolResponse
		wg.Go(func() {
			r2, _ = edit.Run(context.Background(), fantasy.ToolCall{ID: "c2", Name: "edit", Input: second})
		})
		wg.Go(func() { r1, _ = edit.Run(context.Background(), fantasy.ToolCall{ID: "c1", Name: "edit", Input: first}) })
		wg.Wait()
		got, _ := os.ReadFile(path)
		if r1.IsError || r2.IsError || string(got) != "c\n" {
			t.Fatalf("round %d: %q %q, file %q", i, r1.Content, r2.Content, got)
		}
	}
}

// TestCallOrderReleases: a recorded call nothing runs must not hold the
// next call on its file: fantasy answers an invalid call without Run and the
// runner reports it through CallDone; another runner's CallDone releases
// its own ids only, the same id included; a cancelled ctx gives up the wait.
func TestCallOrderReleases(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow},
		perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow})
	p.Seen = &sync.Map{}
	read, edit := toolNamed(t, "read", p), toolNamed(t, "edit", p)
	path := filepath.Join(perm.RealPath(t.TempDir()), "f.txt")
	first := fmt.Sprintf(`{"path":%q,"old_string":"a","new_string":"b"}`, path)
	second := fmt.Sprintf(`{"path":%q,"old_string":"b","new_string":"c"}`, path)
	run := func(ctx context.Context, id, input string) fantasy.ToolResponse {
		done := make(chan fantasy.ToolResponse, 1)
		go func() { r, _ := edit.Run(ctx, fantasy.ToolCall{ID: id, Name: "edit", Input: input}); done <- r }()
		select {
		case r := <-done:
			return r
		case <-time.After(2 * time.Second):
			t.Fatalf("%s blocked", id)
			return fantasy.ToolResponse{}
		}
	}
	bg := context.Background()
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	for _, c := range []struct {
		name    string
		before  func()
		ctx     context.Context
		wantErr bool
		want    string
	}{
		{"invalid first call answered", func() {
			CallStart(bg, "c1", "edit", fmt.Sprintf(`{"path":%q,"old_string":"a"}`, path))
			CallStart(bg, "c2", "edit", second)
			CallDone(bg, "c1")
		}, context.Background(), true, "a\n"},
		{"another runner's step done keeps the order", func() {
			CallStart(bg, "c1", "edit", first)
			CallStart(bg, "c2", "edit", second)
			other := CallScope(bg)
			CallStart(other, "c1", "edit", fmt.Sprintf(`{"path":%q,"old_string":"a","new_string":"b"}`, path+".other"))
			CallDone(other, "c1")
			go func() {
				time.Sleep(50 * time.Millisecond)
				edit.Run(context.Background(), fantasy.ToolCall{ID: "c1", Name: "edit", Input: first})
			}()
		}, context.Background(), false, "c\n"},
		{"cancelled ctx gives up", func() {
			CallStart(bg, "c1", "edit", first)
			CallStart(bg, "c2", "edit", second)
		}, cancelled, true, "a\n"},
	} {
		os.WriteFile(path, []byte("a\n"), 0o644)
		call(t, read, `{"path":"`+path+`","limit":0}`)
		c.before()
		r := run(c.ctx, "c2", second)
		got, _ := os.ReadFile(path)
		if r.IsError != c.wantErr || string(got) != c.want {
			t.Errorf("%s: %+v file %q", c.name, r, got)
		}
		CallDone(bg, "c1", "c2")
	}
}

// TestNotFoundNamesSiblings: a mistyped path names the files it could have
// meant, for read and edit alike, without asking anyone about the edit.
func TestNotFoundNamesSiblings(t *testing.T) {
	p, asked := testPipeline(t, false, perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow})
	p.Seen = &sync.Map{}
	dir := t.TempDir()
	// dotfiles sort first and a directory is no file to read: neither may
	// fill the three suggestions
	for _, name := range []string{".git", ".gitignore", "app.go", "apps.go", "apt.go", "main.go", "app.env"} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}
	os.Mkdir(filepath.Join(dir, "ap"), 0o755)
	for _, c := range []struct{ tool, input string }{
		{"read", `{"path":"` + filepath.Join(dir, "ap.go") + `","limit":0}`},
		{"edit", `{"path":"` + filepath.Join(dir, "ap.go") + `","old_string":"a","new_string":"b"}`},
	} {
		resp := call(t, toolNamed(t, c.tool, p), c.input)
		if !resp.IsError || !strings.Contains(resp.Content, "did you mean app.go, apps.go, apt.go?") || strings.Contains(resp.Content, "app.env") || len(*asked) != 0 {
			t.Errorf("%s: %+v asked %+v", c.tool, resp, *asked)
		}
	}
	if resp := call(t, toolNamed(t, "read", p), `{"path":"`+filepath.Join(dir, "zzz.txt")+`","limit":0}`); !resp.IsError || strings.Contains(resp.Content, "did you mean") {
		t.Errorf("no near name: %+v", resp)
	}
}

// TestSandboxHintForPlanner: a read-only agent is denied sandbox: false, so
// its hint does not offer it; network: true stays.
func TestSandboxHintForPlanner(t *testing.T) {
	p, _ := testPipeline(t, true)
	p.SandboxParam = true
	denied := errors.New("exit status 1")
	out := "open x: operation not permitted"
	if !sandbox.LikelyDenied(out, denied) {
		t.Skip("no denial pattern on this platform")
	}
	if hint := p.sandboxHint(nil, out, denied); !strings.Contains(hint, "sandbox: false") {
		t.Fatalf("plain hint: %q", hint)
	}
	p.Plan = func() bool { return true }
	if hint := p.sandboxHint(nil, out, denied); strings.Contains(hint, "sandbox: false") || !strings.Contains(hint, "network: true") || !strings.Contains(hint, "read-only while planning") {
		t.Fatalf("planner hint: %q", hint)
	}
}

// TestAskDenialText: a denied ask tells the model who decided and why it
// asked, not how the rules failed to match.
func TestAskDenialText(t *testing.T) {
	for _, c := range []struct{ by, why, want string }{
		{"user", "run outside the sandbox", "it asked because run outside the sandbox"},
		{"default", "this project is not trusted (/trust)", "nobody could answer the ask: this project is not trusted (/trust)"},
		{"mode:auto", "", "denied by permission mode auto"},
	} {
		p, _ := testPipeline(t, false)
		p.Ask = func(Request) (bool, string, string) { return false, c.by, c.why }
		resp := call(t, All(p)[0], `{"command":"echo j > out.txt"}`)
		if !resp.IsError || !strings.HasSuffix(resp.Content, "echo j > out.txt: "+c.want) {
			t.Errorf("%s: %q", c.by, resp.Content)
		}
	}
}

func TestPreHookDenyRewriteReentry(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "tool.bash", Pattern: "*", Decision: perm.Allow})
	denied := 0
	rewrites := 0
	p.Pre = func(tool string, input map[string]any) (PreVerdict, string, map[string]any, bool) {
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
	// results name the path relative to the (symlinked, on macOS) cwd
	t.Chdir(dir)
	resp := call(t, write, `{"path":"a.txt","content":"one\ntwo\nthree\n"}`)
	if resp.IsError || resp.Content != "wrote a.txt (14 bytes)" {
		t.Fatalf("write: %+v", resp)
	}
	resp = call(t, read, `{"path":"`+file+`","limit":0}`)
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

// TestGlobAndGrep runs every case through ripgrep and the Go fallback.
func TestGlobAndGrep(t *testing.T) {
	p, _ := testPipeline(t, false, perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow})
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub", "node_modules"), 0o755)
	os.MkdirAll(filepath.Join(dir, "private"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	p.Store.Add(perm.Rule{Subject: "fs.read", Pattern: filepath.Join(dir, "private", "*"), Decision: perm.Deny})
	for name, body := range map[string]string{
		"main.go": "package main\n", "notes.txt": "notes\n", ".env": "package secret\n", "private/k.go": "package priv\n", "call.go": "x := sub(1)\n",
		".gitignore": "node_modules/\ngen/\n", "sub/x.go": "package sub\n", "sub/node_modules/y.go": "package y\n", ".git/HEAD": "ref\n",
		"src/credential.ts": "export function loadCredential() {}\n", "src/a.txt": "needle\n", "gen/b.txt": "needle\n", "locked/c.txt": "needle\n",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	// rg reports the unreadable dir and exits 2 after searching the rest
	os.Chmod(filepath.Join(dir, "locked"), 0)
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "locked"), 0o755) })
	glob, grep := toolNamed(t, "glob", p), toolNamed(t, "grep", p)
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
			{grep, `{"pattern":"package (y|secret)"}`, "(no matches; secret-shaped", "y.go"},
			{grep, `{"pattern":"loadCredential|package secret"}`, "src/credential.ts:1", "package secret"},
			{grep, `{"pattern":"needle"}`, "src/a.txt:1", "gen/"},
			{glob, `{"pattern":"**/*.txt"}`, "src/a.txt", "gen/"},
			{glob, `{"pattern":"**/*.go"}`, "main.go", "private"},
			{grep, `{"pattern":"package priv"}`, "no matches", "k.go"},
			{grep, `{"pattern":"package","path":"sub/x.go"}`, "sub/x.go:1: package sub", "main.go"},
			{grep, `{"pattern":"sub("}`, "(pattern taken literally)\n", "bad pattern"},
			{grep, `{"pattern":"sub("}`, "call.go:1: x := sub(1)", "x.go"},
			{glob, `{"pattern":"./sub/*.go"}`, "sub/x.go", "main.go"},
			{glob, `{"pattern":"DIR/sub/*.go"}`, "sub/x.go", "main.go"},
			{glob, `{"pattern":"/etc/*"}`, "pass its directory as path", "hosts"},
			{glob, `{"pattern":"*"}`, "main.go", ".git/"},
			{glob, `{"pattern":"**"}`, "sub/x.go", "node_modules"},
			{glob, `{"pattern":"**/*"}`, "sub/x.go", ".git/"},
		} {
			if bin == "" && c.not == "gen/" {
				continue // the walker reads no .gitignore
			}
			input := strings.Replace(strings.ReplaceAll(c.input, "DIR", dir), "{", `{"path":"`+dir+`",`, 1)
			if strings.Contains(c.input, `"path":`) {
				input = strings.Replace(c.input, `"path":"`, `"path":"`+dir+`/`, 1)
			}
			resp := call(t, c.tool, input)
			if !strings.Contains(resp.Content, c.want) || strings.Contains(resp.Content, c.not) {
				t.Errorf("rg=%q %s: want %q, not %q, got %q", bin, c.input, c.want, c.not, resp.Content)
			}
		}
		// a hit in .env must not change the answer
		if a, b := call(t, grep, `{"path":"`+dir+`","pattern":"package (y|secret)"}`), call(t, grep, `{"path":"`+dir+`","pattern":"package (y|zzz)"}`); a.Content != b.Content {
			t.Errorf("rg=%q grep answers on .env content: %q vs %q", bin, a.Content, b.Content)
		}
		// a mistyped path must say so instead of reading as an empty tree
		for _, tool := range []fantasy.AgentTool{glob, grep} {
			resp := call(t, tool, `{"path":"`+filepath.Join(dir, "nope")+`","pattern":"x"}`)
			if !resp.IsError || !strings.Contains(resp.Content, "no such file or directory") {
				t.Errorf("rg=%q missing root: %+v", bin, resp)
			}
		}
	}
}

// TestGlobChecksOnlyShownHits: every check resolves symlinks, so a broad
// glob checks the hits it shows, not every file it scanned.
func TestGlobChecksOnlyShownHits(t *testing.T) {
	p, _ := testPipeline(t, true)
	checks := 0
	p.Store.Default = func(string, string) perm.Decision { checks++; return perm.Ask }
	dir := t.TempDir()
	for i := range 1000 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.go", i)), nil, 0o644)
	}
	resp := call(t, toolNamed(t, "glob", p), `{"path":"`+dir+`","pattern":"*.go"}`)
	if !strings.Contains(resp.Content, "up to 800 more") || checks > globMaxFiles+1 {
		t.Errorf("%d checks, got %q", checks, resp.Content[max(0, len(resp.Content)-80):])
	}
	// a scan that hit its cap has no count to give
	defer func(n int) { globMaxScan = n }(globMaxScan)
	globMaxScan = 500
	if resp := call(t, toolNamed(t, "glob", p), `{"path":"`+dir+`","pattern":"*.go"}`); !strings.Contains(resp.Content, "scan stopped at 500 files") {
		t.Errorf("capped scan: %q", resp.Content[max(0, len(resp.Content)-80):])
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
	read, edit := all[1], all[3]
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
			"Written as:\n     1\t\tif x {\n     2\t\t\treturn 1\n     3\t\t}", "\tif x {\n\t\treturn 1\n\t}\n"},
		{"same text", "a\n", false, map[string]any{"old_string": "a", "new_string": "a"}, "are the same; nothing to change", "a\n"},
		{"diverges", "a\nb\nc\n", false, map[string]any{"old_string": "b\nX", "new_string": "y"}, "     2\tb\n     3\tc", "a\nb\nc\n"},
		{"diverges at a common line", "}\na\n}\n}\n}\n", false, map[string]any{"old_string": "}\nX", "new_string": "y"}, "matches lines 1, 3, 4, ..., so a later line differs: read", "}\na\n}\n}\n}\n"},
		{"whole line beats a partial one", "return nil, err\nreturn nil\nx\nreturn nil, err\n", false, map[string]any{"old_string": "return nil\nX", "new_string": "y"}, "     2\treturn nil\n     3\tx", "return nil, err\nreturn nil\nx\nreturn nil, err\n"},
	} {
		path := filepath.Join(dir, c.name)
		os.WriteFile(path, []byte(c.file), 0o644)
		if !c.unread {
			call(t, read, js(map[string]any{"path": path, "limit": 0}))
		}
		c.input["path"] = path
		resp := call(t, edit, js(c.input))
		if got, _ := os.ReadFile(path); !strings.Contains(resp.Content, c.want) || string(got) != c.result {
			t.Errorf("%s: want %q, got %q, file %q", c.name, c.want, resp.Content, got)
		}
	}

	// a directory is named as one: read lists it without marking it seen, so
	// "read it first" would loop
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	for _, in := range []map[string]any{
		{"path": sub, "content": "x"},
		{"path": sub, "old_string": "a", "new_string": "b"},
	} {
		tl := all[2]
		if in["old_string"] != nil {
			tl = edit
		}
		if resp := call(t, tl, js(in)); !resp.IsError || !strings.HasSuffix(resp.Content, "sub is a directory") {
			t.Errorf("%s on a directory: %q", tl.Info().Name, resp.Content)
		}
	}

	path := filepath.Join(dir, "stale")
	os.WriteFile(path, []byte("x\n"), 0o644)
	call(t, read, js(map[string]any{"path": path, "limit": 0}))
	os.WriteFile(path, []byte("y\n"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(path, later, later)
	if resp := call(t, edit, js(map[string]any{"path": path, "old_string": "y", "new_string": "z"})); !strings.Contains(resp.Content, "changed since") {
		t.Fatalf("changed file must be read again: %q", resp.Content)
	}

	// a refused binary still counts as read: write replaces it whole
	bin := filepath.Join(dir, "b.bin")
	os.WriteFile(bin, []byte("a\x00b"), 0o644)
	call(t, read, js(map[string]any{"path": bin, "limit": 0}))
	if resp := call(t, all[2], js(map[string]any{"path": bin, "content": "text\n"})); resp.IsError {
		t.Fatalf("write over a read binary: %q", resp.Content)
	}

	// an unread file is refused before the gate, so the user is never asked
	// to approve a change that cannot happen
	ask, asked := testPipeline(t, true)
	ask.Seen = &sync.Map{}
	unread := filepath.Join(dir, "unasked")
	os.WriteFile(unread, []byte("x\n"), 0o644)
	resp := call(t, All(ask)[2], js(map[string]any{"path": unread, "content": "y\n"}))
	if !strings.Contains(resp.Content, "read ") || len(*asked) != 0 {
		t.Fatalf("unread write: %q, asked %+v", resp.Content, *asked)
	}
}

// TestReadAndBashShapes: read returns images as images, lists a directory
// without secret-shaped names, and refuses binaries and offsets past the end; bash keeps stdout and stderr in order, strips
// colors, and saves cut output to the scratchpad.
func TestReadAndBashShapes(t *testing.T) {
	dir := perm.RealPath(t.TempDir())
	var pngData bytes.Buffer
	png.Encode(&pngData, image.NewGray(image.Rect(0, 0, 1, 1)))
	os.WriteFile(filepath.Join(dir, "i.png"), pngData.Bytes(), 0o644)
	os.WriteFile(filepath.Join(dir, "b.bin"), []byte("a\x00b"), 0o644)
	os.WriteFile(filepath.Join(dir, "t.txt"), []byte("one\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "d", "a"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "b.pem"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "d", "c"), nil, 0o644)
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
		{"d", 0, "text", "a/\nc"},
	} {
		res, _ := runRead(ctx, onPath(t, p, filepath.Join(dir, c.name)), map[string]any{"offset": c.offset})
		if res.Type != c.typ || !strings.Contains(res.Content, c.want) {
			t.Errorf("%s: %+v", c.name, res)
		}
	}
	if res, _ := runRead(ctx, onPath(t, p, os.DevNull), nil); !strings.Contains(res.Content, "not a regular file") {
		t.Errorf("device: %+v", res)
	}
	p.Images = func() bool { return false }
	if res, _ := runRead(ctx, onPath(t, p, filepath.Join(dir, "i.png")), nil); res.Type != "text" || !strings.Contains(res.Content, "does not take images") {
		t.Errorf("text-only model: %+v", res)
	}
	if res, _ := runBash(ctx, p, map[string]any{"command": "echo out; echo err >&2; echo out2"}); res.Content != "out\nerr\nout2" {
		t.Fatalf("merged output: %q", res.Content)
	}
	// just over the cap: the cut marker is longer than what it cuts
	res, _ := runBash(ctx, p, map[string]any{"command": `head -c 10002 /dev/zero | tr '\0' x; printf '\n\033[31mEND\033[0m\n'`})
	_, name, _ := strings.Cut(res.Content, "END\n(output saved to ")
	if full, err := os.ReadFile(strings.TrimSuffix(name, ")")); err != nil || len(full) < 10002 {
		t.Fatalf("cut output not saved (%v): %q", err, res.Content[len(res.Content)-100:])
	}
	// past the 1 MB buffer the note counts what no spill file holds
	if res, _ := runBash(ctx, p, map[string]any{"command": "seq 1 300000"}); !strings.Contains(res.Content, "more bytes in the middle were not kept") {
		t.Fatalf("buffer drop not counted: %q", res.Content[len(res.Content)-200:])
	}
}

func TestValidationErrors(t *testing.T) {
	p, _ := testPipeline(t, false)
	ag := All(p)[0]
	resp := call(t, ag, `{"wrong":1}`)
	if !resp.IsError || !strings.Contains(resp.Content, "missing required field") {
		t.Fatalf("validation: %+v", resp)
	}
	if resp := call(t, toolNamed(t, "read", p), `{"path":"a.txt"}`); !strings.Contains(resp.Content, `missing required field "limit"`) {
		t.Fatalf("read without limit: %+v", resp)
	}
	resp = call(t, ag, `not json`)
	if !resp.IsError || !strings.Contains(resp.Content, "invalid tool input") {
		t.Fatalf("bad json: %+v", resp)
	}
	if resp = call(t, ag, `[1]`); !resp.IsError || resp.Content != "input must be a JSON object" {
		t.Fatalf("non-object: %+v", resp)
	}
	// a misspelt field would run the call with its default and report success
	info := fantasy.ToolInfo{Parameters: map[string]any{
		"command": map[string]any{"type": "string"}, "timeout_ms": map[string]any{"type": "number"},
		"all": map[string]any{"type": "boolean"}, "n": map[string]any{"type": "integer"},
		// an array whose items admit extra fields, under a root that does not
		"env": map[string]any{"type": "array", "items": map[string]any{"properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}, "additionalProperties": true}},
	}}
	for _, c := range []struct {
		input string
		open  bool
		want  string
	}{
		{`{"command":"ls","timeout":1}`, false, `unknown field "timeout" (have all, command, env, n, timeout_ms)`},
		{`{"command":"ls","timeout":null}`, false, ""},
		{`{"command":"ls","timeout":1}`, true, ""},
		{`{"command":"ls","env":[{"name":"a","value":1}]}`, false, ""},
		{`{"env":[{"value":1}]}`, false, `missing required field "env[0].name"`},
		{`{"all":"true","n":"2","timeout_ms":"1.5"}`, false, `{"all":true,"n":2,"timeout_ms":1.5}`},
		{`{"n":"2.5"}`, false, `field "n" must be integer`},
		{`{"timeout_ms":"Inf"}`, false, `field "timeout_ms" must be number`},
		{`{"all":"yes"}`, false, `field "all" must be boolean`},
	} {
		m, err := ParseInput(info, c.input, c.open)
		got := fmt.Sprint(err)
		if err == nil && strings.HasPrefix(c.want, "{") {
			b, _ := json.Marshal(m)
			got = string(b)
		} else if err == nil {
			got = ""
		}
		if got != c.want {
			t.Errorf("%s open=%v: got %q want %q", c.input, c.open, got, c.want)
		}
	}
	if !SchemaOpen(map[string]any{"additionalProperties": true}) || SchemaOpen(map[string]any{"additionalProperties": false}) || SchemaOpen(nil) {
		t.Fatal("SchemaOpen")
	}
}

func TestPostHookRuns(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "tool.bash", Pattern: "echo *", Decision: perm.Allow},
		perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	var posts []string
	p.Post = func(_ context.Context, tool string, input map[string]any, ok bool, output string, changed []string, _ time.Duration) string {
		posts = append(posts, fmt.Sprintf("%s ok=%v %s changed=%v", tool, ok, output, changed))
		return "rewritten"
	}
	p.Changes = func(string) func() (string, string, []string) {
		return func() (string, string, []string) { return "", "", []string{"/by/bash"} }
	}
	ag := All(p)[0]
	if res := call(t, ag, `{"command":"echo posted"}`); res.Content != "rewritten" {
		t.Errorf("the model gets %q, not what post returned", res.Content)
	}
	call(t, ag, `{"command":"echo nope; rm x"}`)
	file := perm.RealPath(filepath.Join(t.TempDir(), "f.txt"))
	call(t, toolNamed(t, "write", p), `{"path":"`+file+`","content":"x"}`)
	if len(posts) != 3 {
		t.Fatalf("post must run for success and denial: %+v", posts)
	}
	for i, want := range []string{"changed=[/by/bash]", "changed=[]", "changed=[" + file + "]"} {
		if !strings.HasSuffix(posts[i], want) {
			t.Errorf("post %d = %q, want %s", i, posts[i], want)
		}
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
	dir := perm.RealPath(t.TempDir())
	path := filepath.Join(dir, "big.txt")
	lines := make([]string, readMaxLines+5)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d match", i+1)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, _ := runRead(ctx, onPath(t, nil, path), nil)
	if !strings.HasSuffix(res.Content, "... (more lines; continue with offset 2001)\n") {
		t.Fatalf("first page tail: %q", res.Content[len(res.Content)-80:])
	}
	res, _ = runRead(ctx, onPath(t, nil, path), map[string]any{"offset": float64(2001)})
	if strings.Contains(res.Content, "more lines") || !strings.HasSuffix(res.Content, "line 2005 match\n") {
		t.Fatalf("last page: %q", res.Content)
	}
	res, _ = runGrep(ctx, nil, map[string]any{"pattern": "match", "path": dir})
	if n := strings.Count(res.Content, "\n"); n != grepMaxMatches || !strings.Contains(res.Content, "stopped at 100 matches") {
		t.Fatalf("grep lines = %d: %q", n, res.Content[len(res.Content)-120:])
	}
}

func TestFitImage(t *testing.T) {
	encode := func(w, h int) []byte {
		var b bytes.Buffer
		png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)))
		return b.Bytes()
	}
	for _, c := range []struct {
		name, mt string
		data     []byte
		want     string // the size sent, or the error
	}{
		{"fits as it is", "image/png", encode(40, 20), "40x20 image/png"},
		{"a long side is shrunk", "image/png", encode(3000, 150), "2000x100 image/png"},
		{"garbage", "image/png", []byte("not a png"), "not a readable image"},
		{"a big webp", "image/webp", make([]byte, imageMaxBytes+1), "over the 3750000 byte limit"},
		{"too many pixels to decode", "image/png", encode(7000, 6000), "over the 40 megapixels"},
	} {
		got := ""
		data, mt, err := FitImage(c.data, c.mt)
		if err != nil {
			got = err.Error()
		} else if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			got = fmt.Sprintf("%dx%d %s", cfg.Width, cfg.Height, mt)
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	img := image.NewNRGBA(image.Rect(3, 0, 5, 1))
	img.Set(3, 0, color.NRGBA{255, 0, 0, 255})
	img.Set(4, 0, color.NRGBA{0, 0, 255, 255})
	if got := boxScale(img, 1, 1).RGBAAt(0, 0); got != (color.RGBA{127, 0, 127, 255}) {
		t.Errorf("boxScale averaged red and blue into %v", got)
	}
}

func TestTruncOut(t *testing.T) {
	for _, c := range []struct {
		name, s string
		n       int
		want    string
	}{
		{"fits", "abc", 10, "abc"},
		{"cuts fall on line breaks", "aaaa\nbb\ncccccc\nd\neeeee\nff", 20, "aaaa\n... (lines 2-3 of 6 cut) ...\nd\neeeee\nff"},
		{"one huge line is cut by bytes", strings.Repeat("x", 30), 10, "xxx\n... (20 bytes cut) ...\nxxxxxxx"},
	} {
		if got := TruncOut(c.s, c.n); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCmdText(t *testing.T) {
	for _, c := range []struct{ name, raw, want string }{
		{"tqdm frames lead with \\r", "\r  0%|   | 0/80\r 50%|#  | 40/80\r100%|###| 80/80\ntrained\n", "100%|###| 80/80\ntrained"},
		{"curl meter ends frames with \\r", "  % Total\n  0  0\r 40 10\r100 25\r\ndone", "  % Total\n100 25\ndone"},
		{"crlf line ends", "a\r\nb\r\n", "a\nb"},
		{"ansi around a frame", "\x1b[32m1/2\x1b[0m\r\x1b[32m2/2\x1b[0m", "2/2"},
	} {
		if got := cmdText(c.raw); got != c.want {
			t.Errorf("%s: cmdText = %q, want %q", c.name, got, c.want)
		}
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

// TestReadPagesAHugeFile: read streams, so a file past the edit ceiling
// still pages; edit refuses it by size instead of allocating it twice.
func TestReadPagesAHugeFile(t *testing.T) {
	dir := perm.RealPath(t.TempDir())
	path := filepath.Join(dir, "huge.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n"
	chunk := strings.Repeat(line, 1000)
	for range (editMaxBytes / len(chunk)) + 1 {
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	p := onPath(t, &Pipeline{Seen: &sync.Map{}}, path)
	res, _ := runRead(context.Background(), p, map[string]any{"offset": float64(100_000)})
	if !strings.Contains(res.Content, "100000\tx") || !strings.Contains(res.Content, "... (more lines; continue with offset") {
		t.Fatalf("page: %q", res.Content[:min(len(res.Content), 120)])
	}
	res, _ = runEdit(context.Background(), p, map[string]any{"old_string": "x", "new_string": "y"})
	if !strings.Contains(res.Content, "over edit's 10 MB limit") {
		t.Fatalf("edit of a huge file: %q", res.Content)
	}
}

func TestFindSymbol(t *testing.T) {
	text := "import \"net/http\"\nr, _ := http.Get(u)\n"
	for _, c := range []struct {
		name, at string
		line     int
	}{
		{"http", "http.Get(u)", 2},
		{"http.Get", "Get(u)", 0},
	} {
		off, _, err := findSymbol(text, c.name, c.line)
		if err != nil || !strings.HasPrefix(text[off:], c.at) {
			t.Errorf("%s: at %q, %v", c.name, text[off:], err)
		}
	}
}
