package hostapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// TestPanicsDoNotKillZemi: a panicking host goroutine is recovered into
// /errors, and a turn that panics leaves the agent idle, not stuck active.
func TestPanicsDoNotKillZemi(t *testing.T) {
	h, _ := newTestHost(t)
	done := make(chan struct{})
	h.GoSafe("test", func() {
		defer close(done)
		panic("boom")
	})
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for !h.HasUnseenErrors() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !h.HasUnseenErrors() || !strings.Contains(h.errs.entries[0].msg, "boom") {
		t.Fatalf("panic not recorded: %+v", h.errs.entries)
	}

	h.agents.mu.Lock()
	h.agents.active = true
	h.agents.mu.Unlock()
	func() {
		defer h.endTurnAfterPanic()
		panic("turn exploded")
	}()
	if h.AgentActive() {
		t.Fatal("agent still active after a turn panic")
	}
}

// TestFailedTurnLeavesErrorRow: a turn that fails ends with an error row in
// the transcript (and the agent idle); an interrupt reads as interrupted.
func TestFailedTurnLeavesErrorRow(t *testing.T) {
	h, _ := newTestHost(t)
	sa := &streamAdapter{h: h}
	for _, c := range []struct {
		err    error
		text   string
		status string
	}{
		{errors.New("dial tcp: no route to host"), "dial tcp: no route to host", ""},
		{context.Canceled, "interrupted", "cancelled"},
	} {
		sa.TurnDone(c.err, fantasy.Usage{})
		tr := h.Session().Transcript
		last := tr[len(tr)-1]
		if last.Kind != "error" || last.Text != c.text || last.Status != c.status {
			t.Fatalf("%v: last row = %+v", c.err, last)
		}
	}
}

// TestBuiltinSkillsShipEnabled: zemi's self-knowledge skills are written
// to the data dir, discovered as builtin and enabled by default.
func TestBuiltinSkillsShipEnabled(t *testing.T) {
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{
		StateHome: t.TempDir(), ProjectDir: t.TempDir(), DataHome: t.TempDir(),
	})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	h.syncSkills()
	for _, name := range []string{"zemi-config", "zemi-customize", "zemi-debug", "zemi-learning", "zemi-lua"} {
		s, ok := h.skillsReg.get(name)
		if !ok || s.Source != "builtin" || !h.skillsReg.isEnabled(name) {
			t.Errorf("%s: found=%v source=%q enabled=%v", name, ok, s.Source, h.skillsReg.isEnabled(name))
		}
	}
	if !strings.Contains(h.skillsIndexPrompt(), "zemi-debug") {
		t.Fatal("builtin skills missing from the prompt index")
	}
}
