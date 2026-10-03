local fmt = require("zemi.tui.text")
local snail = require("zemi.tui.snail")

---@type table<string, boolean>
local READS = { read = true, grep = true, glob = true, fetch = true, web_search = true, skill = true }

-- The composer frame's top rule, all ctx.width cells of it: the mascot a cell
-- off the left corner in the state of the turn and, at the right end of the
-- rule, the interrupt key and the elapsed time. The block keeps a cell of rule
-- on either side and ends a cell off the rule's end, so nothing to
-- its right ever moves and the cells a part grows into come out of the rule on
-- its left. The block stays away for the first 300ms so instant turns do not
-- flash it, and the eyes say how the turn ended for a few seconds after. A
-- busy job shows the same way while idle; an agent view names the run it shows
-- and leaves on esc where the interrupt key stands.

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
		---@type { [1]: zemi.Line, [2]: integer, [3]: integer? }[]
		local parts = {}
		---@param text string
		---@param hl string?
		---@return zemi.Line
		local function plain(text, hl) return { { text = text, hl = hl or "Dim" } } end
		local state = "idle"
		local busy = ctx.busy and ctx.busy.elapsed >= 0.3 and ctx.busy or nil
		-- a printed run takes no key but the shell's interrupt
		local key, verb = "esc", "interrupt"
		if ctx.headless then
			key, verb = "ctrl+c", "stop"
		elseif ctx.interrupt_armed then
			verb = "again to interrupt"
		end
		-- the run the chat shows, whose it is, and the key that leaves it, which
		-- is what esc means there: a rule with nothing else to say draws them
		-- alone, and the key is worth more than the name so a narrow rule keeps
		-- the way out
		if ctx.view then
			parts[#parts + 1] = { plain("viewing " .. ctx.view.agent, "Title"), 5 }
			parts[#parts + 1] = { fmt.hint("esc", "back"), 6, 13 }
		end
		if not ctx.agent_active then
			if busy then
				state = "working"
				-- what the job is, worth least of the three parts: a narrow rule
				-- keeps the key that stops it and the clock that shows it alive
				parts[#parts + 1] = { plain(fmt.lower(busy.label)), 4 }
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
			if ag.compacting or ag.checking_goal then state = "reading" end
			---@type { [1]: zemi.Line, [2]: integer, [3]: integer }?
			local tick = { plain(fmt.duration(ag.elapsed, true)), 5, 7 }
			if ag.retry then
				state = "error"
				tick = {
					plain(
						ag.retry.next_in > 0 and "next try in " .. fmt.duration(math.ceil(ag.retry.next_in), true)
							or "retrying now"
					),
					5,
					18,
				}
			end
			if ctx.editing ~= "" then
				state, tick = "idle", nil
				parts = { { plain("save and close the editor to continue"), 5 } }
			end
			-- the open prompt says what answers it, so the rule shrinks to the
			-- mascot alone rather than repeating it; in a view the back key is
			-- already on the rule
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
		---@return integer
		local function room()
			local n = 0
			for _, p in ipairs(parts) do
				n = n + 3 + math.max(fmt.cells(p[1]), p[3] or 0)
			end
			return n
		end
		local span = ctx.width
		-- the snail, its cells of rule and the block's own keep eight cells
		-- before a part goes
		while #parts > 0 and room() + 8 > span do
			local least = 1
			for i, p in ipairs(parts) do
				if p[2] < parts[least][2] then least = i end
			end
			table.remove(parts, least)
		end
		-- the parts join as a footer joins its keys, each in the highlight it
		-- draws in, a space between the block and the rule
		---@type zemi.Line
		local after = {}
		for i, p in ipairs(parts) do
			if i > 1 then after[#after + 1] = { text = " · ", hl = "Dim" } end
			for _, sg in ipairs(p[1]) do
				after[#after + 1] = sg
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
		local row = { { text = " ", hl = "Rule", copy = false } }
		for _, sg in ipairs(snail.rule(state, ctx.frame - since, span - block - 1)) do
			row[#row + 1] = sg
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
