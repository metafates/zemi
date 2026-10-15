// misc_tools_test.go covers the second batch of built-ins: todos, question
// and skill (misc_tools.go), the job_* family (job_tools.go) and the net
// helpers behind fetch (net_tools.go). No test touches the
// real network: fetch is proven at the URL/permission layer and via a
// loopback httptest server.

package tools

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/runtime"
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
	if resp.IsError || resp.Content != "updated todos: 1/2 done" {
		t.Fatalf("write acks with a count: %+v", resp)
	}
	if len(stored) != 2 || stored[1].ActiveForm != "finishing second" {
		t.Fatalf("stored todos: %+v", stored)
	}

	resp = call(t, todos, `{}`)
	if resp.IsError || resp.Content != "- [pending] first\n- [done] second\n" {
		t.Fatalf("absent todos is read-only: %+v", resp)
	}
	resp = call(t, todos, `{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"in_progress"}]}`)
	if resp.IsError || len(stored) != 2 || resp.Content != "updated todos: 0/2 done; 2 items are in_progress; keep exactly one" {
		t.Fatalf("two in_progress: stored %d, %+v", len(stored), resp)
	}
	resp = call(t, todos, `{"todos":[{"content":"a","status":"cancelled"},{"content":"b","status":"pending"}]}`)
	if resp.IsError || len(stored) != 1 || stored[0].Content != "b" || resp.Content != "updated todos: 0/1 done; dropped 1 cancelled" {
		t.Fatalf("cancelled items drop: stored %+v, %+v", stored, resp)
	}

	for _, bad := range []struct{ in, want string }{
		{`{"todos":[{"content":"","status":"pending"}]}`, "todo 1: content is required"},
		{`{"todos":[{"content":"x","status":"pending"},{"content":"x","status":"nope"}]}`, "todo 2: status must be"},
		{`{"todos":["not-an-object"]}`, "todo 1: not an object"},
	} {
		resp = call(t, todos, bad.in)
		if !resp.IsError || !strings.HasPrefix(resp.Content, bad.want) {
			t.Fatalf("bad input must error: %s -> %+v", bad.in, resp)
		}
	}
	if len(stored) != 1 {
		t.Fatalf("failed writes must not replace the list: %+v", stored)
	}

	broken := &Pipeline{}
	resp = call(t, toolNamed(t, "todos", broken), `{}`)
	if !resp.IsError || !strings.Contains(resp.Content, "no todo list in this run") {
		t.Fatalf("nil read hook: %+v", resp)
	}
}

