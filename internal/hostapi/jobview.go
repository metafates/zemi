// jobview.go ships /jobs: the background processes the model starts, as a
// list with a live output view under it. Jobs belong to the host, so one a
// server started in an earlier session shows too.

package hostapi

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// jobViewLines is how much of a job's output its view holds: the buffer
// keeps a megabyte, and the view wraps it again every second.
const jobViewLines = 200

func (h *Host) jobsCommands() []*Command {
	return []*Command{{
		Name: "jobs", NArgs: NArgsAny, Desc: "see the background processes, read their output or stop them",
		GoFn: func(h *Host, args string, confirm bool) error {
			sub, id, _ := strings.Cut(strings.TrimSpace(args), " ")
			switch sub, id = strings.TrimSpace(sub), strings.TrimSpace(id); sub {
			case "":
				return h.openJobs()
			case "view":
				return h.openJob(id)
			case "stop":
				return h.stopJobs(id)
			}
			return Usagef("unknown /jobs subcommand %q (view <id>, stop <id>|all)", sub)
		},
		GoComplete: SubcommandCompleter([]string{"view", "stop"}, func(h *Host, sub string) []string {
			if sub == "stop" {
				return []string{"all"}
			}
			var ids []string
			for _, j := range h.jobs.List() {
				ids = append(ids, j.ID)
			}
			return ids
		}),
	}}
}

// stopJobs stops one job, or every running one at once for "all", which the
// list's stop-all key names.
func (h *Host) stopJobs(id string) error {
	if id == "" {
		return Usagef("/jobs stop <id>|all")
	}
	if id == "all" {
		if n := h.jobs.KillAll(); n > 0 {
			h.Notify(slog.LevelInfo, fmt.Sprintf("Stopped %d %s", n, Plural(n, "job", "jobs")), "")
		}
		return nil
	}
	if err := h.jobs.Kill(id); err != nil {
		return Usagef("%v", err)
	}
	return nil
}

func (h *Host) openJobs() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(h.jobsOverlay())
	h.watchJobs()
	return nil
}

// openJob opens a job's output view: its tail, followed as it runs.
func (h *Host) openJob(id string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if id == "" {
		return Usagef("/jobs view <id> (ids are in /jobs)")
	}
	j, err := h.jobs.Get(id)
	if err != nil {
		return Usagef("%v", err)
	}
	ui.OpenOverlay(h.jobOverlay(j))
	h.watchJobs()
	return nil
}

// jobsOverlay is /jobs: what runs now, then what has ended, newest first.
// Stop keeps the list open, so jobs go one by one, and the last of them
// stops the whole lot.
func (h *Host) jobsOverlay() Overlay {
	var running, ended []OverlayItem
	all := h.jobs.List()
	slices.Reverse(all)
	for _, j := range all {
		section := "Ended"
		if j.Running() {
			section = sectionRunning
		}
		item := OverlayItem{
			ID:      j.ID,
			Text:    j.Name,
			Desc:    statusCol(j.Status()) + " · " + fmtDuration(j.Elapsed()) + " · " + j.ID,
			Mark:    j.Mark(),
			Section: section,
			Run:     "jobs view " + j.ID, Verb: VerbOpen,
		}
		if j.Running() {
			item.Keys = []ItemKey{{Key: "s", Desc: "stop", Run: "jobs stop " + j.ID, Danger: true}}
			running = append(running, item)
			continue
		}
		ended = append(ended, item)
	}
	o := Overlay{
		ID: "jobs", Title: "Jobs", Origin: lua.OriginBuiltin, Items: slices.Concat(running, ended),
		Empty:  "No background processes; a workflow or an agent run draws its row here.",
		Reload: func() []OverlayItem { return h.jobsOverlay().Items },
	}
	if h.jobs.Running() > 0 {
		o.Keys = []OverlayKey{{Key: "x", Desc: "stop all", Run: "jobs stop all", Danger: true}}
	}
	return o
}

// jobOverlay is one job's view, drawn as the chat draws a shell call's: its
// facts, the command in a well of the canvas, then the tail of its output,
// which follows as it runs unless the reader scrolls up.
func (h *Host) jobOverlay(j *tools.Job) Overlay {
	status, elapsed := j.Status(), fmtDuration(j.Elapsed())
	out := jobTail(j.Output())
	o := Overlay{
		ID: "job:" + j.ID, Title: "Jobs / " + clipLine(j.Name, 50), Origin: lua.OriginBuiltin,
		Live: true, Body: out, Mark: j.Mark(),
		Fields: []Field{{Name: "status", Value: status}, {Name: "time", Value: elapsed}},
		Parts:  []BodyPart{{Text: j.Cmd, Lang: "bash", Fill: FillCanvas}, {Text: out, Fill: FillWell}},
	}
	if out == "" {
		o.Empty = "(no output yet)"
	}
	if j.Running() {
		o.Keys = []OverlayKey{{Key: "s", Desc: "stop", Run: "jobs stop " + j.ID, Danger: true}}
	}
	return o
}

// jobTail is the last jobViewLines lines of a job's output, naming what it
// left above them.
func jobTail(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) <= jobViewLines {
		return strings.TrimRight(out, "\n")
	}
	return strings.Join(append([]string{MoreLines(len(lines) - jobViewLines)}, lines[len(lines)-jobViewLines:]...), "\n")
}

// overlayTicker refreshes a screen once a second while something it shows is
// still moving: an overlay's rows are a snapshot of the moment they were
// built, so a view of a running thing stands still without it. claim takes
// the watcher's one ticker, nil when there is nothing to share; more
// refreshes and says whether anything still moves, so the last pass sees the
// end. An idle zemi runs no ticker.
func (h *Host) overlayTicker(name string, claim, more func() bool) {
	h.tickOverlay(time.Second, name, claim, more)
}

// tickOverlay is overlayTicker at a given period.
func (h *Host) tickOverlay(every time.Duration, name string, claim, more func() bool) {
	if claim != nil && !claim() {
		return
	}
	h.GoSafe(name, func() {
		for range time.Tick(every) {
			if !more() {
				return
			}
		}
	})
}

// watchJobs runs the one ticker that keeps /jobs and the job views current
// while a job lives. A start or an exit asks for it; an idle zemi runs none.
func (h *Host) watchJobs() {
	h.overlayTicker("job views", h.jobs.ClaimWatch, func() bool {
		last := h.jobs.ReleaseWatch()
		h.refreshJobs()
		return !last && h.jobs.ClaimWatch()
	})
}

// refreshJobs rebuilds /jobs and the job views that are open.
func (h *Host) refreshJobs() {
	ui := h.hooks()
	if ui == nil {
		return
	}
	ui.RefreshOverlay("jobs", h.jobsOverlay)
	for _, j := range h.jobs.List() {
		ui.RefreshOverlay("job:"+j.ID, func() Overlay { return h.jobOverlay(j) })
	}
}
