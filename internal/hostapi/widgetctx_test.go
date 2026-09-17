package hostapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// BenchmarkWidgetCtx renders a statusline-like widget that reads one field
// over an empty and a 500-row transcript: the gap is what building the
// transcript for widgets that never read it costs.
func BenchmarkWidgetCtx(b *testing.B) {
	for _, rows := range []int{0, 500} {
		b.Run(fmt.Sprint(rows), func(b *testing.B) {
			log := logging.New(b.TempDir())
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
			h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: b.TempDir()})
			if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
				b.Fatal(err)
			}
			if err := r.ExecuteChunk(context.Background(), `zemi.ui.widget.register({ id = "m", region = "statusline.right", render = function(ctx) return { { { text = ctx.model } } } end })`, "b.lua", lua.OriginUser); err != nil {
				b.Fatal(err)
			}
			diff := strings.Repeat("- old line of code here\n+ new line of code here\n", 20)
			for i := range rows {
				h.appendChatMessage(ChatMessage{Role: "assistant", Kind: "tool", Tool: "edit", Text: "internal/x.go", Label: "edit x", Status: "ok", Diff: diff, Output: "done\nok"})
				if i%2 == 0 {
					h.appendChatMessage(ChatMessage{Role: "user", Text: "please change the thing in the other place " + fmt.Sprint(i)})
				}
			}
			w := h.Widgets.Get("m")
			for b.Loop() {
				if _, err := h.RenderWidget(context.Background(), w, 40, 1, UIState{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestWidgetCtxLazy: the built-on-read fields work inside render, and a
// ctx kept past it keeps what render read and reads nil for the rest.
func TestWidgetCtxLazy(t *testing.T) {
	h, r := newTestHost(t)
	h.AppendMessage("user", "hi")
	runUser(t, r, `
		local kept
		zemi.ui.widget.register({ id = "k", region = "header", render = function(ctx)
			assert(#ctx.transcript == 1 and ctx.transcript_count == 1 and ctx.transcript[1].text == "hi", "transcript")
			assert(type(ctx.agent.tools) == "table" and ctx.recent == nil, "lazy fields")
			kept = ctx
			return {}
		end })
		zemi.command.add("check", function()
			assert(kept.transcript ~= nil and kept.agent ~= nil and kept.width == 10, "what render read stays")
			assert(kept.todos == nil, "the rest reads nil")
		end)`)
	if _, err := h.RenderWidget(t.Context(), h.Widgets.Get("k"), 10, 1, UIState{}); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `assert(zemi.command.run("check"))`)
}
