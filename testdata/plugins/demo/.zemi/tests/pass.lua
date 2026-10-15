local test = zemi.test
local branch = "main"
local git = test.stub(
	"lua.job.start",
	"git rev-parse *",
	function() return branch and { stdout = branch .. "\n" } or { code = 128, stderr = "fatal: not a git repository" } end
)
local body = zemi.json.encode({ current_condition = { { temp_C = "21", weatherDesc = { { value = "Sunny" } } } } })
local wttr = test.stub("lua.http.get", "https://wttr.in/*", { body = body })
local sent = test.stub("lua.agent.send")
local snips = require("./lib/snips")

test("the widget draws the branch git reports", function()
	local text, lines = test.render("demo.branch")
	test.eq(text, { "main" })
	test.eq(lines[1][1], { text = "main", hl = "Dim", click = "branch" })
	test.eq(git[1].args, { "rev-parse", "--abbrev-ref", "HEAD" })
end)

test("a checkout shows on the next poll", function()
	branch = "fix-login"
	test.advance(29999)
	test.eq(test.render("demo.branch"), { "main" })
	test.advance(1)
	test.eq(test.render("demo.branch"), { "fix-login" })
	test.eq(os.date("!%Y-%m-%d %H:%M:%S"), "2026-01-01 00:00:30")
end)

test("outside a repository the widget draws nothing", function()
	branch = nil
	test.advance(30000)
	test.eq(test.render("demo.branch"), {})
end)

test("a click on the branch toasts it", function()
	local before = zemi.notify.list()
	assert(test.click("demo.branch", "branch"))
	test.neq(zemi.notify.list(), before)
	test.eq(zemi.notify.list()[1].body, "main")
end)

test(
	"the cost widget reads the usage a session would have",
	function() test.eq(test.render("demo.cost", { usage = { cost = 0.5 } }), { "50 cents" }) end
)

test("a picked snippet goes to the agent", function()
	snips.add("review", "Review the diff")
	assert(test.run("demo"))
	local o = test.overlay()
	test.eq({ o.title, o.items }, { "Snippets", { "review" } })
	assert(test.answer("review"))
	test.eq(test.overlay(), nil)
	test.eq(sent[1].text, "Review the diff")
end)

