local M = {}
local missing -- the rule line while zemi.job is not allowed

local function poll()
	local ok, err = pcall(zemi.job.start, {
		cmd = "git",
		args = { "rev-parse", "--abbrev-ref", "HEAD" },
		on_exit = function(res)
			-- the bug fail_test.lua finds: git printing nothing
			M.branch = res.code == 0 and res.stdout:match("^(%S+)"):sub(1, 30) or nil
		end,
	})
	missing = not ok and err or nil
end

zemi.health.add("demo", function()
	return missing and { level = "warn", detail = missing } or "reading the branch"
end)

zemi.ui.widget.add({
	id = "demo.branch",
	region = "statusline.right",
	priority = 50,
	render = function()
		return M.branch and { { { text = M.branch, hl = "Dim", click = "branch" } } } or {}
	end,
	on_click = function()
		zemi.notify.info(M.branch, "Branch")
	end,
})

zemi.ui.widget.add({
	id = "demo.cost",
	region = "statusline.right",
	render = function(ctx)
		return { { string.format("%d cents", ctx.usage.cost * 100) } }
	end,
})

zemi.event.on("app.ready", function()
	poll()
	zemi.timer.every(30000, poll)
end)

function M.pick()
	local snips = zemi.state.get("snips") or {}
	local names = {}
	for name in pairs(snips) do
		names[#names + 1] = name
	end
	if #names == 0 then
		return zemi.notify.info("add one with /demo add <name> <text>", "Nothing to pick yet")
	end
	table.sort(names)
	zemi.ui.select(names, { prompt = "Snippets" }, function(choice)
		if choice then
			zemi.agent.send(snips[choice])
		end
	end)
end

zemi.command.add("demo", function(a)
	if a.list[1] ~= "add" then
		return M.pick()
	end
	local name, text = a.args:match("^%s*add%s+(%S+)%s+(.+)$")
	if not name then
		error("usage: /demo add <name> <text>", 0)
	end
	local snips = zemi.state.get("snips") or {}
	snips[name] = text
	zemi.state.set("snips", snips)
end, { desc = "save a snippet, or send one to the agent", nargs = "*" })

zemi.keymap.set("ctrl+x d", M.pick, { desc = "send a snippet" })
zemi.keymap.set("ctrl+x h", "help", { desc = "zemi's help, a Go command" })
zemi.keymap.set("ctrl+x r", "zemi.ui.reasoning.toggle", { desc = "a host action" })

zemi.tool.add({
	name = "weather_now",
	description = "The weather in a city now.",
	input = { city = { type = "string" } },
	required = { "city" },
	execute = function(input)
		local res, err = zemi.http.get("https://wttr.in/" .. input.city:gsub(" ", "%%20") .. "?format=j1"):wait()
		if not res then
			return nil, err
		end
		if res.status ~= 200 then
			return nil, "wttr.in answered " .. res.status
		end
		local now = zemi.json.decode(res.body).current_condition[1]
		return input.city .. ": " .. now.temp_C .. "C, " .. now.weatherDesc[1].value
	end,
})

-- the third failing bash run in a row asks the titles model why
zemi.event.on("tool.post", function(ev)
	if ev.tool ~= "bash" or ev.ok then
		return
	end
	local fails = (zemi.state.get("fails") or 0) + 1
	zemi.state.set("fails", fails % 3)
	if fails < 3 then
		return
	end
	local why = zemi.model.ask({ model = "titles", prompt = "Why does this keep failing?\n" .. ev.output }):wait()
	if why then
		zemi.notify.warn(why, "Tests keep failing")
	end
end)

return M
