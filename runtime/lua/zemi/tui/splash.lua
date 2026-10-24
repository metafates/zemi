-- The splash that fills the empty chat until the first message.
local fmt = require("zemi.tui.text")
local knot = require("zemi.tui.knot")
---@class zemi.tui.splash
local M = {}

--- Cuts a path that is wider than w cells to its tail, where its name is.
---@param path string
---@param w integer
---@return string
local function tail(path, w)
	if zemi.ui.width(path) <= w or w <= 1 then return path end
	---@type string[]
	local chars = {}
	for ch in path:gmatch(fmt.UTF8_CHAR) do
		chars[#chars + 1] = ch
	end
	-- the tail keeps w - 1 cells and the ellipsis the last of them; a wide
	-- character takes two, so it is cells that hold the box's width (ui's tailSegs)
	local keep, used = #chars + 1, 0
	while keep > 1 do
		local cw = zemi.ui.width(chars[keep - 1])
		if used + cw > w - 1 then break end
		keep, used = keep - 1, used + cw
	end
	return "…" .. table.concat(chars, "", keep)
end

--- Flows segments after an indent cells wide into at most two rows of room
--- cells, breaking between words; the second row starts at the indent too.
---@param segs zemi.Segment[]
---@param room integer
---@param indent integer
---@return zemi.Line[]
local function flow(segs, room, indent)
	---@type zemi.Line[]
	local rows = { {} }
	local w = indent
	for _, s in ipairs(segs) do
		for word in s.text:gmatch("%s*%S+%s*") do
			local seg = word
			if w > indent and w + zemi.ui.width((word:gsub("%s+$", ""))) > room then
				if #rows == 2 then
					local last = rows[2][#rows[2]]
					last.text = last.text:gsub("%s+$", "") .. "…"
					return rows
				end
				rows[2] = { { text = string.rep(" ", indent), copy = false } }
				w = indent
				seg = seg:gsub("^%s+", "")
			end
			local row = rows[#rows]
			row[#row + 1] = { text = seg, hl = s.hl, click = s.click }
			w = w + zemi.ui.width(seg)
		end
	end
	return rows
end

-- Discovery tips for a first session, one per hour. The pick happens when
-- the module loads, so the splash's row never changes between renders.
local TIPS = {
	{ "!git status", "fill !git status", " runs in your shell; the output rides your next prompt" },
	{ "@", "fill @", " attaches a project file to the prompt" },
	{ "/btw ", "fill /btw ", " asks a side question the conversation does not keep" },
	{ "/goal ", "fill /goal ", " keeps working until a condition holds" },
	{ "/review", "fill /review", " has several agents review your changes" },
	{ "alt+m", nil, " jumps to an earlier message" },
	{ "+", "fill +", " attaches a skill to the prompt" },
}
local TIP = TIPS[math.floor(os.time() / 3600) % #TIPS + 1]

--- The one tip the splash ends with: the first that applies of a failed
--- init.lua, no model, an untrusted repository, no instruction file, a
--- first session, else how to go on.
---@param ctx zemi.WidgetCtx
---@param listed boolean Recent sessions or projects are listed.
---@return zemi.Segment[]
local function tip(ctx, listed)
	---@param text string
	---@param click string?
	---@return zemi.Segment
	local function key(text, click) return { text = text, hl = "SplashKey", click = click } end
	---@param text string
	---@return zemi.Segment
	local function dim(text) return { text = text, hl = "Dim" } end
	if ctx.init_error ~= "" then
		local at = ctx.init_error:match("([^/%s:]+:%d+):") or "init.lua"
		return { { text = at .. " failed: ", hl = "Danger" }, key("/errors", "errors") }
	elseif ctx.model == "" then
		return { dim("No model yet: " .. ctx.model_hint) }
	elseif not ctx.trusted and ctx.trustable then
		return { key("/trust", "trust"), dim(" lets zemi read this project's instructions and run code") }
	elseif ctx.instructions and #ctx.instructions == 0 then
		return { key("/init", "fill /init"), dim(" writes an AGENTS.md, so zemi knows this project") }
	elseif not listed then
		return { key(TIP[1], TIP[2]), dim(TIP[3]) }
	elseif ctx.projects then
		return { key("/projects", "projects"), dim(" switches to one, or type to stay here") }
	end
	return { key("/resume", "resume"), dim(" picks up the latest session") }
end

--- The row the block ends with: the pending row the chat yields to the
--- splash, clipped to room, else a blank one. The row is kept either way, so
--- the content above it never moves as the boot's work comes and goes.
---@param ctx zemi.WidgetCtx
---@param room integer
---@return zemi.Line
local function lastRow(ctx, room)
	local pending = ctx.view and nil or fmt.pending(ctx)
	if not pending then return { { text = "" } } end
	---@type zemi.Line
	local row = {}
	local used = 0
	for _, s in ipairs(pending) do
		if used >= room then break end
		local text = fmt.clip(s.text, room - used)
		used = used + zemi.ui.width(text)
		row[#row + 1] = { text = text, hl = s.hl, hover = s.hover, copy = s.copy, effect = s.effect }
	end
	return row
end

--- The splash rows: the logo, recent sessions to pick up and one tip, each
--- group centered in ctx.width and the whole a little below the middle of
--- ctx.height, near the composer.
---@param ctx zemi.WidgetCtx
---@return zemi.Line[]
function M.render(ctx)
	local width = ctx.width or 80
	local fit = ctx.height or 0
	local room = math.max(math.min(width - 4, 72), 1)
	-- where zemi is rarely the pick (~, /, a new dir outside a repository) Go
	-- sets projects to switch to, which take the recent sessions' place
	local recent = ctx.projects or ctx.recent or {}
	local tips = flow(tip(ctx, #recent > 0), room, 7)
	table.insert(tips[1], 1, { text = "● Tip  ", hl = "Accent", copy = false })
	local last = lastRow(ctx, room)
	local version = ctx.version or ""

	-- a short chat drops the recent rows, then the version, then the logo's art
	local logo = knot.logo(room)
	local function count()
		return #logo + (version ~= "" and 1 or 0) + (#recent > 0 and #recent + 1 or 0) + 1 + #tips + 1
	end
	if count() > fit then recent = {} end
	if count() > fit then version = "" end
	if count() > fit then logo = knot.logo(0) end
	local n = count()

	if version ~= "" then
		local w = 0
		for _, r in ipairs(logo) do
			w = math.max(w, fmt.cells(r))
		end
		logo[#logo + 1] = {
			{ text = string.rep(" ", math.max(w - zemi.ui.width(version), 0)), copy = false },
			{ text = version, hl = "Dim" },
		}
	end
	---@type zemi.Line[][]
	local groups = { logo }
	if #recent > 0 then
		-- titles left, ages right, across one width
		local w = math.min(room, 56)
		---@type zemi.Line[]
		local rows = {}
		for _, r in ipairs(recent) do
			local ago = zemi.ui.width(r.ago)
			local label = ctx.projects and tail(r.path, w - ago - 2) or r.title
			label = fmt.clip(label, w - ago - 2)
			local click = ctx.projects and ("projects switch " .. r.root) or ("resume " .. r.id)
			rows[#rows + 1] = {
				{ text = label, hl = "Normal", click = click },
				{ text = string.rep(" ", w - zemi.ui.width(label) - ago), copy = false },
				{ text = r.ago, hl = "Dim" },
			}
		end
		groups[#groups + 1] = rows
	end
	groups[#groups + 1] = tips

	-- a little below the middle, near the composer, and placed as if the tip
	-- were one row, so a tip that wraps moves nothing above it: its second row
	-- takes the one left blank under it (flow gives it two at most)
	local top = math.floor((fit - n + #tips - 1) / 2) + 2
	---@type zemi.Line[]
	local out = {}
	for _ = 1, math.min(top, fit - n + #tips - 2) do
		out[#out + 1] = { { text = "" } }
	end
	for g, rows in ipairs(groups) do
		if g > 1 then out[#out + 1] = { { text = "" } } end
		-- each group centered whole, its rows left-aligned to each other
		local w = 0
		for _, r in ipairs(rows) do
			w = math.max(w, fmt.cells(r))
		end
		local pad = { text = string.rep(" ", math.max(0, math.floor((width - math.min(w, room)) / 2))), copy = false }
		for _, r in ipairs(rows) do
			-- clipped so a narrow window never wraps a row
			---@type integer, zemi.Line
			local used, line = 0, { pad }
			for _, s in ipairs(r) do
				if used >= room then break end
				local text = fmt.clip(s.text, room - used)
				line[#line + 1] = { text = text, hl = s.hl, click = s.click, copy = s.copy }
				used = used + zemi.ui.width(text)
			end
			out[#out + 1] = line
		end
	end
	-- the region's last row, where the chat draws its pending row: a block that
	-- already fills the region leaves it none
	if fit > 0 and #out < fit then
		for _ = #out + 1, fit - 1 do
			out[#out + 1] = { { text = "" } }
		end
		out[#out + 1] = last
	end
	return out
end

-- Register a widget with id "splash" in init.lua to replace it (its ctx is a
-- zemi.WidgetCtx).
zemi.ui.widget.add({
	id = "splash",
	region = "chat",
	priority = 5,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if not fmt.empty_chat(ctx) then return {} end
		return M.render(ctx)
	end,
})

return M
