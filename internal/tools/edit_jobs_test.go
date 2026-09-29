//go:build !windows

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sandbox"
)

func TestEditRefusesAmbiguousMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(path, []byte("x = 1\nx = 1\n"), 0o644)
	p := onPath(t, nil, path)
	for _, c := range []struct {
		input map[string]any
		want  string
		file  string
	}{
		{map[string]any{"old_string": "x = 1", "new_string": "x = 2"}, "matches 2 places", "x = 1\nx = 1\n"},
		{map[string]any{"old_string": "", "new_string": "y"}, "empty", "x = 1\nx = 1\n"},
		{map[string]any{"old_string": "x = 1", "new_string": "x = 2", "replace_all": true}, "2 replacements", "x = 2\nx = 2\n"},
	} {
		res, _ := runEdit(context.Background(), p, c.input)
		got, _ := os.ReadFile(path)
		if !strings.Contains(res.Content, c.want) || string(got) != c.file {
			t.Fatalf("%v: %q, file %q", c.input, res.Content, got)
		}
	}
}

// TestJobReadAfterBufferDrop: a job that floods its buffer keeps delivering
// new output. The buffer drops its middle, so its length stops growing and
// a length-based offset would report "no new output" forever.
func TestJobReadAfterBufferDrop(t *testing.T) {
	j := &Job{stdout: &proc.Buffer{Max: 64}, stderr: &proc.Buffer{Max: 64}, done: make(chan struct{})}
	close(j.done)
	j.stdout.Write([]byte(strings.Repeat("a", 40)))
	if out, _, _, _ := j.since(); out.text != strings.Repeat("a", 40) {
		t.Fatalf("first read: %+v", out)
	}
	if out, _, _, _ := j.since(); out.text != "" {
		t.Fatalf("nothing written since: %+v", out)
	}
	// more arrived than the buffer holds, so it returns what is left of it,
	// skip marker and all, rather than reporting nothing new
	j.stdout.Write([]byte(strings.Repeat("b", 200)))
	out, _, _, _ := j.since()
	if !strings.HasSuffix(out.text, "bb") || !strings.Contains(out.text, "skipped") || out.lost <= 0 {
		t.Fatalf("new output after the drop: %+v", out)
	}
	if out, _, _, _ := j.since(); out.text != "" {
		t.Fatalf("nothing written since the drop: %+v", out)
	}
}

// TestJobReadSandboxHint: a sandboxed job's denial read while it ran still
// gets the hint once, after it exits, and the sandbox: false retry is offered
// only where someone can answer the ask.
func TestJobReadSandboxHint(t *testing.T) {
	for _, param := range []bool{true, false} {
		// job_read's own pipeline is unconfined: the job keeps its policy
		p := &Pipeline{Jobs: NewJobs(), SandboxParam: param}
		pol := &sandbox.Policy{Write: []string{"/pad/s1"}}
		out := &proc.Buffer{Max: 1 << 20}
		wait := make(chan error, 1)
		id := p.Jobs.Adopt("touch x", nil, out, 0, wait, pol, func() string { return "\nguard note" })
		out.Write([]byte("touch: /elsewhere/x: Operation not permitted\n"))
		read := func() string {
			res, _ := runJobRead(context.Background(), p, map[string]any{"id": id})
			return res.Content
		}
		if got := read(); !strings.Contains(got, "not permitted") {
			t.Fatalf("while running: %q", got)
		}
		wait <- errors.New("exit status 1")
		j, _ := p.Jobs.Get(id)
		<-j.done
		got := read()
		if !strings.Contains(got, "\nSandbox: commands write only in /pad/s1") ||
			strings.Contains(got, "sandbox: false") != param || !strings.Contains(got, "guard note") {
			t.Errorf("param %v: after exit %q", param, got)
		}
		if got := read(); strings.Contains(got, "Sandbox:") || strings.Contains(got, "guard note") {
			t.Errorf("param %v: the hint came twice: %q", param, got)
		}
	}
}

