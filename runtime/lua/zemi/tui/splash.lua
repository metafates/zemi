-- The splash that fills the empty chat until the first message.
local fmt = require("zemi.tui.text")
local snail = require("zemi.tui.snail")
---@class zemi.tui.splash
local M = {}

--- Appends whole items to line, joined by sep, as many as fit in room cells.
---@param line zemi.Line
---@param items zemi.Segment[][]
---@param sep zemi.Segment
---@param room integer
---@return integer used cells of line
local function fill(line, items, sep, room)
	local used = 0
	for _, s in ipairs(line) do
		used = used + fmt.cells(s.text)
	end
	for i, item in ipairs(items) do
		local w = i > 1 and fmt.cells(sep.text) or 0
		for _, s in ipairs(item) do
			w = w + fmt.cells(s.text)
		end
		if used + w > room then break end
		used = used + w
		if i > 1 then line[#line + 1] = sep end
		for _, s in ipairs(item) do
			line[#line + 1] = s
		end
	end
	return used
end

--- The splash rows: the mascot and wordmark, what zemi will run with, recent sessions
--- to pick up and the keys worth knowing, centered in ctx.width by ctx.height.
---@param ctx zemi.WidgetCtx
---@return zemi.Line[]
function M.render(ctx)
	-- the block is at most 72 cells wide and keeps a 2-cell gutter, so long
	-- paths clip instead of pushing the logo against the edge
	---@type integer, zemi.Line[]
	local width, rows = ctx.width or 80, {}
	local maxw = math.min(width - 4, 72)
	---@param segs zemi.Line
	local function row(segs) rows[#rows + 1] = segs end
	local ver = (ctx.version or "") ~= "" and (ctx.version .. " · ") or ""
	local mark = { text = "@zemi", hl = "Title" }
	local tag = { text = ver .. "context included", hl = "Comment" }
	---@type zemi.Line
	local tip = {}
	local tipw = fill(tip, {
		{ { text = "/", hl = "SplashKey" }, { text = " commands", hl = "Comment" } },
		{ { text = "@", hl = "SplashKey" }, { text = " attach or drop files", hl = "Comment" } },
		{ { text = "!", hl = "SplashKey" }, { text = " shell", hl = "Comment" } },
		{ { text = "/help", hl = "SplashKey" }, { text = " keys", hl = "Comment" } },
	}, { text = "   " }, maxw)
	-- centered on the static rows (the wordmark beside the snail, the tips)
	-- with every value row clipped to the room right of them, so no value
	-- change (a model pick, a count, a title) moves the block
	local pad =
		math.max(0, math.floor((width - math.min(maxw, math.max(tipw, snail.MARK_COL + fmt.cells(tag.text)))) / 2))
	local room = math.min(maxw, width - pad - 2)

	-- a short chat drops the recent sessions first, then the mascot, then the
	-- context row, so the bottom-anchored chat never cuts the block's first
	-- rows (it shows height - 1 of them)
	local recent, fit = ctx.recent or {}, (ctx.height or 0) > 0 and ctx.height - 1 or math.huge
	-- taken before the fit check empties recent: a project with sessions never shows it
	local try = #recent == 0
	-- the no-model hint names its fix, so it wraps under its label instead of clipping
	local models = zemi.ui.wrap(ctx.model ~= "" and ctx.model or ("none yet: " .. ctx.model_hint), room - 9)
	if ctx.init_error ~= "" then
		models = {
			{ { text = (ctx.init_error:match("([^/%s:]+:%d+):") or "init.lua") .. " failed: /errors", hl = "Danger" } },
		}
	end
	local base = 9 + #models
	local extra = #recent > 0 and #recent + 3 or 1
	if base + extra > fit then
		recent, try, extra = {}, false, 0
	end
	local mascot, showctx = width >= 36 and base + extra <= fit, base - 2 <= fit
	if mascot then
		for _, r in ipairs(snail.block({ mark, tag })) do
			row(r)
		end
	else
		row({ mark })
		row({ tag })
	end
	row({ { text = "" } })
	for i, segs in ipairs(models) do
		table.insert(segs, 1, { text = i == 1 and "model    " or string.rep(" ", 9), hl = "Comment" })
		row(segs)
	end
	local cwd, cwdw = ctx.cwd or "", room - 10
	if fmt.cells(cwd) > cwdw and cwdw > 1 then
		-- keep the tail: the project name is at the end
		---@type string[]
		local chars = {}
		for ch in cwd:gmatch(fmt.UTF8_CHAR) do
			chars[#chars + 1] = ch
		end
		cwd = "…" .. table.concat(chars, "", #chars - cwdw + 2)
	end
	row({ { text = "project  ", hl = "Comment" }, { text = cwd, hl = "Normal" } })
	-- built only when shown: ctx.included reads the store when its cache went stale
	if showctx then
		local inc = ctx.included
		---@type zemi.Segment[][]
		local parts = {}
		-- the first part is always there, so no count change adds or removes the row
		local files = inc.instructions
		if not files and not ctx.trusted and ctx.trustable then
			parts[1] = { { text = "instructions off until ", hl = "Comment" }, { text = "/trust", hl = "SplashKey" } }
		elseif not files then
			parts[1] = { { text = "no project instructions", hl = "Comment" } }
		elseif #files == 0 then
			parts[1] = { { text = "/init", hl = "SplashKey" }, { text = " writes AGENTS.md", hl = "Comment" } }
		else
			parts[1] = {
				{
					text = #files == 1 and files[1] or fmt.count(#files, "instruction file", "instruction files"),
					hl = "Normal",
				},
			}
		end
		for _, c in ipairs({
			{ inc.memories, "memory", "memories" },
			{ inc.skills, "skill", "skills" },
			{ inc.mcp, "mcp server", "mcp servers" },
			{ inc.plugins, "plugin", "plugins" },
		}) do
			if c[1] > 0 then parts[#parts + 1] = { { text = fmt.count(c[1], c[2], c[3]), hl = "Normal" } } end
		end
		---@type zemi.Line
		local context = { { text = "context  ", hl = "Comment" } }
		fill(context, parts, { text = " · ", hl = "Comment" }, room)
		row(context)
	end
	if try then
		-- zemi is configured by asking it, which no other screen says
		---@type zemi.Line
		local line = { { text = "try      ", hl = "Comment" } }
		fill(line, {
			{ { text = '"explain this project"', hl = "Normal" } },
			{ { text = '"make the snail green"', hl = "Normal" } },
		}, { text = " · ", hl = "Comment" }, room)
		row(line)
	end
	if #recent > 0 then
		row({ { text = "" } })
		row({ { text = "recent", hl = "Title" } })
		for _, r in ipairs(recent) do
			local title = (r.title ~= "" and r.title) or "(untitled)"
			row({ { text = string.format("  %-8s ", r.ago), hl = "Comment" }, { text = title, hl = "Normal" } })
		end
		row({
			{ text = "  /resume", hl = "SplashKey" },
			{ text = " continues the latest, ", hl = "Comment" },
			{ text = "/sessions", hl = "SplashKey" },
			{ text = " lists them", hl = "Comment" },
		})
	end
	row({ { text = "" } })
	row(tip)

	-- rows stay left-aligned to each other, clipped so a narrow window never wraps the logo
	---@type zemi.Line[]
	local out = {}
	local top = math.max(0, math.floor(((ctx.height or 0) - #rows) / 2))
	for _ = 1, top do
		out[#out + 1] = { { text = "" } }
	end
	for _, r in ipairs(rows) do
		---@type integer, zemi.Line
		local n, clipped = 0, { { text = string.rep(" ", pad), copy = false } }
		for _, seg in ipairs(r) do
			local left = room - n
			if left <= 0 then break end
			local text = fmt.clip(seg.text, left)
			clipped[#clipped + 1] = { text = text, hl = seg.hl }
			n = n + fmt.cells(text)
		end
		out[#out + 1] = clipped
	end
	-- the chat is bottom-anchored: trailing rows keep the block centered
	for _ = 1, math.max(0, (ctx.height or 0) - top - #rows - 1) do
		out[#out + 1] = { { text = "" } }
	end
	return out
end

-- Register a widget with id "splash" in init.lua to replace it (ctx carries
-- version, model, cwd, included, recent, width and height).
zemi.ui.widget.add({
	id = "splash",
	region = "chat",
	priority = 5,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if ctx.transcript_count > 0 or ctx.agent_active then return {} end
		return M.render(ctx)
	end,
})

return M
