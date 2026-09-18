package hostapi

import (
	"context"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/perm"
	glua "github.com/yuin/gopher-lua"
)

// TestJobStartFailure: a command that cannot start is a runtime failure,
// so job.start and job.wait return nil, err instead of raising (DESIGN §6.0).
func TestJobStartFailure(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `
		local res, err = zemi.job.wait({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
		res, err = zemi.job.start({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
	`)
}

func TestJobHandleColonCalls(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `
		local j = zemi.job.start({ cmd = "cat", stdin = true })
		assert(j:write("hi") == true, "write")
		j:close_stdin()
		local res, err = j:wait(2000)
		assert(res and res.stdout == "hi", tostring(err))
	`)
}

// TestJobGateSeesCommandLine: lua.job.start rules match the whole normalized
// line, so arguments are scoped and a compound window command cannot ride.
func TestJobGateSeesCommandLine(t *testing.T) {
	h, r := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "true*", Decision: perm.Allow})
	runUser(t, r, `
		assert(zemi.job.wait({ cmd = "true", args = { "x" } }), "allowed")
		assert(not pcall(zemi.job.wait, { cmd = "echo", args = { "true" } }), "the program leads the line")
		assert(not pcall(zemi.win.open, { cmd = "true; echo" }), "a second command asks")
	`)
}

// TestWaitEndsWithTheRequest: an interpreter-blocking wait ends when the
// request's context is canceled (its ceiling, or ctrl+c), so it cannot
// outlive the watchdog grace and leave the runtime hot (DESIGN §4).
func TestWaitEndsWithTheRequest(t *testing.T) {
	L := glua.NewState()
	defer L.Close()
	ctx, cancel := context.WithCancel(t.Context())
	L.SetContext(ctx)
	cancel()
	if err := waitDone(L, make(chan struct{}), time.Minute); err == nil || err.Error() != "canceled" {
		t.Fatalf("waitDone = %v, want canceled", err)
	}
}
