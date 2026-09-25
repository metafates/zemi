-- The default keymap: user config can rebind or zemi.keymap.del these.
-- Printable keys stay with the composer. The transcript is bottom-anchored,
-- so the scroll keys and the wheel move the viewport; sending snaps back to
-- the tail. A scroll action takes a node id after a space
-- ("zemi.ui.scroll.up sidebar"); without one it moves the stage's scrolled
-- region.

zemi.keymap.set("ctrl+o", "zemi.ui.reasoning.toggle", { desc = "show thinking and every tool call" })

-- ctrl+y flips to yolo and back. The mode it left is remembered here, so the
-- next press restores it whatever the mode list holds.
local before
zemi.keymap.set("ctrl+y", function()
  ---@type zemi.PermMode?
  local cur
  for _, m in ipairs(zemi.permission.mode.list()) do
    if m.active then cur = m end
  end
  if not cur then return end
  if cur.base == "yolo" then
    zemi.permission.mode.set(before or "auto")
    before = nil
  elseif zemi.permission.mode.set("yolo") then
    before = cur.name
  end
end, { desc = "yolo until the next press" })
zemi.keymap.set("ctrl+t", "zemi.agent.profile.next", { desc = "next profile" })
zemi.keymap.set(
  "ctrl+shift+t",
  "zemi.agent.profile.prev",
  { desc = "previous profile, where the terminal tells it from ctrl+t" }
)
zemi.keymap.set("pgup", "zemi.ui.scroll.up", { desc = "scroll chat up a page" })
zemi.keymap.set("pgdown", "zemi.ui.scroll.down", { desc = "scroll chat down a page" })
zemi.keymap.set("ctrl+home", "zemi.ui.scroll.top", { desc = "scroll chat to top" })
zemi.keymap.set("ctrl+end", "zemi.ui.scroll.bottom", { desc = "scroll chat to bottom" })
zemi.keymap.set("shift+up", "zemi.ui.scroll.line_up", { desc = "scroll chat up a line" })
zemi.keymap.set("shift+down", "zemi.ui.scroll.line_down", { desc = "scroll chat down a line" })
