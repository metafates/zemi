-- zemi.tui: the shipped theme, widgets, keymaps and layout (DESIGN §6.6,
-- §8), one module per part. Runs as the builtin origin via
-- require("zemi.tui"); every registration here is replaceable by user
-- config (origin precedence: user > builtin > plugin > project).

require("zemi.tui.working")
require("zemi.tui.chat")

---@class zemi.tui
return {}
