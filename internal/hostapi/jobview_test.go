package hostapi

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/tools"
)

// TestJobsScreen: /jobs lists what runs before what ended, times both, and
// stops every job at once; a job's view is its own output, live.
func TestJobsScreen(t *testing.T) {
	t.Parallel()
	h, _ := newLoopHost(t)
	dir := t.TempDir()

	ended, err := h.jobs.Start(dir, "echo bye", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if n := h.jobs.Running(); n > 0 {
			return fmt.Errorf("%d jobs still running", n)
		}
		return nil
	})
	running, err := h.jobs.Start(dir, "sleep 30", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.jobs.KillAll() })

	o := h.jobsOverlay()
	if len(o.Items) != 2 {
		t.Fatalf("items = %v, want the running job and the ended one", o.Items)
	}
	first := o.Items[0]
	if first.Section != "Running" || first.Text != "sleep 30" || !strings.Contains(first.Desc, running) || first.Mark != "spin" {
		t.Fatalf("first row = %+v, want the running job first with its id and clock", first)
	}
	if !slices.ContainsFunc(first.Keys, func(k ItemKey) bool { return k.Run == "jobs stop "+running && k.Key == "s" && k.Danger }) {
		t.Fatal("a running job offers no stop")
	}
	second := o.Items[1]
	if second.Section != "Ended" || second.Text != "echo bye" || !strings.Contains(second.Desc, ended) {
		t.Fatalf("second row = %+v, want the ended job under its own section", second)
	}
	if !slices.ContainsFunc(o.Keys, func(k OverlayKey) bool { return k.Run == "jobs stop all" && k.Danger }) {
		t.Fatalf("keys = %v, want stop all", o.Keys)
	}

	view := h.jobOverlay(mustJob(t, h, running))
	if !view.Live || view.Mark != "spin" || !slices.ContainsFunc(view.Keys, func(k OverlayKey) bool { return k.Run == "jobs stop "+running && k.Danger }) {
		t.Fatalf("view = %+v, want a live body and its stop key", view)
	}

	if err := h.stopJobs("all"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if n := h.jobs.Running(); n > 0 {
			return fmt.Errorf("%d jobs still running", n)
		}
		return nil
	})
}

// TestJobTail: a view holds the end of a long output and names what it left
// above it.
func TestJobTail(t *testing.T) {
	var long strings.Builder
	for i := range 300 {
		fmt.Fprintf(&long, "line %d\n", i)
	}
	if got := jobTail("a\nb\n"); got != "a\nb" {
		t.Fatalf("short output = %q, want it whole but for the last newline", got)
	}
	lines := strings.Split(jobTail(long.String()), "\n")
	if lines[0] != MoreLines(300-jobViewLines) || lines[len(lines)-1] != "line 299" {
		t.Fatalf("tail starts %q and ends %q, want the cut named and the last line kept", lines[0], lines[len(lines)-1])
	}
	if n := len(lines); n != jobViewLines+1 {
		t.Fatalf("tail holds %d rows, want %d", n, jobViewLines+1)
	}
}

func mustJob(t *testing.T, h *Host, id string) *tools.Job {
	t.Helper()
	j, err := h.jobs.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
