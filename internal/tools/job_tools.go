// job_tools.go ships the job_* built-ins: background processes the model
// can start, read, list and kill. Jobs share the Lua job discipline (DESIGN
// §6.5): the child env is the parent minus secret-shaped names plus
// validated extras, stdio are always pipes, and the working directory is
// the session dir.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
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
	mu     sync.Mutex
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
		state = fmt.Sprintf("exited (%d)", j.code)
		if j.err != "" {
			state += " " + j.err
		}
	}
	return state
}

// snapshot returns the buffered output and the exit state.
func (j *Job) snapshot() (out, errOut string, running bool, errText string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stdout.String(), j.stderr.String(), j.Running(), j.err
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

// Start launches argv in dir with env; returns the job id. Output is
// captured as it arrives, so job_read shows a server's lines while it runs.
func (m *Jobs) Start(dir string, argv []string, env []string) (string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return "", fmt.Errorf("command is required")
	}
	cmd := proc.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	j := &Job{Name: strings.Join(argv, " "), cmd: cmd, done: make(chan struct{}),
		stdout: &proc.Buffer{Max: 1 << 20}, stderr: &proc.Buffer{Max: 1 << 20}}
	cmd.Stdout, cmd.Stderr = j.stdout, j.stderr
	if err := proc.Start(cmd); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.seq++
	j.ID = fmt.Sprintf("job-%d", m.seq)
	m.jobs[j.ID] = j
	m.mu.Unlock()
	go func() {
		err := proc.Wait(cmd)
		j.mu.Lock()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				j.code = ee.ExitCode()
			} else if !errors.Is(err, exec.ErrWaitDelay) {
				j.err = err.Error()
			}
		}
		j.mu.Unlock()
		close(j.done)
	}()
	return j.ID, nil
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

// childEnv builds the job environment: parent minus secret-shaped names
// plus validated extras (Lua-supplied values only, §6.5).
func childEnv(extras map[string]any) ([]string, error) {
	out := make([]string, 0, len(extras)+16)
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && !envIsSecretName(name) {
			out = append(out, kv)
		}
	}
	for k, v := range extras {
		if envIsSecretName(k) || perm.EnvAssignmentGate(k) {
			return nil, fmt.Errorf("env var %q is refused by the environment gate", k)
		}
		out = append(out, k+"="+fmt.Sprint(v))
	}
	return out, nil
}

// envIsSecretName mirrors the hostapi denylist: token/secret/password/
// credential/api_key/private_key appear anywhere in the name.
func envIsSecretName(name string) bool {
	upper := strings.ToUpper(name)
	for _, s := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "API_KEY", "PRIVATE_KEY"} {
		if strings.Contains(upper, s) {
			return true
		}
	}
	return false
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
	env, err := childEnv(extras)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	env = append(env, proc.NonInteractiveEnv...)
	id, err := p.Jobs.Start(workdir(), argv, env)
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
	out, errOut, running, errText := j.snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", j.ID, j.Status())
	if errText != "" {
		fmt.Fprintf(&b, "error: %s\n", errText)
	}
	if out != "" {
		b.WriteString("[stdout]\n" + truncOut(out) + "\n")
	}
	if errOut != "" {
		b.WriteString("[stderr]\n" + truncOut(errOut) + "\n")
	}
	if running && out == "" && errOut == "" {
		b.WriteString("(no output yet)")
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
