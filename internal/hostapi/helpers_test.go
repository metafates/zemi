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

// TestArgTypes pins the same for a positional argument, which a bare
// assertion used to drop without a word.
func TestArgTypes(t *testing.T) {
	_, r := newTestHost(t)
	cases := []struct{ name, src, want string }{
		{"agent.run on_done", `zemi.agent.run({ prompt = "x" }, "x")`, "zemi.agent.run: the argument must be a function, not a string"},
		{"workflow.run on_done", `zemi.workflow.run({ script = "return 1" }, 1)`, "zemi.workflow.run: the argument must be a function, not a number"},
		{"session.list opts", `zemi.session.list("x")`, "zemi.session.list: the argument must be a table, not a string"},
		{"http.get opts", `zemi.http.get("http://127.0.0.1:1/x", "x")`, "zemi.http.get: the argument must be a table, not a string"},
		{"job.start on_stdout", `zemi.job.start({ cmd = "true", on_stdout = "x" })`, "on_stdout must be a function, not a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runUser(t, r, fmt.Sprintf(`local ok, err = pcall(function() %s end)
				assert(not ok and tostring(err):find(%q, 1, true), tostring(err))`, c.src, c.want))
		})
	}
}
