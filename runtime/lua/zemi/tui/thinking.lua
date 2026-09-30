-- The thinking pane right of the chat (ui.reasoning = "pane", DESIGN §8): a
-- header row, then the turn's thinking as it streams, following the tail
-- until the wheel moves it. A click on a digest in the chat shows that run
-- here instead, every row open; tui/chat.lua keeps which run that is.
local fmt = require("zemi.tui.text")
local chat = require("zemi.tui.chat")

-- What the transcript gives the pane, read again only when the transcript
-- or the inspected run moves: ctx.transcript copies every row. body is by
-- the width it was drawn at.
---@class zemi.tui.PaneTurn
---@field key string
---@field head string The header between turns, or of the inspected run.
---@field texts string[] The last turn's thinking, one entry per thought.
---@field first integer? The inspected run's first row.
---@field rows zemi.TranscriptRow[]
---@field body { key: string, lines: zemi.Line[] }?
---@type zemi.tui.PaneTurn?
local turn

---@param ctx zemi.WidgetCtx
---@return zemi.tui.PaneTurn
local function read(ctx)
	local epoch = ctx.transcript_epoch or 0
	local key = table.concat({ epoch, ctx.transcript_count or 0, chat.inspected or 0 }, "|")
	-- an agent run's view changes its rows in place and reports epoch 0
	if turn and epoch ~= 0 and turn.key == key then return turn end
	local tr = ctx.transcript or {}
	local first = chat.inspected
	if first and not chat.activity(tr[first] or {}) then first = nil end
	---@type zemi.tui.PaneTurn
	local t = { key = key, head = "", texts = {}, first = first, rows = tr }
	if first then
		local digest = fmt.digest(tr, first, chat.last(tr, first))
		t.head = digest ~= "" and "turn · " .. digest or "turn"
	else
		local secs, thought = 0, false
		for i = #tr, 1, -1 do
			local m = tr[i]
			if (m.kind or "") == "" and m.role == "user" then break end
			if m.kind == "reasoning" then
				table.insert(t.texts, 1, m.text)
				secs, thought = secs + (m.thought or 0), true
			end
		end
		t.head = thought and fmt.thought(secs) .. " · last turn" or ""
	end
	turn = t
	return t
end

-- The pane follows the new turn, whatever it inspected.
zemi.event.on("agent.start", function()
	chat.inspected = nil
	zemi.ui.scroll("bottom", "thinking")
end)

zemi.ui.widget.add({
	id = "thinking.head",
	region = "thinking.head",
	on_click = chat.click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		local t = read(ctx)
		local width = ctx.width or 36
		if t.first then
			-- a click goes back to the live turn
			local click = "turn:" .. t.first
			return { { { text = fmt.clip(t.head, width), hl = "Dim", click = click } } }
		end
		local ag = ctx.agent or {}
		if not ctx.agent_active then return { { { text = fmt.clip(t.head, width), hl = "Dim" } } } end
		local tools = ag.tools or {}
		if (ag.reasoning or "") ~= "" and (ag.text or "") == "" and #tools == 0 then
			local m = zemi.ui.marks.spin
			return {
				{
					{ text = m.glyph .. " ", hl = m.hl, copy = false, effect = "spin" },
					{ text = "thinking · " .. fmt.duration(ag.thought or 0), hl = "Dim" },
				},
			}
		end
		-- between thoughts, the step: the call running, as the working rule says it
		local label = "working"
		for _, c in ipairs(tools) do
			if c.status == "run" then
				local input = c.label == "" and (fmt.relpath(c.input, ctx.cwd, ctx.home):gsub("%s+", " ")) or ""
				local verb, rest = fmt.head(c.name, c.label, input)
				label = fmt.lower(verb) .. rest
				break
			end
		end
		return { { { text = fmt.clip(label, width), hl = "Dim" } } }
	end,
})

zemi.ui.widget.add({
	id = "thinking",
	region = "thinking",
	wrap = true,
	on_click = chat.click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		local t = read(ctx)
		local width = ctx.width or 34
		---@param text string
		---@param into zemi.Line[]
		local function wrap(text, into)
			if #into > 0 then into[#into + 1] = { { text = "" } } end
			for _, segs in ipairs(zemi.ui.wrap((text:gsub("%s+$", "")), width)) do
				for _, sg in ipairs(segs) do
					sg.hl = "AgentReason"
				end
				into[#into + 1] = segs
			end
		end
		local key = table.concat({ width, zemi.ui.theme.get() }, "|")
		if not t.body or t.body.key ~= key then
			---@type zemi.Line[]
			local lines = {}
			if t.first then
				lines = chat.run(ctx, t.rows, t.first, width)
			else
				for _, text in ipairs(t.texts) do
					wrap(text, lines)
				end
			end
			t.body = { key = key, lines = lines }
		end
		local ag = ctx.agent or {}
		if t.first or not ctx.agent_active or (ag.reasoning or "") == "" then return t.body.lines end
		-- the thought streaming now, under the turn's earlier ones; the cached
		-- lines are shared, never appended to
		---@type zemi.Line[]
		local lines = {}
		for i, l in ipairs(t.body.lines) do
			lines[i] = l
		end
		wrap(ag.reasoning, lines)
		return lines
	end,
})
