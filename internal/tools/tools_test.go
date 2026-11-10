package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
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
	"github.com/metafates/zemi/internal/proc"
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
			b, _ := json.Marshal(got, json.Deterministic(true))
			if string(b) != c.want {
				t.Fatalf("got %s, want %s", b, c.want)
			}
		})
	}
}

// testPipeline builds a pipeline: with allow, everything runs ungated; with
// !allow, a tool.pre ask hands every action to the overlay, the shape a
// plugin's ask-everything hook makes, and the asker answers no and records.
func testPipeline(t *testing.T, allow bool) (*Pipeline, *[]Request) {
	t.Helper()
	var asked []Request
	p := &Pipeline{forceAsk: !allow}
	if !allow {
		p.Ask = func(req Request) (bool, string) {
			asked = append(asked, req)
			return false, "overlay"
		}
	}
	return p, &asked
}

func call(t *testing.T, ag fantasy.AgentTool, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := ag.Run(t.Context(), fantasy.ToolCall{ID: "t1", Name: ag.Info().Name, Input: input})
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

func TestShellGate(t *testing.T) {
	rewrite := func(tool string, input map[string]any) (PreVerdict, string, map[string]any) {
		if str(input["command"]) == "rewrite-me" {
			return "rewrite", "", map[string]any{"command": "echo hi"}
		}
		return "", "", nil
	}
	for _, c := range []struct {
		name     string
		allow    bool
		pre      func(string, map[string]any) (PreVerdict, string, map[string]any)
		command  string
		want     string
		wantErr  bool
		wantAsks []string
	}{
		{name: "ungated runs without asking", allow: true, command: "echo hello", want: "hello"},
		{name: "an ask policy asks the denying overlay", command: "ls /etc", want: "(overlay)", wantErr: true, wantAsks: []string{"ls /etc"}},
		{name: "a compound is one ask", command: "ls; rm -rf x", wantErr: true, wantAsks: []string{"ls; rm -rf x"}},
		{name: "a rewrite re-enters at the gate", pre: rewrite, command: "rewrite-me", want: "permission denied (overlay)", wantErr: true, wantAsks: []string{"echo hi"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, asked := testPipeline(t, c.allow)
			p.Pre = c.pre
			resp := call(t, toolNamed(t, "shell", p), `{"command":"`+c.command+`"}`)
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
	p, _ := testPipeline(t, true)
	p.Seen = &sync.Map{}
	read, edit := toolNamed(t, "read", p), toolNamed(t, "edit", p)
	path := filepath.Join(perm.RealPath(t.TempDir()), "f.txt")
	for i := range 20 {
		os.WriteFile(path, []byte("a\n"), 0o644)
		call(t, read, `{"path":"`+path+`","limit":0}`)
		first := fmt.Sprintf(`{"path":%q,"search":"a","replace":"b"}`, path)
		second := fmt.Sprintf(`{"path":%q,"search":"b","replace":"c"}`, path)
		CallStart(t.Context(), "c1", "edit", first)
		CallStart(t.Context(), "c2", "edit", second)
		var wg sync.WaitGroup
		var r1, r2 fantasy.ToolResponse
		wg.Go(func() {
			r2, _ = edit.Run(t.Context(), fantasy.ToolCall{ID: "c2", Name: "edit", Input: second})
		})
		wg.Go(func() { r1, _ = edit.Run(t.Context(), fantasy.ToolCall{ID: "c1", Name: "edit", Input: first}) })
		wg.Wait()
		got, _ := os.ReadFile(path)
		if r1.IsError || r2.IsError || string(got) != "c\n" {
			t.Fatalf("round %d: %q %q, file %q", i, r1.Content, r2.Content, got)
		}
	}
}

// TestNotFoundNamesSiblings: a mistyped path names the three files nearest
// what it could have meant, for read and edit alike, without asking anyone
// about the edit.
func TestNotFoundNamesSiblings(t *testing.T) {
	p, asked := testPipeline(t, true)
	p.Seen = &sync.Map{}
	for _, c := range []struct {
		name, missing, want string
		files               []string
	}{
		// dotfiles sort first and a directory is no file to read: none of them
		// fills the suggestions
		{"dotfiles and directories left out", "ap.go", "app.go, apt.go, app.env", []string{".git", ".gitignore", "ap/", "app.go", "apps.go", "apt.go", "main.go", "app.env"}},
		{"the nearest three, not the first three", "config.go", "config.ts, confg.go, configs.go", []string{"a_config.go", "confg.go", "config.ts", "config_old.go", "configs.go"}},
		{"no near name", "zzz.txt", "", []string{"app.go"}},
	} {
		dir := t.TempDir()
		for _, name := range c.files {
			if d, ok := strings.CutSuffix(name, "/"); ok {
				os.Mkdir(filepath.Join(dir, d), 0o755)
			} else {
				os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
			}
		}
		path := filepath.Join(dir, c.missing)
		for tool, input := range map[string]string{
			"read": `{"path":"` + path + `","limit":0}`,
			"edit": `{"path":"` + path + `","search":"a","replace":"b"}`,
		} {
			resp := call(t, toolNamed(t, tool, p), input)
			_, got, _ := strings.Cut(strings.TrimSuffix(resp.Content, "?)"), "(did you mean ")
			if !resp.IsError || got != c.want || len(*asked) != 0 {
				t.Errorf("%s, %s: %q asked %+v, want %q", c.name, tool, resp.Content, *asked, c.want)
			}
		}
	}
}

func TestWriteReadEditRoundtrip(t *testing.T) {
	p, _ := testPipeline(t, true)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	write, read, edit := toolNamed(t, "write", p), toolNamed(t, "read", p), toolNamed(t, "edit", p)
	// results name the path relative to the (symlinked, on macOS) cwd
	t.Chdir(dir)
	resp := call(t, write, `{"path":"a.txt","text":"one\ntwo\nthree\n"}`)
	if resp.IsError || resp.Content != "wrote a.txt (14 bytes)" {
		t.Fatalf("write: %+v", resp)
	}
	resp = call(t, read, `{"path":"`+file+`","limit":0}`)
	if !strings.Contains(resp.Content, "2\ttwo") {
		t.Fatalf("read: %+v", resp)
	}
	resp = call(t, edit, `{"path":"`+file+`","search":"two","replace":"TWO"}`)
	if resp.IsError {
		t.Fatalf("edit: %+v", resp)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "TWO") || strings.Contains(string(data), "two") {
		t.Fatalf("edit not applied: %q", data)
	}
}

// TestGlobAndGrep runs over a real git repository, outside one and with no
// git binary.
func TestGlobAndGrep(t *testing.T) {
	p, _ := testPipeline(t, true)
	dir, out := t.TempDir(), t.TempDir()
	files := map[string]string{
		"main.go": "package main\n", ".env": "package secret\n", "private/k.go": "package priv\n", "call.go": "x := sub(1)\n",
		".gitignore": "node_modules/\ngen/\nbuild/\n", "sub/x.go": "package sub\n", "sub/node_modules/y.go": "package y\n",
		"src/credential.ts": "export function loadCredential() {}\n", "src/a.txt": "needle\n", "gen/b.txt": "needle\n", "locked/c.txt": "needle\n",
		".github/workflows/ci.yml": "on: push\n", "build/tracked.go": "package tracked\n", "build/untracked.go": "package tracked\n",
		".agents/worktrees/w/x.go": "package sub\n", "bin.dat": strings.Repeat("a", 600) + "\x00\nneedle\n",
		"two.txt": "one\ntwo anchored\n", "swap/s.txt": "inside\n",
	}
	for i := range 300 {
		files[fmt.Sprintf("many/f%03d.txt", i)] = "dup\n"
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		// old, so a glob's newest-first cut leaves them out
		if strings.HasPrefix(name, "many/") {
			os.Chtimes(filepath.Join(dir, name), time.Time{}, time.Unix(0, 0))
		}
	}
	os.WriteFile(filepath.Join(out, "loose.txt"), []byte("needle\n"), 0o644)
	os.Symlink("main.go", filepath.Join(dir, "link.go"))
	os.Symlink("private/k.go", filepath.Join(dir, "lnk.txt"))
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"add", "-f", "build/tracked.go"}} {
		if _, err := (proc.Git{Dir: dir}).Run(t.Context(), args...); err != nil {
			t.Fatal(err)
		}
	}
	// a tracked directory swapped for a link leads out of the root
	os.RemoveAll(filepath.Join(dir, "swap"))
	os.Symlink(out, filepath.Join(dir, "swap"))
	os.WriteFile(filepath.Join(out, "s.txt"), []byte("outside\n"), 0o644)
	// git lists untracked files first, right after its stderr warning about
	// the unreadable dir, which must not garble notes.txt
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "void"), 0o755)
	os.Chmod(filepath.Join(dir, "locked"), 0)
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "locked"), 0o755) })
	glob, grep := toolNamed(t, "glob", p), toolNamed(t, "grep", p)
	for _, c := range []struct {
		tool      fantasy.AgentTool
		input     string
		want, not string
		root      string // default dir
		noGit     bool
		slowGit   bool // a git that never answers
	}{
		{tool: glob, input: `{"pattern":"*.go"}`, want: "sub/x.go", not: "node_modules"},
		{tool: glob, input: `{"pattern":"*.go"}`, want: "main.go", not: "link.go"},
		{tool: glob, input: `{"pattern":"*.go","path":"main.go"}`, want: "is a file: pass its directory as path", not: "no such file"},
		{tool: glob, input: `{"pattern":"sub/**/*.go"}`, want: "sub/x.go", not: "main.go"},
		{tool: glob, input: `{"pattern":"*.{go,txt}"}`, want: "notes.txt", not: "node_modules"},
		{tool: grep, input: `{"pattern":"package sub"}`, want: "x.go:1", not: "main.go"},
		{tool: grep, input: `{"pattern":"package","include":"[m]*.go"}`, want: "main.go:1", not: "x.go"},
		{tool: grep, input: `{"pattern":"package (y|secret)"}`, want: ".env:1", not: "credential"},
		{tool: grep, input: `{"pattern":"loadCredential|package secret"}`, want: "src/credential.ts:1"},
		{tool: grep, input: `{"pattern":"needle"}`, want: "src/a.txt:1", not: "gen/"},
		{tool: grep, input: `{"pattern":"needle"}`, want: "src/a.txt:1", not: "bin.dat"},
		{tool: glob, input: `{"pattern":"**/*.txt"}`, want: "src/a.txt", not: "gen/"},
		{tool: glob, input: `{"pattern":"**/*.go"}`, want: "main.go"},
		{tool: grep, input: `{"pattern":"package priv"}`, want: "private/k.go:1", not: "lnk.txt"},
		{tool: grep, input: `{"pattern":"outside"}`, want: `(no matches for "outside" in`, not: "swap/"},
		{tool: glob, input: `{"pattern":"*.zig"}`, want: `(no files match "*.zig" under ` + dir + `)`},
		{tool: grep, input: `{"pattern":"package","include":"*.zig"}`, want: `(no files match include "*.zig" under ` + dir + `; not searched)`, not: "in 0 files"},
		{tool: grep, input: `{"pattern":"zzz","path":"sub"}`, want: `(no matches for "zzz" in 1 files under ` + dir + `/sub)`},
		{tool: grep, input: `{"pattern":"zzz","path":"void"}`, want: `(no matches for "zzz" in 0 files under ` + dir + `/void)`},
		{tool: grep, input: `{"pattern":"package","path":"sub/x.go"}`, want: "sub/x.go:1: package sub", not: "main.go"},
		{tool: grep, input: `{"pattern":"sub("}`, want: "(pattern taken literally)\n", not: "bad pattern"},
		{tool: grep, input: `{"pattern":"sub("}`, want: "call.go:1: x := sub(1)", not: "x.go"},
		{tool: grep, input: `{"pattern":"^package tracked$"}`, want: "build/tracked.go:1", not: "untracked"},
		{tool: grep, input: `{"pattern":"\\Atwo anchored"}`, want: "two.txt:2: two anchored"},
		{tool: grep, input: `{"pattern":"dup"}`, want: "many/f099.txt:1: dup\n... (stopped at 100", not: "f100"},
		{tool: glob, input: `{"pattern":"./sub/*.go"}`, want: "sub/x.go", not: "main.go"},
		{tool: glob, input: `{"pattern":"DIR/sub/*.go"}`, want: "sub/x.go", not: "main.go"},
		{tool: glob, input: `{"pattern":"/etc/*"}`, want: "pass its directory as path", not: "hosts"},
		{tool: glob, input: `{"pattern":"*"}`, want: "main.go", not: ".git/"},
		{tool: glob, input: `{"pattern":"**"}`, want: "sub/x.go", not: "node_modules"},
		{tool: glob, input: `{"pattern":"**/*"}`, want: "sub/x.go", not: ".agents/worktrees"},
		{tool: glob, input: `{"pattern":"**/*.yml"}`, want: ".github/workflows/ci.yml", not: _walkedNote},
		{tool: glob, input: `{"pattern":"*","path":"gen"}`, want: _walkedNote + "\n" + dir + "/gen/b.txt"},
		{tool: grep, input: `{"pattern":"needle"}`, want: _walkedNote + "\n" + out + "/loose.txt:1", root: out},
		{tool: glob, input: `{"pattern":"*.go"}`, want: _walkedNote + "\n", not: "build/", noGit: true},
		{tool: grep, input: `{"pattern":"needle"}`, want: "src/a.txt:1", not: "bin.dat", noGit: true},
		{tool: glob, input: `{"pattern":"*.go"}`, want: "(git took over 50ms to list the files here, so a walk did: .gitignore not read", not: "build/", slowGit: true},
	} {
		t.Run(c.input, func(t *testing.T) {
			if c.noGit {
				t.Setenv("PATH", "")
			}
			if c.slowGit {
				bin := t.TempDir()
				os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o755)
				t.Setenv("PATH", bin)
				defer func(w time.Duration) { _listWait = w }(_listWait)
				_listWait = 50 * time.Millisecond
			}
			root := cmp.Or(c.root, dir)
			input := strings.Replace(strings.ReplaceAll(c.input, "DIR", root), "{", `{"path":"`+root+`",`, 1)
			if strings.Contains(c.input, `"path":`) {
				input = strings.Replace(c.input, `"path":"`, `"path":"`+root+`/`, 1)
			}
			resp := call(t, c.tool, input)
			if !strings.Contains(resp.Content, c.want) || c.not != "" && strings.Contains(resp.Content, c.not) {
				t.Errorf("want %q, not %q, got %q", c.want, c.not, resp.Content)
			}
		})
	}
	// the same call answers the same whatever it matched
	for _, pat := range [][2]string{{"package (y|secret)", "package (y|secret)"}, {"dup|needle", "dup|needle"}} {
		if a, b := call(t, grep, `{"path":"`+dir+`","pattern":"`+pat[0]+`"}`), call(t, grep, `{"path":"`+dir+`","pattern":"`+pat[1]+`"}`); a.Content != b.Content {
			t.Errorf("%q and %q answer differently: %q vs %q", pat[0], pat[1], a.Content, b.Content)
		}
	}
	// a mistyped path must say so instead of reading as an empty tree
	for _, tool := range []fantasy.AgentTool{glob, grep} {
		resp := call(t, tool, `{"path":"`+filepath.Join(dir, "nope")+`","pattern":"x"}`)
		if !resp.IsError || !strings.Contains(resp.Content, "no such file or directory") {
			t.Errorf("missing root: %+v", resp)
		}
	}
}

