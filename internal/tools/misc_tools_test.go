// misc_tools_test.go covers the second batch of built-ins: todos, question
// and skill (misc_tools.go), the job_* family (job_tools.go) and the net
// helpers behind fetch/sourcegraph (net_tools.go). No test touches the
// real network: fetch is proven at the URL/permission layer and via a
// loopback httptest server that the SSRF guard must refuse.
package tools

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

func toolNamed(t *testing.T, name string, p *Pipeline) fantasy.AgentTool {
	t.Helper()
	for _, ag := range All(p) {
		if ag.Info().Name == name {
			return ag
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

func TestTodosWriteReadAndValidation(t *testing.T) {
	p, _ := testPipeline(t, false)
	var stored []store.Todo
	p.TodosRead = func() ([]store.Todo, error) { return stored, nil }
	p.TodosWrite = func(list []store.Todo) error { stored = list; return nil }
	todos := toolNamed(t, "todos", p)

	resp := call(t, todos, `{"todos":[
		{"content":"first","status":"pending"},
		{"content":"second","status":"done","active_form":"finishing second"}]}`)
	if resp.IsError || !strings.Contains(resp.Content, "[ ] first") || !strings.Contains(resp.Content, "[x] second") {
		t.Fatalf("write+read: %+v", resp)
	}
	if len(stored) != 2 || stored[1].ActiveForm != "finishing second" || stored[1].Ord != 1 {
		t.Fatalf("stored todos: %+v", stored)
	}

	resp = call(t, todos, `{}`)
	if resp.IsError || !strings.Contains(resp.Content, "[x] second") {
		t.Fatalf("absent todos is read-only: %+v", resp)
	}

	for _, bad := range []string{
		`{"todos":[{"content":"","status":"pending"}]}`,
		`{"todos":[{"content":"x","status":"nope"}]}`,
		`{"todos":["not-an-object"]}`,
	} {
		resp = call(t, todos, bad)
		if !resp.IsError {
			t.Fatalf("bad input must error: %s -> %+v", bad, resp)
		}
	}
	if len(stored) != 2 {
		t.Fatalf("failed writes must not replace the list: %+v", stored)
	}

	broken := &Pipeline{Store: perm.NewStore()}
	resp = call(t, toolNamed(t, "todos", broken), `{}`)
	if !resp.IsError || !strings.Contains(resp.Content, "todos unavailable") {
		t.Fatalf("nil read hook: %+v", resp)
	}
}

// TestQuestionAndPlanTools pins what the model reads back from the user
// tools: answers in order without the "(Recommended)" marker, a dismissal
// that ends the turn, and guidance instead of an error when nobody can
// answer.
func TestQuestionAndPlanTools(t *testing.T) {
	const two = `{"questions":[{"question":"Which path?","options":[{"label":"A (Recommended)","description":"first"},{"label":"B"}]},
		{"question":"Checks?","multi_select":true,"options":[{"label":"tests"},{"label":"vet"}]}]}`
	var asked []Question
	answer := func(ans []string, ok bool) func([]Question) ([]string, bool) {
		return func(qs []Question) ([]string, bool) { asked = qs; return ans, ok }
	}
	verdict := func(v PlanVerdict, ok bool) func(string) (PlanVerdict, bool) {
		return func(string) (PlanVerdict, bool) { return v, ok }
	}
	for _, c := range []struct {
		name, tool, input string
		question          func([]Question) ([]string, bool)
		plan              func(string) (PlanVerdict, bool)
		want              string
		isErr, stop       bool
	}{
		{"answers in order", "question", two, answer([]string{"A (Recommended)", "tests, vet"}, true), nil,
			"The user answered:\n- \"Which path?\": A\n- \"Checks?\": tests, vet", false, false},
		{"dismissal ends the turn", "question", two, answer(nil, false), nil, "dismissed", true, true},
		{"no user: decide and continue", "question", two, nil, nil, "No user is available", false, false},
		{"too many questions", "question", `{"questions":[{"question":"1"},{"question":"2"},{"question":"3"},{"question":"4"},{"question":"5"}]}`, answer(nil, true), nil, "1 to 4", true, false},
		{"option without label", "question", `{"questions":[{"question":"x","options":[{"description":"no label"}]}]}`, answer(nil, true), nil, "options[0].label", true, false},
		{"approved in plan mode", "plan", `{"plan":"1. do it"}`, nil, verdict(PlanVerdict{Approved: true, Mode: "auto", Path: "/p.md"}, true),
			"plan mode is off; the permission mode is now auto. Implement it now", false, false},
		{"changes requested", "plan", `{"plan":"1. do it"}`, nil, verdict(PlanVerdict{Feedback: "use sqlite"}, true), "asked for changes: use sqlite", false, false},
		{"plan dismissed", "plan", `{"plan":"1. do it"}`, nil, verdict(PlanVerdict{}, false), "dismissed the plan", true, true},
		{"plan with no user", "plan", `{"plan":"1. do it"}`, nil, nil, "Put the plan in your reply", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := testPipeline(t, false)
			p.Question, p.ApprovePlan = c.question, c.plan
			resp := call(t, toolNamed(t, c.tool, p), c.input)
			if resp.IsError != c.isErr || resp.StopTurn != c.stop || !strings.Contains(resp.Content, c.want) {
				t.Fatalf("got %+v, want %q error=%v stop=%v", resp, c.want, c.isErr, c.stop)
			}
		})
	}
	if len(asked) != 2 || !asked[1].Multi || asked[0].Options[0].Description != "first" {
		t.Fatalf("question plumbing: %+v", asked)
	}
}

func TestSkillLoadIndexAndGate(t *testing.T) {
	p, _ := testPipeline(t, false)
	data := t.TempDir()
	cfg := t.TempDir()
	mk := func(root, name, body string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	alpha := mk(data, "alpha", "---\r\nname: alpha\r\ndescription: the alpha skill\r\n---\r\n\r\nalpha body")
	beta := mk(data, "beta", "beta body")
	dupCfg := mk(cfg, "dup", "config body")
	mk(data, "dup", "data body")

	p.Skills = func() []SkillInfo {
		return []SkillInfo{
			{Name: "dup", Path: dupCfg},
			{Name: "dup", Path: filepath.Join(data, "dup")},
			{Name: "alpha", Path: alpha},
			{Name: "beta", Path: beta},
		}
	}
	p.Store.Add(perm.Rule{Subject: "fs.read", Pattern: data + "/*", Decision: perm.Allow})
	p.Store.Add(perm.Rule{Subject: "fs.read", Pattern: cfg + "/*", Decision: perm.Allow})
	skill := toolNamed(t, "skill", p)

	resp := call(t, skill, `{"name":"alpha"}`)
	if resp.IsError || resp.Content != "Skill directory: "+alpha+"\n\nalpha body" {
		t.Fatalf("load alpha must strip frontmatter and name the directory: %+v", resp)
	}
	resp = call(t, skill, `{"name":"dup"}`)
	if resp.IsError || !strings.Contains(resp.Content, "config body") {
		t.Fatalf("duplicate name must resolve to the first (config dir) entry: %+v", resp)
	}

	resp = call(t, skill, `{"name":"zeta"}`)
	if !resp.IsError || !strings.Contains(resp.Content, `no such skill "zeta"; available: alpha, beta, dup`) {
		t.Fatalf("unknown name must list sorted skills: %+v", resp)
	}

	p.Store.Add(perm.Rule{Subject: "fs.read", Pattern: beta + "/SKILL.md", Decision: perm.Deny})
	resp = call(t, skill, `{"name":"beta"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "permission denied") {
		t.Fatalf("denied read: %+v", resp)
	}

	p.Skills = nil
	resp = call(t, skill, `{"name":"alpha"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "skills unavailable") {
		t.Fatalf("nil index: %+v", resp)
	}
}

func waitJobDone(t *testing.T, j *Job) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for j.Running() {
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not exit in time", j.ID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestJobToolsLifecycle(t *testing.T) {
	dir := t.TempDir()
	p, _ := testPipeline(t, true)
	SetWorkdir(dir)
	p.Jobs = NewJobs()
	start := toolNamed(t, "job_start", p)
	read := toolNamed(t, "job_read", p)
	list := toolNamed(t, "job_list", p)
	kill := toolNamed(t, "job_kill", p)

	resp := call(t, start, `{"command":"echo","args":["hello-job"]}`)
	if resp.IsError || !strings.Contains(resp.Content, "started job-") {
		t.Fatalf("start echo: %+v", resp)
	}
	j := p.Jobs.List()[0]
	waitJobDone(t, j)
	resp = call(t, read, `{"id":"`+j.ID+`"}`)
	if resp.IsError || !strings.Contains(resp.Content, "exited (0)") || !strings.Contains(resp.Content, "hello-job") {
		t.Fatalf("read echo: %+v", resp)
	}
	resp = call(t, list, `{}`)
	if resp.IsError || !strings.Contains(resp.Content, "echo hello-job") {
		t.Fatalf("list: %+v", resp)
	}

	resp = call(t, start, `{"command":"sh","args":["-c","printf out; printf err >&2; exit 3"],"env":{"ZEMI_T1":"v"}}`)
	if resp.IsError {
		t.Fatalf("start sh: %+v", resp)
	}
	j = p.Jobs.List()[1]
	waitJobDone(t, j)
	resp = call(t, read, `{"id":"`+j.ID+`"}`)
	if !strings.Contains(resp.Content, "exited (3)") ||
		!strings.Contains(resp.Content, "out") || !strings.Contains(resp.Content, "err") {
		t.Fatalf("read sh: %+v", resp)
	}

	resp = call(t, start, `{"command":"sleep","args":["30"]}`)
	if resp.IsError {
		t.Fatalf("start sleep: %+v", resp)
	}
	j = p.Jobs.List()[2]
	resp = call(t, kill, `{"id":"`+j.ID+`"}`)
	if resp.IsError || !strings.Contains(resp.Content, "killed") {
		t.Fatalf("kill: %+v", resp)
	}
	waitJobDone(t, j)
	resp = call(t, kill, `{"id":"`+j.ID+`"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "already exited") {
		t.Fatalf("kill exited: %+v", resp)
	}
	resp = call(t, kill, `{"id":"job-999"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "no such job") {
		t.Fatalf("kill unknown: %+v", resp)
	}

	resp = call(t, start, `{"command":"echo","args":["x"],"env":{"MY_SECRET":"1"}}`)
	if !resp.IsError || !strings.Contains(resp.Content, "refused by the environment gate") {
		t.Fatalf("secret env extra must be refused: %+v", resp)
	}
}

func TestJobNilHooksFailClosed(t *testing.T) {
	p, _ := testPipeline(t, true)
	for _, tc := range []struct{ name, input string }{
		{"job_start", `{"command":"echo"}`},
		{"job_read", `{"id":"job-1"}`},
		{"job_list", `{}`},
		{"job_kill", `{"id":"job-1"}`},
	} {
		resp := call(t, toolNamed(t, tc.name, p), tc.input)
		if !resp.IsError || !strings.Contains(resp.Content, "jobs unavailable") {
			t.Fatalf("%s nil jobs: %+v", tc.name, resp)
		}
	}
}

func TestParseNetURL(t *testing.T) {
	for _, raw := range []string{
		"ftp://example.com/x",
		"file:///etc/passwd",
		"http://user:pass@example.com/",
		"https://example.com@evil.example/",
		"http://",
		"http:///path",
	} {
		if _, err := parseNetURL(raw); err == nil {
			t.Fatalf("parseNetURL(%q) must refuse", raw)
		}
	}
	for _, raw := range []string{"http://example.com/", "https://example.com:8443/x?y=1"} {
		if _, err := parseNetURL(raw); err != nil {
			t.Fatalf("parseNetURL(%q): %v", raw, err)
		}
	}
}

func TestIPDenied(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "127.8.8.8": true, "10.1.2.3": true,
		"172.16.5.5": true, "172.31.255.255": true, "172.32.0.1": false,
		"192.168.0.1": true, "169.254.1.1": true, "0.1.2.3": true,
		"100.64.0.1": true, "100.128.0.1": false, "224.0.0.1": true,
		"8.8.8.8": false, "::1": true, "fe80::1": true, "fc00::1": true,
		"::ffff:127.0.0.1": true, "::ffff:8.8.8.8": false,
		"2606:4700::1": false, "::": true, "64:ff9b::1.2.3.4": true,
	}
	for in, want := range cases {
		if got := ipDenied(net.ParseIP(in)); got != want {
			t.Fatalf("ipDenied(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestFetchGates(t *testing.T) {
	p, _ := testPipeline(t, false)
	fetch := toolNamed(t, "fetch", p)
	resp := call(t, fetch, `{"url":"https://example.com/"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "permission denied") {
		t.Fatalf("denied net gate must stop fetch before dialing: %+v", resp)
	}

	past, _ := testPipeline(t, true)
	for _, raw := range []string{"ftp://example.com/", "http://user:pass@example.com/"} {
		res, err := runFetch(context.Background(), past, map[string]any{"url": raw})
		if err != nil {
			t.Fatalf("runFetch(%q) go error: %v", raw, err)
		}
		if !res.IsError {
			t.Fatalf("runFetch(%q) must refuse: %+v", raw, res)
		}
	}

	allowed, _ := testPipeline(t, true)
	srv := httptest.NewServer(nil)
	defer srv.Close()
	res, err := runFetch(context.Background(), allowed, map[string]any{"url": srv.URL + "/x"})
	if err != nil {
		t.Fatalf("runFetch loopback go error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "refused") {
		t.Fatalf("loopback target must be refused at dial time: %+v", res)
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<html><head><title>hidden</title></head>` +
		`<body><!-- comment --><h1>Title</title>` +
		`<script>alert("x")</script><style>.c{}</style>` +
		`<p>Hello &amp; <b>world</b><br>line2</p></body></html>`
	out := htmlToText(in)
	for _, bad := range []string{"script", "hidden", "alert", ".c{}", "<b>"} {
		if strings.Contains(out, bad) {
			t.Fatalf("htmlToText kept %q: %q", bad, out)
		}
	}
	for _, want := range []string{"Hello & world", "line2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("htmlToText lost %q: %q", want, out)
		}
	}
}

func TestParseSGStream(t *testing.T) {
	body := "event: matches\n" +
		`data: [{"repository":"a/b","path":"x.go","lineMatches":[{"line":"func Foo()","lineNumber":3}]}]` + "\n\n" +
		"event: matches\n" +
		`data: [{"repository":"c/d","path":"README.md"}]` + "\n\n" +
		"event: progress\n\n" +
		"garbage\n\n"
	out := parseSGStream(strings.NewReader(body), 10)
	want := "a/b -> x.go:3: func Foo()\nc/d -> README.md\n"
	if out != want {
		t.Fatalf("parseSGStream = %q, want %q", out, want)
	}
	if out := parseSGStream(strings.NewReader(body), 1); out != "a/b -> x.go:3: func Foo()\n" {
		t.Fatalf("count cap: %q", out)
	}
	if out := parseSGStream(strings.NewReader("event: done\n\n"), 5); out != "(no matches)" {
		t.Fatalf("empty stream: %q", out)
	}
}
