package hostapi

import (
	"testing"

	"github.com/metafates/zemi/internal/perm"
)

// TestJobOutStartFailure: a command that cannot start is a runtime failure,
// so job.out returns nil, err instead of raising (DESIGN §6.0).
func TestJobOutStartFailure(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `
		local res, err = zemi.job.out({ cmd = "zemi-no-such-binary" })
		assert(res == nil and type(err) == "string", tostring(err))
	`)
}