// TestSandboxHintFollowsPolicy: a denial gets one line naming the limit the
// denied path hit and the next step, offering only the escapes the run has:
// none without a UI, no sandbox: false for a read-only agent.
func TestSandboxHintFollowsPolicy(t *testing.T) {
	denied := errors.New("exit status 1")
	network := "curl: (6) Could not resolve host: example.com"
	if !sandbox.LikelyDenied(network, denied) {
		t.Skip("no denial pattern on this platform")
	}
	b, _ := sandbox.Probe()
	proj := perm.RealPath(t.TempDir())
	t.Chdir(proj)
	t.Setenv("PATH", filepath.Join(proj, ".venv", "bin")+string(filepath.ListSeparator)+os.Getenv("PATH"))
	const escapes = ": network: true adds the network, sandbox: false drops every limit; both need approval."
	for _, c := range []struct {
		name, out              string
		plan, noParam, network bool
		noPad, skip            bool
		want                   string
	}{
		{
			"planner writes the project", "touch: PROJ/a: Operation not permitted", true, false, false, false, false,
			"the project is read-only while planning; describe the change in the plan.",
		},
		{
			"network already given", network, false, false, true, false, false,
			"it had the network already; sandbox: false drops every limit (needs approval).",
		},
		{"network already given while planning", network, true, false, true, false, false, "it had the network already."},
		{
			"no network", network, false, false, false, false, false,
			"no network here: call it again with network: true, which keeps the rest of the sandbox (needs approval).",
		},
		{
			"hidden", "ls: /h/zemi/x: Operation not permitted", false, false, false, false, false,
			"/h/zemi is hidden from commands: read it with the read, glob and grep tools, or call it again with sandbox: false (needs approval).",
		},
		{
			"secret", "cat: PROJ/.env: Operation not permitted", false, false, false, false, !b.HidesSecretFiles,
			"secret-shaped files (.env, *.pem, *.key) are unreadable to commands outside $TMPDIR; if the command needs one, call it again with sandbox: false (needs approval).",
		},
		{
			".agents", "mkdir: .agents/workflows: Operation not permitted", false, false, false, false, false,
			".agents is read-only to commands: create and edit files there with the write and edit tools.",
		},
		{
			".git", "fatal: Unable to create 'PROJ/.git/index.lock': Operation not permitted", false, false, false, false, false,
			".git is read-only to commands: git commands that change it ask first when they run on their own; otherwise call it again with sandbox: false (needs approval).",
		},
		{
			"a PATH dir", "pip: PROJ/.venv/bin/x: Operation not permitted", false, false, false, false, false,
			".venv/bin is read-only to commands: zemi runs programs from it unconfined; call it again with sandbox: false (needs approval).",
		},
		{"system temp", "/bin/sh: /tmp/x: Operation not permitted", false, false, false, false, false, "/tmp is not writable: use $TMPDIR."},
		{
			"system temp with no pad", "/bin/sh: /tmp/x: Operation not permitted", false, false, false, true, false,
			"commands write only in the project and $GOCACHE: write there, or call it again with sandbox: false (needs approval).",
		},
		{
			"outside the roots", "touch: /elsewhere/x: Operation not permitted", false, false, false, false, false,
			"commands write only in the project, $TMPDIR and $GOCACHE: write there, or call it again with sandbox: false (needs approval).",
		},
		{
			"outside the roots under -p", "touch: /elsewhere/x: Operation not permitted", false, true, false, false, false,
			"commands write only in the project, $TMPDIR and $GOCACHE: write there.",
		},
		{
			"nothing classified", "open x: operation not permitted", false, false, false, false, false,
			"a limit may have stopped it (Sandbox in the system prompt lists them)" + escapes,
		},
		{
			"nothing classified while planning", "open x: operation not permitted", true, false, false, false, false,
			"a limit may have stopped it (Sandbox in the system prompt lists them): network: true adds the network (needs approval).",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.skip {
				t.Skip("the backend hides no secret-shaped files")
			}
			p := &Pipeline{SandboxParam: !c.noParam, Plan: func() bool { return c.plan }, Pad: "/pad/s1"}
			pol := &sandbox.Policy{
				Network: c.network,
				Write:   []string{proj, "/pad/s1", "/cache/go-build"},
				Protect: []string{filepath.Join(proj, ".agents"), filepath.Join(proj, ".git"), filepath.Join(proj, ".venv", "bin")},
				Hide:    []string{"/h/zemi"},
				Env:     map[string]string{"TMPDIR": "/pad/s1", "TMP": "/pad/s1", "GOCACHE": "/cache/go-build"},
			}
			if c.noPad {
				p.Pad, pol.Write = "", slices.Delete(pol.Write, 1, 2)
			}
			if hint := p.sandboxHint(pol, strings.ReplaceAll(c.out, "PROJ", proj), denied); hint != "\nSandbox: "+c.want {
				t.Errorf("got %q\nwant %q", hint, "\nSandbox: "+c.want)
			}
		})
	}
}

// TestBashTimeoutBecomesJob: a command still running at its timeout keeps
// going as a job, so a slow build is not thrown away half done.
func TestBashTimeoutBecomesJob(t *testing.T) {
	t.Parallel()
	p := &Pipeline{Jobs: NewJobs()}
	res, _ := runBash(context.Background(), p, map[string]any{
		"command": "echo started; sleep 30", "timeout_ms": float64(400),
	})
	if res.IsError || !strings.Contains(res.Content, JobHandoff+"job-1") {
		t.Fatalf("timeout must hand off to a job: %+v", res)
	}
	if !strings.Contains(res.Content, "started") {
		t.Fatalf("output from before the timeout must survive: %q", res.Content)
	}
	if out, _ := runJobRead(context.Background(), p, map[string]any{"id": "job-1"}); !strings.Contains(out.Content, "running") {
		t.Fatalf("the job must still be running: %q", out.Content)
	}
	runJobKill(context.Background(), p, map[string]any{"id": "job-1"})
	j, _ := p.Jobs.Get("job-1")
	select {
	case <-j.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job_kill did not stop the adopted command")
	}
}

