// misc_tools_test.go covers the second batch of built-ins: todos, question
// and skill (misc_tools.go), the job_* family (job_tools.go) and the net
// helpers behind fetch/web_search (net_tools.go). No test touches the
// real network: fetch is proven at the URL/permission layer and via a
// loopback httptest server that the SSRF guard must refuse.
package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	t.Chdir(dir)
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
		if _, err := ParseNetURL(raw); err == nil {
			t.Fatalf("ParseNetURL(%q) must refuse", raw)
		}
	}
	for _, raw := range []string{"http://example.com/", "https://example.com:8443/x?y=1"} {
		if _, err := ParseNetURL(raw); err != nil {
			t.Fatalf("ParseNetURL(%q): %v", raw, err)
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

	allowed, asked := testPipeline(t, true)
	srv := httptest.NewServer(nil)
	defer srv.Close()
	res := call(t, toolNamed(t, "fetch", allowed), fmt.Sprintf(`{"url":%q}`, srv.URL+"/x"))
	if !res.IsError || !strings.Contains(res.Content, "refused") {
		t.Fatalf("loopback target must be refused at dial time: %+v", res)
	}
	if len(*asked) != 1 {
		t.Fatalf("one fetch must ask once: %+v", *asked)
	}

	// the rules an "always" answer writes also cover a redirect hop
	hops, hopAsks := testPipeline(t, false,
		perm.Rule{Subject: "net.example.com", Pattern: "https://example.com/*", Decision: perm.Allow})
	if err := netGate(hops, &url.URL{Scheme: "https", Host: "example.com", Path: "/next"}, "fetch redirect"); err != nil || len(*hopAsks) != 0 {
		t.Fatalf("hop under an allowed url: %v, asked %+v", err, *hopAsks)
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<html><head><title>hidden</title></head>` +
		`<body><!-- comment --><nav><a href="/menu">Menu</a></nav><header><h1>Title</h1></header>` +
		`<script>alert("x")</script><style>.c{}</style>` +
		`<p>Hello &amp; <b>world</b><br>line2 <a class="x" href="../docs?a=1&amp;b=2"><b>Docs</b></a>` +
		` <a href="#top">Top</a> <a href="javascript:void(0)">JS</a></p></body></html>`
	base, _ := url.Parse("https://example.com/blog/post")
	out := htmlToText(in, base)
	for _, bad := range []string{"script", "hidden", "alert", ".c{}", "<b>", "Menu", "#top", "javascript"} {
		if strings.Contains(out, bad) {
			t.Fatalf("htmlToText kept %q: %q", bad, out)
		}
	}
	for _, want := range []string{"Title", "Hello & world", "line2", "[Docs](https://example.com/docs?a=1&b=2)", "Top", "JS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("htmlToText lost %q: %q", want, out)
		}
	}
}

func TestSearchParsers(t *testing.T) {
	lite := `<table><tr><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc&amp;rut=x" class='result-link'>The <b>Go</b> docs</a></td></tr>
<tr><td class='result-snippet'>Learn &amp; build</td></tr>
<tr><td><a rel="nofollow" href="https://pkg.go.dev/" class='result-link'>pkg.go.dev</a></td></tr></table>`
	hits, err := parseDDGLite(200, lite)
	if err != nil {
		t.Fatal(err)
	}
	if got := formatHits(hits, 10); got != "1. The Go docs\n   https://go.dev/doc\n   Learn & build\n2. pkg.go.dev\n   https://pkg.go.dev/\n" {
		t.Fatalf("lite: %q", got)
	}
	if got := formatHits(hits, 1); strings.Contains(got, "pkg.go.dev") {
		t.Fatalf("limit: %q", got)
	}
	for _, c := range []struct {
		status int
		page   string
	}{{202, ""}, {200, `<div class="anomaly-modal__mask">`}} {
		if _, err := parseDDGLite(c.status, c.page); err == nil || !strings.Contains(err.Error(), "bot check") {
			t.Fatalf("bot check %d must be an error, got %v", c.status, err)
		}
	}
	sx, err := parseSearxng([]byte(`{"results":[{"title":"A","url":"https://a.example/","content":"about a"}]}`))
	if err != nil || formatHits(sx, 10) != "1. A\n   https://a.example/\n   about a\n" {
		t.Fatalf("searxng: %q %v", formatHits(sx, 10), err)
	}
	if got := formatHits(nil, 10); got != "(no results)" {
		t.Fatalf("empty: %q", got)
	}
}

func TestOpenInEditor(t *testing.T) {
	p, _ := testPipeline(t, true)
	p.Seen = &sync.Map{}
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	write := func(s string) func(context.Context, string, int, string) error {
		return func(_ context.Context, got string, line int, title string) error {
			if got != path || line != 2 || title != "fix it" {
				t.Fatalf("hook got %q line %d title %q", got, line, title)
			}
			return os.WriteFile(path, []byte(s), 0o644)
		}
	}
	in := func() string { return fmt.Sprintf(`{"path":%q,"line":2,"reason":"fix it"}`, path) }

	resp := call(t, toolNamed(t, "open_in_editor", p), in())
	if !strings.Contains(resp.Content, "No editor is available") {
		t.Fatalf("headless: %+v", resp)
	}

	p.Edit = write("")
	resp = call(t, toolNamed(t, "open_in_editor", p), in())
	if _, err := os.Stat(path); !os.IsNotExist(err) || !strings.Contains(resp.Content, "not kept") {
		t.Fatalf("empty new file must be removed: %+v %v", resp, err)
	}

	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Edit = write("a\nc\n")
	resp = call(t, toolNamed(t, "open_in_editor", p), in())
	if !strings.Contains(resp.Content, "-b") || !strings.Contains(resp.Content, "+c") {
		t.Fatalf("diff: %+v", resp)
	}
	if fi, _ := os.Stat(path); p.stale(path, fi) != "" {
		t.Fatal("the edited file must count as read")
	}

	p.Edit = func(context.Context, string, int, string) error { return errors.New("no tmux client") }
	if resp = call(t, toolNamed(t, "open_in_editor", p), in()); !resp.StopTurn || !strings.Contains(resp.Content, "no tmux client") {
		t.Fatalf("editor failure must stop the turn: %+v", resp)
	}
}
