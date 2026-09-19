-- defaultui: the shipped theme, widgets, keymaps and layout (DESIGN §6.6,
-- §8), one module per part. Runs as the builtin origin via
-- require("zemi.defaultui"); every registration here is replaceable by user
-- config (origin precedence: user > builtin > plugin > project).

require("zemi.defaultui.theme")
require("zemi.defaultui.statusline")
require("zemi.defaultui.working")
require("zemi.defaultui.chat")
local splash = require("zemi.defaultui.splash")
require("zemi.defaultui.layout")
require("zemi.defaultui.keymaps")

-- The schemas a coding turn rarely calls, about 40% of the builtin ones.
-- job_read and job_kill stay: bash hands a command past its timeout to them.
zemi.agent.profile("lean", {
  tools = { "-window", "-workflow", "-Lua", "-job_start", "-open_in_editor", "-lsp" },
})

---@class zemi.defaultui
---@field splash fun(ctx: zemi.WidgetCtx): zemi.Line[] The default splash rows, so a custom splash widget can add to them instead of copying them.
return { splash = splash.render }
