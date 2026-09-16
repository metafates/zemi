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
	tool := h.luaFantasyTools()[0]
	h.gen.Add(1)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: "{}"})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "reload") {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}
