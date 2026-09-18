-- zemi's mascot: a snail whose shell is the "@". Text only, and only the
-- shell takes the accent. It never leaves the left corner: the state shows in
-- its eyes and in the rule beside it. ctx.frame advances every 500ms (0 with
-- ui.animations off, which freezes both).
---@class zemi.defaultui.snail
local M = {}

-- Per state: the head's frames, a mark and its group, and which way ripples
-- run along the rule (1 away from the snail, -1 back toward it).
---@type table<string, { head: string[], mark: string?, mark_hl: string?, flow: integer? }>
local STATES = {
  idle = { head = { "··" } },
  thinking = { head = { "°ᵒ", "ᵒ°" } },
  working = { head = { "··" }, flow = 1 },
  rereading = { head = { "··" }, flow = -1 },
  retracted = { head = { "" } },
  error = { head = { "" }, mark = "✕", mark_hl = "AgentToolErr" },
  done = { head = { "··" }, mark = "✓", mark_hl = "AgentToolOk" },
}

local WAVE, GAP, SPEED = 4, 24, 3 -- ~ cells per ripple, cells between them, cells per frame

--- The snail at the left of a rule of `room` cells. Idle the rule is plain
--- "─"; while a flowing state runs, ripples of "~" travel along it.
---@param state "idle"|"thinking"|"working"|"rereading"|"retracted"|"error"|"done"
---@param frame integer ctx.frame
---@param room integer
---@return zemi.Line
function M.rule(state, frame, room)
  local st = STATES[state] or STATES.idle
  local head = st.head[frame % #st.head + 1]
  ---@type zemi.Line
  local segs = {
    { text = "@", hl = "Accent", copy = false },
    { text = head, hl = "Normal", copy = false },
  }
  if st.mark then segs[3] = { text = " " .. st.mark, hl = st.mark_hl, copy = false } end
  ---@param i integer
  ---@return boolean
  local function wet(i) return st.flow ~= nil and (i - st.flow * frame * SPEED) % GAP < WAVE end
  local far = math.max(room - 1 - zemi.ui.width(head) - (st.mark and 2 or 0), 0)
  local i = 0
  while i < far do
    local on, j = wet(i), i + 1
    while j < far and wet(j) == on do j = j + 1 end
    segs[#segs + 1] = { text = string.rep(on and "~" or "─", j - i), hl = on and "Dim" or "Rule", copy = false }
    i = j
  end
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
