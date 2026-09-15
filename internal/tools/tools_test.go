package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
)

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
	t.Cleanup(func() { SetWorkdir("") })
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

func TestEditTolerantWhitespace(t *testing.T) {
	p, _ := testPipeline(t, false,
		perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow},
		perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow},
	)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("func  main() {\n\treturn\n}\n"), 0o644)
	edit := All(p)[3]
	resp := call(t, edit, `{"path":"`+file+`","old_string":"func main() {\n    return\n}","new_string":"func main() {\n    return nil\n}"}`)
	if resp.IsError {
		t.Fatalf("tolerant edit: %+v", resp)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "return nil") {
		t.Fatalf("tolerant edit not applied: %q", data)
	}
}

func TestEditNotFound(t *testing.T) {
	p, _ := testPipeline(t, false, perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("content"), 0o644)
	edit := All(p)[3]
	resp := call(t, edit, `{"path":"`+file+`","old_string":"nope","new_string":"x"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "not found") {
		t.Fatalf("missing match must error: %+v", resp)
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

func TestGlobAndGrep(t *testing.T) {
	p, _ := testPipeline(t, false, perm.Rule{Subject: "fs.read", Pattern: "*", Decision: perm.Allow})
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub", "node_modules"), 0o755)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "x.go"), []byte("package sub\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "node_modules", "y.go"), []byte("package y\n"), 0o644)

	glob, grep := All(p)[5], All(p)[6]

	resp := call(t, glob, `{"pattern":"*.go","path":"`+dir+`"}`)
	if strings.Contains(resp.Content, "node_modules") {
		t.Fatalf("glob must skip node_modules: %+v", resp)
	}
	if !strings.Contains(resp.Content, "main.go") || !strings.Contains(resp.Content, "x.go") {
		t.Fatalf("glob misses files: %+v", resp)
	}

	resp = call(t, grep, `{"pattern":"package sub","path":"`+dir+`"}`)
	if !strings.Contains(resp.Content, "x.go:1") {
		t.Fatalf("grep: %+v", resp)
	}
	resp = call(t, grep, `{"pattern":"package","path":"`+dir+`","include":"[m]*.go"}`)
	if !strings.Contains(resp.Content, "main.go:1") || strings.Contains(resp.Content, "x.go") {
		t.Fatalf("grep include: %+v", resp)
	}
	resp = call(t, grep, `{"pattern":"package y","path":"`+dir+`"}`)
	if !strings.Contains(resp.Content, "no matches") {
		t.Fatalf("grep must skip node_modules: %+v", resp)
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
