package hostapi

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

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
