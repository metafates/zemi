// tasks.go runs background subagents: the agent tool with background set
// returns at once, the subagent keeps working after the turn ends, and its
// report joins the conversation as a message when it finishes (steering a
// running turn, or starting one). /tasks lists them and cancels one.
package hostapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// NOTE: a fixed cap keeps a model from fanning out without bound; make it
// a config knob if people run bigger batches.
const maxBackgroundAgents = 4

type bgTask struct {
	id, agent, description, sessionID string
	started                           time.Time
	cancel                            context.CancelFunc

	mu       sync.Mutex
	status   string // running, done, failed, cancelled
	finished time.Time
}

type taskReg struct {
	mu    sync.Mutex
	tasks map[string]*bgTask
}

func newTaskReg() *taskReg { return &taskReg{tasks: map[string]*bgTask{}} }

func (r *taskReg) list() []*bgTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*bgTask, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].started.After(out[j].started) })
	return out
}

// startBackgroundAgent launches def on prompt detached from the turn and
// returns the task id.
func (h *Host) startBackgroundAgent(def AgentDef, description, prompt string) (string, error) {
	// count and insert under one lock: parallel tool calls race here
	h.tasks.mu.Lock()
	running := 0
	for _, x := range h.tasks.tasks {
		x.mu.Lock()
		if x.status == "running" {
			running++
		}
		x.mu.Unlock()
	}
	if running >= maxBackgroundAgents {
		h.tasks.mu.Unlock()
		return "", fmt.Errorf("%d background agents are already running; wait for one or run this one in the foreground", maxBackgroundAgents)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &bgTask{id: store.NewID()[:8], agent: def.Name, description: description, sessionID: h.AgentSessionID(),
		started: time.Now(), cancel: cancel, status: "running"}
	h.tasks.tasks[t.id] = t
	h.tasks.mu.Unlock()
	h.bgAgents.Add(1)
	h.refresh()
	h.GoSafe("background agent", func() {
		defer cancel()
		defer h.bgAgents.Add(-1)
		text, err := h.runSubagent(ctx, def, description, prompt, nil)
		t.mu.Lock()
		t.finished = time.Now()
		switch {
		case errors.Is(err, context.Canceled):
			t.status = "cancelled"
		case err != nil:
			t.status = "failed"
		default:
			t.status = "done"
		}
		status := t.status
		t.mu.Unlock()
		h.refresh()
		if status == "cancelled" {
			return
		}
		report := text
		if err != nil {
			report = "the agent failed: " + friendlyError(err)
		}
		head := fmt.Sprintf("Background agent %s (%s, %q) %s.", t.id, def.Name, description, map[bool]string{true: "finished", false: "failed"}[err == nil])
		if h.AgentSessionID() != t.sessionID {
			// the user moved on; do not drop the report into another conversation
			h.Notify("info", "background agent "+t.id, head+"\n"+truncRunes(report, 300))
			return
		}
		if err := h.AgentSend(head + attachMarker + "\n<agent-report>\n" + report + "\n</agent-report>\n</attached-files>"); err != nil {
			h.Notify("error", "background agent "+t.id, err.Error())
		}
	})
	return t.id, nil
}

// TasksCommandsStep registers /tasks.
func (h *Host) TasksCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "tasks commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "tasks", NArgs: "*", Desc: "background agents; cancel <id>",
				GoFn: func(h *Host, args string) error {
					sub, id, _ := strings.Cut(strings.TrimSpace(args), " ")
					switch sub {
					case "":
						return h.openTasks()
					case "cancel":
						return h.cancelTask(strings.TrimSpace(id))
					}
					return fmt.Errorf("unknown /tasks subcommand %q", sub)
				},
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			return err
		},
	}
}

func (h *Host) cancelTask(id string) error {
	h.tasks.mu.Lock()
	t := h.tasks.tasks[id]
	h.tasks.mu.Unlock()
	if t == nil {
		return fmt.Errorf("no background agent %q", id)
	}
	t.cancel()
	return nil
}

func (h *Host) openTasks() error {
	ui := h.hooks()
	list := h.tasks.list()
	if ui == nil || len(list) == 0 {
		return errors.New("no background agents in this run")
	}
	items := make([]any, 0, len(list))
	for _, t := range list {
		t.mu.Lock()
		took := time.Since(t.started)
		if !t.finished.IsZero() {
			took = t.finished.Sub(t.started)
		}
		item := map[string]any{"text": fmt.Sprintf("%s  %-9s %-8s %s  %s", t.id, t.status, t.agent, fmtDuration(took), truncRunes(t.description, 50))}
		if t.status == "running" {
			item["run"] = "tasks cancel " + t.id
		}
		t.mu.Unlock()
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "tasks", Title: "Background agents (enter cancels a running one)", Origin: lua.OriginBuiltin, Items: items})
	return nil
}
