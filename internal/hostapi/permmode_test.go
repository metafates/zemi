package hostapi

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/metafates/zemi/internal/tools"
)

// TestPermModes: with no UI to ask, manual denies, yolo allows, and auto
// allows only what the screening model calls safe.
func TestPermModes(t *testing.T) {
	h, _ := newTestHost(t)
	ask := h.askHook()
	for i, c := range []struct {
		mode, model string
		allowed     bool
		by          string
	}{
		{"manual", "", false, "default"},
		{"yolo", "", true, "mode:yolo"},
		{"auto", "SAFE", true, "mode:auto"},
		{"auto", "UNSAFE: deletes the home dir", false, "default"},
		{"bogus", "", false, "default"},
	} {
		h.Settings.SetHost("permissions.mode", c.mode)
		reflectStub(t, h, c.model)
		// a command auto mode cannot settle locally, new per case (approvals are cached)
		req := tools.Request{Subject: "tool.bash", Arg: fmt.Sprintf("./deploy.sh %d", i)}
		if allowed, by := ask(req); allowed != c.allowed || by != c.by {
			t.Errorf("%s/%q: allowed=%v by=%s", c.mode, c.model, allowed, by)
		}
	}
	if err := h.SetPermMode("bogus"); err == nil {
		t.Error("SetPermMode accepted an unknown mode")
	}
}

// TestPlanModeReadOnlyCommands: plan mode runs commands that only read and
// asks for the rest.
func TestPlanModeReadOnlyCommands(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.SetPermMode("plan"); err != nil {
		t.Fatal(err)
	}
	ask := h.askHook()
	if allowed, by := ask(tools.Request{Subject: "tool.bash", Arg: "find internal -name '*.go' | wc -l"}); !allowed || by != "mode:plan" || len(ui.asked) != 0 {
		t.Fatalf("read-only command: allowed=%v by=%s asked=%d", allowed, by, len(ui.asked))
	}
	ask(tools.Request{Subject: "tool.bash", Arg: "rm -rf build"})
	if len(ui.asked) != 1 {
		t.Fatal("a command that changes files must ask in plan mode")
	}
}

// TestAutoModeLocal: auto mode approves project edits and safe commands
// without the model (which here refuses everything) and still asks for the
// rest.
func TestAutoModeLocal(t *testing.T) {
	h, _ := newTestHost(t)
	reflectStub(t, h, "UNSAFE: refuses everything")
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	ask := h.askHook()
	for _, c := range []struct {
		req     tools.Request
		allowed bool
	}{
		{tools.Request{Subject: "fs.write", Arg: filepath.Join(h.Dirs.ProjectDir, "main.go")}, true},
		{tools.Request{Subject: "tool.bash", Arg: "git status && go test ./..."}, true},
		{tools.Request{Subject: "tool.bash", Arg: "rm -rf build"}, false},
		{tools.Request{Subject: "tool.bash", Arg: "head -5 " + filepath.Join(h.Dirs.ProjectDir, "README.md")}, true},
		{tools.Request{Subject: "tool.bash", Arg: "cat ~/.ssh/id_rsa"}, false},
		{tools.Request{Subject: "tool.bash", Arg: "cat .env"}, false},
	} {
		asked := len(ui.asked)
		allowed, _ := ask(c.req)
		if allowed != c.allowed || (len(ui.asked) > asked) == c.allowed {
			t.Fatalf("%s %q: allowed=%v asked=%v", c.req.Subject, c.req.Arg, allowed, len(ui.asked) > asked)
		}
	}
}
