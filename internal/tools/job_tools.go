// job_tools.go ships the job_* built-ins: background processes the model
// can start, read, list and kill. Jobs share the Lua job discipline (DESIGN
// §6.5): the child env is the parent minus secret-shaped names plus
// validated extras, output lands in capped buffers, and the working
// directory is the project directory.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/proc"
)

// Job is one managed background process.
type Job struct {
	ID     string
	Name   string
	stdout *proc.Buffer
	stderr *proc.Buffer
	cmd    *exec.Cmd
	done   chan struct{}
	code   int
	err    string
	werr   error // what Wait returned
	// sandboxed marks a confined job, whose failure may need the sandbox hint
	sandboxed bool
	after     func() string // runs at exit for a confined job (Pipeline.Guard)
	note      string        // what after returned
	sent      [2]int64      // bytes of stdout/stderr already read, see since
	mu        sync.Mutex
}

// Running reports whether the process is still running.
func (j *Job) Running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// Status renders one list line.
func (j *Job) Status() string {
	state := "running"
	if !j.Running() {
		// ExitCode is -1 when a signal ended the process: /agents kill, an
		// interrupt or the OS, never a status the command chose
		state = fmt.Sprintf("exited (%d)", j.code)
		if j.code < 0 {
			state = "killed"
		}
		if j.err != "" {
			state += " " + j.err
		}
	}
	return state
}

// Output is what the job's buffers still hold, stdout then stderr.
func (j *Job) Output() string {
	so, _ := j.stdout.Snapshot()
	se, _ := j.stderr.Snapshot()
	return so + se
}

// since returns the output written after the previous read and the exit
// state: a job that is polled while it runs would otherwise resend its
// whole buffer every time.
func (j *Job) since() (out, errOut string, running bool, errText string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Count bytes ever written, not buffer length: a saturated buffer drops
	// its middle, so its length stops growing while output keeps arriving.
	// Whatever is new sits at the end of what the buffer still holds.
	rest := func(b *proc.Buffer, sent *int64) string {
		s, written := b.Snapshot()
		n := written - *sent
		*sent = written
		switch {
		case n <= 0:
			return ""
		case n >= int64(len(s)):
			return s
		}
		return s[int64(len(s))-n:]
	}
	return rest(j.stdout, &j.sent[0]), rest(j.stderr, &j.sent[1]), j.Running(), j.err
}

// Jobs owns every model-started job for one host.
type Jobs struct {
	mu   sync.Mutex
	seq  int
	jobs map[string]*Job
}

func NewJobs() *Jobs {
	return &Jobs{jobs: map[string]*Job{}}
}

// Start launches argv in dir with env, confined by wrap unless it is nil,
// and runs after (if not nil) once it exits; returns the job id. Output is
// captured as it arrives, so job_read shows a server's lines while it runs.
func (m *Jobs) Start(dir string, argv []string, env []string, wrap func(*exec.Cmd) error, after func() string) (string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return "", errors.New("command is required")
	}
	cmd := proc.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	j := &Job{Name: strings.Join(argv, " "), cmd: cmd, done: make(chan struct{}),
		stdout: &proc.Buffer{Max: 1 << 20}, stderr: &proc.Buffer{Max: 1 << 20}, after: after}
	cmd.Stdout, cmd.Stderr = j.stdout, j.stderr
	fail := func(err error) (string, error) {
		if after != nil {
			after()
		}
		return "", err
	}
	if wrap != nil {
		if err := wrap(cmd); err != nil {
			return fail(fmt.Errorf("the sandbox failed, so the command did not run: %w", err))
		}
		j.sandboxed = true
	}
	if err := proc.Start(cmd); err != nil {
		return fail(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- proc.Wait(cmd) }()
	return m.add(j, wait), nil
}

// Adopt registers a command that is already running, with the buffer it
// writes to, the byte count the caller already delivered from it, a channel
// carrying its Wait and, for a sandboxed one, what runs at exit (nil when
// unconfined): a bash call past its timeout keeps going as a job instead of
// losing the work done so far.
func (m *Jobs) Adopt(name string, cmd *exec.Cmd, out *proc.Buffer, sent int64, wait <-chan error, after func() string) string {
	j := &Job{Name: name, cmd: cmd, done: make(chan struct{}), stdout: out, stderr: &proc.Buffer{Max: 1 << 20}, sandboxed: after != nil, after: after}
	j.sent[0] = sent
	return m.add(j, wait)
}