// TestQuestionAndPlanTools pins what the model reads back from the user
// tools: answers in order without the "(Recommended)" marker, a dismissal
// that ends the turn, and guidance instead of an error when nobody can
// answer.
func TestQuestionAndPlanTools(t *testing.T) {
	const two = `{"questions":[{"type":"single_choice","question":"Which path?","options":[{"label":"A (Recommended)","description":"first"},{"label":"B"}]},
		{"type":"multi_choice","question":"Checks?","options":[{"label":"tests"},{"label":"vet"}]}]}`
	var asked []Question
	answer := func(ans []Answer, ok bool) func([]Question) ([]Answer, bool) {
		return func(qs []Question) ([]Answer, bool) { asked = qs; return ans, ok }
	}
	verdict := func(v PlanVerdict, ok bool) func(string) (PlanVerdict, bool) {
		return func(string) (PlanVerdict, bool) { return v, ok }
	}
	for _, c := range []struct {
		name, tool, input string
		question          func([]Question) ([]Answer, bool)
		plan              func(string) (PlanVerdict, bool)
		want              string
		isErr, stop       bool
	}{
		{
			"answers in order, a note kept", "question", two, answer([]Answer{{Picked: []string{"A"}, Note: "only here"}, {Picked: []string{"tests", "vet"}}}, true), nil,
			`{"answers":[{"question":"Which path?","answer":"A","note":"only here"},{"question":"Checks?","answer":["tests","vet"]}]}`, false, false,
		},
		{
			"an open answer is null and explained", "question", two, answer([]Answer{{Typed: "C"}, {}}, true), nil,
			`{"answers":[{"question":"Which path?","answer":"C"},{"question":"Checks?","answer":null}],"open":"a null answer is yours`, false, false,
		},
		{"dismissal ends the turn", "question", two, answer(nil, false), nil, "dismissed", true, true},
		{"no user: decide and continue", "question", two, nil, nil, "No user is available", false, false},
		{"too many questions", "question", `{"questions":[{"type":"single_choice","question":"1"},{"type":"single_choice","question":"2"},{"type":"single_choice","question":"3"},{"type":"single_choice","question":"4"},{"type":"single_choice","question":"5"}]}`, answer(nil, true), nil, "1 to 4", true, false},
		{"option without label", "question", `{"questions":[{"type":"single_choice","question":"x","options":[{"description":"no label"}]}]}`, answer(nil, true), nil, "options[0].label", true, false},
		{
			"approved returns to the agent", "plan", `{"plan":"1. do it"}`, nil, verdict(PlanVerdict{Approved: true, Profile: "default", Path: "/p.md"}, true),
			"you are back as the default agent. Implementation runs in the next turn", false, true,
		},
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
	if o := asked[0].Options; len(asked) != 2 || asked[1].Type != TypeMulti || o[0] != (QuestionOption{"A", "first", true}) || o[1].Recommended {
		t.Fatalf("question plumbing: %+v", asked)
	}
	p, _ := testPipeline(t, false)
	var defaulted []Question
	p.Question = func(qs []Question) ([]Answer, bool) { defaulted = qs; return []Answer{{Picked: []string{"A"}}}, true }
	call(t, toolNamed(t, "question", p), `{"questions":[{"question":"x","options":[{"label":"A"}]},{"question":"y"}]}`)
	if defaulted[0].Type != TypeSingle || defaulted[1].Type != TypeFreeText {
		t.Fatalf("an omitted type reads from the shape: %+v", defaulted)
	}
}

// TestPlanOutcome: a plan row reads the verdict back out of each result the
// plan tool writes for the model.
func TestPlanOutcome(t *testing.T) {
	for _, c := range []struct {
		name          string
		v             PlanVerdict
		verdict, path string
		stop          bool
	}{
		{"approved and saved", PlanVerdict{Approved: true, Path: "/p.md"}, "approved", "/p.md", false},
		{"approved, back to the agent", PlanVerdict{Approved: true, Profile: "default"}, "approved", "", true},
		{"approved and held", PlanVerdict{Approved: true, Hold: true, Path: "/p.md"}, "approved", "/p.md", true},
		{"changes over two lines", PlanVerdict{Feedback: "use sqlite\nand go"}, "changes requested: use sqlite\nand go", "", false},
	} {
		p, _ := testPipeline(t, false)
		p.ApprovePlan = func(string) (PlanVerdict, bool) { return c.v, true }
		resp := call(t, toolNamed(t, "plan", p), `{"plan":"1. do it"}`)
		if v, path := PlanOutcome(resp.Content); v != c.verdict || path != c.path {
			t.Errorf("%s: verdict %q, path %q", c.name, v, path)
		}
		if resp.StopTurn != c.stop {
			t.Errorf("%s: stop %v", c.name, resp.StopTurn)
		}
		// a held plan is kept, never started: the model is not told to implement it
		if c.v.Hold && strings.Contains(resp.Content, ImplementPlan) {
			t.Errorf("%s: told to implement a held plan: %q", c.name, resp.Content)
		}
	}
}

// TestQuestionAnswers: a question row reads each question, answer and note
// back out of the result the question tool writes for the model.
func TestQuestionAnswers(t *testing.T) {
	for _, c := range []struct {
		name    string
		answers []Answer
		ok      bool
		want    [][3]string
	}{
		{
			"quotes, a newline, marks, a note and an open one",
			[]Answer{{Typed: `say "hi"`}, {Picked: []string{"tests", "a, b"}, Note: "only <here>"}, {}},
			true,
			[][3]string{{"Name \"it\"?\nor not", `say "hi"`, ""}, {"Checks?", "tests, a, b", "only <here>"}, {"Why?", "", ""}},
		},
		{"dismissed", nil, false, nil},
	} {
		p, _ := testPipeline(t, false)
		p.Question = func([]Question) ([]Answer, bool) { return c.answers, c.ok }
		resp := call(t, toolNamed(t, "question", p), `{"questions":[{"type":"free_text","question":"Name \"it\"?\nor not"},{"type":"multi_choice","question":"Checks?","options":[{"label":"tests"},{"label":"a, b"}]},{"type":"free_text","question":"Why?"}]}`)
		if got := QuestionAnswers(resp.Content); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q from %q", c.name, got, resp.Content)
		}
	}
}

