-- The thinking pane right of the chat (ui.reasoning = "pane", DESIGN §8): a
-- header row over a blank one, then the turn's thoughts, each under its own
-- "✻ thought 4.2s", following the tail while they stream and read from the
-- top once the turn ends. A click on a digest in the chat shows that run here
-- instead, every row open; tui/chat.lua keeps which run that is.
local fmt = require("zemi.tui.text")
local chat = require("zemi.tui.chat")

-- What the transcript gives the pane, read again only when the transcript
-- or the inspected run moves: ctx.transcript copies every row. body is by
-- the width it was drawn at.
---@class zemi.tui.PaneTurn
---@field key string
---@field head string The header between turns, or of the inspected run.
---@field thoughts { text: string, secs: number }[] The last turn's thinking.
---@field first integer? The inspected run's first row.
---@field rows zemi.TranscriptRow[]
---@field body { key: string, lines: zemi.Line[] }?
---@type zemi.tui.PaneTurn?
local turn
-- two cells between the separator and the text, which ends a blank short of
-- the body's scroll bar
local GUTTER = { text = "  ", copy = false }

-- The header row: its state words (before the first " · ", else the first
-- word) in Title and the rest in Dim, then a blank row, which the head region
-- holds so it stays while the body scrolls.
---@param text string
---@param width integer
---@param click string?
---@param lead zemi.Segment? The spinner ahead of the text.
---@return zemi.Line[]
local function header(text, width, click, lead)
	text = fmt.clip(text, width - 4 - (lead and zemi.ui.width(lead.text) or 0))
	local cut = text:find(" · ", 1, true) or text:find(" ", 1, true) or #text + 1
	local row = { GUTTER, lead }
	row[#row + 1] = { text = text:sub(1, cut - 1), hl = "Title", click = click }
	if cut <= #text then row[#row + 1] = { text = text:sub(cut), hl = "Dim", click = click } end
	return { row, { { text = "" } } }
end

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
	local t = { key = key, head = "", thoughts = {}, first = first, rows = tr }
	if first then
		local digest = fmt.digest(tr, first, chat.last(tr, first))
		t.head = digest ~= "" and "turn · " .. digest or "turn"
	else
		-- the last turn: its thinking, and its digest as the idle header
		local start = 1
		for i = #tr, 1, -1 do
			local m = tr[i]
			if (m.kind or "") == "" and m.role == "user" then
				start = i + 1
				break
			end
			if m.kind == "reasoning" then table.insert(t.thoughts, 1, { text = m.text, secs = m.thought or 0 }) end
		end
		local digest = #tr > 0 and fmt.digest(tr, start, #tr) or ""
		t.head = #tr == 0 and "thinking shows here" or (digest ~= "" and "last turn · " .. digest or "last turn")
	end
	turn = t
	return t
end

-- The pane follows the new turn, whatever it inspected, and reads a resumed
-- session's last turn from the top.
zemi.event.on("agent.start", function()
	chat.inspected = nil
	zemi.ui.scroll("bottom", "thinking")
end)
-- a turn's end moves nothing: a pane following the tail keeps following
zemi.event.on("session.open", function() zemi.ui.scroll("top", "thinking") end)

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
			return header(t.head, width, "turn:" .. t.first)
		end
		local ag = ctx.agent or {}
		if not ctx.agent_active then return header(t.head, width) end
		local tools = ag.tools or {}
		if ag.thinking and (ag.reasoning or "") ~= "" and (ag.text or "") == "" then
			local m = zemi.ui.marks.spin
			return header(
				"thinking · " .. fmt.duration(ag.thought or 0),
				width,
				nil,
				{ text = m.glyph .. " ", hl = m.hl, copy = false, effect = "spin" }
			)
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
		return header(label, width)
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
		-- a thought under its heading, a blank row after the one before it
		---@param head string
		---@param text string
		---@param into zemi.Line[]
		local function thought(head, text, into)
			if #into > 0 then into[#into + 1] = { { text = "" } } end
			local m = zemi.ui.marks.thought
			into[#into + 1] =
				{ GUTTER, { text = m.glyph .. " ", hl = m.hl, copy = false }, { text = head, hl = "Dim" } }
			for _, segs in ipairs(zemi.ui.wrap((text:gsub("%s+$", "")), width - 2)) do
				for _, sg in ipairs(segs) do
					sg.hl = "AgentReason"
				end
				table.insert(segs, 1, GUTTER)
				into[#into + 1] = segs
			end
		end
		local key = table.concat({ width, zemi.ui.theme.get() }, "|")
		if not t.body or t.body.key ~= key then
			---@type zemi.Line[]
			local lines = {}
			if t.first then
				-- the run's rows without their rail: the pane's separator is the
				-- only vertical line here, and an air row keeps its gutter as a blank
				for _, l in ipairs(chat.run(ctx, t.rows, t.first, width - 2)) do
					if l[1] and l[1].hl == "ThinkingBar" then table.remove(l, 1) end
					table.insert(l, 1, GUTTER)
					lines[#lines + 1] = l
				end
			else
				for _, th in ipairs(t.thoughts) do
					thought(fmt.thought(th.secs), th.text, lines)
				end
			end
			t.body = { key = key, lines = lines }
		end
		local ag = ctx.agent or {}
		-- the cached lines are shared, never appended to
		---@type zemi.Line[]
		local lines = {}
		for i, l in ipairs(t.body.lines) do
			lines[i] = l
		end
		-- the thought streaming now, under the turn's earlier ones
		if not t.first and ctx.agent_active and (ag.reasoning or "") ~= "" then
			thought("thinking", ag.reasoning, lines)
		end
		-- the region is bottom-anchored: trailing blanks keep short text at the
		-- top, and once it overflows the tail follows
		for _ = #lines + 1, ctx.height or 0 do
			lines[#lines + 1] = { { text = "" } }
		end
		return lines
	end,
})
