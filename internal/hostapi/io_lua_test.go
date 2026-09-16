package hostapi

import (
	"testing"

	"github.com/metafates/zemi/internal/perm"
)

// TestJobStartFailure: a command that cannot start is a runtime failure,
// so job.start and job.out return nil, err instead of raising (DESIGN §6.0).
func TestJobStartFailure(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `
		local res, err = zemi.job.out({ cmd = "zemi-no-such-binary" })
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
		assert(zemi.job.out({ cmd = "true", args = { "x" } }), "allowed")
		assert(not pcall(zemi.job.out, { cmd = "echo", args = { "true" } }), "the program leads the line")
		assert(not pcall(zemi.win.open, { cmd = "true; echo" }), "a second command asks")
	`)
}
