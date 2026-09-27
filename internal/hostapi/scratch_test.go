package hostapi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

func TestEnsureScratchKeepsResumedPad(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	pad := h.ScratchDir()
	old := time.Now().Add(-2 * scratchTTL)
	if err := os.MkdirAll(pad, 0o700); err != nil || os.Chtimes(pad, old, old) != nil {
		t.Fatal(err)
	}
	if h.ensureScratch() != pad {
		t.Fatal("no pad")
	}
	if _, err := os.Stat(pad); err != nil {
		t.Fatalf("a resumed session's idle pad was swept away: %v", err)
	}
}

// TestPadBoundPerPipeline: a pipeline keeps the pad it was built with once
// the session moves on (a background run, /new), so the run reads and writes
// the pad its prompt names with no ask and its commands get it as $TMPDIR;
// another session's pad is zemi's own files to anyone else.
func TestPadBoundPerPipeline(t *testing.T) {
	h, _ := newTestHostDB(t)
	fakeSandbox(h, true)
	h.SetProjectTrusted(true)
	h.agents.sessionID = "s1"
	old, pad := h.PipelineFor(), h.ScratchDir()
	h.agents.sessionID = "s2"
	for _, c := range []struct {
		name, subject string
		p             *tools.Pipeline
		want          perm.Decision
		why           string
	}{
		{"write the bound pad", perm.SubjectWrite, old, perm.Allow, ""},
		{"read the bound pad", perm.SubjectRead, old, perm.Allow, ""},
		{"write another session's pad", perm.SubjectWrite, h.PipelineFor(), perm.Deny, "zemi's own files"},
	} {
		v := c.p.Check(tools.Request{Subject: c.subject, Arg: filepath.Join(pad, "notes.md")})
		if v.Decision != c.want || !strings.Contains(v.Reason, c.why) {
			t.Errorf("%s: %s (%s), want %s", c.name, v.Decision, v.Reason, c.want)
		}
	}
	pol := old.SandboxFor(tools.Request{Subject: perm.SubjectBash, Arg: "ls"}, tools.Verdict{Decision: perm.Allow, DecidedBy: tools.ByMode("auto")})
	if pol == nil || pol.Env["TMPDIR"] != pad || !slices.Contains(pol.Write, pad) {
		t.Errorf("the run's sandbox lost its pad %s: %+v", pad, pol)
	}
}
