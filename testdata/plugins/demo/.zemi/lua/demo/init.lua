local M = {}
local missing -- why the last read failed: a refused job, or its error

local function poll()
	local ok, err = pcall(zemi.job.start, {
		cmd = "git",
		args = { "rev-parse", "--abbrev-ref", "HEAD" },
		on_exit = function(res)
			-- the bug fail.lua finds: git printing nothing
			M.branch = res.code == 0 and res.stdout:match("^(%S+)"):sub(1, 30) or nil
		end,
	})
	missing = not ok and err or nil
end

zemi.health.add("demo", function()
	if missing then return { level = "warn", detail = missing } end
	return M.branch and "on branch " .. M.branch or "no branch"
end)

zemi.config.add("interval", {
	kind = "number",
	default = 30000,
	desc = "how often the branch is read, in ms",
})

-- read once zemi is ready: the user's init.lua sets it after this file runs
zemi.event.on("app.ready", function()
	zemi.timer.after(0, poll) -- an event handler has 50 ms, a timer callback 1 s
	local tick = zemi.timer.every(zemi.config.get("interval"), poll)
	zemi.config.watch("interval", function(ms)
		tick:stop()
		tick = zemi.timer.every(ms, poll)
	end)
end)

local function names(prefix)
	local out = {}
	for name in pairs(zemi.state.get("snips") or {}) do
		if name:sub(1, #prefix) == prefix then out[#out + 1] = name end
	end
	table.sort(out)
	return out
end

function M.pick()
	local snips, list = zemi.state.get("snips") or {}, names("")
	if #list == 0 then return zemi.notify.info("add one with /demo add <name> <text>", "Nothing to pick yet") end
	zemi.ui.select(list, { prompt = "Snippets" }, function(choice)
		if choice then zemi.agent.send(snips[choice]) end
	end)
end

zemi.command.add("demo", function(a)
	if a.list[1] ~= "add" then return M.pick() end
	local name, text = a.args:match("^%s*add%s+(%S+)%s+(.+)$")
	if not name then error("usage: /demo add <name> <text>", 0) end
	local snips = zemi.state.get("snips") or {}
	snips[name] = text
	zemi.state.set("snips", snips)
end, {
	desc = "save a snippet, or send one to the agent",
	nargs = "*",
	complete = function(_, line) return line:find("^/demo%s+%S*$") and { "add" } or {} end,
})

-- #name in a draft completes a snippet's name
zemi.composer.add({ id = "demo.snips", trigger = "#", items = names })

zemi.keymap.set("ctrl+x d", M.pick, { desc = "send a snippet" })
zemi.keymap.set("ctrl+x h", "help", { desc = "zemi's help, a Go command" })
zemi.keymap.set("ctrl+x r", "zemi.ui.scroll.bottom", { desc = "a host action" })

zemi.tool.add({
	name = "weather_now",
	description = "The weather in a city now.",
	input = { city = { type = "string" } },
	required = { "city" },
	execute = function(input)
		local res, err = zemi.http.get("https://wttr.in/" .. input.city:gsub(" ", "%%20") .. "?format=j1"):wait()
		if not res then return nil, err end
		if res.status ~= 200 then return nil, "wttr.in answered " .. res.status end
		local now = zemi.json.decode(res.body).current_condition[1]
		return input.city .. ": " .. now.temp_C .. "C, " .. now.weatherDesc[1].value
	end,
})

-- the third failing shell run in a row asks the titles model why
zemi.event.on("tool.post", function(ev)
	if ev.tool ~= "shell" or ev.ok then return end
	local fails = (zemi.state.get("fails") or 0) + 1
	zemi.state.set("fails", fails % 3)
	if fails < 3 then return end
	local why = zemi.model.ask({ model = "titles", prompt = "Why does this keep failing?\n" .. ev.output }):wait()
	if why then zemi.notify.warn(why, "Tests keep failing") end
end)

return M
