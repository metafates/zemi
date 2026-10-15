-- What zemi prints to the shell when it quits: the logo, the session's title
-- and the command that resumes it. A session with no messages prints nothing.
-- A printed run (zemi run) opens with its prompt row, so it gets no banner.
local fmt = require("zemi.tui.text")
local knot = require("zemi.tui.knot")

zemi.ui.widget.add({
	id = "exit",
	region = "exit",
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if ctx.resume == "" then return {} end
		local blank = { { text = "" } }
		---@type zemi.Line[]
		local rows = { blank }
		if not ctx.headless then
			local title = zemi.session.title()
			-- "zemi" is an untitled session's placeholder (the auto title had not landed)
			if title == "zemi" then title = "" end
			-- a viewed agent run's transcript is the run's, not the session's
			if title == "" and not ctx.view then
				for _, r in ipairs(ctx.transcript) do
					if r.role == "user" and r.kind == "" then
						title = r.text:match("^%s*([^\n]*)")
						break
					end
				end
			end
			for _, r in ipairs(knot.logo(ctx.width - 4)) do
				rows[#rows + 1] = r
			end
			if title ~= "" then
				-- the session's marker leads its title on every surface
				if ctx.emoji ~= "" then title = ctx.emoji .. " " .. title end
				rows[#rows + 1] = blank
				rows[#rows + 1] = { { text = fmt.clip(title, ctx.width - 4), hl = "ExitTitle" } }
			end
			rows[#rows + 1] = blank
		end
		rows[#rows + 1] = { fmt.label("resume"), { text = ctx.resume, hl = "SplashKey" } }
		local cost = fmt.cost(ctx.usage.cost)
		if cost then rows[#rows + 1] = { fmt.label("cost"), { text = cost } } end
		rows[#rows + 1] = blank
		-- a 2-cell gutter, like the shell output around it
		for _, r in ipairs(rows) do
			if r ~= blank then table.insert(r, 1, { text = "  " }) end
		end
		return rows
	end,
})
