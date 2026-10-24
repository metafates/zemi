package tools

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/proc"
)

// TestJobKillReadOnlyDeny: job_kill makes no gate request, so runJobKill
// carries the read-only deny itself instead of killing ungated.
func TestJobKillReadOnlyDeny(t *testing.T) {
	p := &Pipeline{Jobs: NewJobs(), ReadOnly: func() bool { return true }}
	if res, _ := runJobKill(t.Context(), p, jobKillArgs{ID: "job-1"}); !res.IsError || !strings.Contains(res.Content, ByReadOnly) {
		t.Fatalf("a read-only agent's kill must deny: %+v", res)
	}
}

func TestJobsEvictFinished(t *testing.T) {
	t.Parallel()
	m := NewJobs()
	n := maxKeptJobs + 5
	changes := make(chan struct{}, 2*n)
	waits := make(map[string]chan error, n)
	for i := 1; i <= n; i++ {
		w := make(chan error, 1)
		id := m.add(&Job{
			Name: strconv.Itoa(i), Started: time.Now(),
			stdout: &proc.Buffer{Max: 1 << 20}, stderr: &proc.Buffer{Max: 1 << 20},
			done: make(chan struct{}),
		}, w)
		waits[id] = w
	}
	m.OnChange = func() { changes <- struct{}{} } // one per finished job, before its eviction
	for i := 2; i <= n; i++ {                     // job-1 stays running
		waits[fmt.Sprintf("job-%d", i)] <- nil
	}
	for range n - 1 {
		select {
		case <-changes:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for job exits")
		}
	}
	if _, err := m.Get("job-1"); err != nil {
		t.Fatalf("running job evicted: %v", err)
	}
	if _, err := m.Get("job-2"); err == nil {
		t.Fatal("oldest finished job kept")
	}
	if _, err := m.Get("job-6"); err != nil {
		t.Fatalf("finished job inside the cap evicted: %v", err)
	}
	if got := len(m.List()); got != maxKeptJobs+1 {
		t.Fatalf("List = %d jobs, want %d", got, maxKeptJobs+1)
	}
	close(waits["job-1"]) // let its add goroutine exit
}

// TestJobsExitRefreshSeesTheJob: the exit OnChange fires before the eviction
// sweep, so the refresh still finds the job and its view leaves "running".
func TestJobsExitRefreshSeesTheJob(t *testing.T) {
	t.Parallel()
	m := NewJobs()
	n := maxKeptJobs + 1
	changes := make(chan struct{}, 2*n)
	waits := make(map[string]chan error, n)
	for i := 1; i <= n; i++ {
		w := make(chan error, 1)
		id := m.add(&Job{
			Name: strconv.Itoa(i), Started: time.Now(),
			stdout: &proc.Buffer{Max: 1 << 20}, stderr: &proc.Buffer{Max: 1 << 20},
			done: make(chan struct{}),
		}, w)
		waits[id] = w
	}
	var saw bool
	m.OnChange = func() {
		for _, j := range m.List() {
			if j.ID == "job-1" && !j.Running() {
				saw = true
			}
		}
		changes <- struct{}{}
	}
	for i := 2; i <= n; i++ { // job-1 exits last, the oldest finished: its own sweep evicts it
		waits[fmt.Sprintf("job-%d", i)] <- nil
	}
	for range n - 1 {
		select {
		case <-changes:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for job exits")
		}
	}
	waits["job-1"] <- nil
	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for job-1's exit")
	}
	if !saw {
		t.Fatal("no OnChange ever saw the finished job: an open view of it freezes at running")
	}
	if _, err := m.Get("job-1"); err == nil {
		t.Fatal("oldest finished job kept")
	}
}