func TestSkillLoadIndexAndGate(t *testing.T) {
	p, _ := testPipeline(t, false)
	p.Seen = &sync.Map{}
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
	skill := toolNamed(t, "skill", p)

	resp := call(t, skill, `{"name":"alpha"}`)
	if resp.IsError || resp.Content != "Skill directory: "+alpha+"\n\nalpha body" {
		t.Fatalf("load alpha must strip frontmatter and name the directory: %+v", resp)
	}
	// a loaded body was read: an edit of it needs no read first
	if fi, _ := os.Stat(filepath.Join(alpha, "SKILL.md")); p.stale(filepath.Join(alpha, "SKILL.md"), fi, false) != "" {
		t.Fatal("a loaded skill must count as read")
	}
	resp = call(t, skill, `{"name":"dup"}`)
	if resp.IsError || !strings.Contains(resp.Content, "config body") {
		t.Fatalf("duplicate name must resolve to the first (config dir) entry: %+v", resp)
	}

	resp = call(t, skill, `{"name":"zeta"}`)
	if !resp.IsError || !strings.Contains(resp.Content, `no such skill "zeta"; available: alpha, beta, dup`) {
		t.Fatalf("unknown name must list sorted skills: %+v", resp)
	}

	// a cut names lines of SKILL.md itself, so a read at that offset resumes it
	var big strings.Builder
	big.WriteString("---\nname: big\ndescription: d\n---\n\n")
	for i := range 4000 {
		fmt.Fprintf(&big, "step %05d of the procedure\n", i)
	}
	bigDir := mk(data, "big", big.String())
	p.Skills = func() []SkillInfo { return []SkillInfo{{Name: "big", Path: bigDir}, {Name: "beta", Path: beta}} }
	resp = call(t, skill, `{"name":"big"}`)
	var a int
	fmt.Sscanf(resp.Content[strings.Index(resp.Content, "... (lines ")+len("... (lines "):], "%d", &a)
	lines := strings.Split(big.String(), "\n")
	if a < 6 || !strings.Contains(resp.Content, lines[a-2]+"\n... (lines") || strings.Contains(resp.Content, lines[a-1]+"\n") {
		t.Fatalf("cut at line %d does not match SKILL.md:\n%s", a, resp.Content[:200])
	}

	p.Skills = nil
	resp = call(t, skill, `{"name":"alpha"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "skills unavailable") {
		t.Fatalf("nil index: %+v", resp)
	}
}

// TestBuiltinSkillFilesFitOneRead: a SKILL.md sends the model to the files
// beside it, which it reads whole only when one read page holds them.
func TestBuiltinSkillFilesFitOneRead(t *testing.T) {
	err := fs.WalkDir(runtime.Skills, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "SKILL.md" {
			return err
		}
		data, err := fs.ReadFile(runtime.Skills, path)
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		// a page row is the line behind a 7-byte number column
		if page := len(data) + 7*len(lines); page > readMaxBytes || len(lines) > readMaxLines {
			t.Errorf("%s is a %d byte page of %d lines, one read holds %d bytes and %d lines", path, page, len(lines), readMaxBytes, readMaxLines)
		}
		for i, line := range lines {
			if len(line) > readMaxLineLen {
				t.Errorf("%s:%d is %d bytes, read cuts a line at %d", path, i+1, len(line), readMaxLineLen)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
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
	exited := make(chan struct{}, 8)
	p.Jobs.OnExit = func() { exited <- struct{}{} }
	start := toolNamed(t, "job_start", p)
	read := toolNamed(t, "job_read", p)
	list := toolNamed(t, "job_list", p)
	kill := toolNamed(t, "job_kill", p)

	resp := call(t, start, `{"command":"echo hello-job"}`)
	if resp.IsError || !strings.Contains(resp.Content, "started job-") {
		t.Fatalf("start echo: %+v", resp)
	}
	j := p.Jobs.List()[0]
	// the exit hook fires once the job is done: the statusline redraws on it
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("OnExit did not run")
	}
	waitJobDone(t, j)
	if p.Jobs.Running() != 0 {
		t.Fatal("OnExit ran before the exit was recorded")
	}
	resp = call(t, read, `{"id":"`+j.ID+`"}`)
	if resp.IsError || !strings.Contains(resp.Content, "exited (0)") || !strings.Contains(resp.Content, "hello-job") {
		t.Fatalf("read echo: %+v", resp)
	}
	resp = call(t, list, `{}`)
	if resp.IsError || !strings.Contains(resp.Content, "echo hello-job") {
		t.Fatalf("list: %+v", resp)
	}

	resp = call(t, start, `{"command":"printf out; printf err >&2; exit 3"}`)
	if resp.IsError {
		t.Fatalf("start sh: %+v", resp)
	}
	j = p.Jobs.List()[1]
	waitJobDone(t, j)
	resp = call(t, read, `{"id":"`+j.ID+`"}`)
	if !strings.Contains(resp.Content, "exited (3)") ||
		!strings.Contains(resp.Content, "[stdout]\nout") || !strings.Contains(resp.Content, "[stderr]\nerr") {
		t.Fatalf("read sh: %+v", resp)
	}

	// wait_ms returns once new output goes quiet, well before it elapses
	resp = call(t, start, `{"command":"sleep 0.3; echo late; sleep 30"}`)
	if resp.IsError {
		t.Fatalf("start late: %+v", resp)
	}
	j = p.Jobs.List()[2]
	began := time.Now()
	resp = call(t, read, `{"id":"`+j.ID+`","wait_ms":10000}`)
	if took := time.Since(began); resp.IsError || !strings.Contains(resp.Content, "late") || took > 5*time.Second {
		t.Fatalf("read with wait_ms: %+v after %s", resp, took)
	}
	resp = call(t, kill, `{"id":"`+j.ID+`"}`)
	if resp.IsError {
		t.Fatalf("kill late: %+v", resp)
	}
	waitJobDone(t, j)

	// a chatty job is read whole in one wait, not one call per line
	resp = call(t, start, `{"command":"for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do echo line$i; sleep 0.1; done"}`)
	if resp.IsError {
		t.Fatalf("start chatty: %+v", resp)
	}
	j = p.Jobs.List()[3]
	resp = call(t, read, `{"id":"`+j.ID+`","wait_ms":10000}`)
	if !strings.Contains(resp.Content, "exited (0)") || !strings.Contains(resp.Content, "\nline1\n") || strings.Contains(resp.Content, "[stdout]") || !strings.Contains(resp.Content, "line15") {
		t.Fatalf("read chatty: %+v", resp)
	}

	resp = call(t, start, `{"command":"sleep 30"}`)
	if resp.IsError {
		t.Fatalf("start sleep: %+v", resp)
	}
	j = p.Jobs.List()[4]
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

func TestFetchGates(t *testing.T) {
	p, asked := testPipeline(t, false)
	fetch := toolNamed(t, "fetch", p)
	resp := call(t, fetch, `{"url":"https://example.com/"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "permission denied") {
		t.Fatalf("denied net gate must stop fetch before dialing: %+v", resp)
	}
	if len(*asked) != 1 {
		t.Fatalf("one ask: %+v", *asked)
	}

	// a URL the tool cannot fetch is its own error, never an ask
	past, pastAsked := testPipeline(t, true)
	for _, raw := range []string{"ftp://example.com/", "http://user:pass@example.com/", "not a url"} {
		res := call(t, toolNamed(t, "fetch", past), fmt.Sprintf(`{"url":%q}`, raw))
		if !res.IsError || strings.Contains(res.Content, "permission") || len(*pastAsked) != 0 {
			t.Fatalf("fetch(%q) must refuse before the gate: %+v asked %+v", raw, res, *pastAsked)
		}
	}

	allowed, _ := testPipeline(t, true)
	mux := http.NewServeMux()
	mux.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<p>hi</p>")
	})
	mux.HandleFunc("/j", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, "{}")
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(fetchMax+10))
		io.WriteString(w, strings.Repeat("x\n", fetchMax/2+5))
	})
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/r", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusFound)
	})
	defer srv.Close()
	// a private address fetches like any other: localhost development is the
	// point, and ask mode is what gates the URL
	for _, raw := range []string{srv.URL + "/x"} {
		res := call(t, toolNamed(t, "fetch", allowed), fmt.Sprintf(`{"url":%q}`, raw))
		if res.IsError {
			t.Fatalf("%s: %+v", raw, res)
		}
	}
	// a 200 html page is just its text; a status or type worth saying heads the body
	if res := call(t, toolNamed(t, "fetch", allowed), fmt.Sprintf(`{"url":%q}`, srv.URL+"/x")); res.Content != "hi" {
		t.Fatalf("html body = %q", res.Content)
	}
	if res := call(t, toolNamed(t, "fetch", allowed), fmt.Sprintf(`{"url":%q}`, srv.URL+"/j")); res.Content != "418 I'm a teapot\nContent-Type: application/json\n\n{}" {
		t.Fatalf("json body = %q", res.Content)
	}
	if res := call(t, toolNamed(t, "fetch", allowed), fmt.Sprintf(`{"url":%q}`, srv.URL+"/big")); !strings.Contains(res.Content, "\n(only the first 1 MB of the page was read, of 1048586 bytes)\n") {
		t.Fatalf("a page past the cap says so: %q", res.Content[:200])
	}
	// a redirect hop asks like a first URL in ask mode, and passes in allow mode
	hops, hopAsks := testPipeline(t, false)
	if err := netGate(hops, &url.URL{Scheme: "https", Host: "example.com", Path: "/next"}); err == nil || len(*hopAsks) != 1 {
		t.Fatalf("hop in ask mode: %v, asked %+v", err, *hopAsks)
	}
	if err := netGate(allowed, &url.URL{Scheme: "https", Host: "example.com", Path: "/next"}); err != nil {
		t.Fatalf("hop in allow mode: %v", err)
	}
}

