// misc_tools_test.go covers the second batch of built-ins: todos, question
// and skill (misc_tools.go), the job_* family (job_tools.go) and the URL
// gate behind zemi.http (net_tools.go). No test touches the real network.

package tools

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

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
	if resp.IsError || len(stored) != 2 || resp.Content != `updated todos: 0/2 done; 1 open item left the list ("first"): put it back or mark it done; 2 items are in_progress; keep exactly one` {
		t.Fatalf("two in_progress: stored %d, %+v", len(stored), resp)
	}
	resp = call(t, todos, `{"todos":[{"content":"a","status":"cancelled"},{"content":"b","status":"pending"}]}`)
	if resp.IsError || len(stored) != 1 || stored[0].Content != "b" || resp.Content != `updated todos: 0/1 done; dropped 1 cancelled; 1 open item left the list ("a"): put it back or mark it done` {
		t.Fatalf("cancelled items drop: stored %+v, %+v", stored, resp)
	}
	if len(stored) != 1 {
		t.Fatalf("failed writes must not replace the list: %+v", stored)
	}

	resp = call(t, todos, `{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"pending"},{"content":"c","status":"pending"}]}`)
	if resp.IsError || !strings.Contains(resp.Content, "3 items are pending: the ones that need little from each other can run as parallel agent calls") {
		t.Fatalf("a fresh plan points at parallel runs: %+v", resp)
	}
	resp = call(t, todos, `{"todos":[{"content":"a","status":"done"},{"content":"b","status":"pending"},{"content":"c","status":"pending"}]}`)
	if resp.IsError || strings.Contains(resp.Content, "parallel agent calls") {
		t.Fatalf("a plan already underway does not nag: %+v", resp)
	}

	// a write that drops an open item names it, so work never vanishes quietly
	resp = call(t, todos, `{"todos":[{"content":"c","status":"done"}]}`)
	if resp.IsError || !strings.Contains(resp.Content, `1 open item left the list ("b"): put it back or mark it done`) {
		t.Fatalf("a dropped open item is named: %+v", resp)
	}
	resp = call(t, todos, `{"todos":[{"content":"c","status":"done"}]}`)
	if resp.IsError || strings.Contains(resp.Content, "left the list") {
		t.Fatalf("a list that keeps its open items stays quiet: %+v", resp)
	}

	for _, bad := range []struct{ in, want string }{
		{`{"todos":[{"content":"","status":"pending"}]}`, "todo 1: content is required"},
		{`{"todos":[{"content":"x","status":"pending"},{"content":"x","status":"nope"}]}`, "todo 2: status must be"},
		// a non-object item never reaches the run: the typed decode refuses it
		// (the modal verb is Go's, randomized per process)
		{`{"todos":["not-an-object"]}`, `invalid tool input: json: `},
	} {
		resp = call(t, todos, bad.in)
		if !resp.IsError || !strings.HasPrefix(resp.Content, bad.want) {
			t.Fatalf("bad input must error: %s -> %+v", bad.in, resp)
		}
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
	filed := func(got *string, v PlanVerdict, ok bool) func(string) (PlanVerdict, bool) {
		return func(plan string) (PlanVerdict, bool) { *got = plan; return v, ok }
	}
	planPath := filepath.Join(t.TempDir(), "brief.md")
	planBody := "# The brief\n1. do it\n"
	if err := os.WriteFile(planPath, []byte(planBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotPlan string
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
		{
			"a note without a pick reaches the model", "question", two, answer([]Answer{{Note: "my custom path"}, {}}, true), nil,
			`{"answers":[{"question":"Which path?","answer":null,"note":"my custom path"},{"question":"Checks?","answer":null}],"open":"a null answer is yours`, false, false,
		},
		{"dismissal ends the turn", "question", two, answer(nil, false), nil, "dismissed", true, true},
		{"no user: decide and continue", "question", two, nil, nil, "No user is available", false, false},
		{"too many questions", "question", `{"questions":[{"type":"single_choice","question":"1"},{"type":"single_choice","question":"2"},{"type":"single_choice","question":"3"},{"type":"single_choice","question":"4"},{"type":"single_choice","question":"5"}]}`, answer(nil, true), nil, "1 to 4", true, false},
		{"option without label", "question", `{"questions":[{"type":"single_choice","question":"x","options":[{"description":"no label"}]}]}`, answer(nil, true), nil, "options[0].label", true, false},
		{
			"approved and saved", "plan", `{"plan":"1. do it"}`, nil, verdict(PlanVerdict{Approved: true, Path: "/p.md"}, true),
			"The user approved the plan", false, false,
		},
		{"changes requested", "plan", `{"plan":"1. do it"}`, nil, verdict(PlanVerdict{Feedback: "use sqlite"}, true), "asked for changes: use sqlite", false, false},
		{"plan dismissed", "plan", `{"plan":"1. do it"}`, nil, verdict(PlanVerdict{}, false), "dismissed the plan", true, true},
		{"plan with no user", "plan", `{"plan":"1. do it"}`, nil, nil, "Put the plan in your reply", false, false},
		{"plan and file together", "plan", `{"plan":"1. do it","file":"` + planPath + `"}`, nil, verdict(PlanVerdict{Approved: true}, true), "not both", true, false},
		{"a plan read from a file", "plan", `{"file":"` + planPath + `"}`, nil, filed(&gotPlan, PlanVerdict{Approved: true}, true), "The user approved the plan", false, false},
		{"a missing plan file", "plan", `{"file":"` + filepath.Join(filepath.Dir(planPath), "nope.md") + `"}`, nil, verdict(PlanVerdict{Approved: true}, true), "no such file", true, false},
		{"an unexpanded plan path", "plan", `{"file":"~/brief.md"}`, nil, verdict(PlanVerdict{Approved: true}, true), "not expanded", true, false},
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
	if gotPlan != planBody {
		t.Fatalf("the file's plan did not reach approval: %q", gotPlan)
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
		{"approved and held", PlanVerdict{Approved: true, Hold: true, Path: "/p.md"}, "approved (held)", "/p.md", true},
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
	p.Jobs = &Jobs{}
	changed := make(chan struct{}, 8)
	// non-blocking: the hooks outlive the asserts that drain it, and a
	// blocked send would hang the tool call itself
	p.Jobs.OnChange = func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	start := toolNamed(t, "job_start", p)
	read := toolNamed(t, "job_read", p)
	list := toolNamed(t, "job_list", p)
	kill := toolNamed(t, "job_kill", p)

	resp := call(t, start, `{"command":"echo hello-job"}`)
	if resp.IsError || !strings.Contains(resp.Content, "started job-") {
		t.Fatalf("start echo: %+v", resp)
	}
	j := p.Jobs.List()[0]
	// the change hook fires at the start and at the exit: the statusline and
	// the job screens redraw on it
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnChange did not run at the start")
	}
	deadline := time.After(5 * time.Second)
	for p.Jobs.Running() > 0 {
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("OnChange did not run at the exit")
		}
	}
	waitJobDone(t, j)
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

	// a waitless read of a quiet running job rides to the exit instead of
	// making the model poll one roundtrip per quiet stretch
	resp = call(t, start, `{"command":"sleep 1.5; echo done-slow"}`)
	if resp.IsError {
		t.Fatalf("start slow: %+v", resp)
	}
	j = p.Jobs.List()[4]
	began = time.Now()
	resp = call(t, read, `{"id":"`+j.ID+`"}`)
	if took := time.Since(began); resp.IsError || !strings.Contains(resp.Content, "exited (0)") ||
		!strings.Contains(resp.Content, "done-slow") || took > 10*time.Second {
		t.Fatalf("waitless read of a quiet job: %+v after %s", resp, took)
	}

	resp = call(t, start, `{"command":"sleep 30"}`)
	if resp.IsError {
		t.Fatalf("start sleep: %+v", resp)
	}
	j = p.Jobs.List()[5]
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
