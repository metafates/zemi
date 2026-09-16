package hostapi

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestPlanMode: plan mode refuses file changes even under an allow rule
// and turns allowed commands that change things into asks, and the prompt
// says so.
func TestPlanMode(t *testing.T) {
	h, _ := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "make deploy*", Decision: perm.Allow})
	check := func(subject, arg string) tools.Verdict {
		pipe := h.PipelineFor()
		pipe.Ask = nil
		return pipe.Check(tools.Request{Subject: subject, Arg: arg})
	}
	if v := check("fs.write", "/p/a.go"); v.Decision != perm.Allow {
		t.Fatalf("manual write = %v", v.Decision)
	}
	if err := h.SetPermMode("plan"); err != nil {
		t.Fatal(err)
	}
	if v := check("fs.write", "/p/a.go"); v.Decision != perm.Deny || v.DecidedBy != "mode:plan" {
		t.Fatalf("plan write = %v by %s", v.Decision, v.DecidedBy)
	}
	if v := check("tool.bash", "make deploy"); v.Decision != perm.Ask {
		t.Fatalf("plan bash = %v", v.Decision)
	}
	if !strings.Contains(h.buildSystem(nil), "plan mode") {
		t.Fatal("system prompt misses the plan mode section")
	}
}

// TestShellNotes: a !command shows in the transcript and its output rides
// along with the next message to the model, behind the attachment marker.
func TestShellNotes(t *testing.T) {
	h, _ := newTestHostDB(t)
	var seen atomic.Value
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, _ bool) []fantasy.StreamPart {
			seen.Store(last)
			return textParts("ok")
		}}, nil
	}
	h.RunShell("echo shell-output")
	waitFor(t, func() error {
		if tr := h.Session().Transcript; len(tr) != 1 || tr[0].Kind != "shell" || tr[0].Detail != "shell-output" {
			return fmt.Errorf("shell row missing: %+v", tr)
		}
		return nil
	})
	if err := h.AgentSend("what did it print?"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	got, _ := seen.Load().(string)
	if !strings.HasPrefix(got, "what did it print?"+attachMarker) || !strings.Contains(got, "shell-output") {
		t.Fatalf("model saw %q", got)
	}
	if h.takeShellNotes() != "" {
		t.Fatal("notes must be consumed by the message")
	}
}