// add registers j under the next id and records its exit once wait delivers.
func (m *Jobs) add(j *Job, wait <-chan error) string {
	m.mu.Lock()
	m.seq++
	j.ID = fmt.Sprintf("job-%d", m.seq)
	m.jobs[j.ID] = j
	m.mu.Unlock()
	go func() {
		err := <-wait
		note := ""
		if j.after != nil {
			note = j.after()
		}
		j.mu.Lock()
		j.werr, j.note = err, note
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			j.code = exit.ExitCode()
		case err != nil && !errors.Is(err, exec.ErrWaitDelay):
			j.err = err.Error()
		}
		j.mu.Unlock()
		close(j.done)
	}()
	return j.ID
}

// Get returns one job by id.
func (m *Jobs) Get(id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, fmt.Errorf("no such job %q", id)
	}
	return j, nil
}

// List returns every job in start order.
func (m *Jobs) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for i := 1; i <= m.seq; i++ {
		if j, ok := m.jobs[fmt.Sprintf("job-%d", i)]; ok {
			out = append(out, j)
		}
	}
	return out
}

// Kill terminates one job's process.
func (m *Jobs) Kill(id string) error {
	j, err := m.Get(id)
	if err != nil {
		return err
	}
	if !j.Running() {
		return fmt.Errorf("job %s already exited", id)
	}
	proc.Kill(j.cmd)
	return nil
}

func runJobStart(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	name := str(input["command"])
	argv := []string{name}
	if list, ok := input["args"].([]any); ok {
		for _, v := range list {
			argv = append(argv, str(v))
		}
	}
	if len(argv) == 1 && strings.ContainsAny(name, " \t|&;<>()$`'\"") {
		argv = []string{"sh", "-c", name} // a shell line, as the description allows
	}
	extras := map[string]any{}
	if m, ok := input["env"].(map[string]any); ok {
		extras = m
	}
	env, err := p.Store.ChildEnv(extras)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	env = append(env, proc.NonInteractiveEnv...)
	id, err := p.Jobs.Start(workdir(), argv, env, p.wrap(), p.guard())
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("started %s (%s)", id, strings.Join(argv, " "))), nil
}

func runJobRead(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	j, err := p.Jobs.Get(str(input["id"]))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	out, errOut, running, errText := j.since()
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", j.ID, j.Status())
	if errText != "" {
		fmt.Fprintf(&b, "error: %s\n", errText)
	}
	// since() has already moved past what it returns, so a cut chunk is
	// gone unless it is spilled here
	show := func(s string) string {
		if cut := TruncOut(s, cmdMaxOut); cut != s {
			return cut + p.spill("job", s)
		}
		return s
	}
	if out != "" {
		b.WriteString("[stdout]\n" + show(out) + "\n")
	}
	if errOut != "" {
		b.WriteString("[stderr]\n" + show(errOut) + "\n")
	}
	if running && out == "" && errOut == "" {
		b.WriteString("(no new output)")
	}
	if !running && j.sandboxed {
		j.mu.Lock()
		werr, note := j.werr, j.note
		j.mu.Unlock()
		// the whole retained tail: a denial may have been read while it ran
		b.WriteString(p.sandboxHint(j.Output(), werr) + note)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func runJobList(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	jobs := p.Jobs.List()
	if len(jobs) == 0 {
		return fantasy.NewTextResponse("(no jobs)"), nil
	}
	var b strings.Builder
	for _, j := range jobs {
		fmt.Fprintf(&b, "%s: %s - %s\n", j.ID, j.Status(), j.Name)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func runJobKill(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	if p.Jobs == nil {
		return fantasy.NewTextErrorResponse("jobs unavailable"), nil
	}
	id := str(input["id"])
	if err := p.Jobs.Kill(id); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse("killed " + id), nil
}
