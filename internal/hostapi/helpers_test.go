package hostapi

import (
	"fmt"
	"testing"
)

// TestSpecFieldTypes pins DESIGN §6.0 for option tables: a field of the
// wrong type raises at the caller's line naming the key, never reads as absent.
func TestSpecFieldTypes(t *testing.T) {
	_, r := newTestHost(t)
	cases := []struct{ name, opts, want string }{
		{"absent", `{}`, ""},
		{"the right types", `{ desc = "d", nargs = "?", complete = function() return {} end }`, ""},
		{"a number for a string", `{ nargs = 1 }`, "test.lua:1: nargs must be a string, not a number"},
		{"a string for a function", `{ complete = "x" }`, "test.lua:1: complete must be a function, not a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runUser(t, r, fmt.Sprintf(`local ok, err = pcall(function() zemi.command.add("t", function() end, %s) end)
				local want = %q
				assert(ok == (want == ""), tostring(err))
				assert(want == "" or tostring(err):find(want, 1, true), tostring(err))`, c.opts, c.want))
		})
	}
}
