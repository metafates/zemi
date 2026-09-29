-- zemi's mascot: a snail whose shell is the "@". Text only, and only the
-- shell takes a color, the Snail group's. It never leaves the left corner: the state shows in
-- its eyes, stepped by the 50ms frame clock (still with ui.animations off).
---@class zemi.tui.snail
local M = {}

-- eyes per state, and how many ticks each pair stays
---@type table<string, { [integer]: string, ticks: integer }>
local HEADS = {
	idle = { "··", ticks = 1 },
	thinking = { ".?", "?.", "..", "?.", ".?", "..", ticks = 8 },
	working = { "··", ".·", "..", "·.", ticks = 6 },
	reading = { "··", "..", "..", "..", ticks = 8 },
	asking = { "??", ticks = 1 },
	error = { "><", ticks = 1 },
	done = { "^^", ticks = 1 },
}

---@alias zemi.tui.SnailState "idle" | "thinking" | "working" | "reading" | "asking" | "error" | "done"

--- The eyes of `state` at `frame`.
---@param state zemi.tui.SnailState
---@param frame integer                   frames since the state began
---@return string
function M.eyes(state, frame)
	local heads = HEADS[state] or HEADS.idle
	return heads[math.floor(frame / heads.ticks) % #heads + 1]
end

--- The snail at the left of a plain rule of `room` cells.
---@param state zemi.tui.SnailState
---@param frame integer                   frames since the state began
---@param room  integer
---@return zemi.Line
function M.rule(state, frame, room)
	local head = M.eyes(state, frame)
	return {
		{ text = "@", hl = "Snail", copy = false },
		{ text = head, hl = "Normal", copy = false },
		{ text = string.rep("─", math.max(room - 1 - zemi.ui.width(head), 0)), hl = "Rule", copy = false },
	}
end

--- The column `block` sets its `beside` segments at, right of the head.
M.MARK_COL = 13

--- The four-line snail at rest, for the splash and the exit banner.
---@param beside? zemi.Segment[] Up to two segments set beside the head, on its last two rows.
---@return zemi.Line[]
function M.block(beside)
	local shell = { "╭────╮", "│╭──╮│", "│╰──╯│", "╰────╯" }
	local head = { "", "  ╷╷", "╭─┴┴╮", "╯   ╵" }
	---@type zemi.Line[]
	local rows = {}
	for i = 1, #shell do
		rows[i] = { { text = shell[i], hl = "Snail" }, { text = head[i], hl = "Normal" } }
		local seg = (beside or {})[i - 2]
		if seg then
			rows[i][3] = { text = string.rep(" ", M.MARK_COL - zemi.ui.width(shell[i] .. head[i])), copy = false }
			rows[i][4] = seg
		end
	end
	return rows
end

return M