// TestHTMLToText: readability picks the article out of a page, and what it
// keeps reads as markdown: heading, a parameter table as rows, a code
// fence, bullets and absolute links.
func TestHTMLToText(t *testing.T) {
	in := `<html><head><title>hidden</title></head><body>
<nav><a href="/menu">Menu</a></nav><script>alert("x")</script><style>.c{}</style>
<article><h1>Widget API</h1>
<p>The widget API creates <b>widgets</b>. ` + strings.Repeat("Prose that carries the article past readability's content threshold. ", 10) + `</p>
<h2>Options</h2>
<table><thead><tr><th>Name</th><th>Type</th></tr></thead>
<tbody><tr><td>size</td><td>int</td></tr></tbody></table>
<pre><code><span class="k">w</span> := widget.New()
</code></pre>
<ul><li>first</li><li>second <a class="x" href="../docs?a=1&amp;b=2">point</a></li></ul>
</article><footer>&copy; 2020</footer></body></html>`
	base, _ := url.Parse("https://example.com/api/page")
	out := htmlToText(in, base)
	for _, bad := range []string{"script", "hidden", "alert", ".c{}", "<b>", "Menu", "2020"} {
		if strings.Contains(out, bad) {
			t.Fatalf("htmlToText kept %q: %q", bad, out)
		}
	}
	for _, want := range []string{
		"Widget API", "creates **widgets**", "## Options",
		"| Name | Type |", "| size |", "```\nw := widget.New()", "- first",
		"[point](https://example.com/docs?a=1&b=2)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("htmlToText lost %q:\n%s", want, out)
		}
	}
}

