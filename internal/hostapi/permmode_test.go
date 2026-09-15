package hostapi

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/metafates/zemi/internal/tools"
)

// TestApprovePlan: in plan mode the approval choices name the mode to
// implement in, the one active before plan mode first and never yolo;
// approving switches to it, typed feedback stays in plan mode.
func TestApprovePlan(t *testing.T) {
	for _, c := range []struct {
		name, answer, wantMode string
		approved               bool
	}{
		{"approval returns to the earlier mode", "Yes, in auto mode", "auto", true},
		{"feedback keeps planning", "use sqlite instead", "plan", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHost(t)
			ui := &fakeUI{qAnswer: c.answer, qOK: true}
			h.SetUIHooks(ui)
			for _, m := range []string{"auto", "plan"} {
				if err := h.SetPermMode(m); err != nil {
					t.Fatal(err)
				}
			}
			v, ok := h.approvePlan("1. do it")
			if !ok || v.Approved != c.approved || h.PermMode() != c.wantMode || (!c.approved && v.Feedback != c.answer) {
				t.Fatalf("verdict %+v ok=%v mode %s", v, ok, h.PermMode())
			}
			var labels []string
			for _, o := range ui.qAsked[0].Options {
				labels = append(labels, o.Label)
			}
			if got := fmt.Sprint(labels); got != "[Yes, in auto mode Yes, in manual mode]" {
				t.Fatalf("choices %s", got)
			}
		})
	}
}

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

// TestCustomModes: user config removes a mode and adds one whose rules
// answer asks before its base does.
func TestCustomModes(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `
		zemi.permission.mode.remove("yolo")
		zemi.permission.mode.add("tests", {
			base = "manual", description = "tests run",
			rules = { { "tool.bash", "go test*", "allow" }, { subject = "tool.bash", pattern = "git push*", decision = "deny" } },
		})
		local names = {}
		for _, m in ipairs(zemi.permission.mode.list()) do names[#names + 1] = m.name end
		assert(table.concat(names, ",") == "manual,plan,auto,tests", table.concat(names, ","))
		assert(not pcall(zemi.permission.mode.add, "x", { base = "nope" }), "bad base must raise")
	`)
	if err := h.SetPermMode("yolo"); err == nil {
		t.Fatal("a removed mode must not be selectable")
	}
	if err := h.SetPermMode("tests"); err != nil {
		t.Fatal(err)
	}
	ask := h.askHook()
	for _, c := range []struct {
		cmd     string
		allowed bool
		asks    int
	}{
		{"go test ./...", true, 0},
		{"git push origin main", false, 0},
		{"rm -rf build", false, 1},
	} {
		before := len(ui.asked)
		allowed, _ := ask(tools.Request{Subject: "tool.bash", Arg: c.cmd})
		if allowed != c.allowed || len(ui.asked)-before != c.asks {
			t.Errorf("%q: allowed=%v asks=%d", c.cmd, allowed, len(ui.asked)-before)
		}
	}
	if err := h.cyclePermMode(1); err != nil || h.PermMode() != "manual" {
		t.Fatalf("cycle after the last mode: %v %s", err, h.PermMode())
	}
}