test("esc on the key's picker sends nothing", function()
	assert(test.run("demo", "add review x"))
	assert(test.keys("ctrl+x d"))
	assert(test.answer(nil))
	test.eq(#sent, 0)
end)

test("an empty list says how to add one", function()
	assert(test.run("demo"))
	test.eq(test.overlay(), nil)
	test.eq(zemi.notify.list()[1].title, "Nothing to pick yet")
end)

test("add needs a name and text", function()
	local ok, err = test.run("demo", "add")
	test.eq({ ok, err }, { nil, "usage: /demo add <name> <text>" })
end)

test("the tool reports the weather", function()
	test.eq(test.tool("weather_now", { city = "New York" }), "New York: 21C, Sunny")
	test.eq(wttr[1].url, "https://wttr.in/New%20York?format=j1")
end)

test("a 503 is an error the model reads", function()
	test.stub("lua.http.get", "https://wttr.in/*", { status = 503 })
	test.eq({ test.tool("weather_now", { city = "Oslo" }) }, { nil, "weather_now: wttr.in answered 503" })
end)

test("a request that fails at the transport is an error too", function()
	test.stub("lua.http.get", "https://wttr.in/*", { error = "dial tcp: i/o timeout" })
	test.eq({ test.tool("weather_now", { city = "Oslo" }) }, { nil, "weather_now: dial tcp: i/o timeout" })
	local done
	zemi.http.get("https://wttr.in/Oslo", {
		on_done = function(res) done = res end,
	})
	test.advance(0)
	test.eq(done, { url = "https://wttr.in/Oslo", error = "dial tcp: i/o timeout" })
end)

test("the schema refuses a missing city", function()
	local _, err = test.tool("weather_now", {})
	test.contains(err, "city")
	test.eq(#wttr, 0)
end)

test("the third failing run asks the titles model", function()
	local asks = test.stub("lua.model.ask", "*", "The fixture is missing.")
	local function fail()
		return test.emit("tool.post", { tool = "shell", ok = false, output = "FAIL", input = { command = "go test" } })
	end
	fail()
	fail()
	test.eq(#asks, 0)
	test.eq(fail(), "FAIL")
	test.eq(asks[1].model, "titles")
	local n = zemi.notify.list()[1]
	test.eq({ n.level, n.title, n.body }, { "warn", "Tests keep failing", "The fixture is missing." })
end)

test("a refused job leaves a health row", function()
	test.stub("lua.job.start", "*", false)
	test.advance(30000)
	local row
	for _, r in ipairs(zemi.health.list()) do
		row = r.name == "demo" and r or row
	end
	test.eq(row.level, "warn")
	test.contains(row.detail, "permission ask required")
end)

test("a timer from another case is gone", function()
	zemi.timer.every(10, function() zemi.state.set("ticks", (zemi.state.get("ticks") or 0) + 1) end)
	test.advance(25)
	test.eq(zemi.state.get("ticks"), 2)
end)

test("each case starts fresh", function()
	test.advance(25)
	test.eq({ zemi.state.get("ticks"), zemi.state.get("snips") }, { nil, nil })
end)

test("the user's config and skills stay out", function()
	for _, c in ipairs(zemi.command.list()) do
		assert(c.name ~= "from-init", "the user's init.lua ran")
	end
	for _, s in ipairs(zemi.skills.list()) do
		assert(s.name ~= "from-home", "~/.agents/skills loaded")
	end
end)

test("gated calls fail closed, the stub answers first", function()
	test.eq(
		test.raises(function() zemi.http.get("https://example.com/") end),
		'.zemi/tests/pass.lua:149: lua.http.get https://example.com/: no test.stub answers it; add test.stub("lua.http.get", "https://example.com/*", { status = 200, body = "..." })'
	)
end)

test("drivers reach only what the plugin registered", function()
	for _, c in ipairs({
		{ test.keys, "ctrl+y", "ctrl+y is not a Lua key binding of plugin demo" },
		{ test.keys, "ctrl+x h", "ctrl+x h is not a Lua key binding" },
		{ test.keys, "ctrl+x r", "ctrl+x r is not a Lua key binding" },
		{ test.run, "help", "/help is not a command of plugin demo; it registered /demo" },
		{ test.render, "chat", "chat is not a widget of plugin demo; it registered demo.branch, demo.cost" },
		{ test.tool, "read", "read is not a tool of plugin demo; it registered weather_now" },
		{
			test.emit,
			"reflector.done",
			"reflector.done is not a handled event of plugin demo; it registered app.ready, tool.post",
		},
	}) do
		test.raises(function() c[1](c[2]) end, c[3])
	end
end)

test("a reply field no call reads is refused", function()
	for _, c in ipairs({
		{
			"lua.http.get",
			{ bdy = "x" },
			"bdy is not a field of a http reply, which takes body, error, headers, status",
		},
		{ "lua.job.start", { code = "1" }, "code must be a number, not a string" },
		{ "lua.agent.send", { text = "x" }, "text is not a field of a send reply, which takes no fields" },
	}) do
		test.raises(function() test.stub(c[1], "*", c[2]) end, c[3])
	end
end)

test("eq compares functions by identity and follows cycles", function()
	test.raises(function() test.eq({ a = 1, f = print }, { a = 1, f = tostring }) end, "f: got ")
	local a, b = {}, {}
	a.self, b.self = a, {}
	test.raises(function() test.eq(a, b) end)
	test.eq({ 0 / 0 }, { 0 / 0 })
	test.neq({ 1 }, { 2 })
end)

test("the command and the # source complete snippet names", function()
	snips.add("review", "Review the diff")
	snips.add("release", "Tag a release")
	test.eq(test.complete("/demo "), { "add" })
	test.eq(test.complete("/demo add re"), {})
	test.eq(test.complete("see #re"), { "release", "review" })
	test.eq(test.complete("#rev"), { "review" })
	test.raises(function() test.complete("see @x") end, "@ is not a completion trigger of plugin demo; it registered #")
end)

test("the interval setting moves the poll", function()
	assert(test.set("interval", 10000))
	branch = "fix-login"
	test.advance(10000)
	test.eq(test.render("demo.branch"), { "fix-login" })
	test.raises(function() test.set("ui.mouse", false) end, "ui.mouse is not a setting of plugin demo")
end)
