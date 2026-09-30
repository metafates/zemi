-- The splash that fills the empty chat until the first message.
local fmt = require("zemi.tui.text")
local snail = require("zemi.tui.snail")
---@class zemi.tui.splash
local M = {}

--- Cuts a path that is wider than w cells to its tail, where its name is.
---@param path string
---@param w integer
---@return string
local function tail(path, w)
	if fmt.cells(path) <= w or w <= 1 then return path end
	---@type string[]
	local chars = {}
	for ch in path:gmatch(fmt.UTF8_CHAR) do
		chars[#chars + 1] = ch
	end
	return "…" .. table.concat(chars, "", #chars - w + 2)
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
			if w > indent and w + fmt.cells((word:gsub("%s+$", ""))) > room then
				if #rows == 2 then
					local last = rows[2][#rows[2]]
					last.text = last.text:gsub("%s+$", "") .. "…"
					return rows
				end
				rows[2] = { { text = string.rep(" ", indent), copy = false } }
				w = indent
				word = word:gsub("^%s+", "")
			end
			local row = rows[#rows]
			row[#row + 1] = { text = word, hl = s.hl, click = s.click }
			w = w + fmt.cells(word)
		end
	end
	return rows
end

--- The one tip the splash ends with: the first that applies of a failed
--- init.lua, no model, an untrusted repository, no instruction file, a
--- first session, else how to go on.
---@param ctx zemi.WidgetCtx
---@param listed boolean Recent sessions or projects are listed.
---@return zemi.Segment[]
local function tip(ctx, listed)
	---@param text string
	---@param click string
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
		return { key("/trust", "trust"), dim(" lets zemi read this repo's AGENTS.md and run its builds") }
	elseif ctx.instructions and #ctx.instructions == 0 then
		return { key("/init", "fill /init"), dim(" writes an AGENTS.md, so zemi knows this project") }
	elseif not listed then
		-- zemi is configured by asking it, which no other screen says
		return {
			dim("try "),
			key('"make the snail green"', "fill make the snail green"),
			dim(": zemi configures itself"),
		}
	elseif ctx.projects then
		return { key("/projects", "projects"), dim(" switches to one, or type to stay here") }
	end
	return { key("/resume", "resume"), dim(" picks up the latest session") }
end

--- Cells of a line.
---@param line zemi.Line
---@return integer
local function cells(line)
	local w = 0
	for _, s in ipairs(line) do
		w = w + fmt.cells(s.text)
	end
	return w
end

--- The splash rows: the logo, recent sessions to pick up and one tip, each
--- group centered in ctx.width and the whole a little below the middle of
--- ctx.height, near the composer.
---@param ctx zemi.WidgetCtx
---@return zemi.Line[]
function M.render(ctx)
	local width = ctx.width or 80
	-- the chat is bottom-anchored and shows height - 1 of these rows
	local fit = (ctx.height or 0) - 1
	local room = math.max(math.min(width - 4, 72), 1)
	-- where zemi is rarely the pick (~, /, a new dir outside a repository) Go
	-- sets projects to switch to, which take the recent sessions' place
	local recent = ctx.projects or ctx.recent or {}
	local tips = flow(tip(ctx, #recent > 0), room, 7)
	table.insert(tips[1], 1, { text = "● Tip  ", hl = "Accent", copy = false })
	local version = ctx.version or ""

	-- a short chat drops the recent rows, then the version, then the logo's art
	local logo = snail.logo(room)
	local function count() return #logo + (version ~= "" and 1 or 0) + (#recent > 0 and #recent + 1 or 0) + 1 + #tips end
	if count() > fit then recent = {} end
	if count() > fit then version = "" end
	if count() > fit then logo = snail.logo(0) end
	local n = count()

	if version ~= "" then
		logo[#logo + 1] = {
			{ text = string.rep(" ", math.max(cells(logo[1]) - fmt.cells(version), 0)), copy = false },
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
			local ago = fmt.cells(r.ago)
			local label = ctx.projects and tail(r.path, w - ago - 2) or (r.title ~= "" and r.title) or "(untitled)"
			label = fmt.clip(label, w - ago - 2)
			local click = ctx.projects and ("projects switch " .. r.root) or ("resume " .. r.id)
			rows[#rows + 1] = {
				{ text = label, hl = "Normal", click = click },
				{ text = string.rep(" ", w - fmt.cells(label) - ago), copy = false },
				{ text = r.ago, hl = "Dim" },
			}
		end
		groups[#groups + 1] = rows
	end
	groups[#groups + 1] = tips

	-- a little below the middle, near the composer, and placed as if the tip
	-- were one row, so a tip that wraps moves nothing above it
	local top = math.floor((fit - n + #tips - 1) / 2) + 2
	---@type zemi.Line[]
	local out = {}
	for _ = 1, math.min(top, fit - n - 1) do
		out[#out + 1] = { { text = "" } }
	end
	for g, rows in ipairs(groups) do
		if g > 1 then out[#out + 1] = { { text = "" } } end
		-- each group centered whole, its rows left-aligned to each other
		local w = 0
		for _, r in ipairs(rows) do
			w = math.max(w, cells(r))
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
				used = used + fmt.cells(text)
			end
			out[#out + 1] = line
		end
	end
	for _ = #out + 1, fit do
		out[#out + 1] = { { text = "" } }
	end
	return out
end

-- Register a widget with id "splash" in init.lua to replace it (ctx carries
-- version, model, cwd, instructions, recent, projects, width and height).
zemi.ui.widget.add({
	id = "splash",
	region = "chat",
	priority = 5,
	on_click = fmt.run_click,
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if ctx.headless or ctx.transcript_count > 0 or ctx.agent_active then return {} end
		return M.render(ctx)
	end,
})

return M
