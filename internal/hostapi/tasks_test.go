package hostapi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestTaskRegEviction: a full registry forgets its oldest finished run, never
// the one being added, however old that one is.
func TestTaskRegEviction(t *testing.T) {
	reg := newTaskReg()
	for i := range maxKeptRuns {
		reg.add(&agentRun{id: fmt.Sprint(i), status: "done", started: time.Now()})
	}
	old := &agentRun{id: "old", status: "done", started: time.Now().Add(-time.Hour)}
	reg.add(old)
	if reg.find("old") != old || len(reg.runs) != maxKeptRuns {
		t.Fatalf("added run evicted, or nothing was: %d runs", len(reg.runs))
	}
}

// TestBackgroundAgent: the parent turn launches a slow background agent and
// finishes; the report then arrives as a message that starts a new turn,
// shown in the transcript as a one-line notice.
func TestBackgroundAgent(t *testing.T) {
	h, _ := newTestHostDB(t)
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reportMsg string
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				time.Sleep(150 * time.Millisecond)
				return textParts("found the thing")
			case toolResult:
				return textParts("started it")
			case strings.HasPrefix(last, "Background agent"):
				reportMsg = last
				return textParts("the agent reported: " + last[strings.Index(last, "found"):strings.Index(last, "found")+15])
			}
			return toolCallParts("c1", "agent", `{"description":"slow @main.go search","prompt":"find it","agent":"explore","background":true}`)
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if n := h.tasks.running(); n != 1 {
		t.Fatalf("background agents running = %d, want 1", n)
	}
	waitFor(t, func() error {
		if tr := h.Session().Transcript; tr[len(tr)-1].Text != "the agent reported: found the thing" || h.AgentActive() {
			return fmt.Errorf("report never arrived: %+v", tr)
		}
		return nil
	})
	if !strings.Contains(reportMsg, "slow @main.go search") || strings.Contains(reportMsg, "<file path=") {
		t.Fatalf("the model-written description must reach the model but attach nothing: %q", reportMsg)
	}
	notice := false
	for _, m := range h.Session().Transcript {
		if m.Role == "user" && strings.HasPrefix(m.Text, "Background agent ") {
			notice = !strings.Contains(m.Text, "found the thing")
		}
	}
	if !notice {
		t.Fatalf("transcript should show a one-line notice: %+v", h.Session().Transcript)
	}
}

// TestMessageAgent: a message to a running background agent reaches it at its
// next step boundary, or as one more turn when its last step already started,
// and is stored in its child session; a finished run refuses one.
func TestMessageAgent(t *testing.T) {
	viaTool := func(h *Host, id, text string) (string, error) {
		resp, err := h.agentTool().Run(t.Context(), fantasy.ToolCall{ID: "c2", Input: fmt.Sprintf(`{"description":"d","agent":"explore","prompt":%q,"send_to":%q}`, text, id)})
		if err == nil && resp.IsError {
			err = errors.New(resp.Content)
		}
		return resp.Content, err
	}
	viaView := func(h *Host, id, text string) (string, error) {
		if err := h.ViewAgent(id); err != nil {
			return "", err
		}
		return "", h.MessageAgent(id, text)
	}
	run := func(name string, firstCallsTool bool, send func(*Host, string, string) (string, error)) {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			started, release := make(chan struct{}), make(chan struct{})
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
					switch {
					case !strings.Contains(system, "a subagent of zemi"):
						return textParts("ok")
					case last == "look":
						close(started)
						<-release
						if firstCallsTool {
							return toolCallParts("t1", "glob", `{"pattern":"*.none"}`)
						}
						return textParts("first")
					case last == "also this" && !toolResult:
						return textParts("got it")
					}
					return textParts("unexpected: " + last)
				}}, nil
			}
			if _, err := viaTool(h, "", ""); err == nil {
				t.Fatal("an empty prompt must be refused")
			}
			resp, err := h.agentTool().Run(t.Context(), fantasy.ToolCall{ID: "c1", Input: `{"description":"d","agent":"explore","prompt":"look","background":true}`})
			if err != nil || resp.IsError {
				t.Fatalf("start: %v %s", err, resp.Content)
			}
			<-started
			id := h.tasks.list("")[0].id
			out, err := send(h, id, "also this")
			if err != nil || (out != "" && !strings.Contains(out, "Queued")) {
				t.Fatalf("send: %q %v", out, err)
			}
			close(release)
			var r *agentRun
			waitFor(t, func() error {
				if r = h.tasks.find(id); r.isRunning() {
					return errors.New("run never finished")
				}
				return nil
			})
			tr := r.transcript()
			if len(tr) < 3 || tr[len(tr)-2].Text != "also this" || tr[len(tr)-1].Text != "got it" {
				t.Fatalf("transcript: %+v", tr)
			}
			_, _, _, sid := r.state()
			stored, err := h.loadRun(sid)
			if err != nil || !strings.Contains(fmt.Sprint(stored.transcript()), "{user  also this") {
				t.Fatalf("stored run: %v %+v", err, stored.transcript())
			}
			if _, err := send(h, id, "too late"); err == nil || !strings.Contains(err.Error(), "not running") {
				t.Fatalf("finished run took a message: %v", err)
			}
			waitIdle(t, h)
		})
	}
	run("model at a step boundary", true, viaTool)
	run("user after the last step", false, viaView)
}
