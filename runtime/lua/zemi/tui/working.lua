local fmt = require("zemi.tui.text")
local snail = require("zemi.tui.snail")

---@type table<string, boolean>
local READS = { read = true, grep = true, glob = true, fetch = true, web_search = true, skill = true }

-- The composer frame's top rule, all ctx.width cells of it: the mascot in the
-- left corner in the state of the turn and, at the right end of the rule, the
-- interrupt key and the elapsed time. A part that ticks keeps the cells it can
-- grow to, so the block is anchored once and the key never slides. The block
-- stays away for the first 300ms so instant turns do not flash it, and the
-- eyes say how the turn ended for a few seconds after. A busy job shows the
-- same way while idle.

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
		-- each part with its worth: a narrow rule sheds the least worth first;
		-- a ticking part also has the cells it grows to, drawn as rule after it
		---@type { [1]: string, [2]: integer, [3]: integer? }[]
		local parts = {}
		local state = "idle"
		local busy = ctx.busy and ctx.busy.elapsed >= 0.3 and ctx.busy or nil
		-- a printed run takes no key but the shell's interrupt
		local stop = ctx.headless and "ctrl+c stop" or ctx.interrupt_armed and "esc again" or "esc interrupt"
		if not ctx.agent_active then
			if busy then
				state = "working"
				if busy.cancellable and not ctx.view then parts[1] = { stop, 6, 13 } end
				parts[#parts + 1] = { fmt.duration(busy.elapsed, true), 5, 7 }
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
			---@type { [1]: string, [2]: integer, [3]: integer }?
			local tick = { fmt.duration(ag.elapsed, true), 5, 7 }
			if ag.retry then
				state = "error"
				tick = {
					ag.retry.next_in > 0 and "next try in " .. fmt.duration(math.ceil(ag.retry.next_in), true)
						or "retrying now",
					5,
					18,
				}
			end
			if ctx.editing ~= "" then
				state, tick = "idle", nil
				parts = { { "save and close the editor to continue", 5 } }
			end
			-- the open prompt says what answers it, so the rule shrinks to the
			-- mascot alone rather than repeating it; in an agent view esc goes back
			if ctx.waiting ~= "" then
				state, parts, tick = "asking", {}, nil
			elseif ctx.cancelling then
				parts[#parts + 1] = { "ctrl+c quit", 6, 13 }
			elseif not ctx.view then
				parts[#parts + 1] = { stop, 6, 13 }
			end
			-- last, where its changing width moves nothing
			if tick then parts[#parts + 1] = tick end
		end
		---@return integer
		local function room()
			local n = 0
			for _, p in ipairs(parts) do
				n = n + 3 + math.max(zemi.ui.width(p[1]), p[3] or 0)
			end
			return n
		end
		local span = ctx.width
		-- the snail keeps six cells of rule before a part goes
		while #parts > 0 and room() + 6 > span do
			local least = 1
			for i, p in ipairs(parts) do
				if p[2] < parts[least][2] then least = i end
			end
			table.remove(parts, least)
		end
		---@type string[]
		local texts = {}
		for i, p in ipairs(parts) do
			texts[i] = p[1]
		end
		local after = #texts > 0 and " " .. table.concat(texts, " · ") .. " " or ""
		local block = zemi.ui.width(after)
		if state ~= shown then
			shown, since = state, ctx.frame
		end
		-- the block ends the rule: its own reserved cells grow into the margin,
		-- so the key keeps its column whatever the clock reads
		local left = block > 0 and ctx.width - room() - 2 or ctx.width
		local row = snail.rule(state, ctx.frame - since, left)
		if block > 0 then
			row[#row + 1] = { text = after, hl = "Dim" }
			row[#row + 1] = {
				text = string.rep("─", ctx.width - left - block),
				hl = "Rule",
				copy = false,
			}
		end
		return { row }
	end,
})
