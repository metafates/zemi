-- zemi.tui: the shipped theme, widgets, keymaps and layout (DESIGN §6.6,
-- §8), one module per part. Runs as the builtin origin via
-- require("zemi.tui"); every registration here is replaceable by user
-- config (origin precedence: user > builtin > plugin > project).

require("zemi.tui.statusline")
require("zemi.tui.working")
require("zemi.tui.chat")
local splash = require("zemi.tui.splash")
require("zemi.tui.layout")
require("zemi.tui.keymaps")

---@class zemi.tui
---@field splash fun(ctx: zemi.WidgetCtx): zemi.Line[] The default splash rows, so a custom splash widget can add to them instead of copying them.
return { splash = splash.render }
