package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
)

// TestSplashExtends: user config can wrap the default splash through
// require("zemi.defaultui").splash (the zemi-customize skill's example).
func TestSplashExtends(t *testing.T) {
	app, host := bootAgentApp(t, 80, 30)
	src := `
local default = require("zemi.defaultui")
zemi.ui.widget.register({
  id = "splash", region = "chat", priority = 5,
  render = function(ctx)
    local rows = default.splash(ctx)
    for i, row in ipairs(rows) do
      if row[2] and row[2].hl == "Logo6" then
        table.insert(rows, i + 1, { row[1], { text = "hack the planet", hl = "Title" } })
        break
      end
    end
    return rows
  end,
})`
	if err := host.RT.ExecuteChunk(context.Background(), src, "init.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "hack the planet") || !strings.Contains(frame, "╚══════╝") {
		t.Fatalf("splash not extended:\n%s", frame)
	}
}
