-- The default keymap: user config can rebind or zemi.keymap.del these.
-- Printable keys stay with the composer. The transcript is bottom-anchored,
-- so the scroll keys and the wheel move the viewport; sending snaps back to
-- the tail. A scroll action takes a node id after a space
-- ("zemi.ui.scroll.up sidebar"); without one it moves the stage's scrolled
-- region.

zemi.keymap.set("ctrl+o", "zemi.ui.reasoning.toggle", { desc = "show thinking and every tool call" })
zemi.keymap.set("tab", "zemi.permission.mode.cycle", { desc = "next permission mode" })
zemi.keymap.set("shift+tab", "zemi.permission.mode.prev", { desc = "previous permission mode" })
zemi.keymap.set("pgup", "zemi.ui.scroll.up", { desc = "scroll chat up a page" })
zemi.keymap.set("pgdown", "zemi.ui.scroll.down", { desc = "scroll chat down a page" })
zemi.keymap.set("ctrl+home", "zemi.ui.scroll.top", { desc = "scroll chat to top" })
zemi.keymap.set("ctrl+end", "zemi.ui.scroll.bottom", { desc = "scroll chat to bottom" })
