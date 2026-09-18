-- zemi's mascot: a snail whose shell is the "@". Text only, and only the
-- shell takes the accent. It never leaves the left corner: the state shows in
-- its eyes. ctx.frame advances every 500ms (0 with ui.animations off).
---@class zemi.defaultui.snail
local M = {}

---@type table<string, string[]>
local HEADS = {
  idle = { "··" },
  thinking = { "°ᵒ", "ᵒ°" },
  working = { "•·", "·•" },
  rereading = { "·•", "•·" },
  asking = { "??" },
  error = { "><" },
  done = { "^^" },
}
--- The snail at the left of a plain rule of `room` cells.
---@param state "idle"|"thinking"|"working"|"rereading"|"asking"|"error"|"done"
---@param frame integer ctx.frame
---@param room integer
---@return zemi.Line
function M.rule(state, frame, room)
  local heads = HEADS[state] or HEADS.idle
  local head = heads[frame % #heads + 1]
  return {
    { text = "@", hl = "Accent", copy = false },
    { text = head, hl = "Normal", copy = false },
    { text = string.rep("─", math.max(room - 1 - zemi.ui.width(head), 0)), hl = "Rule", copy = false },
  }
end

--- The four-line snail at rest, for the splash.
---@return zemi.Line[]
function M.block()
  local shell = { "╭────╮", "│╭──╮│", "│╰──╯│", "╰────╯" }
  local head = { "", "  ╷╷", "╭─┴┴╮", "╯   ╵" }
  ---@type zemi.Line[]
  local rows = {}
  for i = 1, #shell do
    rows[i] = { { text = shell[i], hl = "Accent" }, { text = head[i], hl = "Normal" } }
  end
  return rows
end

return M
