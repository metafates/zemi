-- zemi's mascot: a snail whose shell is the "@". Text only, and only the
-- shell takes the accent. ctx.frame advances every 500ms (0 with
-- ui.animations off, which leaves every state on its first frame).
---@class zemi.defaultui.snail
local M = {}

--- Cells M.row always takes, so text beside a moving snail never shifts.
M.WIDTH = 6

-- Per state, the frames it cycles: head, trail length, mark and its group.
-- The head wiggles while thinking, the trail grows and the shell advances a
-- cell while working, and the head goes in while zemi waits for the user.
---@type table<string, { [1]: string, [2]: integer?, [3]: string?, [4]: string? }[]>
local FRAMES = {
  idle = { { "··" } },
  thinking = { { "°ᵒ" }, { "ᵒ°" }, { "··" } },
  working = { { "··", 0 }, { "··", 1 }, { "··", 2 }, { "··", 3 } },
  retracted = { { "" } },
  error = { { "", 0, "✕", "AgentToolErr" } },
  done = { { "··", 0, "✓", "AgentToolOk" } },
}

--- The one-line snail as a padded row of segments.
---@param state "idle"|"thinking"|"working"|"retracted"|"error"|"done"
---@param frame integer ctx.frame
---@return zemi.Line
function M.row(state, frame)
  local frames = FRAMES[state] or FRAMES.idle
  local f = frames[frame % #frames + 1]
  local trail = string.rep("~", f[2] or 0)
  ---@type zemi.Line
  local segs = {
    { text = trail, hl = "Comment", copy = false },
    { text = "@", hl = "Title", copy = false },
    { text = f[1], hl = "Normal", copy = false },
  }
  if f[3] then segs[#segs + 1] = { text = " " .. f[3], hl = f[4], copy = false } end
  local w = #trail + 1 + zemi.ui.width(f[1]) + (f[3] and 2 or 0)
  segs[#segs + 1] = { text = string.rep(" ", math.max(0, M.WIDTH - w)), copy = false }
  return segs
end

--- The four-line snail at rest, for the splash.
---@return zemi.Line[]
function M.block()
  local shell = { "╭────╮", "│╭──╮│", "│╰──╯│", "╰────╯" }
  local head = { "", "  ╷╷", "╭─┴┴╮", "╯   ╵" }
  ---@type zemi.Line[]
  local rows = {}
  for i = 1, #shell do
    rows[i] = { { text = shell[i], hl = "Title" }, { text = head[i], hl = "Normal" } }
  end
  return rows
end

return M