// TestEditSafety: write tools refuse files the model has not seen as they
// are, keep CRLF endings, and point a failed match at where it diverges.
func TestEditSafety(t *testing.T) {
	p, _ := testPipeline(t, true)
	p.Seen = &sync.Map{}
	read, write, edit := toolNamed(t, "read", p), toolNamed(t, "write", p), toolNamed(t, "edit", p)
	dir := perm.RealPath(t.TempDir())
	js := func(m map[string]any) string { b, _ := json.Marshal(m, json.Deterministic(true)); return string(b) }
	for _, c := range []struct {
		name, file   string
		unread       bool
		input        map[string]any
		want, result string
	}{
		{"unread, one exact match", "a\n", true, map[string]any{"search": "a", "replace": "b"}, "edited", "b\n"},
		{"crlf", "one\r\ntwo\r\n", false, map[string]any{"search": "one\ntwo", "replace": "1\n2"}, "edited", "1\r\n2\r\n"},
		{"mostly crlf", "one\r\ntwo\r\nthree\n", false, map[string]any{"search": "one", "replace": "1"}, "edited", "1\r\ntwo\r\nthree\r\n"},
		{"mostly lf", "one\ntwo\nthree\r\n", false, map[string]any{"search": "one", "replace": "1"}, "edited", "1\ntwo\nthree\n"},
		{"same text", "a\n", false, map[string]any{"search": "a", "replace": "a"}, "are the same; nothing to change", "a\n"},
		{"ambiguous", "x = 1\nx = 1\n", false, map[string]any{"search": "x = 1", "replace": "x = 2"}, "matches 2 places", "x = 1\nx = 1\n"},
		{"empty search", "x\n", false, map[string]any{"search": "", "replace": "y"}, "empty", "x\n"},
		{"diverges", "a\nb\nc\n", false, map[string]any{"search": "b\nX", "replace": "y"}, "     2\tb\n     3\tc", "a\nb\nc\n"},
		{"diverges at a common line", "}\na\n}\n}\n}\n", false, map[string]any{"search": "}\nX", "replace": "y"}, "matches lines 1, 3, 4, ..., so a later line differs: read", "}\na\n}\n}\n}\n"},
		{"whole line beats a partial one", "return nil, err\nreturn nil\nx\nreturn nil, err\n", false, map[string]any{"search": "return nil\nX", "replace": "y"}, "     2\treturn nil\n     3\tx", "return nil, err\nreturn nil\nx\nreturn nil, err\n"},
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
		{"path": sub, "text": "x"},
		{"path": sub, "search": "a", "replace": "b"},
	} {
		tl := write
		if in["search"] != nil {
			tl = edit
		}
		if resp := call(t, tl, js(in)); !resp.IsError || !strings.HasSuffix(resp.Content, "sub is a directory") {
			t.Errorf("%s on a directory: %q", tl.Info().Name, resp.Content)
		}
	}

	// a file changed since its read takes only an exact match, like an unread
	// one, and stays unseen after it, so a write cannot drop the change
	path := filepath.Join(dir, "stale")
	os.WriteFile(path, []byte("x\n"), 0o644)
	call(t, read, js(map[string]any{"path": path, "limit": 0}))
	os.WriteFile(path, []byte("y\ny\nu\n"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(path, later, later)
	for _, c := range []struct {
		tool fantasy.AgentTool
		in   map[string]any
		want string
	}{
		{edit, map[string]any{"search": "y", "replace": "z"}, "matches 2 places"},
		{edit, map[string]any{"search": "u", "replace": "v"}, "edited"},
		{write, map[string]any{"text": "x\n"}, "changed since you read it"},
	} {
		c.in["path"] = path
		if resp := call(t, c.tool, js(c.in)); !strings.Contains(resp.Content, c.want) {
			t.Fatalf("%v: want %q, got %q", c.in, c.want, resp.Content)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "y\ny\nv\n" {
		t.Fatalf("file %q", got)
	}

	// a refused binary still counts as read: write replaces it whole
	bin := filepath.Join(dir, "b.bin")
	os.WriteFile(bin, []byte("a\x00b"), 0o644)
	call(t, read, js(map[string]any{"path": bin, "limit": 0}))
	if resp := call(t, write, js(map[string]any{"path": bin, "text": "text\n"})); resp.IsError {
		t.Fatalf("write over a read binary: %q", resp.Content)
	}

	// an unread file is refused before the gate, so the user is never asked
	// to approve a change that cannot happen
	ask, asked := testPipeline(t, true)
	ask.Seen = &sync.Map{}
	unread := filepath.Join(dir, "unasked")
	os.WriteFile(unread, []byte("x\n"), 0o644)
	resp := call(t, toolNamed(t, "write", ask), js(map[string]any{"path": unread, "text": "y\n"}))
	if !strings.Contains(resp.Content, "read ") || len(*asked) != 0 {
		t.Fatalf("unread write: %q, asked %+v", resp.Content, *asked)
	}
}

// TestScratchpad: the pad is outside every project rule, and its writes
// gate like any other write - the ask-everything gate asks, one with no
// asker runs - except a read-only agent, whose pad is the one place it may
// write. A file-tool path expands $VARS ($ZEMI_SCRATCH names the pad); one
// whose variable does not resolve is refused naming it, and
// a command keeps the caller's environ.
func TestScratchpad(t *testing.T) {
	pad, proj := perm.RealPath(t.TempDir()), perm.RealPath(t.TempDir())
	free := perm.RealPath(t.TempDir())
	t.Chdir(proj)
	p, asked := testPipeline(t, false)
	p.Pad, p.Seen = pad, &sync.Map{}
	os.WriteFile(filepath.Join(pad, "stale.txt"), []byte("a\n"), 0o644)
	for _, c := range []struct {
		name, tool, input string
		before            func()
		want              string
		asks              bool
	}{
		{"write asks", string(ToolWrite), `{"path":"PAD/n.md","text":"x"}`, nil, "permission denied (overlay): fs.write", true},
		{"write in a temp dir asks", string(ToolWrite), `{"path":"FREE/notes.txt","text":"x"}`, nil, "permission denied (overlay): fs.write", true},
		{"read", string(ToolRead), `{"path":"PAD/stale.txt","limit":0}`, nil, "1\ta", false},
		{"$ZEMI_SCRATCH write asks", string(ToolWrite), `{"path":"$ZEMI_SCRATCH/n.md","text":"x"}`, nil, "permission denied (overlay): fs.write", true},
		{"$ZEMI_SCRATCH read", string(ToolRead), `{"path":"$ZEMI_SCRATCH/stale.txt","limit":0}`, nil, "1\ta", false},
		{"$UNKNOWN", string(ToolWrite), `{"path":"$NOPE/n.md","text":"x"}`, nil, `directory called "$NOPE" in the project`, false},
		{"~", string(ToolRead), `{"path":"~/n.md","limit":0}`, nil, "home is", false},
		{"%TEMP%", string(ToolWrite), `{"path":"%TEMP%\\n.md","text":"x"}`, nil, "not expanded", false},
	} {
		if c.before != nil {
			c.before()
		}
		n := len(*asked)
		in := strings.ReplaceAll(strings.ReplaceAll(c.input, "PAD", pad), "FREE", free)
		got := call(t, toolNamed(t, c.tool, p), in).Content
		want := strings.ReplaceAll(strings.ReplaceAll(c.want, "PAD", pad), "FREE", free)
		if !strings.Contains(got, want) || len(*asked) > n != c.asks {
			t.Errorf("%s: asked %v, got %q, want %q", c.name, len(*asked) > n, got, want)
		}
	}
	// the pad is a read-only agent's one writable place; anything else of
	// theirs denies
	ro, roAsked := testPipeline(t, false)
	ro.Pad = pad
	ro.ReadOnly = func() bool { return true }
	if v := ro.Check(Request{Tool: string(ToolWrite), Subject: perm.SubjectWrite, Arg: filepath.Join(pad, "n.md")}); !v.Allowed || len(*roAsked) != 0 {
		t.Fatalf("a read-only agent's pad write: %+v, asked %+v", v, *roAsked)
	}
	if v := ro.Check(Request{Tool: string(ToolWrite), Subject: perm.SubjectWrite, Arg: filepath.Join(proj, "n.md")}); v.Allowed || v.DecidedBy != ByReadOnly {
		t.Fatalf("a read-only agent's project write: %+v", v)
	}
	// an ungated pipeline writes the pad and a temp dir, and neither lands
	// in the project: a path is taken as the pad or the temp dir names it
	allow, _ := testPipeline(t, true)
	allow.Pad, allow.Seen = pad, p.Seen
	for _, path := range []string{filepath.Join(pad, "made.txt"), filepath.Join(free, "notes.txt")} {
		if res := call(t, toolNamed(t, string(ToolWrite), allow), fmt.Sprintf(`{"path":%q,"text":"a\n"}`, path)); res.IsError {
			t.Errorf("ungated write to %s: %q", path, res.Content)
		}
	}
	if res := call(t, toolNamed(t, string(ToolEdit), allow), fmt.Sprintf(`{"path":%q,"search":"a","replace":"b"}`, filepath.Join(pad, "made.txt"))); res.IsError {
		t.Errorf("ungated edit in the pad: %q", res.Content)
	}
	if entries, _ := os.ReadDir(proj); len(entries) > 0 {
		t.Errorf("a path was taken literally: %v", entries)
	}
	if res, _ := runShell(t.Context(), p, shellArgs{Command: `printf %s "$TMPDIR"`}); res.Content != os.Getenv("TMPDIR") {
		t.Errorf("a shell command's TMPDIR is %q, want the caller's %q", res.Content, os.Getenv("TMPDIR"))
	}
}

// TestReadAndShellShapes: read returns images as images, lists a directory
// one level deep, and refuses binaries and offsets past the end; shell keeps stdout and stderr in order, strips
// colors, and saves cut output to the scratchpad.
func TestReadAndShellShapes(t *testing.T) {
	dir := perm.RealPath(t.TempDir())
	var pngData bytes.Buffer
	png.Encode(&pngData, image.NewGray(image.Rect(0, 0, 1, 1)))
	os.WriteFile(filepath.Join(dir, "i.png"), pngData.Bytes(), 0o644)
	os.WriteFile(filepath.Join(dir, "b.bin"), []byte("a\x00b"), 0o644)
	os.WriteFile(filepath.Join(dir, "t.txt"), []byte("one\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "d", "a"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "b.pem"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "d", "c"), nil, 0o644)
	ctx := t.Context()
	p := &Pipeline{Pad: filepath.Join(dir, "pad")}
	for _, c := range []struct {
		name      string
		offset    int
		typ, want string
	}{
		{"i.png", 0, "image", ""},
		{"b.bin", 0, "text", "binary file"},
		{"t.txt", 5, "text", "past the end"},
		{"d", 0, "text", "a/\nb.pem\nc"},
	} {
		res, _ := runRead(ctx, onPath(t, p, filepath.Join(dir, c.name)), readArgs{Offset: c.offset})
		if res.Type != c.typ || !strings.Contains(res.Content, c.want) {
			t.Errorf("%s: %+v", c.name, res)
		}
	}
	if res, _ := runRead(ctx, onPath(t, p, os.DevNull), readArgs{}); !strings.Contains(res.Content, "not a regular file") {
		t.Errorf("device: %+v", res)
	}
	p.Images = func() bool { return false }
	if res, _ := runRead(ctx, onPath(t, p, filepath.Join(dir, "i.png")), readArgs{}); res.Type != "text" || !strings.Contains(res.Content, "does not take images") {
		t.Errorf("text-only model: %+v", res)
	}
	if res, _ := runShell(ctx, p, shellArgs{Command: "echo out; echo err >&2; echo out2"}); res.Content != "out\nerr\nout2" {
		t.Fatalf("merged output: %q", res.Content)
	}
	// just over the cap: the cut marker is longer than what it cuts
	res, _ := runShell(ctx, p, shellArgs{Command: `head -c 10002 /dev/zero | tr '\0' x; printf '\n\033[31mEND\033[0m\n'`})
	_, name, _ := strings.Cut(res.Content, "END\n(output saved to ")
	if full, err := os.ReadFile(strings.TrimSuffix(name, ")")); err != nil || len(full) < 10002 {
		t.Fatalf("cut output not saved (%v): %q", err, res.Content[len(res.Content)-100:])
	}
	// past the 1 MB buffer the note counts what no spill file holds
	if res, _ := runShell(ctx, p, shellArgs{Command: "seq 1 300000"}); !strings.Contains(res.Content, "more bytes in the middle were not kept") {
		t.Fatalf("buffer drop not counted: %q", res.Content[len(res.Content)-200:])
	}
}

func TestValidationErrors(t *testing.T) {
	p, _ := testPipeline(t, false)
	ag := toolNamed(t, "shell", p)
	resp := call(t, ag, `{"wrong":1}`)
	if !resp.IsError || !strings.Contains(resp.Content, "missing required field") {
		t.Fatalf("validation: %+v", resp)
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
			b, _ := json.Marshal(m, json.Deterministic(true))
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
	p, _ := testPipeline(t, true)
	var posts []string
	p.Post = func(_ context.Context, _, tool string, input map[string]any, ok bool, output string, changed []string, _ time.Duration) string {
		posts = append(posts, fmt.Sprintf("%s ok=%v %s changed=%v", tool, ok, output, changed))
		return "rewritten"
	}
	p.Changes = func(string) func() (string, string, []string) {
		return func() (string, string, []string) { return "", "", []string{"/by/shell"} }
	}
	ag := toolNamed(t, "shell", p)
	if res := call(t, ag, `{"command":"echo posted"}`); res.Content != "rewritten" {
		t.Errorf("the model gets %q, not what post returned", res.Content)
	}
	call(t, ag, `{"command":"echo nope; rm x"}`)
	file := perm.RealPath(filepath.Join(t.TempDir(), "f.txt"))
	call(t, toolNamed(t, "write", p), `{"path":"`+file+`","text":"x"}`)
	if len(posts) != 3 {
		t.Fatalf("post must run for success and denial: %+v", posts)
	}
	for i, want := range []string{"changed=[/by/shell]", "changed=[/by/shell]", "changed=[" + file + "]"} {
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

func TestFitImage(t *testing.T) {
	encode := func(w, h int) []byte {
		var b bytes.Buffer
		png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)))
		return b.Bytes()
	}
	// a jpeg whose SOF declares 0x0, padded past imageMaxBytes so it reaches
	// the shrink path
	zero := func(pad int) []byte {
		var b bytes.Buffer
		jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 8, 8)), nil)
		d := b.Bytes()
		for i := 2; i+9 < len(d) && d[i] == 0xff; i += 2 + int(d[i+2])<<8 + int(d[i+3]) {
			if d[i+1] == 0xc0 {
				clear(d[i+5 : i+9])
			}
		}
		return append(d, make([]byte, pad)...)
	}
	for _, c := range []struct {
		name, mt string
		data     []byte
		want     string // the size sent, or the error
	}{
		{"fits as it is", "image/png", encode(40, 20), "40x20 image/png"},
		{"a long side is shrunk", "image/png", encode(3000, 150), "2000x100 image/png"},
		{"garbage", "image/png", []byte("not a png"), "not a readable image"},
		{"a big webp", "image/webp", make([]byte, _imageMaxBytes+1), "over the 3750000 byte limit"},
		{"too many pixels to decode", "image/png", encode(7000, 6000), "over the 40 megapixels"},
		{"a 0x0 jpeg", "image/jpeg", zero(_imageMaxBytes + 1), "no pixels to scale"},
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
	for range (_editMaxBytes / len(chunk)) + 1 {
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	p := onPath(t, &Pipeline{Seen: &sync.Map{}}, path)
	res, _ := runRead(t.Context(), p, readArgs{Offset: 100_000})
	if !strings.Contains(res.Content, "100000\tx") || !strings.Contains(res.Content, "... (more lines; continue with offset") {
		t.Fatalf("page: %q", res.Content[:min(len(res.Content), 120)])
	}
	res, _ = runEdit(t.Context(), p, editArgs{Search: "x", Replace: "y"})
	if !strings.Contains(res.Content, "over edit's 10 MB limit") {
		t.Fatalf("edit of a huge file: %q", res.Content)
	}
}

// argsInput is an args struct as the pipeline hands it to a tool: the map
// ParseInput's marshalling produces, so a test names fields, not keys.
func argsInput(t *testing.T, args any) map[string]any {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestChangeDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := ChangeDiff(ToolEdit, argsInput(t, editArgs{Path: path, Search: "two", Replace: "TWO"}))
	for _, want := range []string{"@@ -1,3 +1,3 @@", " one", "-two", "+TWO", " three"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff misses %q:\n%s", want, d)
		}
	}
	if d := ChangeDiff(ToolEdit, argsInput(t, editArgs{Path: path, Search: "1\n3", Replace: "2"})); d != "--- "+path+"\n+++ "+path+"\n@@\n-1\n-3\n+2" {
		t.Fatalf("fallback = %q", d)
	}
	if d := ChangeDiff(ToolWrite, argsInput(t, writeArgs{Path: os.DevNull, Text: "a\nb\n"})); d != "--- "+os.DevNull+"\n+++ "+os.DevNull+"\n@@\n+a\n+b" {
		t.Fatalf("device fallback = %q", d)
	}
	if d := ChangeDiff(ToolRead, argsInput(t, readArgs{Path: path})); d != "" {
		t.Fatalf("a tool that changes no file previewed %q", d)
	}
}

func TestFileToolsRefuseSymlink(t *testing.T) {
	p, _ := testPipeline(t, true)
	write, edit := toolNamed(t, "write", p), toolNamed(t, "edit", p)
	dir := perm.RealPath(t.TempDir())
	target, link := filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")
	os.WriteFile(target, []byte("one\n"), 0o644)
	os.Symlink(target, link)
	want := link + " is a symbolic link to " + target + "; edit " + target + " instead"
	for _, c := range []struct {
		tool  fantasy.AgentTool
		input string
	}{
		{write, `{"path":"` + link + `","text":"x"}`},
		{edit, `{"path":"` + link + `","search":"one","replace":"x"}`},
	} {
		if resp := call(t, c.tool, c.input); !resp.IsError || resp.Content != want {
			t.Errorf("%s: %+v", c.tool.Info().Name, resp)
		}
	}
	if data, _ := os.ReadFile(target); string(data) != "one\n" {
		t.Errorf("target changed: %q", data)
	}
}

// TestLeadOrder: a call that opens with the bulk content argument still
// runs, but its output reminds the model to lead with the label argument.
// The keys come from LeadPairs, pinned to the schemas by
// TestLeadPairsAreProperties.
func TestLeadOrder(t *testing.T) {
	for _, tl := range All(nil) {
		lead, bulk := LeadPairs[tl.Info().Name][0], LeadPairs[tl.Info().Name][1]
		if lead == "" || bulk == "" {
			continue
		}
		for _, c := range []struct {
			name, input string
			ok          bool
		}{
			{"label first", `{"` + lead + `":"x","` + bulk + `":"y"}`, true},
			{"content first", `{"` + bulk + `":"y","` + lead + `":"x"}`, false},
			{"content only, no label", `{"` + bulk + `":"y"}`, false},
			{"nested content keys do not count", `{"nested":{"` + bulk + `":"z"},"` + lead + `":"x","` + bulk + `":"y"}`, true},
			{"no object", `true`, true},
		} {
			if got := LeadOK(c.input, lead, bulk); got != c.ok {
				t.Errorf("%s: %s: LeadOK = %v, want %v", tl.Info().Name, c.name, got, c.ok)
			}
		}
	}
	p, _ := testPipeline(t, true)
	run := func(name ToolName, input string) string {
		res, err := toolNamed(t, string(name), p).Run(t.Context(), fantasy.ToolCall{ID: "t1", Name: string(name), Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content
	}
	for _, c := range []struct {
		tool ToolName
		bulk string
	}{
		{ToolShell, "true"},
		{ToolGrep, "x"},
	} {
		lead, bulk := LeadPairs[string(c.tool)][0], LeadPairs[string(c.tool)][1]
		if out := run(c.tool, `{"`+lead+`":"probe","`+bulk+`":"`+c.bulk+`"}`); strings.Contains(out, "system-reminder") {
			t.Errorf("%s: a label-first call was reminded: %q", c.tool, out)
		}
		want := `<system-reminder>Pass "` + lead + `" before "` + bulk + `" in ` + string(c.tool)
		if out := run(c.tool, `{"`+bulk+`":"`+c.bulk+`","`+lead+`":"probe"}`); !strings.Contains(out, want) {
			t.Errorf("%s: a content-first call carried no reminder: %q", c.tool, out)
		}
	}
	lead, bulk := LeadPairs[string(ToolShell)][0], LeadPairs[string(ToolShell)][1]
	for _, c := range []struct {
		name, input, want string
	}{
		{"buries the label", `{"` + bulk + `":"true","` + lead + `":"late"}`, `{"` + lead + `": "late", "` + bulk + `": "true"}`},
		{"keeps raw values", `{"` + bulk + `":"a","` + lead + `":"b","nested":{"` + bulk + `":"z"}}`, `{"` + lead + `": "b", "` + bulk + `": "a", "nested": {"` + bulk + `":"z"}}`},
		{"label first already", `{"` + lead + `":"a","` + bulk + `":"b"}`, `{"` + lead + `":"a","` + bulk + `":"b"}`},
		{"no label", `{"` + bulk + `":"b"}`, `{"` + bulk + `":"b"}`},
		{"no object", `true`, `true`},
		{"unknown tool", `{"x":"y"}`, `{"x":"y"}`},
	} {
		if got := LeadInput(string(ToolShell), c.input); got != c.want {
			t.Errorf("%s: LeadInput = %s, want %s", c.name, got, c.want)
		}
	}
	// read has no bulk key: its label jumps ahead of any other property
	readLead := LeadPairs[string(ToolRead)][0]
	if got := LeadInput(string(ToolRead), `{"limit":5,"`+readLead+`":"p"}`); got != `{"`+readLead+`": "p", "limit": 5}` {
		t.Errorf("read: LeadInput = %s", got)
	}
}
