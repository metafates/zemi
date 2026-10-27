local fmt = require("zemi.tui.text")
local snail = require("zemi.tui.snail")

---@type table<string, boolean>
local READS = { read = true, grep = true, glob = true, skill = true }

-- The composer frame's top rule, all ctx.width cells of it: the mascot a cell
-- off the left corner in the state of the turn and, at the right end of the
-- rule, the interrupt key and the elapsed time. The block keeps a cell of rule
-- on either side and ends a cell off the rule's end, so nothing to
-- its right ever moves and the cells a part grows into come out of the rule on
-- its left. The block stays away for the first 300ms so instant turns do not
-- flash it, and the eyes say how the turn ended for a few seconds after. A
-- busy job shows the same way while idle; an agent view names the run it shows
-- in the middle of the rule and leaves on esc where the interrupt key stands.

-- the state the eyes last showed and the frame it began on: an eye cycle
-- starts with its state, whatever the clock read
---@type string?, integer
local shown, since = nil, 0

zemi.ui.widget.add({
	id = "working",
	region = "working",
	priority = 10,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		local ag = ctx.agent or {}
		-- each part with its worth and the cells it grows to: a narrow rule
		-- sheds the least worth first, sized by what a part grows to, so a
		-- ticking clock never sheds one mid-turn
		---@type { [1]: zemi.Line, [2]: integer, [3]: integer?, center: boolean? } []
		local parts = {}
		---@param text string
		---@param hl   string?
		---@param effect string?
		---@return zemi.Line
		local function plain(text, hl, effect) return { { text = text, hl = hl or "Dim", effect = effect } } end
		local state = "idle"
		---@type { label: string, elapsed: number, cancellable: boolean }?
		local busy = (ctx.busy or {})[1]
		-- a job that just started is not worth the block that comes and goes
		if busy and busy.elapsed < 0.3 then busy = nil end
		local key, verb = "esc", "interrupt"
		if ctx.interrupt_armed then verb = "again to interrupt" end
		-- the run the chat shows and the key that leaves it, which is what esc
		-- means there: the name takes the row's centre, the key the rule's right
		-- end, and the key is worth more than the name so a narrow rule keeps it
		if ctx.view then
			parts[#parts + 1] = { plain(ctx.view.agent, "Title"), 5, center = true }
			parts[#parts + 1] = { fmt.hint("esc", "back"), 6, 13 }
		end
		if not ctx.agent_active then
			if busy then
				state = "working"
				-- what the job is, worth least of the three parts: a narrow rule
				-- keeps the key that stops it and the clock that shows it alive
				parts[#parts + 1] = { plain(fmt.lower(busy.label), nil, "shimmer"), 4 }
				if busy.cancellable and not ctx.view then parts[#parts + 1] = { fmt.hint(key, verb), 6, 13 } end
				parts[#parts + 1] = { plain(fmt.duration(busy.elapsed, true)), 5, 7 }
			elseif ctx.ended == "ok" then
				state = "done"
			elseif ctx.ended == "error" then
				state = "error"
			end
		elseif (ag.elapsed or 0) >= 0.3 or ctx.cancelling then
			state = "working"
			-- the eyes read the running call: serial calls run in call order, and
			-- a parallel batch starts together
			local running = false
			for _, t in ipairs(ag.tools or {}) do
				if t.status == "run" then
					running = true
					if READS[t.name] then state = "reading" end
					break
				end
			end
			if not running and ag.reasoning ~= "" and ag.text == "" then state = "thinking" end
			if ag.checking_goal then state = "reading" end
			---@type { [1]: zemi.Line, [2]: integer, [3]: integer }?
			local tick = { plain(fmt.duration(ag.elapsed, true)), 5, 7 }
			if ag.retry then
				state = "error"
				local why = ag.retry.why ~= "" and " - " .. fmt.clip(ag.retry.why, 60) or ""
				tick = {
					plain(
						(
							ag.retry.next_in > 0
								and "next try in " .. fmt.duration(math.ceil(ag.retry.next_in), true)
							or "retrying now"
						) .. why
					),
					5,
					18,
				}
			end
			if ctx.editing ~= "" then
				state, tick = "idle", nil
				parts = { { plain("save and close the editor to continue"), 5 } }
			end
			-- the open prompt names what answers it and esc answers it, so the
			-- rule shrinks to the mascot alone rather than repeating a key
			if ctx.waiting ~= "" then
				state, parts, tick = "asking", {}, nil
			elseif ctx.cancelling then
				parts[#parts + 1] = { fmt.hint("ctrl+c", "quit"), 6, 13 }
			elseif not ctx.view then
				parts[#parts + 1] = { fmt.hint(key, verb), 6, 13 }
			end
			-- last: the block ends on the clock, its digits eating the rule on
			-- the left
			if tick then parts[#parts + 1] = tick end
		end
		local span = ctx.width
		-- the row holds the snail, the block and the part it centres, a cell of
		-- rule on either side of it
		---@return boolean
		local function fit()
			local room, w, block = 0, 0, 0
			for _, p in ipairs(parts) do
				local cells = math.max(fmt.cells(p[1]), p[3] or 0)
				room = room + 3 + cells
				if p.center then
					w = fmt.cells(p[1]) + 2
				else
					block = block + (block > 0 and 3 or 0) + cells
				end
			end
			-- the snail, its cells of rule and the block's own keep eight cells
			if room + 8 > span then return false end
			if w == 0 then return true end
			-- the snail, a cell of rule, the centred part, a cell, the block
			local start = math.floor((span - w) / 2) + 1
			return start >= 6 and start + w <= span - block - 2
		end
		while #parts > 0 and not fit() do
			local least = 1
			for i, p in ipairs(parts) do
				if p[2] < parts[least][2] then least = i end
			end
			table.remove(parts, least)
		end
		-- the parts join as a footer joins its keys, each in the highlight it
		-- draws in, a space between the block and the rule; the part the row
		-- centres stands in the middle of the rule instead
		---@type zemi.Line
		local after = {}
		---@type zemi.Line?
		local mid
		for _, p in ipairs(parts) do
			if p.center then
				mid = p[1]
			else
				if #after > 0 then after[#after + 1] = { text = " · ", hl = "Dim" } end
				for _, sg in ipairs(p[1]) do
					after[#after + 1] = sg
				end
			end
		end
		-- the block keeps a cell of rule on either side, as the statusline's
		-- clusters do, and ends at the rule's last cell but one: the key and the
		-- clock keep their right end whatever the clock reads and whatever a view
		-- adds to the left
		local block = #after > 0 and fmt.cells(after) + 2 or 0
		if state ~= shown then
			shown, since = state, ctx.frame
		end
		-- the centred part at the row's middle, in the rule fit left for it
		local w = mid and fmt.cells(mid) + 2 or 0
		local start = w > 0 and math.floor((span - w) / 2) + 1 or 0
		local row = { { text = " ", hl = "Rule", copy = false } }
		-- the rule from the snail to the centred part
		local left = mid and start - 2 or span - block - 1
		for _, sg in ipairs(snail.rule(state, ctx.frame - since, left)) do
			row[#row + 1] = sg
		end
		if mid then
			row[#row + 1] = { text = " ", hl = "Rule", copy = false }
			for _, sg in ipairs(mid) do
				row[#row + 1] = sg
			end
			row[#row + 1] = { text = " ", hl = "Rule", copy = false }
			row[#row + 1] = { text = string.rep("─", span - block - (start + w - 1)), hl = "Rule", copy = false }
		end
		if block > 0 then
			row[#row + 1] = { text = " ", hl = "Dim", copy = false }
			for _, sg in ipairs(after) do
				row[#row + 1] = sg
			end
			row[#row + 1] = { text = " ", hl = "Dim", copy = false }
		end
		return { row }
	end,
})
