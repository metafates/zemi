package hostapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
)

// TestLuaToolAfterReload: a runner built before a reload still holds the old
// generation's tool, which runs the one the reload registered under its
// name and refuses when there is none, never calling into the old
// generation (DESIGN §4).
func TestLuaToolAfterReload(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ reloaded, want string }{
		{`zemi.tool.add({ name = "weather", description = "the weather", execute = function() return "rain" end })`, "rain"},
		{"", "weather: the tool is gone after a config reload"},
	} {
		h, r := newTestHost(t)
		runUser(t, r, `zemi.tool.add({ name = "weather", description = "the weather", execute = function() return "sunny" end })`)
		tool := h.luaFantasyTools(h.PipelineFor())[0]
		if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
			t.Fatal(err)
		}
		runUser(t, r, c.reloaded)
		if resp, err := tool.Run(t.Context(), fantasy.ToolCall{Input: "{}"}); err != nil || resp.Content != c.want {
			t.Errorf("reloaded %q: %+v, %v", c.reloaded, resp, err)
		}
	}
}

// TestLuaToolAskCarriesWholeInput: the ask's preview is the whole input, so
// the user approves what will run.
func TestLuaToolAskCarriesWholeInput(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	askEverything(t, r)
	runUser(t, r, `zemi.tool.add({ name = "weather", description = "the weather", execute = function() return "sunny" end })`)
	input := `{"city":"` + strings.Repeat("a", 400) + `b"}`
	if _, err := h.luaFantasyTools(h.PipelineFor())[0].Run(t.Context(), fantasy.ToolCall{Input: input}); err != nil {
		t.Fatal(err)
	}
	if len(ui.asked) != 1 || ui.asked[0].Preview != input {
		t.Fatalf("asked %+v", ui.asked)
	}
}

// TestLuaToolFinalStopsTurn: a final tool's success is the turn's last word,
// a failing one lets the model retry.
func TestLuaToolFinalStopsTurn(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `zemi.tool.add({ name = "verdict", description = "the verdict", final = true, execute = function() return "done" end })
		zemi.tool.add({ name = "flaky", description = "fails", final = true, execute = function() return nil, "no" end })`)
	byName := map[string]fantasy.AgentTool{}
	for _, lt := range h.luaFantasyTools(h.PipelineFor()) {
		byName[lt.Info().Name] = lt
	}
	resp, err := byName["verdict"].Run(t.Context(), fantasy.ToolCall{Input: "{}"})
	if err != nil || !resp.StopTurn {
		t.Fatalf("a successful final tool must end the turn: resp=%+v err=%v", resp, err)
	}
	resp, err = byName["flaky"].Run(t.Context(), fantasy.ToolCall{Input: "{}"})
	if err != nil || resp.StopTurn {
		t.Fatalf("a failed final tool must not end the turn: resp=%+v err=%v", resp, err)
	}
}

// TestToolPreHandlerErrorRecorded: a tool.pre handler that raises lets the
// call proceed, and one that raises or returns a bad shape shows in /errors.
func TestToolPreHandlerErrorRecorded(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ body, want string }{
		{`return ev.input.path:match("%.env$") and { decision = "deny" }`, ""},
		{`return "deny"`, "deny"},
	} {
		h, r := newTestHost(t)
		runUser(t, r, `zemi.event.on("tool.pre", function(ev) `+c.body+` end)`)
		if d, _, _ := h.toolPreHook()("shell", map[string]any{"command": "cat .env"}); string(d) != c.want || !h.HasUnseenErrors() {
			t.Errorf("%s: decision %q, want %q; unseen errors %v", c.body, d, c.want, h.HasUnseenErrors())
		}
	}
}

