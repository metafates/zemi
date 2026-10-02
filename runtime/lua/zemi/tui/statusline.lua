-- The statusline widgets: statusline.left holds the mode, the project and
-- what needs attention, statusline.right the model and context use. Each
-- side draws from its outer edge in priority order and a short row drops the
-- highest priority first, so what lasts sits outermost and what comes and
-- goes sits inner, where it moves nothing.
local fmt = require("zemi.tui.text")

-- The permission mode, colored like the composer prompt. A click opens the
-- mode picker.
zemi.ui.widget.add({
	id = "statusline.mode",
	region = "statusline.left",
	priority = 5,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		local mode = ctx.permission_mode or "auto"
		local hl = ({ auto = "ModeAuto", yolo = "ModeYolo" })[ctx.permission_mode_base or mode] or "ModeAuto"
		return { { { text = mode, hl = hl, click = "mode" } } }
	end,
})

-- The project: its path when short, else its name (the splash has the whole
-- path), and whether it is untrusted. A click on the path opens /projects.
zemi.ui.widget.add({
	id = "statusline.cwd",
	region = "statusline.left",
	priority = 10,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		local p = ctx.project or ctx.cwd or ""
		if zemi.ui.width(p) > math.floor((ctx.width or 80) / 4) then p = p:match("([^/]+)/*$") or p end
		---@type zemi.Line
		local line = { { text = p, hl = "Dim", click = "projects" } }
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
		local flows, agents, jobs = ctx.workflows_running or 0, ctx.agents_running or 0, ctx.jobs_running or 0
		if flows == 0 and agents == 0 and jobs == 0 then return {} end
		---@type string[]
		local parts = {}
		if flows > 0 then parts[#parts + 1] = fmt.count(flows, "workflow", "workflows") end
		if agents > 0 then parts[#parts + 1] = fmt.count(agents, "agent", "agents") end
		if jobs > 0 then parts[#parts + 1] = fmt.count(jobs, "job", "jobs") end
		local cmd = flows > 0 and "workflows" or "agents"
		return { fmt.hint("⧗ " .. table.concat(parts, ", "), "/" .. cmd) }
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
		return { fmt.hint(fmt.count(n, "memory", "memories") .. " to review", "/memory") }
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
		return { fmt.hint("↓ " .. ctx.rows_below, "ctrl+end", "bottom") }
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
	-- a session with nothing worth keeping ends quietly
	if #parts == 0 then return end
	zemi.notify({ title = "Memories learned", body = table.concat(parts, ", ") })
end)

-- Statusline right, read left to right: "$0.12 · 45% · 24.5k/200k ctx
-- · planner · model"; the lowest priority (model) sits at the right edge. The
-- context meter falls back to the session token total when the model's
-- context window is unknown. The percent colors by threshold:
-- muted, warn at 80%, err at 95%. A click on the model opens the model
-- picker, on the profile the profile picker, on the rest /usage. One widget
-- per part, so config can replace or delete any of them.
---@type { [1]: string, [2]: integer, [3]: fun(ctx: zemi.WidgetCtx): string?, string?, string? }[]
local right = {
	{
		"model",
		8,
		function(ctx)
			if (ctx.model or "") == "" then return end
			return ctx.model .. ((ctx.effort or "") ~= "" and (" " .. ctx.effort) or ""), "Dim", "model"
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
	-- inside the meter, so it drops first and never stands without its unit;
	-- it draws whenever the meter does, 0% included, so it moves nothing
	{
		"percent",
		41,
		function(ctx)
			local cx = ctx.context or {}
			if (cx.tokens or 0) <= 0 or (cx.window or 0) <= 0 then return end
			local pct = math.floor(tonumber(cx.pct) or 0)
			return string.format("%d%%", pct),
				pct >= 95 and "StatusUsageErr" or pct >= 80 and "StatusUsageWarn" or "StatusUsage"
		end,
	},
	{
		"cost",
		42,
		function(ctx) return fmt.cost((ctx.usage or {}).cost) end,
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
