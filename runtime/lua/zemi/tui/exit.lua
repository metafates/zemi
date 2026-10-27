-- What zemi prints to the shell when it quits: the logo, the chat's first
-- prompt and the cost. A chat with no messages prints nothing.
local fmt = require("zemi.tui.text")
local knot = require("zemi.tui.knot")

zemi.ui.widget.add({
	id = "exit",
	region = "exit",
	---@param ctx zemi.WidgetCtx
	---@return zemi.Line[]
	render = function(ctx)
		if #ctx.transcript == 0 then return {} end
		local blank = { { text = "" } }
		---@type zemi.Line[]
		local rows = { blank }
		local title = ""
		-- a viewed agent run's transcript is the run's, not the chat's
		if not ctx.view then
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
			rows[#rows + 1] = blank
			rows[#rows + 1] = { { text = fmt.clip(title, ctx.width - 4), hl = "ExitTitle" } }
		end
		rows[#rows + 1] = blank
		local cost = fmt.cost(ctx.usage.cost)
		if cost then rows[#rows + 1] = { fmt.label("cost"), { text = cost } } end
		return rows
	end,
})