// TestBashOrphanIsNotAnError: a command that leaves a child holding the
// pipe exited 0; the child is stopped and named, not reported as a failure.
func TestBashOrphanIsNotAnError(t *testing.T) {
	t.Parallel()
	res, _ := runBash(context.Background(), &Pipeline{}, map[string]any{"command": "sleep 30 &\necho started"})
	if res.IsError || !strings.HasPrefix(res.Content, "started\na background process it started was stopped") {
		t.Fatalf("orphan: %+v", res)
	}
}

// TestJobStreamsOutput: job_read shows a long-running process's output
// while it runs, and a shell line works without args.
func TestJobStreamsOutput(t *testing.T) {
	p := &Pipeline{Jobs: NewJobs()}
	res, _ := runJobStart(context.Background(), p, map[string]any{"command": "echo ready && sleep 30"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := runJobRead(context.Background(), p, map[string]any{"id": "job-1"})
		if strings.Contains(out.Content, "ready") && strings.Contains(out.Content, "running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no output while running: %q", out.Content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, _ := runJobRead(context.Background(), p, map[string]any{"id": "job-1"}); strings.Contains(out.Content, "ready") {
		t.Fatalf("a second read must not repeat delivered output: %q", out.Content)
	}
	runJobKill(context.Background(), p, map[string]any{"id": "job-1"})
	j, _ := p.Jobs.Get("job-1")
	select {
	case <-j.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job_kill did not stop the shell and its sleep")
	}
}

// TestJobReadSpillsTheCut: a read that cuts the middle saves the whole
// chunk, which since() has already moved past.
func TestJobReadSpillsTheCut(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{Jobs: NewJobs(), Pad: dir}
	if res, _ := runJobStart(context.Background(), p, map[string]any{"command": "seq 1 4000"}); res.IsError {
		t.Fatal(res.Content)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := runJobRead(context.Background(), p, map[string]any{"id": "job-1"})
		if _, file, ok := strings.Cut(out.Content, "(output saved to "); ok {
			file, _, _ = strings.Cut(file, ")")
			b, err := os.ReadFile(file)
			if err != nil || !strings.Contains(string(b), "\n2000\n") {
				t.Fatalf("spill %s: %v", file, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no spill: %q", out.Content)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFileToolsSymlinkRace: a directory flipped between a real one and a
// symlink out of the jail while write, edit and read run never lets one of
// them act outside.
func TestFileToolsSymlinkRace(t *testing.T) {
	t.Parallel()
	proj, outside := perm.RealPath(t.TempDir()), perm.RealPath(t.TempDir())
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600)
	s := new(perm.Store)
	s.Default = func(subject, arg string) perm.Decision {
		if strings.HasPrefix(perm.RealPath(arg), proj+string(filepath.Separator)) {
			return perm.Allow
		}
		return perm.Deny
	}
	byName := map[string]fantasy.AgentTool{}
	for _, tl := range All(&Pipeline{Store: s}) {
		byName[tl.Info().Name] = tl
	}
	run := func(name string, in map[string]any) string {
		b, _ := json.Marshal(in)
		res, _ := byName[name].Run(context.Background(), fantasy.ToolCall{ID: "1", Name: name, Input: string(b)})
		return res.Content
	}
	d := filepath.Join(proj, "d")
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.RemoveAll(d)
			os.Mkdir(d, 0o755)
			os.WriteFile(filepath.Join(d, "secret.txt"), []byte("mine"), 0o644)
			os.RemoveAll(d)
			os.Symlink(outside, d)
		}
	}()
	leaked := false
	for i := range 2000 {
		run("write", map[string]any{"path": filepath.Join(d, fmt.Sprintf("race%d.txt", i)), "content": "x"})
		run("edit", map[string]any{"path": filepath.Join(d, "secret.txt"), "old_string": "secret", "new_string": "owned"})
		leaked = leaked || strings.Contains(run("read", map[string]any{"path": filepath.Join(d, "secret.txt"), "limit": 0}), "\tsecret")
	}
	close(stop)
	<-done
	entries, _ := os.ReadDir(outside)
	got, _ := os.ReadFile(filepath.Join(outside, "secret.txt"))
	if len(entries) != 1 || string(got) != "secret" || leaked {
		t.Fatalf("outside the jail: %d entries, secret %q, read leaked %v", len(entries), got, leaked)
	}
}

// TestBashResultText pins what the model reads back: binary is named, not
// sent, and a failure ends in its exit status.
func TestBashResultText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ cmd, want string }{
		{`printf 'a\0b'`, "(binary output, 3 bytes)"},
		{"echo hi; exit 3", "hi\nexit status 3"},
	} {
		res, _ := runBash(context.Background(), &Pipeline{}, map[string]any{"command": tc.cmd})
		if res.Content != tc.want {
			t.Errorf("%s: %q, want %q", tc.cmd, res.Content, tc.want)
		}
	}
}
