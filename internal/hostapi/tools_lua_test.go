package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// TestLuaToolAfterReload: a runner built before a reload still holds the old
// generation's tool, which must refuse instead of calling into it (DESIGN §4).
func TestLuaToolAfterReload(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "tool.weather", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `zemi.tool.add({ name = "weather", description = "the weather", execute = function() return "sunny" end })`)
	tool := h.luaFantasyTools(h.PipelineFor())[0]
	h.gen.Add(1)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: "{}"})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "reload") {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}

// TestLuaToolAskCarriesWholeInput: an approval (auto mode caches them) covers
// the input the ask showed, so the ask must show all of it.
func TestLuaToolAskCarriesWholeInput(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `zemi.tool.add({ name = "weather", description = "the weather", execute = function() return "sunny" end })`)
	input := `{"city":"` + strings.Repeat("a", 400) + `b"}`
	if _, err := h.luaFantasyTools(h.PipelineFor())[0].Run(context.Background(), fantasy.ToolCall{Input: input}); err != nil {
		t.Fatal(err)
	}
	if len(ui.asked) != 1 || ui.asked[0].Preview != input {
		t.Fatalf("asked %+v", ui.asked)
	}
}

// TestToolPreHandlerErrorRecorded: a tool.pre handler that raises lets the
// call proceed, and one that raises or returns a bad shape shows in /errors.
func TestToolPreHandlerErrorRecorded(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`return ev.input.path:match("%.env$") and { decision = "deny" }`, ""},
		{`return "deny"`, "deny"},
	} {
		h, r := newTestHost(t)
		runUser(t, r, `zemi.event.on("tool.pre", function(ev) `+c.body+` end)`)
		if d, _, _, _ := h.toolPreHook()("bash", map[string]any{"command": "cat .env"}); d != c.want || !h.HasUnseenErrors() {
			t.Errorf("%s: decision %q, want %q; unseen errors %v", c.body, d, c.want, h.HasUnseenErrors())
		}
	}
}

func TestLuaToolFailures(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "tool.ticket", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `zemi.tool.add({
		name = "ticket", description = "look up a ticket",
		input = { id = { type = "string" }, n = { type = "integer" } }, required = { "id" },
		execute = function(args)
			if args.id == "raise" then error("boom") end
			if args.id == "spin" then while true do end end
			if args.id == "fail" then return nil, "no such ticket" end
			return "ticket " .. args.id
		end,
	})`)
	tool := h.luaFantasyTools(h.PipelineFor())[0]
	for input, want := range map[string]string{
		`{"id":"raise"}`:               "boom",
		`{"id":"fail","n":2,"x":null}`: "ticket: no such ticket",
		`{}`:                           `missing required field "id"`,
		`{"id":1}`:                     `field "id" must be string`,
		`{"id":"a","n":"one"}`:         `field "n" must be integer`,
		`{"id":"a","n":1.5}`:           `field "n" must be integer`,
	} {
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: input})
		if err != nil || !resp.IsError || !strings.HasSuffix(resp.Content, want) {
			t.Errorf("%s: resp = %+v, %v, want an error ending in %q", input, resp, err, want)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if resp, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"id":"spin"}`}); err != nil || !resp.IsError {
		t.Errorf("interrupted: resp = %+v, %v, want an error result", resp, err)
	}
	if errs := h.errs.entries; len(errs) != 1 || !strings.Contains(errs[0].msg, "stack traceback") {
		t.Errorf("/errors = %+v, want the raise with its traceback", errs)
	}
}

func TestToolRegCollision(t *testing.T) {
	cases := []struct {
		name      string
		old, new  lua.Origin
		wantError bool
	}{
		{"higher origin replaces", lua.OriginPlugin, lua.OriginUser, false},
		{"same origin last writer wins", lua.OriginUser, lua.OriginUser, false},
		{"lower origin refused", lua.OriginUser, lua.OriginProject, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newToolReg()
			if err := r.add(&luaTool{Name: "search", Origin: c.old}); err != nil {
				t.Fatal(err)
			}
			err := r.add(&luaTool{Name: "search", Origin: c.new})
			want := c.new
			if c.wantError {
				want = c.old
			}
			if (err != nil) != c.wantError || r.byName["search"].Origin != want {
				t.Fatalf("err=%v, kept %s, want %s", err, r.byName["search"].Origin, want)
			}
		})
	}
}

// TestDeferredCallbackKeepsOrigin: a callback runs later under the origin
// that handed it over, so project code cannot launder a refused call
// through one (DESIGN §9).
func TestDeferredCallbackKeepsOrigin(t *testing.T) {
	cases := map[string]string{
		"timer override":   `zemi.timer.after(0, function() zemi.tool.override("bash", { pre = function() end }) end)`,
		"ui.edit override": `zemi.ui.edit("f.txt", {}, function() zemi.tool.override("bash", { pre = function() end }) end)`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			h, r := newTestHost(t)
			if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "f.txt"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := r.ExecuteChunk(context.Background(), src, "project-chunk", lua.OriginProject); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() error {
				h.errs.mu.Lock()
				defer h.errs.mu.Unlock()
				for _, e := range h.errs.entries {
					if strings.Contains(e.msg, "project origin may not register") {
						return nil
					}
				}
				return fmt.Errorf("callback was not refused: %+v", h.errs.entries)
			})
			if len(h.luaTools.overridesFor("bash")) != 0 || len(h.luaTools.list()) != 0 {
				t.Fatal("project callback registered a tool or override")
			}
		})
	}
}

// TestLuaToolWaitFreesInterpreter: execute waiting on a job suspends, so a
// command runs meanwhile (here it is what ends the wait), and a reload ends
// the wait at once with a tool error (DESIGN §4).
func TestLuaToolWaitFreesInterpreter(t *testing.T) {
	for _, c := range []struct{ act, want string }{
		{"command", "hi"},
		{"reload", "reload"},
	} {
		t.Run(c.act, func(t *testing.T) {
			h, r := newTestHost(t)
			h.Perm.Add(perm.Rule{Subject: "tool.slow", Pattern: "*", Decision: perm.Allow})
			h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
			runUser(t, r, `
				local job
				zemi.tool.add({ name = "slow", description = "waits on a job", execute = function()
					job = zemi.job.start({ cmd = "cat", stdin = true })
					return job:wait(5000).stdout
				end })
				zemi.command.add("ready", function() assert(job, "not waiting yet") end, {})
				zemi.command.add("release", function() job:write("hi") job:close_stdin() end, {})
			`)
			tool := h.luaFantasyTools(h.PipelineFor())[0]
			done := make(chan fantasy.ToolResponse, 1)
			go func() {
				resp, _ := tool.Run(context.Background(), fantasy.ToolCall{Input: "{}"})
				done <- resp
			}()
			// a command runs while execute waits
			waitFor(t, func() error { return h.RunCommand(context.Background(), "ready", "") })
			var err error
			if c.act == "reload" {
				err = r.ForceReload(context.Background(), []lua.BootStep{h.NamespacesStep()}, h.PurgeGeneration)
			} else {
				err = h.RunCommand(context.Background(), "release", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			var resp fantasy.ToolResponse
			select {
			case resp = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("the tool did not return within 3s")
			}
			if resp.IsError != (c.act == "reload") || !strings.Contains(resp.Content, c.want) {
				t.Fatalf("resp = %+v, want %q", resp, c.want)
			}
		})
	}
}