func TestLuaToolFailures(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `zemi.tool.add({
		name = "ticket", description = "look up a ticket",
		input = { id = { type = "string" }, n = { type = "integer" } }, required = { "id" },
		execute = function(args)
			if args.id == "raise" then error("boom") end
			if args.id == "spin" then while true do end end
			if args.id == "fail" then return nil, "no such ticket" end
			if args.id == "nan" then return { n = 0/0 } end
			return "ticket " .. args.id
		end,
	})`)
	tool := h.luaFantasyTools(h.PipelineFor())[0]
	for input, want := range map[string]string{
		`{"id":"raise"}`:               "boom",
		`{"id":"fail","n":2,"x":null}`: "ticket: no such ticket",
		`{"id":"nan"}`:                 "unsupported value: NaN",
		`{}`:                           `missing required field "id"`,
		`{"id":1}`:                     `field "id" must be string`,
		`{"id":"a","n":"one"}`:         `field "n" must be integer`,
		`{"id":"a","n":1.5}`:           `field "n" must be integer`,
	} {
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
		if err != nil || !resp.IsError || !strings.HasSuffix(resp.Content, want) {
			t.Errorf("%s: resp = %+v, %v, want an error ending in %q", input, resp, err, want)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if resp, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"id":"spin"}`}); err != nil || !resp.IsError {
		t.Errorf("interrupted: resp = %+v, %v, want an error result", resp, err)
	}
	if errs := h.errs.entries; len(errs) != 1 || !strings.Contains(errs[0].msg, "stack traceback") {
		t.Errorf("/errors = %+v, want the raise with its traceback", errs)
	}
}

// TestDeferredCallbackKeepsOrigin: a callback runs later as the code that
// handed it over, so the override it registers is attributed to that code.
func TestDeferredCallbackKeepsOrigin(t *testing.T) {
	h, r := newTestHost(t)
	if err := r.ExecuteChunk(t.Context(), `zemi.timer.after(0, function() zemi.tool.override("shell", { pre = function() end }) end)`, "project-chunk", lua.OriginProject); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if o := h.luaTools.overridesFor("shell"); len(o) == 1 && o[0].Origin == lua.OriginProject {
			return nil
		}
		return fmt.Errorf("callback not attributed: %+v", h.luaTools.overridesFor("shell"))
	})
}

// TestLuaToolWaitFreesInterpreter: execute waiting on a job suspends, so a
// command runs meanwhile (here it is what ends the wait), and a reload ends
// the wait at once with a tool error (DESIGN §4).
func TestLuaToolWaitFreesInterpreter(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ act, want string }{
		{"command", "hi"},
		{"reload", "reload"},
	} {
		t.Run(c.act, func(t *testing.T) {
			h, r := newTestHost(t)
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
				resp, _ := tool.Run(t.Context(), fantasy.ToolCall{Input: "{}"})
				done <- resp
			}()
			// a command runs while execute waits
			waitFor(t, func() error { return h.RunCommand(t.Context(), "ready", "") })
			var err error
			if c.act == "reload" {
				err = r.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}, h.PurgeGeneration)
			} else {
				err = h.RunCommand(t.Context(), "release", "")
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

// TestToolPostRewrite: the tool.post handlers, then the tool's override post,
// each see the output the one before left; a project handler's return and a
// failing handler change nothing.
func TestToolPostRewrite(t *testing.T) {
	for _, c := range []struct{ name, user, project, want string }{
		{"handlers then the override", `
			zemi.tool.override("echo", { post = function(ev) return { output = ev.output .. " b" } end })
			zemi.tool.override("other", { post = function() return { output = "another tool's" } end })
			zemi.event.on("tool.post", function(ev) return { output = ev.output .. " a" } end)`, "", "x a b"},
		{"the payload", `zemi.event.on("tool.post", function(ev)
			return { output = table.concat({ ev.tool, tostring(ev.ok), type(ev.ms), ev.output }, " ") }
		end)`, "", "echo true number x"},
		{"a project return applies", "", `zemi.event.on("tool.post", function() return { output = "planted" } end)`, "planted"},
		{"a failing handler leaves it", `zemi.event.on("tool.post", function() error("boom") end)`, "", "x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, r := newTestHost(t)
			runUser(t, r, `zemi.tool.add({ name = "echo", description = "echo", execute = function() return "x" end })`+c.user)
			if c.project != "" {
				if err := r.ExecuteChunk(t.Context(), c.project, "project-chunk", lua.OriginProject); err != nil {
					t.Fatal(err)
				}
			}
			resp, err := h.luaFantasyTools(h.PipelineFor())[0].Run(t.Context(), fantasy.ToolCall{Input: "{}"})
			if err != nil || resp.Content != c.want {
				t.Fatalf("output %q, %v; want %q", resp.Content, err, c.want)
			}
			if failed := strings.Contains(c.user, "boom"); h.HasUnseenErrors() != failed {
				t.Errorf("unseen errors %v, want %v", h.HasUnseenErrors(), failed)
			}
		})
	}
}

// TestTurnTelemetry: a Lua tool's run:progress shows on its row, and each
// model call of the turn emits agent.step with its tool calls.
func TestTurnTelemetry(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	runUser(t, r, `
		zemi.tool.add({ name = "fetch3", description = "fetch", execute = function(_, run)
			run:progress("fetching\n3/5")
			return "done"
		end })
		zemi.event.on("agent.step", function(ev)
			zemi.state.set("step" .. ev.step, ev.tools .. " " .. type(ev.first_token_ms) .. " " .. type(ev.usage.cost))
		end)`)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, toolResult bool) []fantasy.StreamPart {
			if toolResult {
				return textParts("ok")
			}
			return toolCallParts("c1", "fetch3", "{}")
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	var detail string
	for _, m := range h.Session().Transcript {
		if m.Tool == "fetch3" {
			detail = m.Detail
		}
	}
	if detail != "fetching 3/5" {
		t.Errorf("row detail = %q", detail)
	}
	// each emit runs on its own goroutine, so the two may land in either order
	waitFor(t, func() error {
		one, _ := h.GlobalDB.GetKV("step1")
		two, _ := h.GlobalDB.GetKV("step2")
		if one != `"1 number number"` || two != `"0 number number"` {
			h.evlog.mu.Lock()
			emitted := []any{}
			for _, e := range h.evlog.entries {
				if e.name == "agent.step" {
					emitted = append(emitted, e.payload["step"])
				}
			}
			h.evlog.mu.Unlock()
			return fmt.Errorf("agent.step = %s, %s (events emitted for steps %v)", one, two, emitted)
		}
		return nil
	})
}
