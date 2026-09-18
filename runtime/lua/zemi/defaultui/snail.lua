-- zemi's mascot: a snail whose shell is the "@". Text only, and only the
-- shell takes the accent. ctx.frame advances every 500ms (0 with
-- ui.animations off, which leaves the snail where it is).
---@class zemi.defaultui.snail
local M = {}

-- Per state: the head's frames, a mark and its group, and how the shell moves
-- along the rule.
---@type table<string, { head: string[], mark: string?, mark_hl: string?, move: ("crawl"|"roll")? }>
local STATES = {
  idle = { head = { "··" } },
  thinking = { head = { "°ᵒ", "ᵒ°" } },
  working = { head = { "··" }, move = "crawl" },
  rereading = { head = { "" }, move = "roll" },
  retracted = { head = { "" } },
  error = { head = { "" }, mark = "✕", mark_hl = "AgentToolErr" },
  done = { head = { "··" }, mark = "✓", mark_hl = "AgentToolOk" },
}

-- where the shell is on the rule, which way it faces and the last frame it
-- moved on: it keeps its place between states and turns
local pos, dir = 0, 1
---@type string?
local moving
---@type integer?
local last

--- The snail on a rule of `room` cells: "~" where it has been, "─" ahead.
--- Crawling, it turns around at either end and the trail dries as it heads
--- back; rereading, the shell alone rolls over its trail twice as fast.
---@param state "idle"|"thinking"|"working"|"rereading"|"retracted"|"error"|"done"
---@param frame integer ctx.frame
---@param room integer
---@return zemi.Line
function M.rule(state, frame, room)
  local st = STATES[state] or STATES.idle
  local head = st.head[frame % #st.head + 1]
  local body = 1 + zemi.ui.width(head) + (st.mark and 2 or 0)
  local far = math.max(room - body, 0)
  local steps = st.move and last and math.min(frame - last, 4) or 0
  if st.move == "roll" then steps = steps * 2 end
  -- a roll sets off back over what the crawl did, the next crawl forward
  if st.move and st.move ~= moving then
    moving = st.move
    dir = moving == "roll" and -1 or 1
  end
  last = st.move and frame or nil
  for _ = 1, steps do
    if pos + dir < 0 or pos + dir > far then dir = -dir end
    pos = pos + dir
  end
  pos = math.min(pos, far)
  local shell = { text = "@", hl = "Accent", copy = false }
  local face = { text = head, hl = "Normal", copy = false }
  ---@type zemi.Line
  local segs = { { text = string.rep("~", pos), hl = "Dim", copy = false } }
  -- the head leads, so heading back it is left of the shell
  segs[2] = dir < 0 and face or shell
  segs[3] = dir < 0 and shell or face
  if st.mark then segs[4] = { text = " " .. st.mark, hl = st.mark_hl, copy = false } end
  segs[#segs + 1] = { text = string.rep("─", math.max(room - pos - body, 0)), hl = "Rule", copy = false }
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
    rows[i] = { { text = shell[i], hl = "Accent" }, { text = head[i], hl = "Normal" } }
  end
  return rows
end

return M
