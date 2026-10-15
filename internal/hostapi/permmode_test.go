package hostapi

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// TestModeToggle: the mode cycle action flips between ask and allow, and a
// permissions.mode outside the schema is refused at the write.
func TestModeToggle(t *testing.T) {
	for _, c := range []struct {
		name, setup string
		want        []string // get().name first, then after each press
	}{
		{"allow flips to ask and back", ``, []string{"allow", "ask", "allow"}},
		{"an unknown permissions.mode is refused", `assert(not pcall(zemi.config.set, "permissions.mode", "gone"))`, []string{"allow"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHostUI(t)
			runUser(t, h.RT, c.setup)
			for i, want := range c.want {
				if i > 0 {
					if err := h.cyclePermMode(1); err != nil {
						t.Fatal(err)
					}
				}
				runUser(t, h.RT, fmt.Sprintf(`local m = zemi.permission.mode.get()
					assert(m.name == %q, "mode " .. m.name)
					for _, row in ipairs(zemi.permission.mode.list()) do assert(row.active == nil) end`, want))
			}
		})
	}
	if err := newTestHostUI(t).SetPermMode("nope"); err == nil {
		t.Error("an unknown mode name was taken")
	}
}

// TestPermAsk: zemi.perm.ask is plugin origin only, needs the manifest's
// "perm" word, waits only from a task, and a headless run denies and
// records the ask.
func TestPermAsk(t *testing.T) {
	const ask = `zemi.perm.ask({ subject = "fs.write", arg = "x", reason = "why" }):wait()`
	for _, c := range []struct {
		name   string
		origin lua.Origin
		uses   string
		err    string
	}{
		{name: "user code may not ask", origin: lua.OriginUser, uses: `["perm"]`, err: "user code may not ask the user"},
		{name: "project code may not ask", origin: lua.OriginProject, uses: `["perm"]`, err: "project code may not ask the user"},
		{name: "the manifest must declare perm", origin: lua.OriginPlugin, uses: `["job"]`, err: `declares no "perm"`},
		{name: "a plugin's ask, answered by the user", origin: lua.OriginPlugin, uses: `["perm"]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostPlugin(t, c.uses)
			seedSession(t, h)
			h.SetUIHooks(&fakeUI{askAllowed: true, askBy: "user:once"})
			err := chunkAs(t.Context(), h, r, c.origin, ask)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ui := h.hooks().(*fakeUI)
			if len(ui.asked) != 1 || ui.asked[0].Subject != "fs.write" || ui.asked[0].Owner != "plugin:a" {
				t.Fatalf("the overlay saw %+v, want one fs.write ask from plugin:a", ui.asked)
			}
		})
	}
	h, r := newTestHostPlugin(t, `["perm"]`)
	seedSession(t, h)
	if err := chunkAs(t.Context(), h, r, lua.OriginPlugin, ask); err != nil {
		t.Fatal(err)
	}
	if d := h.HeadlessDenials(); len(d) != 1 || d[0].Subject != "fs.write" {
		t.Fatalf("headless denials = %+v, want one fs.write ask", d)
	}
}

// TestHeadlessDenialNotes: an ask nobody can answer in zemi run is denied and
// noted for the run to report.
func TestHeadlessDenialNotes(t *testing.T) {
	h, _ := newTestHost(t)
	if err := h.SetPermMode("ask"); err != nil {
		t.Fatal(err)
	}
	ok, by := h.askHook()(tools.Request{Subject: "tool.shell", Arg: "make out"})
	if ok || by != tools.ByNobody {
		t.Fatalf("an ask with no UI = %v by %q", ok, by)
	}
	d := h.HeadlessDenials()
	if len(d) != 1 || d[0].Why != DenyApproval || d[0].Subject != "tool.shell" || d[0].Arg != "make out" {
		t.Fatalf("noted %+v", d)
	}
}

// TestPlanApprovalAfterModeGone: a mode the config dropped since the read-only
// agent was picked is no reason to read an approved plan as a dismissal. The
// restore skips it, the fallback mode stands, and the handoff is still recorded.
func TestPlanApprovalAfterModeGone(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}}
	h.SetUIHooks(ui)
	h.Settings.SetQuiet("permissions.mode", "gone") // the mode the agent was picked under, gone since
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	v, ok := h.approvePlan("# The plan")
	if !ok || !v.Approved {
		t.Fatalf("approve: %+v ok=%v", v, ok)
	}
	if got := h.PermMode(); got != config.PermModeAllow {
		t.Fatalf("mode after the approval = %q, want the fallback", got)
	}
	if h.takePlanApproved() != implementNow {
		t.Fatal("the approval recorded no handoff")
	}
}

// TestAskWaiterJudgedAgain: asks reach the user one at a time, and one
// that waited behind a prompt is judged again (the mode now) once the prompt
// is answered, so a mode switch that settles it ends without a second prompt.
func TestAskWaiterJudgedAgain(t *testing.T) {
	for _, c := range []struct {
		name          string
		first, second tools.Request
		by            string        // the first prompt's answer
		during        func(h *Host) // what happens while it is open
		prompts       int
		allowed       [2]bool
		secondBy      string
	}{
		{
			"an answer does not cover the waiter",
			tools.Request{Subject: "tool.shell", Arg: "make out"},
			tools.Request{Subject: "tool.shell", Arg: "make test"},
			"user:once", nil, 2,
			[2]bool{true, true},
			"user:once",
		},
		{
			"an allow switch settles the waiter",
			tools.Request{Subject: "tool.shell", Arg: "make out"},
			tools.Request{Subject: "tool.shell", Arg: "make test"},
			"user:once", func(h *Host) { h.SetPermMode("allow") }, 1,
			[2]bool{true, true},
			tools.ByMode("allow"),
		},
		{
			"a tell denies the waiter",
			tools.Request{Subject: "tool.shell", Arg: "make out"},
			tools.Request{Subject: "tool.shell", Arg: "make test"},
			tools.ByUser, func(h *Host) { h.DenyWaitingAsks() }, 1,
			[2]bool{false, false},
			tools.ByUser,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHost(t)
			if err := h.SetPermMode("ask"); err != nil {
				t.Fatal(err)
			}
			opened := make(chan struct{})
			ui := &fakeUI{}
			ui.onAsk = func(PermRequest) (bool, string) {
				if len(ui.asked) == 1 {
					close(opened)
					// the second is at the prompt stage; t.Error, since this is no test goroutine
					for deadline := time.Now().Add(3 * time.Second); h.AsksWaiting() < 2; runtime.Gosched() {
						if time.Now().After(deadline) {
							t.Error("the second ask never reached the prompt stage")
							break
						}
					}
					if c.during != nil {
						c.during(h)
					}
				}
				return c.by != tools.ByUser, c.by
			}
			h.SetUIHooks(ui)
			pipe := h.PipelineFor()
			var first tools.Verdict
			done := make(chan struct{})
			go func() { first = pipe.Check(c.first); close(done) }()
			<-opened
			second := pipe.Check(c.second)
			<-done
			if got := [2]bool{first.Allowed, second.Allowed}; got != c.allowed || len(ui.asked) != c.prompts || second.DecidedBy != c.secondBy {
				t.Fatalf("allowed %v, %d prompts, second by %q", got, len(ui.asked), second.DecidedBy)
			}
		})
	}
}
