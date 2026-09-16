package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
)

// TestLuaToolAfterReload: a runner built before a reload still holds the old
// generation's tool, which must refuse instead of calling into it (DESIGN §4).
func TestLuaToolAfterReload(t *testing.T) {
	h, r := newTestHost(t)
	h.Perm.Add(perm.Rule{Subject: "tool.weather", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `zemi.tool.register({ name = "weather", execute = function() return "sunny" end })`)
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
	runUser(t, r, `zemi.tool.register({ name = "weather", execute = function() return "sunny" end })`)
	input := `{"city":"` + strings.Repeat("a", 400) + `b"}`
	if _, err := h.luaFantasyTools(h.PipelineFor())[0].Run(context.Background(), fantasy.ToolCall{Input: input}); err != nil {
		t.Fatal(err)
	}
	if len(ui.asked) != 1 || ui.asked[0].Preview != input {
		t.Fatalf("asked %+v", ui.asked)
	}
}

// TestDeferredCallbackKeepsOrigin: a callback runs later under the origin
// that handed it over, so learned code cannot launder a refused call
// through one (DESIGN §9).
func TestDeferredCallbackKeepsOrigin(t *testing.T) {
	cases := map[string]string{
		"timer override":   `zemi.timer.set(0, function() zemi.tool.override("bash", { pre = function() end }) end)`,
		"timer register":   `zemi.timer.set(0, function() zemi.tool.register({ name = "evil", execute = function() end }) end)`,
		"ui.edit override": `zemi.ui.edit("f.txt", {}, function() zemi.tool.override("bash", { pre = function() end }) end)`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			h, r := newTestHost(t)
			if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "f.txt"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := r.ExecuteChunk(context.Background(), src, "lua-tool", lua.OriginLearned); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() error {
				h.errs.mu.Lock()
				defer h.errs.mu.Unlock()
				for _, e := range h.errs.entries {
					if strings.Contains(e.msg, "learned origin may not register") {
						return nil
					}
				}
				return fmt.Errorf("callback was not refused: %+v", h.errs.entries)
			})
			if len(h.luaTools.overridesFor("bash")) != 0 || len(h.luaTools.list()) != 0 {
				t.Fatal("learned callback registered a tool or override")
			}
		})
	}
}
