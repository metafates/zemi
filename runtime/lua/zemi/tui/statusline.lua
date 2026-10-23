-- The statusline widgets: statusline.left holds the project and what needs
-- attention, statusline.right the model and context use. Each
-- side draws from its outer edge in priority order and a short row drops the
-- highest priority first, so what lasts sits outermost and what comes and
-- goes sits inner, where it moves nothing.
local fmt = require("zemi.tui.text")

-- fish's prompt_pwd: every directory but the last down to a cell, a leading
-- dot kept
---@param p string
---@return string
local function short_path(p)
	return (p:gsub("([^/]+)/", function(dir) return dir:sub(1, dir:sub(1, 1) == "." and 2 or 1) .. "/" end))
end

-- The project, short_path'd, the name alone when even that outgrows a quarter
-- of the row (the splash has the whole path), and whether it is untrusted. A
-- click on it opens /projects.
zemi.ui.widget.add({
	id = "statusline.cwd",
	region = "statusline.left",
	priority = 10,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		local p = short_path(ctx.project or ctx.cwd or "")
		if zemi.ui.width(p) > math.floor((ctx.width or 80) / 4) then p = p:match("([^/]+)/*$") or p end
		---@type zemi.Line
		local line = { { text = p, hl = "Dim", click = "projects" } }
		if (ctx.branch or "") ~= "" then line[#line + 1] = { text = ":" .. ctx.branch, hl = "Accent" } end
		if ctx.trusted == false then
			line = p == "" and {} or { line[1], { text = " · ", hl = "Dim" } }
			for _, seg in ipairs(fmt.hint("untrusted", "/trust")) do
				line[#line + 1] = seg
			end
		end
		return { line }
	end,
})

-- The session's goal, while one is set; no count, so a check moves nothing.
zemi.ui.widget.add({
	id = "statusline.goal",
	region = "statusline.left",
	priority = 20,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if not ctx.goal then return {} end
		return { fmt.hint("⚑ goal", "/goal") }
	end,
})

-- The session's loop and when its next run fires, five cells whatever the
-- time, so a reschedule moves nothing.
zemi.ui.widget.add({
	id = "statusline.loop",
	region = "statusline.left",
	priority = 21,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if not ctx.loop then return {} end
		return { fmt.hint("↻ " .. fmt.clock(ctx.loop.next), "/loop") }
	end,
})

-- Subagents at work: the turn's own, background ones, workflows and Lua.
zemi.ui.widget.add({
	id = "statusline.agents",
	region = "statusline.left",
	priority = 25,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		local flows, agents = ctx.workflows_running or 0, ctx.agents_running or 0
		if flows == 0 and agents == 0 then return {} end
		local n = flows > 0 and flows or agents
		local word = flows > 0 and "workflow" or "agent"
		---@type zemi.Line
		local line = fmt.hint("⧗ " .. n, word .. (n > 1 and "s" or ""), word .. "s")
		if flows > 0 and agents > 0 then
			line[#line + 1] = { text = " · ", hl = "Dim" }
			for _, seg in ipairs(fmt.hint(tostring(agents), agents == 1 and "agent" or "agents", "agents")) do
				line[#line + 1] = seg
			end
		end
		return { line }
	end,
})

-- Background processes the model started: their own screen, apart from the
-- agents one.
zemi.ui.widget.add({
	id = "statusline.jobs",
	region = "statusline.left",
	priority = 26,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if (ctx.jobs_running or 0) == 0 then return {} end
		return { fmt.hint("⚙ " .. ctx.jobs_running, ctx.jobs_running == 1 and "job" or "jobs", "jobs") }
	end,
})

-- Learned proposals waiting for review; they persist across restarts.
zemi.ui.widget.add({
	id = "statusline.learn",
	region = "statusline.left",
	priority = 30,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		local n = ctx.pending_memory or 0
		if n == 0 then return {} end
		return { fmt.hint(tostring(n), n == 1 and "memory" or "memories", "memory") }
	end,
})

-- The keys of a bound sequence typed so far, waiting for the rest.
zemi.ui.widget.add({
	id = "statusline.keys",
	region = "statusline.left",
	priority = 95,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if (ctx.key_seq or "") == "" then return {} end
		return { { { text = ctx.key_seq .. " …", hl = "Title" } } }
	end,
})

-- While the chat is scrolled up, how many rows are below and the key that
-- goes back: the bar shows only where the reader is, not what a running turn
-- added under them or how a keyboard gets back. A click goes back too: its id
-- is the scroll target, not a command's name.
zemi.ui.widget.add({
	id = "statusline.scroll",
	region = "statusline.left",
	priority = 100,
	on_click = function() zemi.ui.scroll("bottom") end,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if (ctx.rows_below or 0) == 0 then return {} end
		return { fmt.hint("↓ " .. ctx.rows_below, "ctrl+b", "bottom") }
	end,
})

-- A first idle ctrl+c arms quit; say what the second one does.
zemi.ui.widget.add({
	id = "statusline.quit",
	region = "statusline.left",
	priority = 110,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if not ctx.quit_armed then return {} end
		return { fmt.hint("ctrl+c", "again to quit") }
	end,
})

zemi.event.on("reflector.done", function(ev)
	if ev.error then return end
	---@type string[]
	local parts = {}
	---@param n integer?
	---@param one string
	---@param many string
	local function add(n, one, many)
		if n and n > 0 then parts[#parts + 1] = fmt.count(n, one, many) end
	end
	add(ev.stored, "memory stored", "memories stored")
	add(ev.queued, "memory to review in /memory", "memories to review in /memory")
	add(ev.superseded, "memory superseded", "memories superseded")
	add(ev.retired, "memory retired", "memories retired")
	-- a session with nothing worth keeping ends quietly
	if #parts == 0 then return end
	zemi.notify({ title = "Memories learned", body = table.concat(parts, ", ") })
end)

-- Statusline right, read left to right: "$0.12 · 24.5k/200k ctx
-- · profile · effort · model"; the lowest priority (model) sits at the right
-- edge. The context meter falls back to the session token total when the
-- model's context window is unknown. A click on the model opens the model
-- picker, on the effort the effort picker, on the profile the profile
-- picker, on the rest /usage. One widget per part, so config can replace or
-- delete any of them.
---@type { [1]: string, [2]: integer, [3]: fun(ctx: zemi.WidgetCtx): string?, string?, string? }[]
local right = {
	{
		"model",
		8,
		function(ctx)
			if (ctx.model or "") == "" then return end
			return ctx.model, "Dim", "model"
		end,
	},
	{
		"effort",
		9,
		function(ctx)
			if (ctx.effort or "") == "" then return end
			return ctx.effort, "Dim", "effort"
		end,
	},
	-- the shipped profile goes unsaid
	{
		"profile",
		20,
		function(ctx)
			if ctx.profile_read_only then return ctx.profile, "ModePlan", "profile" end
			if ctx.profile ~= "default" then return ctx.profile, "Dim", "profile" end
		end,
	},
	-- how much of the window is in use, not the running session total: the
	-- total says nothing about how much room is left.
	{
		"context",
		40,
		function(ctx)
			local cx = ctx.context or {}
			local used, window = fmt.tokens(cx.tokens), fmt.tokens(cx.window)
			if window then return used and used .. "/" .. window .. " ctx" end
			local total = fmt.tokens((ctx.usage or {}).total)
			return total and total .. " tok"
		end,
	},
	{
		"cost",
		42,
		-- the session's tally may carry a side call on a priced model; the
		-- widget shows a price only when the conversation's own model has one
		function(ctx)
			local u = ctx.usage or {}
			if not u.priced then return nil end
			return fmt.cost(u.cost)
		end,
	},
}
for _, part in ipairs(right) do
	zemi.ui.widget.add({
		id = "statusline." .. part[1],
		region = "statusline.right",
		priority = part[2],
		on_click = fmt.run_click,
		---@param ctx zemi.WidgetCtx
		---@return zemi.Line[]
		render = function(ctx)
			local text, hl, click = part[3](ctx)
			if not text then return {} end
			return { { { text = text, hl = hl or "Dim", click = click or "usage" } } }
		end,
	})
end

-- The two keys worth knowing, on an empty session only; innermost, so the
-- first send takes them away without moving the model or the cost, and the
-- first to go on a short row.
zemi.ui.widget.add({
	id = "statusline.hints",
	region = "statusline.right",
	priority = 130,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if ctx.transcript_count > 0 or ctx.agent_active then return {} end
		return {
			{
				{ text = "/", hl = "SplashKey", click = "help" },
				{ text = " commands  ", hl = "Dim", click = "help" },
				{ text = "@", hl = "SplashKey" },
				{ text = " files", hl = "Dim" },
			},
		}
	end,
})
