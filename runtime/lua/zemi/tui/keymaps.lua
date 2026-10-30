-- The default keymap: user config can rebind or zemi.keymap.del these.
-- Printable keys stay with the composer. The transcript scrolls, so the
-- scroll keys and the wheel move the viewport; sending snaps back to the
-- tail. A scroll action takes a node id after a space
-- ("zemi.ui.scroll.up sidebar"); without one it moves the stage's scrolled
-- region.

zemi.keymap.set("pgdown", "zemi.ui.scroll.down", { desc = "scroll chat down a page" })
zemi.keymap.set("pgup", "zemi.ui.scroll.up", { desc = "scroll chat up a page" })
zemi.keymap.set("ctrl+home", "zemi.ui.scroll.top", { desc = "scroll chat to top" })
zemi.keymap.set("ctrl+b", "zemi.ui.scroll.bottom", { desc = "scroll chat to bottom" })
zemi.keymap.set("shift+up", "zemi.ui.scroll.line_up", { desc = "scroll chat up a line" })
zemi.keymap.set("shift+down", "zemi.ui.scroll.line_down", { desc = "scroll chat down a line" })
zemi.keymap.set("ctrl+k", "zemi.ui.scroll.line_up", { desc = "scroll chat up a line" })
zemi.keymap.set("ctrl+j", "zemi.ui.scroll.line_down", { desc = "scroll chat down a line" })
zemi.keymap.set("alt+up", "zemi.ui.scroll.sticky_up", { desc = "scroll chat to the previous prompt or opened run" })
zemi.keymap.set("alt+down", "zemi.ui.scroll.sticky_down", { desc = "scroll chat to the next prompt or opened run" })
zemi.keymap.set("ctrl+m", function() assert(zemi.command.run("messages")) end, { desc = "read or copy a message" })
zemi.keymap.set("alt+o", function() assert(zemi.command.run("memories")) end, { desc = "browse the chat's memory" })