// TestHTMLToTextNoArticle: a page readability finds no article in still
// converts whole, so a short answer page is not returned empty.
func TestHTMLToTextNoArticle(t *testing.T) {
	base, _ := url.Parse("https://example.com/")
	if got := htmlToText(`<html><body><p>Hello &amp; bye</p></body></html>`, base); got != "Hello & bye" {
		t.Fatalf("htmlToText = %q", got)
	}
}

// TestMemoryExamplesStore: every example the memory tool's description
// quotes is stored under auto, not queued as instruction-shaped.
func TestMemoryExamplesStore(t *testing.T) {
	var desc string
	for _, tl := range All(&Pipeline{}) {
		if tl.Info().Name == ToolMemory {
			desc = tl.Info().Description
		}
	}
	examples := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(desc, -1)
	if len(examples) < 2 {
		t.Fatalf("examples: %q", desc)
	}
	for _, m := range examples {
		if learn.InstructionShaped(m[1]) {
			t.Errorf("example reads as instruction: %q", m[1])
		}
	}
}

// TestDeniedOutsideRun: the tools that gate themselves fire the same
// tool.denied notification Run gives a gated one, so a handler sees every
// refusal (DESIGN §6.2).
func TestDeniedOutsideRun(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(t *testing.T, p *Pipeline)
		want string
	}{
		{"a refused fetch redirect hop", func(t *testing.T, p *Pipeline) {
			if err := netGate(p, &url.URL{Scheme: "https", Host: "example.com", Path: "/x"}); err == nil {
				t.Fatal("a hop ask mode denies must be refused")
			}
		}, ToolFetch},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := testPipeline(t, false)
			var tools []string
			p.Denied = func(tool string, _ Request, _ Verdict) { tools = append(tools, tool) }
			c.run(t, p)
			if !slices.Equal(tools, []string{c.want}) {
				t.Fatalf("tool.denied for %v, want %q", tools, c.want)
			}
		})
	}
}
